package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
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
