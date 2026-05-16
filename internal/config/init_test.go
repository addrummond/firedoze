package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitTOMLSSLIPHostUsesSSLIPAndRandomNetworks(t *testing.T) {
	text, err := InitTOML(InitOptions{SSLIPHost: "203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`base_domain = "203.0.113.10.sslip.io"`,
		`endpoint = "203.0.113.10:51820"`,
		`address = "fd`,
		`subnet = "fd`,
		`ipv4_subnet = "10.88.0.0/16"`,
		`[dns]`,
		`domain = "firedoze"`,
		"firedozed -wg-add-peer alice-laptop <ALICE_PUBLIC_KEY>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated config missing %q:\n%s", want, text)
		}
	}
}

func TestDNSListenIPDefaultsToFirstUsableVMNetworkIP(t *testing.T) {
	cfg := Default()
	cfg.VMNetwork.Subnet = "fd7a:115c:a1e0::/64"
	cfg.DNS.ListenIP = ""
	if err := cfg.applyDerivedDefaults(); err != nil {
		t.Fatal(err)
	}
	if cfg.DNS.ListenIP != "fd7a:115c:a1e0::1" {
		t.Fatalf("dns.listen_ip = %q, want fd7a:115c:a1e0::1", cfg.DNS.ListenIP)
	}
}

func TestInitTOMLHostSetsBaseDomain(t *testing.T) {
	text, err := InitTOML(InitOptions{Host: "203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`base_domain = "203.0.113.10"`,
		`endpoint = "203.0.113.10:51820"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated config missing %q:\n%s", want, text)
		}
	}
}

func TestInitTOMLHostBaseDomainCanBeOverridden(t *testing.T) {
	text, err := InitTOML(InitOptions{Host: "firedoze.example.com", BaseDomain: "dev.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`base_domain = "dev.example.com"`,
		`endpoint = "firedoze.example.com:51820"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated config missing %q:\n%s", want, text)
		}
	}
}

func TestInitTOMLHostAndSSLIPHostAreMutuallyExclusive(t *testing.T) {
	if _, err := InitTOML(InitOptions{Host: "example.com", SSLIPHost: "203.0.113.10"}); err == nil {
		t.Fatal("InitTOML allowed both Host and SSLIPHost")
	}
}

func TestInitFileRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firedoze.toml")
	if err := os.WriteFile(path, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InitFile(path, InitOptions{Host: "example.com"}); err == nil {
		t.Fatal("InitFile overwrote existing config without force")
	}
	if err := InitFile(path, InitOptions{Host: "example.com", Force: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WireGuard.Endpoint != "example.com:51820" {
		t.Fatalf("endpoint = %q, want example.com:51820", cfg.WireGuard.Endpoint)
	}
}
