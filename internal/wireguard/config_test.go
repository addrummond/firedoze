package wireguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"firedoze/internal/config"
)

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
		"# WireGuard client config template for alice-laptop.",
		"# Save this on the client laptop and replace <client-private-key> locally.",
		"#   export FIREDOZE_API=http://[fd7a:115c:a1e1::1]",
		"[Interface]",
		"PrivateKey = <client-private-key>",
		"Address = fd7a:115c:a1e1::2/128",
		"Endpoint = example.com:51820",
		"AllowedIPs = fd7a:115c:a1e1::1/128, fd7a:115c:a1e0::/64",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if !strings.Contains(output, "<client-private-key>") {
		t.Fatalf("output missing client private key placeholder:\n%s", output)
	}

	info, err := os.Stat(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("server key mode = %v, want 0600", got)
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
default_memory_mib = 512
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
