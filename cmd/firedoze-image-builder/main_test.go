package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path"
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
		{name: "build error", args: []string{"build", "-does-not-exist"}, want: 1, err: "flag provided but not defined"},
		{name: "install error", args: []string{"install", "-src", t.TempDir(), "-dst", t.TempDir(), "-user", "", "-group", ""}, want: 1, err: "open"},
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

func TestInstallImage(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	files := map[string]string{
		"vmlinux.bin":  "kernel",
		"initrd.img":   "initrd",
		"rootfs.ext4":  "rootfs",
		"manifest.txt": "manifest",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stdout, stderr, code := captureOutput(t, func() int {
		return run([]string{"install", "-src", src, "-dst", dst, "-user", "", "-group", ""})
	})
	if code != 0 {
		t.Fatalf("install code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "Installed firedoze base image artifacts") {
		t.Fatalf("stdout = %q, want install summary", stdout)
	}
	for name, want := range files {
		path := filepath.Join(dst, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read installed %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat installed %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("%s mode = %o, want 0644", name, got)
		}
	}
}

func TestInstallImageRejectsUnexpectedArgs(t *testing.T) {
	err := installImage([]string{"extra"})
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments: extra") {
		t.Fatalf("installImage unexpected arg error = %v", err)
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
		{name: "unsupported size suffix", args: []string{"-size", "5T"}, want: "unsupported size suffix"},
		{name: "too small", args: []string{"-size", "128M"}, want: "image size must be at least 512M"},
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
	if got := defaultImageURL(); got != "https://cloud-images.ubuntu.com/resolute/20260421/resolute-server-cloudimg-amd64-root.tar.xz" {
		t.Fatalf("defaultImageURL() = %q", got)
	}
	if got := defaultKernelURL(); got != "https://cloud-images.ubuntu.com/resolute/20260421/unpacked/resolute-server-cloudimg-amd64-vmlinuz-generic" {
		t.Fatalf("defaultKernelURL() = %q", got)
	}
	if got := defaultInitrdURL(); got != "https://cloud-images.ubuntu.com/resolute/20260421/unpacked/resolute-server-cloudimg-amd64-initrd-generic" {
		t.Fatalf("defaultInitrdURL() = %q", got)
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
	if !overlay.shouldSkip("usr/share/doc/base-files/copyright") {
		t.Fatal("overlay should skip documentation paths")
	}
	if !overlay.shouldSkip("var/lib/cloud/instances/demo/user-data.txt") {
		t.Fatal("overlay should skip cloud-init state paths")
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
	if _, err := efs.Stat("usr/share/doc/base-files/readme"); err == nil {
		t.Fatal("/usr/share/doc/base-files/readme was written; want docs skipped")
	}
	if _, err := efs.Stat("var/lib/cloud/instance/user-data.txt"); err == nil {
		t.Fatal("/var/lib/cloud/instance/user-data.txt was written; want cloud-init state skipped")
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
	assertExt4FileContains(t, efs, "usr/local/sbin/firedoze-zram", "modprobe zram")
	assertExt4FileContains(t, efs, "usr/local/sbin/firedoze-zram", "swapon -p 100")
	assertExt4Missing(t, efs, "usr/local/sbin/firedoze-slim")
	assertExt4FileContains(t, efs, "etc/systemd/system/firedoze-network.service", "ExecStart=/usr/local/sbin/firedoze-guest-network")
	assertExt4FileContains(t, efs, "etc/systemd/system/firedoze-zram.service", "ExecStart=/usr/local/sbin/firedoze-zram")
	assertExt4Missing(t, efs, "etc/systemd/system/firedoze-slim.service")
	assertExt4FileContains(t, efs, "etc/systemd/system/firedoze-sshd.service", "ExecStart=/usr/sbin/sshd -D -e")
	assertExt4FileContains(t, efs, "etc/sysctl.d/90-firedoze-zram.conf", "vm.swappiness=100")
	assertExt4FileContains(t, efs, "usr/local/bin/firedoze-hello-service", "ExecStart=/usr/local/bin/firedoze-hello $port$verbose")
	assertExt4FileContains(t, efs, "usr/local/bin/firedoze-hello-service", "if [ \"$#\" -gt 0 ]; then\n  shift\nfi")
	assertExt4FileContains(t, efs, "usr/local/bin/firedoze-stop", "stopping this Firedoze VM")
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
	assertExt4Missing(t, efs, "etc/systemd/system/multi-user.target.wants/firedoze-network.service")
	assertExt4Missing(t, efs, "etc/systemd/system/multi-user.target.wants/firedoze-sshd.service")
	if target := readExt4Link(t, efs, "etc/systemd/system/sysinit.target.wants/firedoze-network.service"); target != "/etc/systemd/system/firedoze-network.service" {
		t.Fatalf("firedoze-network enable symlink -> %q", target)
	}
	if target := readExt4Link(t, efs, "etc/systemd/system/sysinit.target.wants/firedoze-sshd.service"); target != "/etc/systemd/system/firedoze-sshd.service" {
		t.Fatalf("firedoze-sshd enable symlink -> %q", target)
	}
	assertExt4Missing(t, efs, "etc/systemd/system/sysinit.target.wants/firedoze-zram.service")
	if target := readExt4Link(t, efs, "etc/systemd/system/multi-user.target.wants/firedoze-zram.service"); target != "/etc/systemd/system/firedoze-zram.service" {
		t.Fatalf("firedoze-zram enable symlink -> %q", target)
	}
	assertExt4Missing(t, efs, "etc/systemd/system/multi-user.target.wants/firedoze-slim.service")
	if target := readExt4Link(t, efs, "etc/systemd/system/cloud-init.service"); target != "/dev/null" {
		t.Fatalf("cloud-init mask symlink -> %q", target)
	}
	if target := readExt4Link(t, efs, "etc/systemd/system/cloud-init-main.service"); target != "/dev/null" {
		t.Fatalf("cloud-init-main mask symlink -> %q", target)
	}
	if target := readExt4Link(t, efs, "etc/systemd/system/cloud-init-network.service"); target != "/dev/null" {
		t.Fatalf("cloud-init-network mask symlink -> %q", target)
	}
	if target := readExt4Link(t, efs, "etc/systemd/system/snapd.service"); target != "/dev/null" {
		t.Fatalf("snapd mask symlink -> %q", target)
	}
	for _, path := range []string{
		"etc/systemd/system/apparmor.service",
		"etc/systemd/system/snapd.apparmor.service",
		"etc/systemd/system/man-db.timer",
		"etc/systemd/system/update-notifier-download.timer",
		"etc/systemd/system/sysstat-collect.timer",
		"etc/systemd/system/pollinate.service",
		"etc/systemd/system/networkd-dispatcher.service",
		"etc/systemd/system/ModemManager.service",
		"etc/systemd/system/systemd-binfmt.service",
		"etc/systemd/system/proc-sys-fs-binfmt_misc.mount",
		"etc/systemd/system/sysstat.service",
		"etc/systemd/system/udisks2.service",
		"etc/systemd/system/e2scrub_reap.service",
		"etc/systemd/system/lxd-installer.socket",
	} {
		if target := readExt4Link(t, efs, path); target != "/dev/null" {
			t.Fatalf("%s mask symlink -> %q", path, target)
		}
	}
	info := statExt4(t, efs, "usr/local/bin/firedoze-hello")
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("firedoze-hello mode = %v, want 0755", got)
	}
	info = statExt4(t, efs, "usr/local/bin/firedoze-stop")
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("firedoze-stop mode = %v, want 0755", got)
	}
	info = statExt4(t, efs, "etc/sudoers.d/90-firedoze-ubuntu")
	if got := info.Mode().Perm(); got != 0o440 {
		t.Fatalf("sudoers mode = %v, want 0440", got)
	}
}

func TestEnsureZramModuleDeps(t *testing.T) {
	efs := newTestExt4(t)
	kernelVersion := "7.0.0-test"
	moduleDir := path.Join("usr/lib/modules", kernelVersion)
	for _, module := range []string{
		"kernel/drivers/block/zram/zram.ko.zst",
		"kernel/lib/lz4/lz4hc_compress.ko.zst",
		"kernel/lib/lz4/lz4_compress.ko.zst",
		"kernel/lib/842/842_decompress.ko.zst",
		"kernel/lib/842/842_compress.ko.zst",
	} {
		if err := writeFile(efs, path.Join(moduleDir, module), []byte("module"), 0o644, 0, 0, timeNow()); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFile(efs, path.Join(moduleDir, "modules.dep"), []byte("kernel/drivers/virtio/virtio.ko.zst:\n"), 0o644, 0, 0, timeNow()); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(efs, path.Join(moduleDir, "modules.dep.bin"), []byte("stale"), 0o644, 0, 0, timeNow()); err != nil {
		t.Fatal(err)
	}

	if err := ensureZramModuleDeps(efs, kernelVersion); err != nil {
		t.Fatalf("ensureZramModuleDeps: %v", err)
	}

	assertExt4FileContains(t, efs, path.Join(moduleDir, "modules.dep"), "kernel/drivers/block/zram/zram.ko.zst: kernel/lib/lz4/lz4hc_compress.ko.zst")
	assertExt4Missing(t, efs, path.Join(moduleDir, "modules.dep.bin"))
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

func TestBuildGuestHelloBinaryUsesPackagedBinary(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "firedoze-hello-linux-amd64")
	if err := os.WriteFile(binPath, []byte("packaged-hello"), 0o755); err != nil {
		t.Fatal(err)
	}

	old := packagedGuestHelloBinaries
	packagedGuestHelloBinaries = map[string]string{"amd64": binPath}
	t.Cleanup(func() {
		packagedGuestHelloBinaries = old
	})

	got, err := buildGuestHelloBinary("amd64")
	if err != nil {
		t.Fatalf("buildGuestHelloBinary: %v", err)
	}
	if string(got) != "packaged-hello" {
		t.Fatalf("buildGuestHelloBinary = %q, want packaged binary", got)
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
	addDir("usr/share", 0o755)
	addDir("usr/share/doc", 0o755)
	addDir("usr/share/doc/base-files", 0o755)
	addDir("var", 0o755)
	addDir("var/lib", 0o755)
	addDir("var/lib/cloud", 0o755)
	addDir("var/lib/cloud/instance", 0o755)
	addDir("boot", 0o755)
	addFile("etc/hostname", 0o644, "demo\n")
	addFile("etc/passwd", 0o644, "root:x:0:0:root:/root:/bin/bash\n")
	addFile("usr/bin/tool", 0o755, "tool-data")
	addFile("usr/share/doc/base-files/readme", 0o644, "docs")
	addFile("var/lib/cloud/instance/user-data.txt", 0o644, "cloud")
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

func assertExt4Missing(t *testing.T, efs *ext4.FileSystem, path string) {
	t.Helper()
	if _, err := efs.Stat(path); err == nil {
		t.Fatalf("/%s exists, want missing", path)
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
