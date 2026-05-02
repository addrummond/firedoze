package main

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		value string
		want  int64
	}{
		{value: "512M", want: 512 * 1024 * 1024},
		{value: "4G", want: 4 * 1024 * 1024 * 1024},
		{value: "1024", want: 1024},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.value)
		if err != nil {
			t.Fatalf("parseSize(%q): %v", tt.value, err)
		}
		if got != tt.want {
			t.Fatalf("parseSize(%q) = %d, want %d", tt.value, got, tt.want)
		}
	}
}

func TestCleanTarPath(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
		want string
	}{
		{name: "./etc/os-release", ok: true, want: "etc/os-release"},
		{name: "/boot/vmlinuz-test", ok: true, want: "boot/vmlinuz-test"},
		{name: "../etc/passwd", ok: false},
		{name: "./._etc", ok: false},
		{name: "__MACOSX/file", ok: false},
	}
	for _, tt := range tests {
		got, ok := cleanTarPath(tt.name)
		if ok != tt.ok || got != tt.want {
			t.Fatalf("cleanTarPath(%q) = %q, %v; want %q, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRememberBootArtifactsOnlyUsesBootDirectory(t *testing.T) {
	var artifacts bootArtifacts
	artifacts.remember("usr/lib/needrestart/vmlinuz-get-version", []byte("not a kernel"))
	artifacts.remember("boot/vmlinuz-1.0.0", []byte("kernel"))
	artifacts.remember("boot/initrd.img-1.0.0", []byte("initrd"))

	if artifacts.kernel == nil || string(artifacts.kernel.data) != "kernel" {
		t.Fatalf("kernel artifact = %#v, want /boot kernel", artifacts.kernel)
	}
	if artifacts.initrd == nil || string(artifacts.initrd.data) != "initrd" {
		t.Fatalf("initrd artifact = %#v, want /boot initrd", artifacts.initrd)
	}
}

func TestVerifySHA256(t *testing.T) {
	const sum = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if err := verifySHA256("test", []byte("hello"), sum); err != nil {
		t.Fatalf("verifySHA256 returned error: %v", err)
	}
	if err := verifySHA256("test", []byte("hello"), strings.Repeat("0", 64)); err == nil {
		t.Fatal("verifySHA256 accepted a mismatched checksum")
	}
}
