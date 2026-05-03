package config

import (
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
