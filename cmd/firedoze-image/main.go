package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const (
	defaultRelease = "noble"
	defaultArch    = "amd64"
	defaultSize    = "4G"
	defaultOutDir  = "dist/base-image"

	nobleRootSHA256   = "13dc3c9ed4e76688ce3efaee45551dd4b5d706c2579bd91fd0de5464e34dd777"
	nobleKernelSHA256 = "5b2a4fe174dacb18281f8f7d72ae32ac4b92801f0b7b5cb43ea55dee29fb789d"
	nobleInitrdSHA256 = "cd0b64a5498e583a820a5b842369df83d036b4200b33bc51cadc58176184aaca"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage()
		return 0
	}
	switch args[0] {
	case "build":
		if err := build(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  firedoze-image build [options]

Build a Firecracker-ready Ubuntu root filesystem and matching boot artifacts.

Options:
  --out DIR        Output directory. Default: dist/base-image
  --release NAME   Ubuntu cloud image release. Default: noble
  --arch ARCH      Ubuntu architecture. Default: amd64
  --size SIZE      Root filesystem image size. Default: 4G
  --url URL        Override the Ubuntu root tarball URL
  --tar PATH       Use a local Ubuntu root tarball instead of downloading one
  --kernel PATH    Use a local kernel image instead of downloading one
  --initrd PATH    Use a local initrd image instead of downloading one
  --kernel-url URL Override the kernel image URL
  --initrd-url URL Override the initrd image URL
  --root-sha256 SUM   Expected SHA-256 for the root tarball
  --kernel-sha256 SUM Expected SHA-256 for the kernel image
  --initrd-sha256 SUM Expected SHA-256 for the initrd image
  --insecure-skip-checksums
                    Allow unverified artifact overrides
  -h, --help       Show this help

The builder is native Go. It does not require Docker, Podman, root, mounting,
or host ext4 support. Default Ubuntu artifacts are pinned and SHA-256 verified.
`)
}

func build(args []string) error {
	fs := flag.NewFlagSet("firedoze-image build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	release := fs.String("release", defaultRelease, "Ubuntu cloud image release")
	arch := fs.String("arch", defaultArch, "Ubuntu architecture")
	sizeText := fs.String("size", defaultSize, "root filesystem image size")
	outDir := fs.String("out", defaultOutDir, "output directory")
	imageURL := fs.String("url", "", "Ubuntu root tarball URL")
	tarPath := fs.String("tar", "", "local Ubuntu root tarball")
	kernelPath := fs.String("kernel", "", "local kernel image")
	initrdPath := fs.String("initrd", "", "local initrd image")
	kernelURL := fs.String("kernel-url", "", "kernel image URL")
	initrdURL := fs.String("initrd-url", "", "initrd image URL")
	rootSHA256 := fs.String("root-sha256", "", "expected root tarball SHA-256")
	kernelSHA256 := fs.String("kernel-sha256", "", "expected kernel image SHA-256")
	initrdSHA256 := fs.String("initrd-sha256", "", "expected initrd image SHA-256")
	insecureSkipChecksums := fs.Bool("insecure-skip-checksums", false, "allow unverified artifact overrides")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *arch != "amd64" {
		return errors.New("only amd64 is supported for now; firedoze currently targets x86_64 hosts")
	}
	rootURLSet := *imageURL != ""
	if *imageURL == "" {
		*imageURL = defaultImageURL(*release)
	}
	kernelURLSet := *kernelURL != ""
	initrdURLSet := *initrdURL != ""
	if *kernelURL == "" {
		*kernelURL = defaultKernelURL(*release)
	}
	if *initrdURL == "" {
		*initrdURL = defaultInitrdURL(*release)
	}
	if err := applyDefaultChecksums(*release, rootURLSet, kernelURLSet, initrdURLSet, *tarPath, *kernelPath, *initrdPath, rootSHA256, kernelSHA256, initrdSHA256, *insecureSkipChecksums); err != nil {
		return err
	}
	size, err := parseSize(*sizeText)
	if err != nil {
		return err
	}
	if size < 512*1024*1024 {
		return errors.New("image size must be at least 512M")
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	absOut, err := filepath.Abs(*outDir)
	if err != nil {
		return err
	}

	rootfsPath := filepath.Join(absOut, "rootfs.ext4")
	tmpRootfsPath := rootfsPath + ".tmp"
	_ = os.Remove(tmpRootfsPath)
	backend, err := file.CreateFromPath(tmpRootfsPath, size)
	if err != nil {
		return err
	}

	efs, err := ext4.Create(backend, size, 0, 512, &ext4.Params{
		SectorsPerBlock:       8,
		VolumeName:            "firedoze-rootfs",
		ReservedBlocksPercent: 0,
	})
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}

	source, err := readArtifact(*tarPath, *imageURL, *rootSHA256, *insecureSkipChecksums)
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}

	xzr, err := xz.NewReader(bytes.NewReader(source.data))
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return fmt.Errorf("open xz stream: %w", err)
	}

	artifacts, err := populateRootfs(efs, tar.NewReader(xzr))
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	if err := customizeGuest(efs); err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	if err := efs.Close(); err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	if err := backend.Close(); err != nil {
		_ = os.Remove(tmpRootfsPath)
		return err
	}

	if artifacts.kernel == nil || *kernelPath != "" || kernelURLSet {
		kernel, err := readBootArtifact(*kernelPath, *kernelURL, *kernelSHA256, *insecureSkipChecksums)
		if err != nil {
			_ = os.Remove(tmpRootfsPath)
			return fmt.Errorf("read kernel image: %w", err)
		}
		artifacts.kernel = kernel
	}
	kernelELF, err := extractKernelELF(artifacts.kernel.data)
	if err != nil {
		_ = os.Remove(tmpRootfsPath)
		return fmt.Errorf("extract kernel ELF: %w", err)
	}
	artifacts.kernel.data = kernelELF
	if artifacts.initrd == nil || *initrdPath != "" || initrdURLSet {
		initrd, err := readBootArtifact(*initrdPath, *initrdURL, *initrdSHA256, *insecureSkipChecksums)
		if err != nil {
			_ = os.Remove(tmpRootfsPath)
			return fmt.Errorf("read initrd image: %w", err)
		}
		artifacts.initrd = initrd
	}

	if err := replaceFile(filepath.Join(absOut, "vmlinux.bin"), artifacts.kernel.data, 0o644); err != nil {
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	if err := replaceFile(filepath.Join(absOut, "initrd.img"), artifacts.initrd.data, 0o644); err != nil {
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	manifest := fmt.Sprintf(`release=%s
arch=%s
source=%s
rootfs=rootfs.ext4
root_sha256=%s
kernel=vmlinux.bin
kernel_source=%s
kernel_sha256=%s
initrd=initrd.img
initrd_source=%s
initrd_sha256=%s
size=%s
ssh_authorized_keys=/etc/firedoze/authorized_keys
network=06:00:<guest-ip-octets> with guest /30 and host at guest_ip-1
builder=firedoze-image native-go
`, *release, *arch, source.name, *rootSHA256, artifacts.kernel.path, *kernelSHA256, artifacts.initrd.path, *initrdSHA256, *sizeText)
	if err := replaceFile(filepath.Join(absOut, "manifest.txt"), []byte(manifest), 0o644); err != nil {
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	if err := os.Rename(tmpRootfsPath, rootfsPath); err != nil {
		_ = os.Remove(tmpRootfsPath)
		return err
	}

	fmt.Printf("Built firedoze base image artifacts in %s:\n", absOut)
	fmt.Println("  rootfs.ext4")
	fmt.Println("  vmlinux.bin")
	fmt.Println("  initrd.img")
	fmt.Println("  manifest.txt")
	return nil
}

func defaultImageURL(release string) string {
	if release == "noble" {
		return "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64-root.tar.xz"
	}
	return fmt.Sprintf("https://cloud-images.ubuntu.com/%s/current/%s-server-cloudimg-amd64-root.tar.xz", release, release)
}

func defaultKernelURL(release string) string {
	if release == "noble" {
		return "https://cloud-images.ubuntu.com/releases/noble/release/unpacked/ubuntu-24.04-server-cloudimg-amd64-vmlinuz-generic"
	}
	return fmt.Sprintf("https://cloud-images.ubuntu.com/%s/current/unpacked/%s-server-cloudimg-amd64-vmlinuz-generic", release, release)
}

func defaultInitrdURL(release string) string {
	if release == "noble" {
		return "https://cloud-images.ubuntu.com/releases/noble/release/unpacked/ubuntu-24.04-server-cloudimg-amd64-initrd-generic"
	}
	return fmt.Sprintf("https://cloud-images.ubuntu.com/%s/current/unpacked/%s-server-cloudimg-amd64-initrd-generic", release, release)
}

func applyDefaultChecksums(release string, rootURLSet bool, kernelURLSet bool, initrdURLSet bool, tarPath string, kernelPath string, initrdPath string, rootSHA256 *string, kernelSHA256 *string, initrdSHA256 *string, insecure bool) error {
	if release == "noble" {
		if !rootURLSet && tarPath == "" && *rootSHA256 == "" {
			*rootSHA256 = nobleRootSHA256
		}
		if !kernelURLSet && kernelPath == "" && *kernelSHA256 == "" {
			*kernelSHA256 = nobleKernelSHA256
		}
		if !initrdURLSet && initrdPath == "" && *initrdSHA256 == "" {
			*initrdSHA256 = nobleInitrdSHA256
		}
	}
	if insecure {
		return nil
	}
	if *rootSHA256 == "" {
		return errors.New("root artifact checksum is required for overrides; pass --root-sha256 or --insecure-skip-checksums")
	}
	if *kernelSHA256 == "" && (kernelPath != "" || kernelURLSet) {
		return errors.New("kernel artifact checksum is required for overrides; pass --kernel-sha256 or --insecure-skip-checksums")
	}
	if *initrdSHA256 == "" && (initrdPath != "" || initrdURLSet) {
		return errors.New("initrd artifact checksum is required for overrides; pass --initrd-sha256 or --insecure-skip-checksums")
	}
	return nil
}

func parseSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("size is required")
	}
	unit := int64(1)
	last := value[len(value)-1]
	if last < '0' || last > '9' {
		switch last {
		case 'k', 'K':
			unit = 1024
		case 'm', 'M':
			unit = 1024 * 1024
		case 'g', 'G':
			unit = 1024 * 1024 * 1024
		default:
			return 0, fmt.Errorf("unsupported size suffix %q", string(last))
		}
		value = value[:len(value)-1]
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size: %s", value)
	}
	return n * unit, nil
}

type artifactData struct {
	name string
	data []byte
}

func readArtifact(localPath string, url string, expectedSHA256 string, insecure bool) (artifactData, error) {
	name := url
	var data []byte
	if localPath != "" {
		name = localPath
		localData, err := os.ReadFile(localPath)
		if err != nil {
			return artifactData{}, err
		}
		data = localData
	} else {
		resp, err := http.Get(url)
		if err != nil {
			return artifactData{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return artifactData{}, fmt.Errorf("download %s: %s", url, resp.Status)
		}
		downloaded, err := io.ReadAll(resp.Body)
		if err != nil {
			return artifactData{}, err
		}
		data = downloaded
	}
	if expectedSHA256 == "" && !insecure {
		return artifactData{}, fmt.Errorf("no SHA-256 configured for %s", name)
	}
	if expectedSHA256 != "" {
		if err := verifySHA256(name, data, expectedSHA256); err != nil {
			return artifactData{}, err
		}
	}
	return artifactData{name: name, data: data}, nil
}

func readBootArtifact(localPath string, url string, expectedSHA256 string, insecure bool) (*bootArtifact, error) {
	artifact, err := readArtifact(localPath, url, expectedSHA256, insecure)
	if err != nil {
		return nil, err
	}
	return &bootArtifact{path: artifact.name, data: artifact.data}, nil
}

func verifySHA256(name string, data []byte, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("invalid SHA-256 for %s: expected 64 hex chars", name)
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s", name, actual, expected)
	}
	return nil
}

func extractKernelELF(kernel []byte) ([]byte, error) {
	if isELF(kernel) {
		return kernel, nil
	}
	type decompressor struct {
		name  string
		magic []byte
		run   func([]byte) ([]byte, error)
	}
	decompressors := []decompressor{
		{
			name:  "zstd",
			magic: []byte{0x28, 0xb5, 0x2f, 0xfd},
			run: func(data []byte) ([]byte, error) {
				dec, err := zstd.NewReader(bytes.NewReader(data))
				if err != nil {
					return nil, err
				}
				defer dec.Close()
				return io.ReadAll(dec)
			},
		},
		{
			name:  "gzip",
			magic: []byte{0x1f, 0x8b, 0x08},
			run: func(data []byte) ([]byte, error) {
				r, err := gzip.NewReader(bytes.NewReader(data))
				if err != nil {
					return nil, err
				}
				defer r.Close()
				return io.ReadAll(r)
			},
		},
		{
			name:  "xz",
			magic: []byte{0xfd, '7', 'z', 'X', 'Z', 0x00},
			run: func(data []byte) ([]byte, error) {
				r, err := xz.NewReader(bytes.NewReader(data))
				if err != nil {
					return nil, err
				}
				return io.ReadAll(r)
			},
		},
	}
	for _, dec := range decompressors {
		for offset := 0; ; {
			found := bytes.Index(kernel[offset:], dec.magic)
			if found < 0 {
				break
			}
			offset += found
			data, _ := dec.run(kernel[offset:])
			if isELF(data) {
				return data, nil
			}
			offset += len(dec.magic)
		}
	}
	return nil, errors.New("could not find an embedded ELF kernel payload")
}

func isELF(data []byte) bool {
	return len(data) >= 4 && bytes.Equal(data[:4], []byte{0x7f, 'E', 'L', 'F'})
}

type bootArtifact struct {
	path string
	data []byte
}

type bootArtifacts struct {
	kernel *bootArtifact
	initrd *bootArtifact
}

type pendingHardlink struct {
	path   string
	target string
	header *tar.Header
}

func populateRootfs(efs *ext4.FileSystem, tr *tar.Reader) (bootArtifacts, error) {
	var artifacts bootArtifacts
	var hardlinks []pendingHardlink

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return artifacts, err
		}
		clean, ok := cleanTarPath(hdr.Name)
		if !ok {
			continue
		}
		mode := os.FileMode(hdr.Mode)
		if hdr.FileInfo().IsDir() {
			if err := mkdirAll(efs, clean, mode, hdr.Uid, hdr.Gid, hdr.ModTime); err != nil {
				return artifacts, fmt.Errorf("create dir /%s: %w", clean, err)
			}
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			data, err := io.ReadAll(tr)
			if err != nil {
				return artifacts, fmt.Errorf("read /%s: %w", clean, err)
			}
			if err := writeFile(efs, clean, data, mode, hdr.Uid, hdr.Gid, hdr.ModTime); err != nil {
				return artifacts, fmt.Errorf("write /%s: %w", clean, err)
			}
			artifacts.remember(clean, data)
		case tar.TypeDir:
			if err := mkdirAll(efs, clean, mode, hdr.Uid, hdr.Gid, hdr.ModTime); err != nil {
				return artifacts, fmt.Errorf("create dir /%s: %w", clean, err)
			}
		case tar.TypeSymlink:
			if err := symlink(efs, hdr.Linkname, clean, hdr.ModTime); err != nil {
				return artifacts, fmt.Errorf("symlink /%s: %w", clean, err)
			}
		case tar.TypeLink:
			target, ok := cleanTarPath(hdr.Linkname)
			if !ok {
				continue
			}
			hcopy := *hdr
			hardlinks = append(hardlinks, pendingHardlink{path: clean, target: target, header: &hcopy})
		default:
			// Device nodes, fifos, and other special entries are not required in the
			// base image because devtmpfs/systemd recreate runtime devices at boot.
		}
	}

	for _, link := range hardlinks {
		data, err := efs.ReadFile(link.target)
		if err != nil {
			return artifacts, fmt.Errorf("read hardlink target /%s for /%s: %w", link.target, link.path, err)
		}
		if err := writeFile(efs, link.path, data, os.FileMode(link.header.Mode), link.header.Uid, link.header.Gid, link.header.ModTime); err != nil {
			return artifacts, fmt.Errorf("write hardlink copy /%s: %w", link.path, err)
		}
		artifacts.remember(link.path, data)
	}

	return artifacts, nil
}

func (a *bootArtifacts) remember(p string, data []byte) {
	switch {
	case path.Dir(p) == "boot" && strings.HasPrefix(path.Base(p), "vmlinuz-"):
		if a.kernel == nil || versionLess(a.kernel.path, p) {
			a.kernel = &bootArtifact{path: p, data: append([]byte(nil), data...)}
		}
	case path.Dir(p) == "boot" && strings.HasPrefix(path.Base(p), "initrd.img-"):
		if a.initrd == nil || versionLess(a.initrd.path, p) {
			a.initrd = &bootArtifact{path: p, data: append([]byte(nil), data...)}
		}
	}
}

func versionLess(a, b string) bool {
	values := []string{a, b}
	sort.Strings(values)
	return values[0] == a && a != b
}

func cleanTarPath(name string) (string, bool) {
	name = strings.TrimPrefix(name, "/")
	clean := path.Clean(name)
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "__MACOSX" || strings.HasPrefix(part, "._") {
			return "", false
		}
	}
	return clean, true
}

func customizeGuest(efs *ext4.FileSystem) error {
	now := time.Now()
	files := []struct {
		path string
		mode os.FileMode
		data string
	}{
		{
			path: "etc/ssh/sshd_config.d/99-firedoze.conf",
			mode: 0o644,
			data: "PubkeyAuthentication yes\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nAuthorizedKeysFile /etc/firedoze/authorized_keys .ssh/authorized_keys\n",
		},
		{
			path: "usr/local/sbin/firedoze-guest-network",
			mode: 0o755,
			data: `#!/bin/sh
set -eu

dev="${1:-}"
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if [ -z "$dev" ]; then
    for candidate in /sys/class/net/*; do
      name="${candidate##*/}"
      [ "$name" = "lo" ] && continue
      [ -e "$candidate/address" ] || continue
      mac="$(cat "$candidate/address")"
      case "$mac" in
        06:00:*) dev="$name"; break ;;
      esac
    done
  fi
  if [ -n "$dev" ]; then
    break
  fi
  sleep 1
done

if [ -z "$dev" ]; then
  for candidate in /sys/class/net/*; do
    name="${candidate##*/}"
    [ "$name" = "lo" ] && continue
    echo "seen network interface $name mac=$(cat "$candidate/address" 2>/dev/null || true)" >&2
  done
fi

if [ -z "$dev" ]; then
  echo "could not find firedoze network interface" >&2
  exit 1
fi

mac="$(cat "/sys/class/net/$dev/address")"
old_ifs="$IFS"
IFS=:
set -- $mac
IFS="$old_ifs"

if [ "$1:$2" != "06:00" ]; then
  echo "unexpected firedoze MAC prefix: $mac" >&2
  exit 1
fi

o1="$(printf "%d" "0x$3")"
o2="$(printf "%d" "0x$4")"
o3="$(printf "%d" "0x$5")"
o4="$(printf "%d" "0x$6")"
guest_ip="$o1.$o2.$o3.$o4"
host_ip="$o1.$o2.$o3.$((o4 - 1))"

/bin/ip addr flush dev "$dev"
/bin/ip addr add "$guest_ip/30" dev "$dev"
/bin/ip link set "$dev" up
/bin/ip route replace default via "$host_ip" dev "$dev"

rm -f /etc/resolv.conf
cat >/etc/resolv.conf <<RESOLV
nameserver 1.1.1.1
nameserver 8.8.8.8
RESOLV
`,
		},
		{
			path: "etc/systemd/system/firedoze-network.service",
			mode: 0o644,
			data: `[Unit]
Description=Configure firedoze Firecracker guest networking
DefaultDependencies=no
Before=network-pre.target
Wants=network-pre.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/firedoze-guest-network
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`,
		},
		{
			path: "etc/systemd/system/firedoze-sshd.service",
			mode: 0o644,
			data: `[Unit]
Description=firedoze SSH daemon
After=network.target
ConditionPathExists=!/etc/ssh/sshd_not_to_be_run

[Service]
Type=simple
ExecStartPre=/bin/mkdir -p /run/sshd
ExecStartPre=/usr/sbin/sshd -t
ExecStart=/usr/sbin/sshd -D -e
Restart=on-failure

[Install]
WantedBy=multi-user.target
`,
		},
		{
			path: "etc/cloud/cloud.cfg.d/99-firedoze.cfg",
			mode: 0o644,
			data: "datasource_list: [ None ]\npreserve_hostname: true\nmanage_etc_hosts: false\nssh_pwauth: false\ndisable_root: false\n",
		},
		{
			path: "etc/fstab",
			mode: 0o644,
			data: "/dev/vda / ext4 defaults,errors=remount-ro 0 1\n",
		},
	}

	for _, dir := range []string{
		"etc/cloud/cloud.cfg.d",
		"etc/firedoze",
		"etc/ssh/sshd_config.d",
		"etc/systemd/system",
		"etc/systemd/system/multi-user.target.wants",
		"etc/sudoers.d",
		"home/ubuntu",
		"usr/local/sbin",
	} {
		if err := mkdirAll(efs, dir, 0o755, 0, 0, now); err != nil {
			return err
		}
	}
	for _, file := range files {
		if err := writeFile(efs, file.path, []byte(file.data), file.mode, 0, 0, now); err != nil {
			return err
		}
	}
	if err := ensureLine(efs, "etc/passwd", "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash", 0o644, 0, 0, now); err != nil {
		return err
	}
	if err := ensureLine(efs, "etc/shadow", "ubuntu:!:19723:0:99999:7:::", 0o640, 0, 42, now); err != nil {
		return err
	}
	if err := ensureGroupMember(efs, "etc/group", "ubuntu", "x", "1000", "ubuntu", 0o644, 0, 0, now); err != nil {
		return err
	}
	if err := ensureGroupMember(efs, "etc/group", "sudo", "x", "27", "ubuntu", 0o644, 0, 0, now); err != nil {
		return err
	}
	if err := ensureGroupMember(efs, "etc/gshadow", "ubuntu", "!", "", "ubuntu", 0o640, 0, 42, now); err != nil {
		return err
	}
	if err := ensureGroupMember(efs, "etc/gshadow", "sudo", "*", "", "ubuntu", 0o640, 0, 42, now); err != nil {
		return err
	}
	if err := writeFile(efs, "etc/sudoers.d/90-firedoze-ubuntu", []byte("ubuntu ALL=(ALL) NOPASSWD:ALL\n"), 0o440, 0, 0, now); err != nil {
		return err
	}
	_ = efs.Chown("home/ubuntu", 1000, 1000)
	for _, p := range []string{
		"usr/bin/chfn",
		"usr/bin/chsh",
		"usr/bin/gpasswd",
		"usr/bin/mount",
		"usr/bin/newgrp",
		"usr/bin/passwd",
		"usr/bin/su",
		"usr/bin/sudo",
		"usr/bin/umount",
	} {
		chmodIfExists(efs, p, os.ModeSetuid|0o755)
	}

	if err := symlink(efs, "/etc/systemd/system/firedoze-network.service", "etc/systemd/system/multi-user.target.wants/firedoze-network.service", now); err != nil {
		return err
	}
	_ = efs.Remove("etc/systemd/system/sockets.target.wants/ssh.socket")
	if err := symlink(efs, "/dev/null", "etc/systemd/system/ssh.socket", now); err != nil {
		return err
	}
	if err := symlink(efs, "/etc/systemd/system/firedoze-sshd.service", "etc/systemd/system/multi-user.target.wants/firedoze-sshd.service", now); err != nil {
		return err
	}
	for _, unit := range []string{"cloud-init.service", "cloud-init-local.service", "cloud-config.service", "cloud-final.service", "systemd-networkd-wait-online.service", "multipathd.service", "multipathd.socket"} {
		if err := symlink(efs, "/dev/null", "etc/systemd/system/"+unit, now); err != nil {
			return err
		}
	}
	return nil
}

func ensureLine(efs *ext4.FileSystem, p string, line string, mode os.FileMode, uid int, gid int, modTime time.Time) error {
	data, err := efs.ReadFile(p)
	if err != nil {
		return fmt.Errorf("read /%s: %w", p, err)
	}
	if hasLine(string(data), line) {
		return nil
	}
	text := string(data)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += line + "\n"
	return writeFile(efs, p, []byte(text), mode, uid, gid, modTime)
}

func ensureGroupMember(efs *ext4.FileSystem, p string, name string, password string, gid string, member string, mode os.FileMode, uid int, fileGID int, modTime time.Time) error {
	data, err := efs.ReadFile(p)
	if err != nil {
		return fmt.Errorf("read /%s: %w", p, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	found := false
	for i, line := range lines {
		fields := strings.Split(line, ":")
		if len(fields) != 4 || fields[0] != name {
			continue
		}
		found = true
		members := splitCSV(fields[3])
		if !stringInSlice(members, member) {
			members = append(members, member)
		}
		lines[i] = strings.Join([]string{name, fields[1], fields[2], strings.Join(members, ",")}, ":")
	}
	if !found {
		lines = append(lines, strings.Join([]string{name, password, gid, member}, ":"))
	}
	return writeFile(efs, p, []byte(strings.Join(lines, "\n")+"\n"), mode, uid, fileGID, modTime)
}

func hasLine(text string, want string) bool {
	for _, line := range strings.Split(text, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func stringInSlice(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func chmodIfExists(efs *ext4.FileSystem, p string, mode os.FileMode) {
	if _, err := efs.Stat(p); err != nil {
		return
	}
	_ = efs.Chmod(p, mode)
}

func mkdirAll(efs *ext4.FileSystem, p string, mode os.FileMode, uid int, gid int, modTime time.Time) error {
	if p == "" {
		return nil
	}
	current := ""
	for _, part := range strings.Split(p, "/") {
		if part == "" {
			continue
		}
		current = path.Join(current, part)
		if err := efs.Mkdir(current); err != nil {
			return err
		}
	}
	full := p
	_ = efs.Chmod(full, mode)
	_ = efs.Chown(full, uid, gid)
	_ = efs.Chtimes(full, modTime, modTime, modTime)
	return nil
}

func writeFile(efs *ext4.FileSystem, p string, data []byte, mode os.FileMode, uid int, gid int, modTime time.Time) error {
	if err := mkdirAll(efs, path.Dir(p), 0o755, 0, 0, modTime); err != nil {
		return err
	}
	full := p
	_ = efs.Remove(full)
	f, err := efs.OpenFile(full, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = efs.Chmod(full, mode)
	_ = efs.Chown(full, uid, gid)
	_ = efs.Chtimes(full, modTime, modTime, modTime)
	return nil
}

func symlink(efs *ext4.FileSystem, target string, p string, modTime time.Time) error {
	if err := mkdirAll(efs, path.Dir(p), 0o755, 0, 0, modTime); err != nil {
		return err
	}
	full := p
	_ = efs.Remove(full)
	return efs.Symlink(target, full)
}

func replaceFile(p string, data []byte, mode os.FileMode) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
