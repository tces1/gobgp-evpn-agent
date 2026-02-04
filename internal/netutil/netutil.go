package netutil

import (
	"fmt"
	"net"
)

// IPv4ForInterface returns the first IPv4 address on the given interface.
func IPv4ForInterface(name string) (net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("lookup interface %s: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("list addresses for %s: %w", name, err)
	}
	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPNet); ok {
			if v4 := ip.IP.To4(); v4 != nil {
				return v4, nil
			}
		}
	}
	return nil, fmt.Errorf("no IPv4 found on interface %s", name)
}
