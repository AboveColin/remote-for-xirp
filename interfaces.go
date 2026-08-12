package main

import (
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
)

// Which address to serve on, and which address to put in the pairing code, are the
// same question asked twice — and on a laptop with Wi-Fi, a VPN tunnel and a handful
// of virtual interfaces, guessing wrong means a QR code that cannot be reached.
//
// So: enumerate the interfaces, say which one the default route uses, and let the
// choice be made explicitly. `interfaces` prints them; `install --interface en0` or
// `--bind 192.168.1.50` picks one.

type iface struct {
	Name      string
	Addr      string
	Kind      string // wifi, ethernet, vpn, loopback, other
	Default   bool   // carries the default route
	Reachable bool   // a phone on that network could plausibly reach it
}

func kindOf(name string, ip net.IP) string {
	switch {
	case ip.IsLoopback():
		return "loopback"
	case strings.HasPrefix(name, "utun"), strings.HasPrefix(name, "ipsec"), strings.HasPrefix(name, "wg"):
		return "vpn"
	case strings.HasPrefix(name, "en"):
		// en0 is Wi-Fi on laptops and Ethernet on desktops; the distinction is not
		// worth a system call here, so both are reported as "en".
		return "ethernet/wifi"
	case strings.HasPrefix(name, "bridge"), strings.HasPrefix(name, "vmenet"), strings.HasPrefix(name, "vnic"):
		return "virtual"
	case strings.HasPrefix(name, "awdl"), strings.HasPrefix(name, "llw"):
		return "apple-wireless-direct"
	default:
		return "other"
	}
}

// defaultRouteInterface asks the routing table which interface the internet goes out
// of. That is the one a phone on the same network almost always shares.
func defaultRouteInterface() string {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "interface:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	return ""
}

func listInterfaces() ([]iface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	def := defaultRouteInterface()
	var out []iface
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			// IPv4 only. A link-local IPv6 address in a QR code is not something
			// anyone can usefully type or reach.
			if ip == nil || ip.To4() == nil {
				continue
			}
			k := kindOf(i.Name, ip)
			out = append(out, iface{
				Name:      i.Name,
				Addr:      ip.String(),
				Kind:      k,
				Default:   i.Name == def,
				Reachable: k != "loopback" && k != "apple-wireless-direct" && k != "virtual",
			})
		}
	}
	// Most useful first: the default route, then plausibly reachable, then the rest.
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Default != out[b].Default {
			return out[a].Default
		}
		if out[a].Reachable != out[b].Reachable {
			return out[a].Reachable
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

func cmdInterfaces() error {
	list, err := listInterfaces()
	if err != nil {
		return err
	}
	fmt.Printf("%-10s %-16s %-22s %s\n", "INTERFACE", "ADDRESS", "KIND", "NOTE")
	for _, i := range list {
		note := ""
		switch {
		case i.Default:
			note = "default route — usually the one you want"
		case !i.Reachable:
			note = "not reachable from another device"
		case i.Kind == "vpn":
			note = "reachable over the VPN only"
		}
		fmt.Printf("%-10s %-16s %-22s %s\n", i.Name, i.Addr, i.Kind, note)
	}
	fmt.Println("\nServe on one of them:")
	fmt.Println("  xirp-remote install --interface <name>     # e.g. --interface en0")
	fmt.Println("  xirp-remote install --bind <address>       # e.g. --bind 192.168.1.50")
	fmt.Println("  xirp-remote install --bind 0.0.0.0         # every interface (the default)")
	return nil
}

// resolveInterface turns an interface name into the address to bind and advertise.
func resolveInterface(name string) (string, error) {
	list, err := listInterfaces()
	if err != nil {
		return "", err
	}
	for _, i := range list {
		if i.Name == name {
			return i.Addr, nil
		}
	}
	var names []string
	for _, i := range list {
		names = append(names, i.Name)
	}
	return "", fmt.Errorf("no interface %q with an IPv4 address (have: %s)", name, strings.Join(names, ", "))
}

// advertiseAddress is the host to put in a pairing URL: the bound address when a
// specific one was chosen, otherwise the default route's.
func advertiseAddress(bindHost string) string {
	if bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" {
		return bindHost
	}
	list, err := listInterfaces()
	if err == nil {
		for _, i := range list {
			if i.Default {
				return i.Addr
			}
		}
		for _, i := range list {
			if i.Reachable {
				return i.Addr
			}
		}
	}
	return "127.0.0.1"
}
