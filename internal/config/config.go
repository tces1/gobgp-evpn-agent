package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for the EVPN agent.
type Config struct {
	LogLevel      string      `yaml:"logLevel"`
	AdvertiseSelf bool        `yaml:"advertiseSelf"`
	CommunityASN  uint32      `yaml:"communityAsn"`
	GoBGP         GoBGPConfig `yaml:"gobgp"`
	Node          NodeConfig  `yaml:"node"`
	VNIs          []VNIConfig `yaml:"vnis"`
}

// GoBGPConfig defines how the agent talks to gobgpd.
type GoBGPConfig struct {
	Address string        `yaml:"address"`
	Timeout time.Duration `yaml:"timeout"`
}

// NodeConfig defines local interface settings.
type NodeConfig struct {
	LocalAddress      string `yaml:"localAddress"`
	LocalInterface    string `yaml:"localInterface"`
	VXLANPort         uint16 `yaml:"vxlanPort"`
	SkipLinkCleanup   bool   `yaml:"skipLinkCleanup"`
	AutoRecreateVxlan bool   `yaml:"autoRecreateVxlan"`
}

// VNIConfig represents a single overlay instance.
type VNIConfig struct {
	ID                uint32 `yaml:"id"`
	Community         string `yaml:"community"`
	Device            string `yaml:"device"`
	UnderlayInterface string `yaml:"underlayInterface"`
}

// Load reads configuration from a YAML file and applies defaults.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.GoBGP.Address == "" {
		cfg.GoBGP.Address = "127.0.0.1:50051"
	}
	if cfg.GoBGP.Timeout == 0 {
		cfg.GoBGP.Timeout = 5 * time.Second
	}
	if cfg.CommunityASN == 0 {
		cfg.CommunityASN = 0
	}
	if cfg.Node.LocalInterface == "" {
		cfg.Node.LocalInterface = "eth0"
	}
	if cfg.Node.VXLANPort == 0 {
		cfg.Node.VXLANPort = 4789
	}
	// Do not auto-recreate vxlan by default (deletion is treated as withdrawal).
	// AutoRecreateVxlan defaults to false.
	// Keep VNIs empty unless explicitly configured.
	for i := range cfg.VNIs {
		if cfg.VNIs[i].Device == "" {
			cfg.VNIs[i].Device = fmt.Sprintf("vxlan%d", cfg.VNIs[i].ID)
		}
		if cfg.VNIs[i].UnderlayInterface == "" {
			cfg.VNIs[i].UnderlayInterface = cfg.Node.LocalInterface
		}
		if cfg.VNIs[i].Community == "" && cfg.CommunityASN != 0 {
			cfg.VNIs[i].Community = fmt.Sprintf("%d:%d", cfg.CommunityASN, cfg.VNIs[i].ID)
		}
	}
}

// Validate performs basic sanity checks.
func (c *Config) Validate() error {
	if len(c.VNIs) == 0 {
		if c.CommunityASN == 0 {
			return fmt.Errorf("at least one VNI must be configured or communityAsn must be set")
		}
		return nil
	}
	for _, v := range c.VNIs {
		if v.ID == 0 {
			return fmt.Errorf("vni id must be > 0")
		}
		if v.Community == "" {
			if c.CommunityASN == 0 {
				return fmt.Errorf("vni %d missing community and communityAsn not set", v.ID)
			}
			continue
		}
		if _, err := ParseCommunity(v.Community); err != nil {
			return fmt.Errorf("vni %d invalid community %q: %w", v.ID, v.Community, err)
		}
	}
	if c.Node.LocalAddress != "" {
		if ip := net.ParseIP(c.Node.LocalAddress); ip == nil || ip.To4() == nil {
			return fmt.Errorf("node.localAddress must be IPv4 when set")
		}
	}
	return nil
}

// ParseCommunity parses "ASN:VALUE" into uint32.
func ParseCommunity(raw string) (uint32, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("format must be ASN:VALUE")
	}
	asn, err := strconv.ParseUint(parts[0], 10, 16)
	if err != nil {
		return 0, fmt.Errorf("asn: %w", err)
	}
	val, err := strconv.ParseUint(parts[1], 10, 16)
	if err != nil {
		return 0, fmt.Errorf("value: %w", err)
	}
	return uint32(asn<<16 | val), nil
}
