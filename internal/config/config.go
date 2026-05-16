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
	BaseDomain      string             `toml:"base_domain"`
	DefaultHTTPPort int                `toml:"default_http_port"`
	StateDir        string             `toml:"state_dir"`
	API             APIConfig          `toml:"api"`
	Caddy           CaddyConfig        `toml:"caddy"`
	Metadata        MetadataConfig     `toml:"metadata"`
	WireGuard       WireGuardConfig    `toml:"wireguard"`
	HostFirewall    HostFirewallConfig `toml:"host_firewall"`
	GuestControl    GuestControlConfig `toml:"guest_control"`
	VMNetwork       VMNetworkConfig    `toml:"vm_network"`
	DNS             DNSConfig          `toml:"dns"`
	SSH             SSHConfig          `toml:"ssh"`
	Idle            IdleConfig         `toml:"idle"`
	ColdStorage     ColdStorageConfig  `toml:"cold_storage"`
	Firecracker     FirecrackerConfig  `toml:"firecracker"`
}

type MetadataConfig struct {
	Path string `toml:"path"`
}

type APIConfig struct {
	Port int `toml:"port"`
}

type CaddyConfig struct {
	HTTPPort          int    `toml:"http_port"`
	HTTPSPort         int    `toml:"https_port"`
	InternalProxyPort int    `toml:"internal_proxy_port"`
	TLSMode           string `toml:"tls_mode"`
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
	if ip, _, err := net.ParseCIDR(c.Address); err != nil {
		return fmt.Errorf("wireguard.address must be CIDR: %w", err)
	} else if ip.To4() != nil {
		return fmt.Errorf("wireguard.address must be IPv6")
	}
	peerNames := map[string]int{}
	peerPublicKeys := map[string]int{}
	peerAllowedIPs := map[string]int{}
	for i, peer := range c.Peers {
		if peer.Name == "" {
			return fmt.Errorf("wireguard.peers[%d].name is required", i)
		}
		if first, ok := peerNames[peer.Name]; ok {
			return fmt.Errorf("wireguard.peers[%d].name duplicates wireguard.peers[%d].name %q", i, first, peer.Name)
		}
		peerNames[peer.Name] = i
		if peer.PublicKey == "" {
			return fmt.Errorf("wireguard.peers[%d].public_key is required", i)
		}
		if first, ok := peerPublicKeys[peer.PublicKey]; ok {
			return fmt.Errorf("wireguard.peers[%d].public_key duplicates wireguard.peers[%d].public_key", i, first)
		}
		peerPublicKeys[peer.PublicKey] = i
		if len(peer.AllowedIPs) == 0 {
			return fmt.Errorf("wireguard.peers[%d].allowed_ips is required", i)
		}
		for j, allowedIP := range peer.AllowedIPs {
			if ip, ipNet, err := net.ParseCIDR(allowedIP); err != nil {
				return fmt.Errorf("wireguard.peers[%d].allowed_ips[%d] must be CIDR: %w", i, j, err)
			} else if ip.To4() != nil {
				return fmt.Errorf("wireguard.peers[%d].allowed_ips[%d] must be IPv6", i, j)
			} else {
				normalizedAllowedIP := ipNet.String()
				if first, ok := peerAllowedIPs[normalizedAllowedIP]; ok {
					return fmt.Errorf("wireguard.peers[%d].allowed_ips[%d] duplicates wireguard.peers[%d] allowed IP %s", i, j, first, normalizedAllowedIP)
				}
				peerAllowedIPs[normalizedAllowedIP] = i
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

type HostFirewallConfig struct {
	Enabled bool   `toml:"enabled"`
	Backend string `toml:"backend"`
}

type GuestControlConfig struct {
	MemoryPort int `toml:"memory_port"`
}

type VMNetworkConfig struct {
	Subnet     string `toml:"subnet"`
	IPv4Subnet string `toml:"ipv4_subnet"`
}

type DNSConfig struct {
	Enabled         bool     `toml:"enabled"`
	Domain          string   `toml:"domain"`
	ListenIP        string   `toml:"listen_ip"`
	Port            int      `toml:"port"`
	TTLSeconds      int      `toml:"ttl_seconds"`
	UpstreamServers []string `toml:"upstream_servers"`
}

type SSHConfig struct {
	User          string `toml:"user"`
	WakeProxyPort int    `toml:"wake_proxy_port"`
}

type IdleConfig struct {
	CheckIntervalSeconds     int `toml:"check_interval_seconds"`
	DefaultSleepAfterSeconds int `toml:"default_sleep_after_seconds"`
}

type ColdStorageConfig struct {
	Dir                        string `toml:"dir"`
	ArchiveStoppedAfterSeconds int    `toml:"archive_stopped_after_seconds"`
}

type FirecrackerConfig struct {
	BinaryPath          string `toml:"binary_path"`
	BaseKernelPath      string `toml:"base_kernel_path"`
	BaseInitrdPath      string `toml:"base_initrd_path"`
	BaseRootfsPath      string `toml:"base_rootfs_path"`
	DefaultVCPUs        int    `toml:"default_vcpus"`
	DefaultMemoryMinMiB int    `toml:"default_memory_min_mib"`
	DefaultMemoryMaxMiB int    `toml:"default_memory_max_mib"`
	DefaultDiskBytes    int64  `toml:"default_disk_bytes"`
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
			HTTPPort:          80,
			HTTPSPort:         443,
			InternalProxyPort: 18082,
			TLSMode:           "auto",
		},
		Metadata: MetadataConfig{
			Path: "/var/lib/firedoze/firedoze.db",
		},
		WireGuard: WireGuardConfig{
			Interface:      "fdwg0",
			ListenPort:     51820,
			Address:        "fd7a:115c:a1e1::1/64",
			PrivateKeyFile: "/etc/firedoze/wg.key",
		},
		HostFirewall: HostFirewallConfig{
			Enabled: true,
			Backend: "ip6tables",
		},
		GuestControl: GuestControlConfig{
			MemoryPort: 18084,
		},
		VMNetwork: VMNetworkConfig{
			Subnet:     "fd7a:115c:a1e0::/64",
			IPv4Subnet: "10.88.0.0/16",
		},
		DNS: DNSConfig{
			Enabled:         true,
			Domain:          "firedoze",
			Port:            53,
			TTLSeconds:      30,
			UpstreamServers: []string{"1.1.1.1:53", "8.8.8.8:53"},
		},
		SSH: SSHConfig{
			User:          "ubuntu",
			WakeProxyPort: 18022,
		},
		Idle: IdleConfig{
			CheckIntervalSeconds:     30,
			DefaultSleepAfterSeconds: 6 * 60 * 60,
		},
		ColdStorage: ColdStorageConfig{
			ArchiveStoppedAfterSeconds: 30 * 24 * 60 * 60,
		},
		Firecracker: FirecrackerConfig{
			BinaryPath:          "/usr/lib/firedoze/firecracker",
			BaseKernelPath:      "/var/lib/firedoze/images/vmlinux.bin",
			BaseRootfsPath:      "/var/lib/firedoze/images/rootfs.ext4",
			DefaultVCPUs:        1,
			DefaultMemoryMinMiB: 384,
			DefaultMemoryMaxMiB: 1024,
			DefaultDiskBytes:    512 * 1024 * 1024,
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
			if err := cfg.applyDerivedDefaults(); err != nil {
				return Config{}, err
			}
			return cfg, cfg.Validate()
		}
		return Config{}, err
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if err := validateExplicitHostFirewall(data); err != nil {
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
	if c.ColdStorage.ArchiveStoppedAfterSeconds == 0 {
		c.ColdStorage.ArchiveStoppedAfterSeconds = 30 * 24 * 60 * 60
	}
	if c.VMNetwork.IPv4Subnet == "" {
		c.VMNetwork.IPv4Subnet = "10.88.0.0/16"
	}
	if c.DNS.Enabled {
		if c.DNS.Domain == "" {
			c.DNS.Domain = "firedoze"
		}
		if c.DNS.Port == 0 {
			c.DNS.Port = 53
		}
		if c.DNS.TTLSeconds == 0 {
			c.DNS.TTLSeconds = 30
		}
		if len(c.DNS.UpstreamServers) == 0 {
			c.DNS.UpstreamServers = []string{"1.1.1.1:53", "8.8.8.8:53"}
		}
		if c.DNS.ListenIP == "" {
			ip, err := FirstUsableIP(c.VMNetwork.Subnet)
			if err != nil {
				return err
			}
			c.DNS.ListenIP = ip.String()
		}
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
	if c.Caddy.HTTPSPort <= 0 || c.Caddy.HTTPSPort > 65535 {
		return fmt.Errorf("caddy.https_port must be between 1 and 65535")
	}
	if c.Caddy.InternalProxyPort <= 0 || c.Caddy.InternalProxyPort > 65535 {
		return fmt.Errorf("caddy.internal_proxy_port must be between 1 and 65535")
	}
	if c.Caddy.TLSMode == "" {
		return fmt.Errorf("caddy.tls_mode is required")
	}
	if c.Caddy.TLSMode != "auto" && c.Caddy.TLSMode != "behind_proxy" {
		return fmt.Errorf("caddy.tls_mode must be auto or behind_proxy")
	}
	if c.Metadata.Path == "" {
		return fmt.Errorf("metadata.path is required")
	}
	if err := c.WireGuard.Validate(); err != nil {
		return err
	}
	if err := c.HostFirewall.Validate(); err != nil {
		return err
	}
	if c.GuestControl.MemoryPort <= 0 || c.GuestControl.MemoryPort > 65535 {
		return fmt.Errorf("guest_control.memory_port must be between 1 and 65535")
	}
	if ip, _, err := net.ParseCIDR(c.VMNetwork.Subnet); err != nil {
		return fmt.Errorf("vm_network.subnet must be CIDR: %w", err)
	} else if ip.To4() != nil {
		return fmt.Errorf("vm_network.subnet must be IPv6")
	}
	if ip, _, err := net.ParseCIDR(c.VMNetwork.IPv4Subnet); err != nil {
		return fmt.Errorf("vm_network.ipv4_subnet must be CIDR: %w", err)
	} else if ip.To4() == nil {
		return fmt.Errorf("vm_network.ipv4_subnet must be IPv4")
	}
	if c.DNS.Enabled {
		if c.DNS.Domain == "" {
			return fmt.Errorf("dns.domain is required when dns is enabled")
		}
		if strings.Contains(c.DNS.Domain, "://") {
			return fmt.Errorf("dns.domain must be a DNS name, not a URL")
		}
		if net.ParseIP(c.DNS.ListenIP) == nil {
			return fmt.Errorf("dns.listen_ip must be an IP address")
		}
		if c.DNS.Port <= 0 || c.DNS.Port > 65535 {
			return fmt.Errorf("dns.port must be between 1 and 65535")
		}
		if c.DNS.TTLSeconds <= 0 {
			return fmt.Errorf("dns.ttl_seconds must be positive")
		}
		for i, upstream := range c.DNS.UpstreamServers {
			host, port, err := net.SplitHostPort(upstream)
			if err != nil || host == "" || port == "" {
				return fmt.Errorf("dns.upstream_servers[%d] must be host:port", i)
			}
		}
	}
	if c.SSH.User == "" {
		return fmt.Errorf("ssh.user is required")
	}
	if c.SSH.WakeProxyPort <= 0 || c.SSH.WakeProxyPort > 65535 {
		return fmt.Errorf("ssh.wake_proxy_port must be between 1 and 65535")
	}
	if c.Idle.CheckIntervalSeconds <= 0 {
		return fmt.Errorf("idle.check_interval_seconds must be positive")
	}
	if c.Idle.DefaultSleepAfterSeconds < 0 {
		return fmt.Errorf("idle.default_sleep_after_seconds cannot be negative")
	}
	if c.ColdStorage.ArchiveStoppedAfterSeconds < 0 {
		return fmt.Errorf("cold_storage.archive_stopped_after_seconds cannot be negative")
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
	if c.Firecracker.DefaultMemoryMinMiB <= 0 {
		return fmt.Errorf("firecracker.default_memory_min_mib must be positive")
	}
	if c.Firecracker.DefaultMemoryMaxMiB <= 0 {
		return fmt.Errorf("firecracker.default_memory_max_mib must be positive")
	}
	if c.Firecracker.DefaultMemoryMinMiB > c.Firecracker.DefaultMemoryMaxMiB {
		return fmt.Errorf("firecracker.default_memory_min_mib must be less than or equal to firecracker.default_memory_max_mib")
	}
	if c.Firecracker.DefaultDiskBytes <= 0 {
		return fmt.Errorf("firecracker.default_disk_bytes must be positive")
	}
	return nil
}

func (c HostFirewallConfig) Validate() error {
	if c.Enabled && c.Backend == "" {
		return fmt.Errorf("host_firewall.backend is required when host_firewall.enabled is true")
	}
	if c.Backend != "" && c.Backend != "ip6tables" {
		return fmt.Errorf("host_firewall.backend must be ip6tables")
	}
	return nil
}

func validateExplicitHostFirewall(data []byte) error {
	var raw struct {
		HostFirewall struct {
			Enabled *bool   `toml:"enabled"`
			Backend *string `toml:"backend"`
		} `toml:"host_firewall"`
	}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return err
	}
	enabled := true
	if raw.HostFirewall.Enabled != nil {
		enabled = *raw.HostFirewall.Enabled
	}
	if enabled && raw.HostFirewall.Backend == nil {
		return fmt.Errorf("host_firewall.backend is required when host_firewall.enabled is true")
	}
	return nil
}

func FirstUsableIP(cidr string) (net.IP, error) {
	ip, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse subnet: %w", err)
	}
	ip = append(net.IP(nil), ip...)
	if ip4 := ip.To4(); ip4 != nil {
		ip = append(net.IP(nil), ip4...)
		ip[3]++
	} else {
		ip = ip.To16()
		if ip == nil {
			return nil, fmt.Errorf("subnet must contain an IP address: %s", cidr)
		}
		for i := len(ip) - 1; i >= 0; i-- {
			ip[i]++
			if ip[i] != 0 {
				break
			}
		}
	}
	if !subnet.Contains(ip) {
		return nil, fmt.Errorf("subnet has no usable DNS IP: %s", cidr)
	}
	return ip, nil
}
