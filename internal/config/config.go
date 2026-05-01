package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const DefaultPath = "/etc/firedoze/firedoze.toml"

type Config struct {
	BaseDomain      string          `toml:"base_domain"`
	DefaultHTTPPort int             `toml:"default_http_port"`
	StateDir        string          `toml:"state_dir"`
	Metadata        MetadataConfig  `toml:"metadata"`
	WireGuard       WireGuardConfig `toml:"wireguard"`
	VMNetwork       VMNetworkConfig `toml:"vm_network"`
	SSH             SSHConfig       `toml:"ssh"`
}

type MetadataConfig struct {
	Path string `toml:"path"`
}

type WireGuardConfig struct {
	Interface      string   `toml:"interface"`
	ListenPort     int      `toml:"listen_port"`
	Address        string   `toml:"address"`
	PrivateKeyFile string   `toml:"private_key_file"`
	Peers          []WGPeer `toml:"peers"`
}

func (c WireGuardConfig) Validate() error {
	if c.Interface == "" {
		return fmt.Errorf("wireguard.interface is required")
	}
	if c.ListenPort <= 0 || c.ListenPort > 65535 {
		return fmt.Errorf("wireguard.listen_port must be between 1 and 65535")
	}
	if c.PrivateKeyFile == "" {
		return fmt.Errorf("wireguard.private_key_file is required")
	}
	if _, _, err := net.ParseCIDR(c.Address); err != nil {
		return fmt.Errorf("wireguard.address must be CIDR: %w", err)
	}
	for i, peer := range c.Peers {
		if peer.Name == "" {
			return fmt.Errorf("wireguard.peers[%d].name is required", i)
		}
		if peer.PublicKey == "" {
			return fmt.Errorf("wireguard.peers[%d].public_key is required", i)
		}
		if _, _, err := net.ParseCIDR(peer.AllowedIP); err != nil {
			return fmt.Errorf("wireguard.peers[%d].allowed_ip must be CIDR: %w", i, err)
		}
	}
	return nil
}

type WGPeer struct {
	Name      string `toml:"name"`
	PublicKey string `toml:"public_key"`
	AllowedIP string `toml:"allowed_ip"`
}

type VMNetworkConfig struct {
	Subnet string `toml:"subnet"`
}

type SSHConfig struct {
	User               string   `toml:"user"`
	AuthorizedKeyFiles []string `toml:"authorized_key_files"`
}

func Default() Config {
	return Config{
		BaseDomain:      "dev.example.com",
		DefaultHTTPPort: 8080,
		StateDir:        "/var/lib/firedoze",
		Metadata: MetadataConfig{
			Path: "/var/lib/firedoze/firedoze.db",
		},
		WireGuard: WireGuardConfig{
			Interface:      "fdwg0",
			ListenPort:     51820,
			Address:        "10.77.0.1/24",
			PrivateKeyFile: "/etc/firedoze/wg.key",
		},
		VMNetwork: VMNetworkConfig{
			Subnet: "10.88.0.0/16",
		},
		SSH: SSHConfig{
			User: "ubuntu",
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && path == DefaultPath {
			return cfg, cfg.Validate()
		}
		return Config{}, err
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.applyDerivedDefaults(); err != nil {
		return Config{}, err
	}
	return cfg, cfg.Validate()
}

func (c Config) TOML() string {
	var buf bytes.Buffer
	encoder := toml.NewEncoder(&buf)
	encoder.SetIndentTables(true)
	if err := encoder.Encode(c); err != nil {
		return ""
	}
	return buf.String()
}

func (c *Config) applyDerivedDefaults() error {
	if c.StateDir == "" {
		c.StateDir = "/var/lib/firedoze"
	}
	if c.Metadata.Path == "" {
		c.Metadata.Path = filepath.Join(c.StateDir, "firedoze.db")
	}
	return nil
}

func (c Config) Validate() error {
	if c.BaseDomain == "" {
		return fmt.Errorf("base_domain is required")
	}
	if strings.Contains(c.BaseDomain, "://") {
		return fmt.Errorf("base_domain must be a DNS name, not a URL")
	}
	if c.DefaultHTTPPort <= 0 || c.DefaultHTTPPort > 65535 {
		return fmt.Errorf("default_http_port must be between 1 and 65535")
	}
	if c.StateDir == "" {
		return fmt.Errorf("state_dir is required")
	}
	if c.Metadata.Path == "" {
		return fmt.Errorf("metadata.path is required")
	}
	if err := c.WireGuard.Validate(); err != nil {
		return err
	}
	if _, _, err := net.ParseCIDR(c.VMNetwork.Subnet); err != nil {
		return fmt.Errorf("vm_network.subnet must be CIDR: %w", err)
	}
	if c.SSH.User == "" {
		return fmt.Errorf("ssh.user is required")
	}
	return nil
}
