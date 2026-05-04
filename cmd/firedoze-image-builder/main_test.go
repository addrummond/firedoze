package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"github.com/klauspost/compress/zstd"
)

func TestRunCommandDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
		err  string
	}{
		{name: "no args", args: nil, want: 0, err: "Usage:"},
		{name: "help", args: []string{"help"}, want: 0, err: "Usage:"},
		{name: "dash help", args: []string{"-h"}, want: 0, err: "Usage:"},
		{name: "unknown", args: []string{"bogus"}, want: 2, err: "unknown command: bogus"},
		{name: "build error", args: []string{"build", "-arch", "arm64"}, want: 1, err: "only amd64 is supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, stderr, got := captureOutput(t, func() int {
				return run(tt.args)
			})
			if got != tt.want {
				t.Fatalf("run(%v) = %d, want %d", tt.args, got, tt.want)
			}
			if !strings.Contains(stderr, tt.err) {
				t.Fatalf("stderr = %q, want substring %q", stderr, tt.err)
			}
		})
	}
}

func TestBuildFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "help", args: []string{"-h"}, want: ""},
		{name: "unexpected args", args: []string{"extra"}, want: "unexpected arguments: extra"},
		{name: "unsupported arch", args: []string{"-arch", "arm64"}, want: "only amd64 is supported"},
		{name: "unsupported size suffix", args: []string{"-size", "5T"}, want: "unsupported size suffix"},
		{name: "too small", args: []string{"-size", "128M"}, want: "image size must be at least 512M"},
		{name: "root override checksum", args: []string{"-url", "https://example.test/root.tar.xz"}, want: "root artifact checksum is required"},
		{name: "kernel override checksum", args: []string{"-kernel-url", "https://example.test/vmlinuz"}, want: "kernel artifact checksum is required"},
		{name: "initrd override checksum", args: []string{"-initrd-url", "https://example.test/initrd"}, want: "initrd artifact checksum is required"},
		{name: "bad flag", args: []string{"-does-not-exist"}, want: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := build(tt.args)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("build(%v): %v", tt.args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("build(%v) error = %v, want containing %q", tt.args, err, tt.want)
			}
		})
	}
}

func TestDefaultArtifactURLs(t *testing.T) {
	if got := defaultImageURL("noble"); got != "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64-root.tar.xz" {
		t.Fatalf("defaultImageURL(noble) = %q", got)
	}
	if got := defaultImageURL("oracular"); got != "https://cloud-images.ubuntu.com/oracular/current/oracular-server-cloudimg-amd64-root.tar.xz" {
		t.Fatalf("defaultImageURL(oracular) = %q", got)
	}
	if got := defaultKernelURL("noble"); got != "https://cloud-images.ubuntu.com/releases/noble/release/unpacked/ubuntu-24.04-server-cloudimg-amd64-vmlinuz-generic" {
		t.Fatalf("defaultKernelURL(noble) = %q", got)
	}
	if got := defaultKernelURL("oracular"); got != "https://cloud-images.ubuntu.com/oracular/current/unpacked/oracular-server-cloudimg-amd64-vmlinuz-generic" {
		t.Fatalf("defaultKernelURL(oracular) = %q", got)
	}
	if got := defaultInitrdURL("noble"); got != "https://cloud-images.ubuntu.com/releases/noble/release/unpacked/ubuntu-24.04-server-cloudimg-amd64-initrd-generic" {
		t.Fatalf("defaultInitrdURL(noble) = %q", got)
	}
	if got := defaultInitrdURL("oracular"); got != "https://cloud-images.ubuntu.com/oracular/current/unpacked/oracular-server-cloudimg-amd64-initrd-generic" {
		t.Fatalf("defaultInitrdURL(oracular) = %q", got)
	}
}

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

func TestParseSizeErrors(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "abc"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseSize(value); err == nil {
				t.Fatalf("parseSize(%q) succeeded, want error", value)
			}
		})
	}
}

func TestApplyDefaultChecksums(t *testing.T) {
	root := ""
	kernel := ""
	initrd := ""
	if err := applyDefaultChecksums("noble", false, false, false, "", "", "", &root, &kernel, &initrd, false); err != nil {
		t.Fatalf("applyDefaultChecksums(default noble): %v", err)
	}
	if root != nobleRootSHA256 {
		t.Fatalf("root checksum = %q, want noble default", root)
	}
	if kernel != nobleKernelSHA256 {
		t.Fatalf("kernel checksum = %q, want noble default", kernel)
	}
	if initrd != nobleInitrdSHA256 {
		t.Fatalf("initrd checksum = %q, want noble default", initrd)
	}

	root, kernel, initrd = "", "", ""
	err := applyDefaultChecksums("noble", true, false, false, "", "", "", &root, &kernel, &initrd, false)
	if err == nil || !strings.Contains(err.Error(), "root artifact checksum is required") {
		t.Fatalf("override without root checksum err = %v, want checksum error", err)
	}

	root, kernel, initrd = "", "", ""
	if err := applyDefaultChecksums("mantic", false, false, false, "", "", "", &root, &kernel, &initrd, true); err != nil {
		t.Fatalf("insecure checksum override returned error: %v", err)
	}
	if root != "" || kernel != "" || initrd != "" {
		t.Fatalf("insecure non-default release checksums = %q/%q/%q, want empty", root, kernel, initrd)
	}
}

func TestApplyDefaultChecksumsOverrideCases(t *testing.T) {
	root, kernel, initrd := "", "", ""
	err := applyDefaultChecksums("noble", false, true, false, "", "", "", &root, &kernel, &initrd, false)
	if err == nil || !strings.Contains(err.Error(), "kernel artifact checksum is required") {
		t.Fatalf("kernel override err = %v, want checksum error", err)
	}
	if root != nobleRootSHA256 {
		t.Fatalf("root checksum = %q, want default even with kernel override", root)
	}

	root, kernel, initrd = "", "", ""
	err = applyDefaultChecksums("noble", false, false, true, "", "", "", &root, &kernel, &initrd, false)
	if err == nil || !strings.Contains(err.Error(), "initrd artifact checksum is required") {
		t.Fatalf("initrd override err = %v, want checksum error", err)
	}

	root, kernel, initrd = strings.Repeat("1", 64), "", ""
	err = applyDefaultChecksums("noble", false, true, true, "", "", "", &root, &kernel, &initrd, true)
	if err != nil {
		t.Fatalf("insecure overrides: %v", err)
	}
	if root != strings.Repeat("1", 64) || kernel != "" || initrd != "" {
		t.Fatalf("insecure checksums = %q/%q/%q, want custom root and empty overrides", root, kernel, initrd)
	}
}

func TestReadArtifactLocalChecksum(t *testing.T) {
	data := []byte("artifact")
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	got, err := readArtifact(path, "unused", sum, false)
	if err != nil {
		t.Fatalf("readArtifact(local): %v", err)
	}
	if got.name != path || !bytes.Equal(got.data, data) {
		t.Fatalf("artifact = (%q, %q), want (%q, %q)", got.name, got.data, path, data)
	}

	if _, err := readArtifact(path, "unused", "", false); err == nil {
		t.Fatal("readArtifact accepted a local artifact without checksum")
	}
	if _, err := readArtifact(path, "unused", strings.Repeat("0", 64), false); err == nil {
		t.Fatal("readArtifact accepted mismatched checksum")
	}
}

func TestReadBootArtifactLocal(t *testing.T) {
	data := []byte("kernel")
	path := filepath.Join(t.TempDir(), "vmlinuz")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	got, err := readBootArtifact(path, "unused", sum, false)
	if err != nil {
		t.Fatalf("readBootArtifact(local): %v", err)
	}
	if got.path != path || !bytes.Equal(got.data, data) {
		t.Fatalf("boot artifact = %#v, want path %q data %q", got, path, data)
	}
}

func TestReadBusyBoxStaticRejectsUnsupportedTarget(t *testing.T) {
	if _, err := readBusyBoxStatic("oracular", "amd64", false); err == nil || !strings.Contains(err.Error(), "busybox-static is pinned only") {
		t.Fatalf("readBusyBoxStatic(oracular) error = %v", err)
	}
	if _, err := readBusyBoxStatic("noble", "arm64", false); err == nil || !strings.Contains(err.Error(), "busybox-static is pinned only") {
		t.Fatalf("readBusyBoxStatic(arm64) error = %v", err)
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

func TestGuestOverlaySkipApplyDefaultsAndSymlinks(t *testing.T) {
	efs := newTestExt4(t)
	overlay := newGuestOverlay()
	if !overlay.shouldSkip("etc/passwd") {
		t.Fatal("overlay should skip etc/passwd")
	}
	if !overlay.shouldSkip("etc/systemd/system/cloud-init.service") {
		t.Fatal("overlay should skip masked cloud-init service")
	}
	if overlay.shouldSkip("etc/hostname") {
		t.Fatal("overlay should not skip unrelated files")
	}
	if err := overlay.apply(efs, timeNow()); err != nil {
		t.Fatalf("overlay.apply: %v", err)
	}
	assertExt4FileContains(t, efs, "etc/passwd", "ubuntu:x:1000:1000")
	assertExt4FileContains(t, efs, "etc/shadow", "ubuntu::19723")
	assertExt4FileContains(t, efs, "etc/group", "sudo:x:27:ubuntu")
	assertExt4FileContains(t, efs, "etc/gshadow", "sudo:*::ubuntu")
	assertExt4FileContains(t, efs, "etc/fstab", "/dev/vda / ext4")
	if target := readExt4Link(t, efs, "etc/systemd/system/cloud-init.service"); target != "/dev/null" {
		t.Fatalf("cloud-init mask = %q, want /dev/null", target)
	}
	if _, err := efs.Stat("etc/ssh/sshd_config.d/60-cloudimg-settings.conf"); err == nil {
		t.Fatal("empty overlay symlink target should not create a file")
	}
}

func TestGuestOverlayTextTransforms(t *testing.T) {
	overlay := newGuestOverlay()

	if !overlay.captureFile("etc/passwd", []byte("root:x:0:0:root:/root:/bin/bash\nubuntu:x:999:999:old:/old:/bin/sh\n")) {
		t.Fatal("etc/passwd was not captured")
	}
	passwd := string(overlay.files["etc/passwd"].data)
	if !strings.Contains(passwd, "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n") {
		t.Fatalf("passwd overlay did not rewrite ubuntu user:\n%s", passwd)
	}
	if strings.Contains(passwd, "ubuntu:x:999") {
		t.Fatalf("passwd overlay kept stale ubuntu entry:\n%s", passwd)
	}

	if !overlay.captureFile("etc/group", []byte("sudo:x:27:\n")) {
		t.Fatal("etc/group was not captured")
	}
	group := string(overlay.files["etc/group"].data)
	if !strings.Contains(group, "sudo:x:27:ubuntu\n") {
		t.Fatalf("group overlay did not add ubuntu to sudo:\n%s", group)
	}
	if !strings.Contains(group, "ubuntu:x:1000:ubuntu\n") {
		t.Fatalf("group overlay did not add ubuntu group:\n%s", group)
	}

	if overlay.captureFile("etc/hostname", []byte("vm")) {
		t.Fatal("unexpected capture for non-overlay file")
	}
}

func TestPopulateRootfsSyntheticTar(t *testing.T) {
	efs := newTestExt4(t)
	overlay := newGuestOverlay()
	tr := tar.NewReader(syntheticRootTar(t))

	artifacts, err := populateRootfs(efs, tr, overlay)
	if err != nil {
		t.Fatalf("populateRootfs: %v", err)
	}

	if got := readExt4File(t, efs, "etc/hostname"); got != "demo\n" {
		t.Fatalf("/etc/hostname = %q, want demo", got)
	}
	if got := readExt4File(t, efs, "usr/bin/tool"); got != "tool-data" {
		t.Fatalf("/usr/bin/tool = %q, want hardlink data", got)
	}
	if target := readExt4Link(t, efs, "usr/bin/tool-link"); target != "tool" {
		t.Fatalf("/usr/bin/tool-link -> %q, want tool", target)
	}
	if got := readExt4File(t, efs, "usr/bin/tool-hard"); got != "tool-data" {
		t.Fatalf("/usr/bin/tool-hard = %q, want copied hardlink data", got)
	}
	if _, err := efs.Stat("etc/passwd"); err == nil {
		t.Fatal("/etc/passwd was written before overlay apply; want captured only")
	}
	if !strings.Contains(string(overlay.files["etc/passwd"].data), "ubuntu:x:1000:1000") {
		t.Fatalf("captured passwd overlay = %q, want ubuntu rewrite", overlay.files["etc/passwd"].data)
	}
	if artifacts.kernel == nil || artifacts.kernel.path != "boot/vmlinuz-6.8.0" || string(artifacts.kernel.data) != "kernel-new" {
		t.Fatalf("kernel artifact = %#v, want newest boot kernel", artifacts.kernel)
	}
	if artifacts.initrd == nil || artifacts.initrd.path != "boot/initrd.img-6.8.0" || string(artifacts.initrd.data) != "initrd-new" {
		t.Fatalf("initrd artifact = %#v, want newest boot initrd", artifacts.initrd)
	}
}

func TestCustomizeGuestWritesGuestContract(t *testing.T) {
	efs := newTestExt4(t)
	overlay := newGuestOverlay()
	overlay.captureFile("etc/passwd", []byte("root:x:0:0:root:/root:/bin/bash\n"))
	overlay.captureFile("etc/group", []byte("root:x:0:\nsudo:x:27:\n"))
	overlay.captureFile("etc/shadow", []byte("root:*:19723:0:99999:7:::\n"))
	overlay.captureFile("etc/gshadow", []byte("root:*::\nsudo:*::\n"))

	if err := customizeGuest(efs, overlay, []byte("hello-binary"), []byte("busybox-binary")); err != nil {
		t.Fatalf("customizeGuest: %v", err)
	}

	assertExt4FileContains(t, efs, "etc/ssh/sshd_config.d/99-firedoze.conf", "PermitEmptyPasswords yes")
	assertExt4FileContains(t, efs, "usr/local/sbin/firedoze-guest-network", "firedoze.guest_ip")
	assertExt4FileContains(t, efs, "etc/systemd/system/firedoze-network.service", "ExecStart=/usr/local/sbin/firedoze-guest-network")
	assertExt4FileContains(t, efs, "etc/systemd/system/firedoze-sshd.service", "ExecStart=/usr/sbin/sshd -D -e")
	assertExt4FileContains(t, efs, "usr/local/bin/firedoze-hello-service", "ExecStart=/usr/local/bin/firedoze-hello $port$verbose")
	assertExt4FileContains(t, efs, "etc/profile.d/firedoze-prompt.sh", "firedoze_prompt_host")
	assertExt4FileContains(t, efs, "etc/cloud/cloud.cfg.d/99-firedoze.cfg", "datasource_list: [ None ]")
	assertExt4FileContains(t, efs, "etc/sudoers.d/90-firedoze-ubuntu", "ubuntu ALL=(ALL) NOPASSWD:ALL")
	assertExt4FileContains(t, efs, "etc/passwd", "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash")
	assertExt4FileContains(t, efs, "etc/group", "sudo:x:27:ubuntu")
	assertExt4FileContains(t, efs, "etc/fstab", "/dev/vda / ext4")

	if got := readExt4File(t, efs, "usr/local/bin/firedoze-hello"); got != "hello-binary" {
		t.Fatalf("firedoze-hello binary = %q, want injected binary", got)
	}
	if got := readExt4File(t, efs, "usr/bin/busybox"); got != "busybox-binary" {
		t.Fatalf("busybox binary = %q, want injected binary", got)
	}
	if target := readExt4Link(t, efs, "etc/systemd/system/multi-user.target.wants/firedoze-network.service"); target != "/etc/systemd/system/firedoze-network.service" {
		t.Fatalf("firedoze-network enable symlink -> %q", target)
	}
	if target := readExt4Link(t, efs, "etc/systemd/system/cloud-init.service"); target != "/dev/null" {
		t.Fatalf("cloud-init mask symlink -> %q", target)
	}
	info := statExt4(t, efs, "usr/local/bin/firedoze-hello")
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("firedoze-hello mode = %v, want 0755", got)
	}
	info = statExt4(t, efs, "etc/sudoers.d/90-firedoze-ubuntu")
	if got := info.Mode().Perm(); got != 0o440 {
		t.Fatalf("sudoers mode = %v, want 0440", got)
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

func TestExtractBusyBoxFromDebErrors(t *testing.T) {
	tests := []struct {
		name string
		deb  []byte
		want string
	}{
		{name: "bad header", deb: []byte("not ar"), want: "invalid deb ar header"},
		{name: "truncated member", deb: []byte("!<arch>\nshort"), want: "truncated deb ar member header"},
		{name: "no data", deb: makeDebAr(t, map[string][]byte{"debian-binary": []byte("2.0\n")}), want: "deb package has no data.tar member"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := extractBusyBoxFromDeb(tt.deb); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("extractBusyBoxFromDeb error = %v, want containing %q", err, tt.want)
			}
		})
	}

	var deb bytes.Buffer
	deb.WriteString("!<arch>\n")
	fmt.Fprintf(&deb, "%-16s%-12d%-6d%-6d%-8o%-10s`\n", "data.tar.zst/", 0, 0, 0, 0o100644, "bad")
	if _, err := extractBusyBoxFromDeb(deb.Bytes()); err == nil || !strings.Contains(err.Error(), "invalid deb ar member size") {
		t.Fatalf("extractBusyBoxFromDeb bad size error = %v", err)
	}

	deb.Reset()
	deb.WriteString("!<arch>\n")
	fmt.Fprintf(&deb, "%-16s%-12d%-6d%-6d%-8o%-10dxx", "data.tar.zst/", 0, 0, 0, 0o100644, 0)
	if _, err := extractBusyBoxFromDeb(deb.Bytes()); err == nil || !strings.Contains(err.Error(), "invalid deb ar member trailer") {
		t.Fatalf("extractBusyBoxFromDeb bad trailer error = %v", err)
	}

	deb.Reset()
	deb.WriteString("!<arch>\n")
	fmt.Fprintf(&deb, "%-16s%-12d%-6d%-6d%-8o%-10d`\n", "data.tar.zst/", 0, 0, 0, 0o100644, 10)
	deb.WriteString("short")
	if _, err := extractBusyBoxFromDeb(deb.Bytes()); err == nil || !strings.Contains(err.Error(), "truncated deb ar member") {
		t.Fatalf("extractBusyBoxFromDeb truncated data error = %v", err)
	}
}

func TestExtractBusyBoxFromDataTarGzip(t *testing.T) {
	data := compressedBusyBoxTarGzip(t, "busybox-gzip")
	got, err := extractBusyBoxFromDataTar("data.tar.gz", data)
	if err != nil {
		t.Fatalf("extractBusyBoxFromDataTar(gzip): %v", err)
	}
	if string(got) != "busybox-gzip" {
		t.Fatalf("busybox = %q, want gzip payload", got)
	}
}

func TestExtractBusyBoxFromDataTarErrors(t *testing.T) {
	if _, err := extractBusyBoxFromDataTar("data.tar.br", []byte("data")); err == nil || !strings.Contains(err.Error(), "unsupported deb data member compression") {
		t.Fatalf("unsupported compression error = %v", err)
	}
	if _, err := extractBusyBoxFromDataTar("data.tar.gz", []byte("not gzip")); err == nil {
		t.Fatal("bad gzip data succeeded")
	}
	var empty bytes.Buffer
	gz := gzip.NewWriter(&empty)
	if err := tar.NewWriter(gz).Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBusyBoxFromDataTar("data.tar.gz", empty.Bytes()); err == nil || !strings.Contains(err.Error(), "does not contain busybox") {
		t.Fatalf("missing busybox error = %v", err)
	}
	data := compressedTarGzip(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "bin/busybox", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := extractBusyBoxFromDataTar("data.tar.gz", data); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("busybox dir error = %v", err)
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

func TestExtractKernelELFGzipAndErrors(t *testing.T) {
	elf := []byte{0x7f, 'E', 'L', 'F', 'g', 'z'}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(elf); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	wrapped := append([]byte("boot-stub"), compressed.Bytes()...)
	got, err := extractKernelELF(wrapped)
	if err != nil {
		t.Fatalf("extractKernelELF(gzip): %v", err)
	}
	if !bytes.Equal(got, elf) {
		t.Fatalf("extractKernelELF(gzip) = %q, want %q", got, elf)
	}
	if _, err := extractKernelELF([]byte("no compressed elf here")); err == nil || !strings.Contains(err.Error(), "could not find") {
		t.Fatalf("extractKernelELF(no payload) error = %v", err)
	}
}

func TestFindRepoRoot(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot from package dir: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !strings.Contains(string(data), "module firedoze") {
		t.Fatalf("repo root = %q, go.mod err = %v", root, err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatal(err)
		}
	})
	empty := t.TempDir()
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	if _, err := findRepoRoot(); err == nil || !strings.Contains(err.Error(), "could not find firedoze repo root") {
		t.Fatalf("findRepoRoot outside repo error = %v", err)
	}
}

func TestBuildGuestHelloBinaryRejectsUnsupportedArch(t *testing.T) {
	if _, err := buildGuestHelloBinary("not-an-arch"); err == nil || !strings.Contains(err.Error(), "unsupported GOOS/GOARCH pair") {
		t.Fatalf("buildGuestHelloBinary unsupported arch error = %v", err)
	}
}

func TestTextHelpers(t *testing.T) {
	if got := ensureNamedLineText("", "ubuntu", "ubuntu:x:1000"); got != "ubuntu:x:1000\n" {
		t.Fatalf("ensureNamedLineText empty = %q", got)
	}
	text := "root:x:0\nubuntu:x:999\n"
	if got := ensureNamedLineText(text, "ubuntu", "ubuntu:x:1000"); got != "root:x:0\nubuntu:x:1000\n" {
		t.Fatalf("ensureNamedLineText replace = %q", got)
	}
	if got := ensureGroupMemberText("sudo:x:27:root,ubuntu\n", "sudo", "x", "27", "ubuntu"); got != "sudo:x:27:root,ubuntu\n" {
		t.Fatalf("ensureGroupMemberText duplicate = %q", got)
	}
	if got := splitCSV(""); got != nil {
		t.Fatalf("splitCSV empty = %#v, want nil", got)
	}
	if !stringInSlice([]string{"root", "ubuntu"}, "ubuntu") {
		t.Fatal("stringInSlice did not find ubuntu")
	}
	if stringInSlice([]string{"root"}, "ubuntu") {
		t.Fatal("stringInSlice found missing ubuntu")
	}
}

func TestExt4HelpersErrorsAndReplaceFile(t *testing.T) {
	efs := newTestExt4(t)
	now := timeNow()
	if err := writeFile(efs, "etc/notdir", []byte("file"), 0o644, 0, 0, now); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAll(efs, "etc/notdir/child", 0o755, 0, 0, now); err == nil || !strings.Contains(err.Error(), "exists and is not a directory") {
		t.Fatalf("mkdirAll through file error = %v", err)
	}
	if err := symlink(efs, "/target", "etc/link", now); err != nil {
		t.Fatalf("symlink create: %v", err)
	}
	if err := symlink(efs, "/target", "etc/link", now); err != nil {
		t.Fatalf("symlink same target: %v", err)
	}
	if err := symlink(efs, "/other", "etc/link", now); err == nil || !strings.Contains(err.Error(), "already points") {
		t.Fatalf("symlink retarget error = %v", err)
	}
	if err := symlink(efs, "/target", "etc/notdir", now); err == nil || !strings.Contains(err.Error(), "already exists and is not a symlink") {
		t.Fatalf("symlink over file error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "artifact")
	if err := replaceFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("replaceFile create: %v", err)
	}
	if err := replaceFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("replaceFile replace: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("replaceFile data = %q, want second", data)
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

func compressedBusyBoxTarGzip(t *testing.T, data string) []byte {
	t.Helper()
	return compressedTarGzip(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{
			Name:     "usr/bin/busybox",
			Typeflag: tar.TypeReg,
			Mode:     0o755,
			Size:     int64(len(data)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	})
}

func compressedTarGzip(t *testing.T, write func(*tar.Writer)) []byte {
	t.Helper()
	var dataTar bytes.Buffer
	tw := tar.NewWriter(&dataTar)
	write(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(dataTar.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func syntheticRootTar(t *testing.T) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	addDir := func(name string, mode int64) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: mode}); err != nil {
			t.Fatal(err)
		}
	}
	addFile := func(name string, mode int64, data string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	addSymlink := func(name string, target string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}); err != nil {
			t.Fatal(err)
		}
	}
	addHardlink := func(name string, target string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: target, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}

	addDir("etc", 0o755)
	addDir("usr", 0o755)
	addDir("usr/bin", 0o755)
	addDir("boot", 0o755)
	addFile("etc/hostname", 0o644, "demo\n")
	addFile("etc/passwd", 0o644, "root:x:0:0:root:/root:/bin/bash\n")
	addFile("usr/bin/tool", 0o755, "tool-data")
	addSymlink("usr/bin/tool-link", "tool")
	addHardlink("usr/bin/tool-hard", "usr/bin/tool")
	addFile("boot/vmlinuz-6.7.0", 0o644, "kernel-old")
	addFile("boot/vmlinuz-6.8.0", 0o644, "kernel-new")
	addFile("boot/initrd.img-6.7.0", 0o644, "initrd-old")
	addFile("boot/initrd.img-6.8.0", 0o644, "initrd-new")
	addFile("__MACOSX/ignored", 0o644, "ignored")
	addFile("../escape", 0o644, "ignored")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func newTestExt4(t *testing.T) *ext4.FileSystem {
	t.Helper()
	const size = 512 * 1024 * 1024
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	backend, err := file.CreateFromPath(rootfs, size)
	if err != nil {
		t.Fatal(err)
	}
	efs, err := ext4.Create(backend, size, 0, 512, &ext4.Params{
		SectorsPerBlock:       8,
		VolumeName:            "firedoze-test",
		ReservedBlocksPercent: 0,
	})
	if err != nil {
		_ = backend.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = efs.Close()
		_ = backend.Close()
	})
	return efs
}

func readExt4File(t *testing.T, efs *ext4.FileSystem, path string) string {
	t.Helper()
	data, err := efs.ReadFile(path)
	if err != nil {
		t.Fatalf("read /%s: %v", path, err)
	}
	return string(data)
}

func readExt4Link(t *testing.T, efs *ext4.FileSystem, path string) string {
	t.Helper()
	target, err := efs.ReadLink(path)
	if err != nil {
		t.Fatalf("readlink /%s: %v", path, err)
	}
	return target
}

func assertExt4FileContains(t *testing.T, efs *ext4.FileSystem, path string, want string) {
	t.Helper()
	got := readExt4File(t, efs, path)
	if !strings.Contains(got, want) {
		t.Fatalf("/%s =\n%s\nwant substring %q", path, got, want)
	}
}

func statExt4(t *testing.T, efs *ext4.FileSystem, path string) os.FileInfo {
	t.Helper()
	info, err := efs.Stat(path)
	if err != nil {
		t.Fatalf("stat /%s: %v", path, err)
	}
	return info
}

func timeNow() time.Time {
	return time.Unix(1_700_000_000, 0)
}

func captureOutput(t *testing.T, fn func() int) (stdout string, stderr string, code int) {
	t.Helper()
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter
	defer func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	}()
	code = fn()
	if err := stdoutWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrWriter.Close(); err != nil {
		t.Fatal(err)
	}
	stdoutBytes, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatal(err)
	}
	stderrBytes, err := io.ReadAll(stderrReader)
	if err != nil {
		t.Fatal(err)
	}
	return string(stdoutBytes), string(stderrBytes), code
}
