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
