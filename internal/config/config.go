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
	BaseDomain      string            `toml:"base_domain"`
	DefaultHTTPPort int               `toml:"default_http_port"`
	StateDir        string            `toml:"state_dir"`
	API             APIConfig         `toml:"api"`
	Caddy           CaddyConfig       `toml:"caddy"`
	DNS             DNSConfig         `toml:"dns"`
	Metadata        MetadataConfig    `toml:"metadata"`
	WireGuard       WireGuardConfig   `toml:"wireguard"`
	VMNetwork       VMNetworkConfig   `toml:"vm_network"`
	SSH             SSHConfig         `toml:"ssh"`
	Idle            IdleConfig        `toml:"idle"`
	Firecracker     FirecrackerConfig `toml:"firecracker"`
}

type MetadataConfig struct {
	Path string `toml:"path"`
}

type APIConfig struct {
	Port int `toml:"port"`
}

type CaddyConfig struct {
	HTTPPort int `toml:"http_port"`
}

type DNSConfig struct {
	Port int `toml:"port"`
}

type WireGuardConfig struct {
	Interface      string   `toml:"interface"`
	ListenPort     int      `toml:"listen_port"`
	Address        string   `toml:"address"`
	Endpoint       string   `toml:"endpoint"`
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
		if len(peer.AllowedIPs) == 0 {
			return fmt.Errorf("wireguard.peers[%d].allowed_ips is required", i)
		}
		for j, allowedIP := range peer.AllowedIPs {
			if _, _, err := net.ParseCIDR(allowedIP); err != nil {
				return fmt.Errorf("wireguard.peers[%d].allowed_ips[%d] must be CIDR: %w", i, j, err)
			}
		}
	}
	return nil
}

type WGPeer struct {
	Name       string   `toml:"name"`
	PublicKey  string   `toml:"public_key"`
	AllowedIPs []string `toml:"allowed_ips"`
}

type VMNetworkConfig struct {
	Subnet string `toml:"subnet"`
}

type SSHConfig struct {
	User               string   `toml:"user"`
	AuthorizedKeyFiles []string `toml:"authorized_key_files"`
}

type IdleConfig struct {
	CheckIntervalSeconds     int `toml:"check_interval_seconds"`
	DefaultSleepAfterSeconds int `toml:"default_sleep_after_seconds"`
}

type FirecrackerConfig struct {
	BinaryPath       string `toml:"binary_path"`
	BaseKernelPath   string `toml:"base_kernel_path"`
	BaseRootfsPath   string `toml:"base_rootfs_path"`
	DefaultVCPUs     int    `toml:"default_vcpus"`
	DefaultMemoryMiB int    `toml:"default_memory_mib"`
	DefaultDiskBytes int64  `toml:"default_disk_bytes"`
}

func Default() Config {
	return Config{
		BaseDomain:      "dev.example.com",
		DefaultHTTPPort: 8080,
		StateDir:        "/var/lib/firedoze",
		API: APIConfig{
			Port: 8081,
		},
		Caddy: CaddyConfig{
			HTTPPort: 8080,
		},
		DNS: DNSConfig{
			Port: 53,
		},
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
		Idle: IdleConfig{
			CheckIntervalSeconds:     30,
			DefaultSleepAfterSeconds: 30 * 60,
		},
		Firecracker: FirecrackerConfig{
			BinaryPath:       "/usr/local/bin/firecracker",
			BaseKernelPath:   "/var/lib/firedoze/images/vmlinux.bin",
			BaseRootfsPath:   "/var/lib/firedoze/images/rootfs.ext4",
			DefaultVCPUs:     1,
			DefaultMemoryMiB: 128,
			DefaultDiskBytes: 512 * 1024 * 1024,
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
	if c.API.Port <= 0 || c.API.Port > 65535 {
		return fmt.Errorf("api.port must be between 1 and 65535")
	}
	if c.Caddy.HTTPPort <= 0 || c.Caddy.HTTPPort > 65535 {
		return fmt.Errorf("caddy.http_port must be between 1 and 65535")
	}
	if c.DNS.Port <= 0 || c.DNS.Port > 65535 {
		return fmt.Errorf("dns.port must be between 1 and 65535")
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
	if c.Idle.CheckIntervalSeconds <= 0 {
		return fmt.Errorf("idle.check_interval_seconds must be positive")
	}
	if c.Idle.DefaultSleepAfterSeconds < 0 {
		return fmt.Errorf("idle.default_sleep_after_seconds cannot be negative")
	}
	if c.Firecracker.BinaryPath == "" {
		return fmt.Errorf("firecracker.binary_path is required")
	}
	if c.Firecracker.BaseKernelPath == "" {
		return fmt.Errorf("firecracker.base_kernel_path is required")
	}
	if c.Firecracker.BaseRootfsPath == "" {
		return fmt.Errorf("firecracker.base_rootfs_path is required")
	}
	if c.Firecracker.DefaultVCPUs <= 0 {
		return fmt.Errorf("firecracker.default_vcpus must be positive")
	}
	if c.Firecracker.DefaultMemoryMiB <= 0 {
		return fmt.Errorf("firecracker.default_memory_mib must be positive")
	}
	if c.Firecracker.DefaultDiskBytes <= 0 {
		return fmt.Errorf("firecracker.default_disk_bytes must be positive")
	}
	return nil
}
