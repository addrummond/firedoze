package host

import (
	"os"
	"path/filepath"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestEnsureWireGuardPrivateKeyCreatesAndReusesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "firedoze", "wg.key")

	key, err := ensureWireGuardPrivateKey(path)
	if err != nil {
		t.Fatalf("ensureWireGuardPrivateKey create: %v", err)
	}
	if key == (wgtypes.Key{}) {
		t.Fatal("created key is zero")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := wgtypes.ParseKey(string(data))
	if err != nil {
		t.Fatalf("created key file is not parseable: %v", err)
	}
	if parsed != key {
		t.Fatalf("key file = %s, want %s", parsed, key)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("key mode = %v, want 0640", got)
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("key directory mode = %v, want 0700", got)
	}

	reused, err := ensureWireGuardPrivateKey(path)
	if err != nil {
		t.Fatalf("ensureWireGuardPrivateKey reuse: %v", err)
	}
	if reused != key {
		t.Fatalf("reused key = %s, want %s", reused, key)
	}
}

func TestEnsureWireGuardPrivateKeyRejectsMalformedExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg.key")
	if err := os.WriteFile(path, []byte("not-a-wireguard-key\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureWireGuardPrivateKey(path); err == nil {
		t.Fatal("ensureWireGuardPrivateKey accepted malformed existing key")
	}
}

func TestLoopbackCIDR(t *testing.T) {
	tests := []struct {
		address string
		want    string
		ok      bool
	}{
		{address: "127.0.0.1", want: "127.0.0.1/32", ok: true},
		{address: "fd7a:115c:a1e0::1", want: "fd7a:115c:a1e0::1/128", ok: true},
		{address: "not-an-ip", ok: false},
	}
	for _, tt := range tests {
		got, err := loopbackCIDR(tt.address)
		if tt.ok && err != nil {
			t.Fatalf("loopbackCIDR(%q): %v", tt.address, err)
		}
		if !tt.ok && err == nil {
			t.Fatalf("loopbackCIDR(%q) succeeded, want error", tt.address)
		}
		if tt.ok && got != tt.want {
			t.Fatalf("loopbackCIDR(%q) = %q, want %q", tt.address, got, tt.want)
		}
	}
}
