package wireguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"firedoze/internal/config"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestGenerateClientKeyPair(t *testing.T) {
	pair, err := GenerateClientKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := wgtypes.ParseKey(pair.PrivateKey)
	if err != nil {
		t.Fatalf("private key is not parseable: %v", err)
	}
	publicKey, err := wgtypes.ParseKey(pair.PublicKey)
	if err != nil {
		t.Fatalf("public key is not parseable: %v", err)
	}
	if privateKey.PublicKey() != publicKey {
		t.Fatalf("public key = %s, want %s", publicKey, privateKey.PublicKey())
	}
}

func TestServerPublicKey(t *testing.T) {
	dir := t.TempDir()
	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.WireGuard.PrivateKeyFile = filepath.Join(dir, "wg.key")
	if err := os.WriteFile(cfg.WireGuard.PrivateKeyFile, []byte(privateKey.String()+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	got, err := ServerPublicKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != privateKey.PublicKey().String() {
		t.Fatalf("ServerPublicKey = %q, want %q", got, privateKey.PublicKey().String())
	}
}

func TestServerPublicKeyErrors(t *testing.T) {
	cfg := config.Default()
	cfg.WireGuard.PrivateKeyFile = filepath.Join(t.TempDir(), "missing.key")
	if _, err := ServerPublicKey(cfg); err == nil {
		t.Fatal("ServerPublicKey succeeded for missing key")
	}

	if err := os.WriteFile(cfg.WireGuard.PrivateKeyFile, []byte("not-a-key\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerPublicKey(cfg); err == nil {
		t.Fatal("ServerPublicKey succeeded for malformed key")
	}
}

func TestNewPeerSetup(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.WireGuard.Endpoint = "example.com:51820"
	cfg.WireGuard.PrivateKeyFile = filepath.Join(dir, "wg.key")
	publicKey := "1uDjQl5bwgSTZjHCXG3nUH1upZUhPz4PZvXeNwL7ESE="

	peer, output, err := NewPeerSetup(cfg, "alice-laptop", publicKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if peer.Name != "alice-laptop" {
		t.Fatalf("peer name = %q, want alice-laptop", peer.Name)
	}
	if len(peer.AllowedIPs) != 1 || peer.AllowedIPs[0] != "fd7a:115c:a1e1::2/128" {
		t.Fatalf("allowed IPs = %#v, want fd7a:115c:a1e1::2/128", peer.AllowedIPs)
	}

	for _, want := range []string{
		"# Firedoze client import config.",
		"#   firedoze server import <this-file> -default",
		`api_url = "http://[fd7a:115c:a1e1::1]"`,
		`client_public_key = "1uDjQl5bwgSTZjHCXG3nUH1upZUhPz4PZvXeNwL7ESE="`,
		"[wireguard]",
		`address = "fd7a:115c:a1e1::2/128"`,
		`endpoint = "example.com:51820"`,
		`allowed_ips = ["fd7a:115c:a1e1::1/128", "fd7a:115c:a1e0::/64"]`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "private_key") || strings.Contains(output, "<client-private-key>") {
		t.Fatalf("output exposed or requested a client private key:\n%s", output)
	}
	if strings.Contains(output, "\nname = ") {
		t.Fatalf("output should not force a client-side server profile name:\n%s", output)
	}

	info, err := os.Stat(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("server key mode = %v, want 0640", got)
	}
}

func TestAPIURL(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    string
	}{
		{address: "fd7a:115c:a1e1::1/64", want: "http://[fd7a:115c:a1e1::1]"},
		{address: "10.77.0.1/24", want: "http://10.77.0.1"},
	} {
		got, err := APIURL(tc.address)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("APIURL(%q) = %q, want %q", tc.address, got, tc.want)
		}
	}
	if _, err := APIURL("not-cidr"); err == nil {
		t.Fatal("APIURL accepted invalid CIDR")
	}
}

func TestNewPeerSetupSkipsUsedAllowedIPs(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.WireGuard.Endpoint = "example.com:51820"
	cfg.WireGuard.PrivateKeyFile = filepath.Join(dir, "wg.key")
	publicKey := "Kv3AQjMlBJIbgO3gxhwWyRLDaeInBG3nYJjnzTFROVU="
	cfg.WireGuard.Peers = []config.WGPeer{{
		Name:       "alice-laptop",
		PublicKey:  "1uDjQl5bwgSTZjHCXG3nUH1upZUhPz4PZvXeNwL7ESE=",
		AllowedIPs: []string{"fd7a:115c:a1e1::2/128"},
	}}

	peer, _, err := NewPeerSetup(cfg, "bob-laptop", publicKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(peer.AllowedIPs) != 1 || peer.AllowedIPs[0] != "fd7a:115c:a1e1::3/128" {
		t.Fatalf("allowed IPs = %#v, want fd7a:115c:a1e1::3/128", peer.AllowedIPs)
	}
}

func TestNewPeerSetupRejectsDuplicateAllowedIP(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.WireGuard.Endpoint = "example.com:51820"
	cfg.WireGuard.PrivateKeyFile = filepath.Join(dir, "wg.key")
	cfg.WireGuard.Peers = []config.WGPeer{{
		Name:       "alice-laptop",
		PublicKey:  "1uDjQl5bwgSTZjHCXG3nUH1upZUhPz4PZvXeNwL7ESE=",
		AllowedIPs: []string{"fd7a:115c:a1e1::2/128"},
	}}

	_, _, err := NewPeerSetup(cfg, "bob-laptop", "Kv3AQjMlBJIbgO3gxhwWyRLDaeInBG3nYJjnzTFROVU=", "fd7a:115c:a1e1:0:0:0:0:2/128")
	if err == nil || !strings.Contains(err.Error(), "already uses fd7a:115c:a1e1::2/128") {
		t.Fatalf("NewPeerSetup error = %v, want duplicate allowed IP error", err)
	}
}

func TestNewPeerSetupValidationErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.WireGuard.PrivateKeyFile = filepath.Join(dir, "wg.key")
	existingPublicKey := "1uDjQl5bwgSTZjHCXG3nUH1upZUhPz4PZvXeNwL7ESE="
	cfg.WireGuard.Peers = []config.WGPeer{{
		Name:       "alice-laptop",
		PublicKey:  existingPublicKey,
		AllowedIPs: []string{"fd7a:115c:a1e1::2/128"},
	}}

	tests := []struct {
		name      string
		peerName  string
		publicKey string
		allowedIP string
		want      string
	}{
		{name: "empty peer name", peerName: "", publicKey: "Kv3AQjMlBJIbgO3gxhwWyRLDaeInBG3nYJjnzTFROVU=", want: "peer name is required"},
		{name: "empty public key", peerName: "bob-laptop", publicKey: "", want: "peer public key is required"},
		{name: "invalid public key", peerName: "bob-laptop", publicKey: "not-a-key", want: "peer public key"},
		{name: "non cidr allowed ip", peerName: "bob-laptop", publicKey: "Kv3AQjMlBJIbgO3gxhwWyRLDaeInBG3nYJjnzTFROVU=", allowedIP: "fd7a:115c:a1e1::3", want: "allowed IP must be CIDR"},
		{name: "subnet allowed ip", peerName: "bob-laptop", publicKey: "Kv3AQjMlBJIbgO3gxhwWyRLDaeInBG3nYJjnzTFROVU=", allowedIP: "fd7a:115c:a1e1::/64", want: "allowed IP must be a single client address"},
		{name: "ipv4 allowed ip", peerName: "bob-laptop", publicKey: "Kv3AQjMlBJIbgO3gxhwWyRLDaeInBG3nYJjnzTFROVU=", allowedIP: "10.77.0.2/32", want: "allowed IP must be IPv6"},
		{name: "duplicate peer name", peerName: "alice-laptop", publicKey: "Kv3AQjMlBJIbgO3gxhwWyRLDaeInBG3nYJjnzTFROVU=", allowedIP: "fd7a:115c:a1e1::3/128", want: `wireguard peer "alice-laptop" already exists`},
		{name: "duplicate public key", peerName: "bob-laptop", publicKey: existingPublicKey, allowedIP: "fd7a:115c:a1e1::3/128", want: `wireguard peer "alice-laptop" already uses that public key`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := NewPeerSetup(cfg, tt.peerName, tt.publicKey, tt.allowedIP)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewPeerSetup error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNextPeerAllowedIPErrors(t *testing.T) {
	cfg := config.Default()
	cfg.WireGuard.Address = "10.77.0.1/24"
	if _, err := nextPeerAllowedIP(cfg); err == nil || !strings.Contains(err.Error(), "automatic peer addresses require an IPv6") {
		t.Fatalf("IPv4 nextPeerAllowedIP error = %v", err)
	}

	cfg.WireGuard.Address = "fd7a:115c:a1e1::1/128"
	if _, err := nextPeerAllowedIP(cfg); err == nil || !strings.Contains(err.Error(), "too small") {
		t.Fatalf("small subnet nextPeerAllowedIP error = %v", err)
	}

	cfg.WireGuard.Address = "fd7a:115c:a1e1::1/126"
	cfg.WireGuard.Peers = []config.WGPeer{
		{Name: "p2", PublicKey: "unused", AllowedIPs: []string{"fd7a:115c:a1e1::2/128"}},
		{Name: "p3", PublicKey: "unused", AllowedIPs: []string{"fd7a:115c:a1e1::3/128"}},
	}
	if _, err := nextPeerAllowedIP(cfg); err == nil || !strings.Contains(err.Error(), "no free wireguard peer addresses") {
		t.Fatalf("full subnet nextPeerAllowedIP error = %v", err)
	}
}

func TestEndpointFallback(t *testing.T) {
	cfg := config.Default()
	cfg.WireGuard.Endpoint = ""
	cfg.WireGuard.ListenPort = 51821
	if got, want := Endpoint(cfg), "<firedoze-public-host>:51821"; got != want {
		t.Fatalf("Endpoint fallback = %q, want %q", got, want)
	}
	cfg.WireGuard.Endpoint = "example.com:51820"
	if got, want := Endpoint(cfg), "example.com:51820"; got != want {
		t.Fatalf("Endpoint explicit = %q, want %q", got, want)
	}
}

func TestPeerClientAddressesFiltersNonHostCIDRs(t *testing.T) {
	got := peerClientAddresses([]string{
		"fd7a:115c:a1e1::2/128",
		"fd7a:115c:a1e1::/64",
		"bad",
		"10.77.0.2/32",
	})
	want := []string{"fd7a:115c:a1e1::2/128", "10.77.0.2/32"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("peerClientAddresses = %#v, want %#v", got, want)
	}
}

func TestWireGuardHostCIDRAndCompactStrings(t *testing.T) {
	if got, want := wireGuardHostCIDR("fd7a:115c:a1e1::1/64"), "fd7a:115c:a1e1::1/128"; got != want {
		t.Fatalf("wireGuardHostCIDR = %q, want %q", got, want)
	}
	if got, want := wireGuardHostCIDR("bad"), "bad"; got != want {
		t.Fatalf("wireGuardHostCIDR invalid = %q, want %q", got, want)
	}
	got := compactStrings([]string{"a", "", "b", "a", "b", "c"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("compactStrings = %#v, want a,b,c", got)
	}
}

func TestAppendPeer(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "firedoze.toml")
	if err := os.WriteFile(configPath, []byte(`base_domain = "dev.example.com"
default_http_port = 8080
state_dir = "`+dir+`/state"

[api]
port = 8081

[caddy]
http_port = 80
https_port = 443
internal_proxy_port = 18082

[metadata]
path = "`+dir+`/firedoze.db"

[wireguard]
interface = "fdwg0"
listen_port = 51820
address = "fd7a:115c:a1e1::1/64"
endpoint = "example.com:51820"
private_key_file = "`+dir+`/wg.key"

[host_firewall]
enabled = true
backend = "ip6tables"

[vm_network]
subnet = "fd7a:115c:a1e0::/64"

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
default_memory_min_mib = 512
default_memory_max_mib = 1024
default_disk_bytes = 4294967296
`), 0o640); err != nil {
		t.Fatal(err)
	}

	peer := config.WGPeer{
		Name:       "alice-laptop",
		PublicKey:  "1uDjQl5bwgSTZjHCXG3nUH1upZUhPz4PZvXeNwL7ESE=",
		AllowedIPs: []string{"fd7a:115c:a1e1::2/128"},
	}
	if err := AppendPeer(configPath, peer); err != nil {
		t.Fatal(err)
	}

	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.WireGuard.Peers) != 1 {
		t.Fatalf("peer count = %d, want 1", len(loaded.WireGuard.Peers))
	}
	if got := loaded.WireGuard.Peers[0].Name; got != "alice-laptop" {
		t.Fatalf("peer name = %q, want alice-laptop", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[[wireguard.peers]]") {
		t.Fatalf("config missing appended peer:\n%s", data)
	}
}
