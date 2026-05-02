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

	output, err := NewPeerSetup(cfg, "alice-laptop", "10.77.0.2/32")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"[[wireguard.peers]]",
		`name = "alice-laptop"`,
		`allowed_ips = ["10.77.0.2/32"]`,
		"[Interface]",
		"Address = 10.77.0.2/32",
		"DNS = 10.77.0.1",
		"Endpoint = example.com:51820",
		"AllowedIPs = 10.77.0.1/32, 10.88.0.0/16",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "<client-private-key>") {
		t.Fatalf("output still contains client private key placeholder:\n%s", output)
	}

	info, err := os.Stat(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("server key mode = %v, want 0600", got)
	}
}
