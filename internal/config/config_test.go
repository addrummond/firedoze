package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWireGuardConfigRejectsDuplicatePeerNames(t *testing.T) {
	cfg := Default().WireGuard
	cfg.Peers = []WGPeer{
		{Name: "alice-laptop", PublicKey: "key-a", AllowedIPs: []string{"fd7a:115c:a1e1::2/128"}},
		{Name: "alice-laptop", PublicKey: "key-b", AllowedIPs: []string{"fd7a:115c:a1e1::3/128"}},
	}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("Validate error = %v, want duplicate peer name error", err)
	}
}

func TestWireGuardConfigRejectsDuplicatePeerPublicKeys(t *testing.T) {
	cfg := Default().WireGuard
	cfg.Peers = []WGPeer{
		{Name: "alice-laptop", PublicKey: "same-key", AllowedIPs: []string{"fd7a:115c:a1e1::2/128"}},
		{Name: "bob-laptop", PublicKey: "same-key", AllowedIPs: []string{"fd7a:115c:a1e1::3/128"}},
	}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "public_key duplicates") {
		t.Fatalf("Validate error = %v, want duplicate peer public key error", err)
	}
}

func TestWireGuardConfigRejectsDuplicatePeerAllowedIPs(t *testing.T) {
	cfg := Default().WireGuard
	cfg.Peers = []WGPeer{
		{Name: "alice-laptop", PublicKey: "key-a", AllowedIPs: []string{"fd7a:115c:a1e1::2/128"}},
		{Name: "bob-laptop", PublicKey: "key-b", AllowedIPs: []string{"fd7a:115c:a1e1::2/128"}},
	}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("Validate error = %v, want duplicate peer allowed IP error", err)
	}
}

func TestColdStorageDefaultThreshold(t *testing.T) {
	cfg := Default()
	if cfg.ColdStorage.Dir != "" {
		t.Fatalf("cold storage dir = %q, want empty default", cfg.ColdStorage.Dir)
	}
	if cfg.ColdStorage.ArchiveStoppedAfterSeconds != 30*24*60*60 {
		t.Fatalf("archive threshold = %d, want 30 days", cfg.ColdStorage.ArchiveStoppedAfterSeconds)
	}
}

func TestColdStorageRejectsNegativeThreshold(t *testing.T) {
	cfg := Default()
	cfg.DNS.ListenIP = "fd7a:115c:a1e0::1"
	cfg.ColdStorage.ArchiveStoppedAfterSeconds = -1
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "cold_storage.archive_stopped_after_seconds") {
		t.Fatalf("Validate error = %v, want cold storage threshold error", err)
	}
}

func TestLoadAppliesDerivedDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firedoze.toml")
	if err := os.WriteFile(path, []byte(`
state_dir = "/tmp/firedoze-state"

[metadata]
path = ""

[vm_network]
subnet = "fd7a:115c:a1e0::/64"

[dns]
enabled = true
domain = ""
listen_ip = ""
port = 0
ttl_seconds = 0
upstream_servers = []

[host_firewall]
enabled = true
backend = "ip6tables"

[cold_storage]
archive_stopped_after_seconds = 0
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Metadata.Path, "/tmp/firedoze-state/firedoze.db"; got != want {
		t.Fatalf("metadata.path = %q, want %q", got, want)
	}
	if got, want := cfg.DNS.Domain, "firedoze"; got != want {
		t.Fatalf("dns.domain = %q, want %q", got, want)
	}
	if got, want := cfg.DNS.ListenIP, "fd7a:115c:a1e0::1"; got != want {
		t.Fatalf("dns.listen_ip = %q, want %q", got, want)
	}
	if cfg.DNS.Port != 53 || cfg.DNS.TTLSeconds != 30 {
		t.Fatalf("dns port/ttl = %d/%d, want 53/30", cfg.DNS.Port, cfg.DNS.TTLSeconds)
	}
	if strings.Join(cfg.DNS.UpstreamServers, ",") != "1.1.1.1:53,8.8.8.8:53" {
		t.Fatalf("dns upstreams = %#v", cfg.DNS.UpstreamServers)
	}
	if got, want := cfg.ColdStorage.ArchiveStoppedAfterSeconds, 30*24*60*60; got != want {
		t.Fatalf("cold storage threshold = %d, want %d", got, want)
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firedoze.toml")
	if err := os.WriteFile(path, []byte("[bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted invalid TOML")
	}
}

func TestValidateAllowsDNSDisabledWithoutDNSFields(t *testing.T) {
	cfg := validConfig()
	cfg.DNS = DNSConfig{Enabled: false}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with DNS disabled: %v", err)
	}
}

func TestConfigValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "base domain required", mutate: func(c *Config) { c.BaseDomain = "" }, want: "base_domain is required"},
		{name: "base domain url", mutate: func(c *Config) { c.BaseDomain = "https://example.com" }, want: "base_domain must be a DNS name"},
		{name: "default http port", mutate: func(c *Config) { c.DefaultHTTPPort = 0 }, want: "default_http_port"},
		{name: "state dir", mutate: func(c *Config) { c.StateDir = "" }, want: "state_dir is required"},
		{name: "api port", mutate: func(c *Config) { c.API.Port = 70000 }, want: "api.port"},
		{name: "caddy http port", mutate: func(c *Config) { c.Caddy.HTTPPort = 0 }, want: "caddy.http_port"},
		{name: "caddy https port", mutate: func(c *Config) { c.Caddy.HTTPSPort = 0 }, want: "caddy.https_port"},
		{name: "caddy internal proxy port", mutate: func(c *Config) { c.Caddy.InternalProxyPort = 0 }, want: "caddy.internal_proxy_port"},
		{name: "caddy tls mode empty", mutate: func(c *Config) { c.Caddy.TLSMode = "" }, want: "caddy.tls_mode is required"},
		{name: "caddy tls mode invalid", mutate: func(c *Config) { c.Caddy.TLSMode = "off" }, want: "caddy.tls_mode must be auto or behind_proxy"},
		{name: "metadata path", mutate: func(c *Config) { c.Metadata.Path = "" }, want: "metadata.path is required"},
		{name: "host firewall backend required", mutate: func(c *Config) { c.HostFirewall.Backend = "" }, want: "host_firewall.backend is required"},
		{name: "host firewall backend invalid", mutate: func(c *Config) { c.HostFirewall.Backend = "nftables" }, want: "host_firewall.backend must be ip6tables"},
		{name: "vm subnet cidr", mutate: func(c *Config) { c.VMNetwork.Subnet = "bad" }, want: "vm_network.subnet must be CIDR"},
		{name: "vm subnet ipv4", mutate: func(c *Config) { c.VMNetwork.Subnet = "10.88.0.0/16" }, want: "vm_network.subnet must be IPv6"},
		{name: "dns domain empty", mutate: func(c *Config) { c.DNS.Domain = "" }, want: "dns.domain is required"},
		{name: "dns domain url", mutate: func(c *Config) { c.DNS.Domain = "https://firedoze" }, want: "dns.domain must be a DNS name"},
		{name: "dns listen ip", mutate: func(c *Config) { c.DNS.ListenIP = "bad" }, want: "dns.listen_ip must be an IP address"},
		{name: "dns port", mutate: func(c *Config) { c.DNS.Port = 0 }, want: "dns.port"},
		{name: "dns ttl", mutate: func(c *Config) { c.DNS.TTLSeconds = 0 }, want: "dns.ttl_seconds"},
		{name: "dns upstream", mutate: func(c *Config) { c.DNS.UpstreamServers = []string{"1.1.1.1"} }, want: "dns.upstream_servers[0]"},
		{name: "ssh user", mutate: func(c *Config) { c.SSH.User = "" }, want: "ssh.user is required"},
		{name: "ssh wake port", mutate: func(c *Config) { c.SSH.WakeProxyPort = 0 }, want: "ssh.wake_proxy_port"},
		{name: "idle interval", mutate: func(c *Config) { c.Idle.CheckIntervalSeconds = 0 }, want: "idle.check_interval_seconds"},
		{name: "idle sleep negative", mutate: func(c *Config) { c.Idle.DefaultSleepAfterSeconds = -1 }, want: "idle.default_sleep_after_seconds"},
		{name: "firecracker binary", mutate: func(c *Config) { c.Firecracker.BinaryPath = "" }, want: "firecracker.binary_path"},
		{name: "firecracker kernel", mutate: func(c *Config) { c.Firecracker.BaseKernelPath = "" }, want: "firecracker.base_kernel_path"},
		{name: "firecracker rootfs", mutate: func(c *Config) { c.Firecracker.BaseRootfsPath = "" }, want: "firecracker.base_rootfs_path"},
		{name: "firecracker vcpus", mutate: func(c *Config) { c.Firecracker.DefaultVCPUs = 0 }, want: "firecracker.default_vcpus"},
		{name: "firecracker memory min", mutate: func(c *Config) { c.Firecracker.DefaultMemoryMinMiB = 0 }, want: "firecracker.default_memory_min_mib"},
		{name: "firecracker memory max", mutate: func(c *Config) { c.Firecracker.DefaultMemoryMaxMiB = 0 }, want: "firecracker.default_memory_max_mib"},
		{name: "firecracker memory range", mutate: func(c *Config) { c.Firecracker.DefaultMemoryMinMiB = c.Firecracker.DefaultMemoryMaxMiB + 1 }, want: "firecracker.default_memory_min_mib"},
		{name: "firecracker disk", mutate: func(c *Config) { c.Firecracker.DefaultDiskBytes = 0 }, want: "firecracker.default_disk_bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateAllowsHostFirewallDisabledWithoutBackend(t *testing.T) {
	cfg := validConfig()
	cfg.HostFirewall = HostFirewallConfig{Enabled: false}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with host firewall disabled: %v", err)
	}
}

func TestLoadRequiresExplicitHostFirewallBackendWhenEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firedoze.toml")
	if err := os.WriteFile(path, []byte(`
base_domain = "dev.example.test"
default_http_port = 8080
state_dir = "/tmp/firedoze-state"

[api]
port = 8081

[caddy]
http_port = 80
https_port = 443
internal_proxy_port = 18082
tls_mode = "auto"

[metadata]
path = "/tmp/firedoze-state/firedoze.db"

[wireguard]
interface = "fdwg0"
listen_port = 51820
address = "fd7a:115c:a1e1::1/64"
private_key_file = "/tmp/firedoze-wg.key"

[vm_network]
subnet = "fd7a:115c:a1e0::/64"

[dns]
enabled = false

[ssh]
user = "ubuntu"
wake_proxy_port = 18022

[idle]
check_interval_seconds = 30
default_sleep_after_seconds = 21600

[firecracker]
binary_path = "/usr/local/bin/firecracker"
base_kernel_path = "/var/lib/firedoze/images/vmlinux.bin"
base_rootfs_path = "/var/lib/firedoze/images/rootfs.ext4"
default_vcpus = 1
default_memory_min_mib = 256
default_memory_max_mib = 1024
default_disk_bytes = 4294967296
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "host_firewall.backend is required") {
		t.Fatalf("Load missing host firewall backend error = %v", err)
	}
}

func TestLoadAllowsExplicitHostFirewallDisabledWithoutBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firedoze.toml")
	if err := os.WriteFile(path, []byte(`
base_domain = "dev.example.test"
default_http_port = 8080
state_dir = "/tmp/firedoze-state"

[api]
port = 8081

[caddy]
http_port = 80
https_port = 443
internal_proxy_port = 18082
tls_mode = "auto"

[metadata]
path = "/tmp/firedoze-state/firedoze.db"

[wireguard]
interface = "fdwg0"
listen_port = 51820
address = "fd7a:115c:a1e1::1/64"
private_key_file = "/tmp/firedoze-wg.key"

[host_firewall]
enabled = false

[vm_network]
subnet = "fd7a:115c:a1e0::/64"

[dns]
enabled = false

[ssh]
user = "ubuntu"
wake_proxy_port = 18022

[idle]
check_interval_seconds = 30
default_sleep_after_seconds = 21600

[firecracker]
binary_path = "/usr/local/bin/firecracker"
base_kernel_path = "/var/lib/firedoze/images/vmlinux.bin"
base_rootfs_path = "/var/lib/firedoze/images/rootfs.ext4"
default_vcpus = 1
default_memory_min_mib = 256
default_memory_max_mib = 1024
default_disk_bytes = 4294967296
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load with host firewall disabled: %v", err)
	}
}

func TestWireGuardValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*WireGuardConfig)
		want   string
	}{
		{name: "interface", mutate: func(c *WireGuardConfig) { c.Interface = "" }, want: "wireguard.interface is required"},
		{name: "listen port", mutate: func(c *WireGuardConfig) { c.ListenPort = 0 }, want: "wireguard.listen_port"},
		{name: "private key file", mutate: func(c *WireGuardConfig) { c.PrivateKeyFile = "" }, want: "wireguard.private_key_file"},
		{name: "address cidr", mutate: func(c *WireGuardConfig) { c.Address = "bad" }, want: "wireguard.address must be CIDR"},
		{name: "address ipv4", mutate: func(c *WireGuardConfig) { c.Address = "10.77.0.1/24" }, want: "wireguard.address must be IPv6"},
		{name: "peer name", mutate: func(c *WireGuardConfig) {
			c.Peers = []WGPeer{{PublicKey: "key", AllowedIPs: []string{"fd7a:115c:a1e1::2/128"}}}
		}, want: "name is required"},
		{name: "peer public key", mutate: func(c *WireGuardConfig) {
			c.Peers = []WGPeer{{Name: "alice", AllowedIPs: []string{"fd7a:115c:a1e1::2/128"}}}
		}, want: "public_key is required"},
		{name: "peer allowed ips empty", mutate: func(c *WireGuardConfig) { c.Peers = []WGPeer{{Name: "alice", PublicKey: "key"}} }, want: "allowed_ips is required"},
		{name: "peer allowed ip cidr", mutate: func(c *WireGuardConfig) {
			c.Peers = []WGPeer{{Name: "alice", PublicKey: "key", AllowedIPs: []string{"bad"}}}
		}, want: "must be CIDR"},
		{name: "peer allowed ip ipv4", mutate: func(c *WireGuardConfig) {
			c.Peers = []WGPeer{{Name: "alice", PublicKey: "key", AllowedIPs: []string{"10.77.0.2/32"}}}
		}, want: "must be IPv6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig().WireGuard
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestFirstUsableIP(t *testing.T) {
	tests := []struct {
		cidr string
		want string
		ok   bool
	}{
		{cidr: "10.88.0.0/16", want: "10.88.0.1", ok: true},
		{cidr: "fd7a:115c:a1e0::/64", want: "fd7a:115c:a1e0::1", ok: true},
		{cidr: "192.0.2.1/32", ok: false},
		{cidr: "bad", ok: false},
	}
	for _, tt := range tests {
		got, err := FirstUsableIP(tt.cidr)
		if tt.ok && err != nil {
			t.Fatalf("FirstUsableIP(%q): %v", tt.cidr, err)
		}
		if !tt.ok && err == nil {
			t.Fatalf("FirstUsableIP(%q) succeeded, want error", tt.cidr)
		}
		if tt.ok && got.String() != tt.want {
			t.Fatalf("FirstUsableIP(%q) = %s, want %s", tt.cidr, got, tt.want)
		}
	}
}

func TestRenderExamplePlaceholderComments(t *testing.T) {
	text := RenderExample(ConfigTemplate{})
	for _, want := range []string{
		"# EDIT PLACEHOLDER\nbase_domain = \"dev.example.com\"",
		"# EDIT PLACEHOLDER\nendpoint = \"YOUR_SERVER_PUBLIC_IP_OR_DNS:51820\"",
		"tls_mode = \"auto\"",
		"[host_firewall]\nenabled = true\n# Required when host firewalling is enabled.",
		"backend = \"ip6tables\"",
		"archive_stopped_after_seconds = 2592000",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("RenderExample missing %q:\n%s", want, text)
		}
	}

	text = RenderExample(ConfigTemplate{
		BaseDomain: "dev.example.test",
		Endpoint:   "example.com:51820",
		WGAddress:  "fd00::1/64",
		VMSubnet:   "fd01::/64",
	})
	if strings.Contains(text, "# EDIT PLACEHOLDER\nbase_domain") || strings.Contains(text, "# EDIT PLACEHOLDER\nendpoint") {
		t.Fatalf("custom RenderExample unexpectedly contains placeholder comments:\n%s", text)
	}
	for _, want := range []string{
		`base_domain = "dev.example.test"`,
		`endpoint = "example.com:51820"`,
		`address = "fd00::1/64"`,
		`subnet = "fd01::/64"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("custom RenderExample missing %q:\n%s", want, text)
		}
	}
}

func validConfig() Config {
	cfg := Default()
	cfg.DNS.ListenIP = "fd7a:115c:a1e0::1"
	return cfg
}
