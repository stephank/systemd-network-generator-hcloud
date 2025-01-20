package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"gopkg.in/yaml.v3"
)

// Metadata is the YAML root structure of Hetzner Cloud metadata.
type Metadata struct {
	NetworkConfig `yaml:"network-config"`
}

type NetworkConfig struct {
	Version int
	Config  []NetworkConfigEntry
}

type NetworkConfigEntry struct {
	Name       string
	Type       string
	MACAddress string `yaml:"mac_address"`
	Subnets    []Subnet
}

type Subnet struct {
	Ipv4           bool
	Ipv6           bool
	Type           string
	Address        string
	Gateway        string
	DNSNameservers []string `yaml:"dns_nameservers"`
}

const (
	// ConfigDir is the location for systemd-networkd runtime configs.
	ConfigDir = "/run/systemd/network"
	// ConfigPrefix is the common prefix for generated configs.
	// The ordering is systemd-network-generator + 10, so kernel commandline
	// options override Hetzner Cloud metadata.
	ConfigPrefix = "80-hetzner-"
	// ConfigSuffix is the extension of systemd-networkd config files.
	ConfigSuffix = ".network"
)

func writeConfigs(entries []NetworkConfigEntry) bool {
	ok := true

	if err := os.MkdirAll(ConfigDir, 0755); err != nil {
		log.Printf("creating \"%s\": %v", ConfigDir, err)
		ok = false
	}

	// Clean up old configs.
	if dirEntries, err := os.ReadDir(ConfigDir); err != nil {
		log.Printf("reading directory \"%s\": %v", ConfigDir, err)
		ok = false
	} else {
		for _, dirEntry := range dirEntries {
			name := dirEntry.Name()
			if strings.HasPrefix(name, ConfigPrefix) && strings.HasSuffix(name, ConfigSuffix) {
				configPath := ConfigDir + "/" + name
				if err := os.Remove(configPath); err != nil {
					log.Printf("removing \"%s\": %v", configPath, err)
					ok = false
				}
			}
		}
	}

	// Write new configs.
	for _, entry := range entries {
		if entry.Type != "physical" {
			continue
		}

		// Interface names don't match, so match by MAC address.
		config := "[Match]\n"
		config += fmt.Sprintf("MACAddress=%s\n", entry.MACAddress)
		config += "\n"

		config += "[Network]\n"

		// The metadata service uses an IPv4 link-local address. In practice, it
		// works without this, but that's probably a quirk of the virtual interface.
		// (As for IPv6 link-local addressing, that's typically always enabled.)
		config += "LinkLocalAddressing=yes\n"

		wantDHCPv4 := false
		wantDHCPv6 := false
		for _, subnet := range entry.Subnets {
			if subnet.Type == "dhcp" {
				wantDHCPv4 = wantDHCPv4 || subnet.Ipv4
				wantDHCPv6 = wantDHCPv6 || subnet.Ipv6
			}
			if subnet.Address != "" {
				config += fmt.Sprintf("Address=%s\n", subnet.Address)
			}
			if subnet.Gateway != "" {
				config += fmt.Sprintf("Gateway=%s\n", subnet.Gateway)
			}
			for _, ns := range subnet.DNSNameservers {
				config += fmt.Sprintf("DNS=%s\n", ns)
			}
		}
		if wantDHCPv4 {
			if wantDHCPv6 {
				config += "DHCP=yes\n"
			} else {
				config += "DHCP=ipv4\n"
			}
		} else if wantDHCPv6 {
			config += "DHCP=ipv6\n"
		}

		configPath := ConfigDir + "/" + ConfigPrefix + entry.Name + ConfigSuffix
		if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
			log.Printf("writing \"%s\": %v", configPath, err)
			ok = false
		}
	}

	return ok
}

func main() {
	// Check if we have link on an ethernet interface.
	links, err := netlink.LinkList()
	if err != nil {
		log.Printf("listing network interfaces: %v", err)
		links = nil
	}

	var haveLink bool
	var firstEn netlink.Link
	var firstEnName string
	for _, link := range links {
		linkAttrs := link.Attrs()
		if strings.HasPrefix(linkAttrs.Name, "en") || strings.HasPrefix(linkAttrs.Name, "eth") {
			if (linkAttrs.Flags & net.FlagUp) != 0 {
				haveLink = true
				break
			}
			if firstEn == nil {
				firstEn = link
				firstEnName = linkAttrs.Name
			}
		}
	}

	// If there is no link, temporarily bring up the first
	// ethernet interface with a link-local address.
	llAddr := &netlink.Addr{
		Scope: int(netlink.SCOPE_LINK),
		IPNet: &net.IPNet{
			IP:   []byte{169, 254, 0, 1},
			Mask: []byte{255, 255, 0, 0},
		},
	}
	if !haveLink {
		if firstEn == nil {
			log.Printf("no ethernet interfaces")
		}
		if firstEn != nil {
			if err := netlink.LinkSetUp(firstEn); err != nil {
				log.Printf("bringing up %s: %v", firstEnName, err)
				firstEn = nil
			}
		}
		if firstEn != nil {
			if err := netlink.AddrAdd(firstEn, llAddr); err != nil {
				log.Printf("adding link-local address to %s: %v", firstEnName, err)
				netlink.LinkSetDown(firstEn)
				firstEn = nil
			}
		}
	}

	// Fetch metadata.
	ok := false
	client := &http.Client{
		// Should respond quick, so reasonably short timeout.
		// Don't want to immobilize system startup because of an outage.
		Timeout: 10 * time.Second,
	}
	var metadata Metadata
	if resp, err := client.Get("http://169.254.169.254/hetzner/v1/metadata"); err != nil {
		log.Printf("fetching metadata: %v", err)
	} else {
		if resp.StatusCode != 200 {
			log.Printf("fetching metadata: unexpected http status %d", resp.StatusCode)
		} else if body, err := io.ReadAll(resp.Body); err != nil {
			log.Printf("fetching metadata: read error: %v", err)
		} else if err := yaml.Unmarshal(body, &metadata); err != nil {
			log.Printf("fetching metadata: parse error: %v", err)
		} else if metadata.NetworkConfig.Version != 1 {
			log.Printf("fetching metadata: unknown network-config version %d", metadata.NetworkConfig.Version)
		} else {
			ok = writeConfigs(metadata.NetworkConfig.Config)
		}
		resp.Body.Close()
	}

	// Bring down the interface again.
	if !haveLink && firstEn != nil {
		netlink.AddrDel(firstEn, llAddr)
		netlink.LinkSetDown(firstEn)
	}

	if !ok {
		os.Exit(1)
	}
}
