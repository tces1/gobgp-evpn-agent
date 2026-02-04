package vxlan

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"

	"gobgp-evpn-agent/internal/config"
)

var broadcastMAC = net.HardwareAddr{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

// Manager owns one VXLAN interface and its FDB entries.
type Manager struct {
	cfg      config.VNIConfig
	port     uint16
	localIP  net.IP
	link     *netlink.Vxlan
	linkOnce bool
}

func NewManager(cfg config.VNIConfig, port uint16, localIP net.IP) *Manager {
	return &Manager{cfg: cfg, port: port, localIP: localIP}
}

// LoadLink verifies the VXLAN interface exists and refreshes cached handle.
func (m *Manager) LoadLink() error {
	link, err := netlink.LinkByName(m.cfg.Device)
	if err != nil {
		m.link = nil
		m.linkOnce = false
		return err
	}
	vx, ok := link.(*netlink.Vxlan)
	if !ok {
		return fmt.Errorf("link %s exists but is not vxlan", m.cfg.Device)
	}
	m.link = vx
	m.linkOnce = true
	return nil
}

// SyncFDB ensures the FDB matches the desired remote VTEPs.
func (m *Manager) SyncFDB(desired map[string]struct{}) error {
	if err := m.LoadLink(); err != nil {
		return err
	}
	current, err := m.currentFDB()
	if err != nil {
		return err
	}
	for dst := range desired {
		if _, ok := current[dst]; !ok {
			if err := m.add(dst); err != nil {
				return err
			}
		}
	}
	for dst := range current {
		if _, ok := desired[dst]; !ok {
			if err := m.del(dst); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close removes the VXLAN interface if it was created by the manager.
func (m *Manager) Close() error {
	if m.link == nil {
		return nil
	}
	return netlink.LinkDel(m.link)
}

func (m *Manager) currentFDB() (map[string]struct{}, error) {
	res := make(map[string]struct{})
	if m.link == nil {
		return res, errors.New("vxlan link not ready")
	}
	neigh, err := netlink.NeighList(m.link.Attrs().Index, syscall.AF_BRIDGE)
	if err != nil {
		return nil, fmt.Errorf("list fdb: %w", err)
	}
	for _, n := range neigh {
		if n.IP == nil || n.HardwareAddr == nil {
			continue
		}
		if len(n.HardwareAddr) != len(broadcastMAC) {
			continue
		}
		match := true
		for i := range broadcastMAC {
			if n.HardwareAddr[i] != broadcastMAC[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		res[n.IP.String()] = struct{}{}
	}
	return res, nil
}

func (m *Manager) add(dst string) error {
	ip := net.ParseIP(dst)
	if ip == nil {
		return fmt.Errorf("invalid dst ip %q", dst)
	}
	n := &netlink.Neigh{
		LinkIndex:    m.link.Attrs().Index,
		State:        netlink.NUD_PERMANENT,
		Family:       syscall.AF_BRIDGE,
		Flags:        netlink.NTF_SELF,
		IP:           ip,
		HardwareAddr: broadcastMAC,
	}
	// Use Append to allow multiple flood entries (same MAC, different dst) without replace errors.
	if err := netlink.NeighAppend(n); err != nil {
		return fmt.Errorf("add fdb %s: %w", dst, err)
	}
	return nil
}

func (m *Manager) del(dst string) error {
	ip := net.ParseIP(dst)
	if ip == nil {
		return fmt.Errorf("invalid dst ip %q", dst)
	}
	n := &netlink.Neigh{
		LinkIndex:    m.link.Attrs().Index,
		Family:       syscall.AF_BRIDGE,
		Flags:        netlink.NTF_SELF,
		IP:           ip,
		HardwareAddr: broadcastMAC,
	}
	if err := netlink.NeighDel(n); err != nil {
		return fmt.Errorf("del fdb %s: %w", dst, err)
	}
	return nil
}
