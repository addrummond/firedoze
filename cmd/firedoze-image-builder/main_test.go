package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
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

func TestTarFileModePreservesSpecialBits(t *testing.T) {
	got := tarFileMode(&tar.Header{Mode: 0o6755})
	want := os.ModeSetuid | os.ModeSetgid | 0o755
	if got != want {
		t.Fatalf("tarFileMode(06755) = %v, want %v", got, want)
	}

	got = tarFileMode(&tar.Header{Mode: 0o1777})
	want = os.ModeSticky | 0o777
	if got != want {
		t.Fatalf("tarFileMode(01777) = %v, want %v", got, want)
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

func TestExtractBusyBoxFromDeb(t *testing.T) {
	const want = "busybox-binary"
	var dataTar bytes.Buffer
	tw := tar.NewWriter(&dataTar)
	if err := tw.WriteHeader(&tar.Header{
		Name: "./bin/busybox",
		Mode: 0o755,
		Size: int64(len(want)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(want)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var compressed bytes.Buffer
	zw, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(dataTar.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	deb := makeDebAr(t, map[string][]byte{
		"debian-binary": []byte("2.0\n"),
		"data.tar.zst":  compressed.Bytes(),
	})
	got, err := extractBusyBoxFromDeb(deb)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("busybox = %q, want %q", got, want)
	}
}

func TestExtractKernelELF(t *testing.T) {
	elf := []byte{0x7f, 'E', 'L', 'F', 't', 'e', 's', 't'}
	got, err := extractKernelELF(elf)
	if err != nil {
		t.Fatalf("extractKernelELF returned error for raw ELF: %v", err)
	}
	if !bytes.Equal(got, elf) {
		t.Fatalf("extractKernelELF(raw ELF) = %q, want %q", got, elf)
	}

	var compressed bytes.Buffer
	zw, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(elf); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	wrapped := append([]byte("boot-stub"), compressed.Bytes()...)
	got, err = extractKernelELF(wrapped)
	if err != nil {
		t.Fatalf("extractKernelELF returned error for zstd payload: %v", err)
	}
	if !bytes.Equal(got, elf) {
		t.Fatalf("extractKernelELF(zstd payload) = %q, want %q", got, elf)
	}
}

func makeDebAr(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	for _, name := range []string{"debian-binary", "data.tar.zst"} {
		data, ok := members[name]
		if !ok {
			continue
		}
		if len(name) > 15 {
			t.Fatalf("ar member name too long: %s", name)
		}
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8o%-10d`\n", name+"/", 0, 0, 0, 0o100644, len(data))
		out.Write(data)
		if len(data)%2 != 0 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}
