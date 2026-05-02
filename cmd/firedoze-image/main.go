package main

import (
	"archive/tar"
	"bytes"
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
	"github.com/ulikunitz/xz"
)

const (
	defaultRelease = "noble"
	defaultArch    = "amd64"
	defaultSize    = "4G"
	defaultOutDir  = "dist/base-image"
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
  -h, --help       Show this help

The builder is native Go. It does not require Docker, Podman, root, mounting,
or host ext4 support.
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

	source, err := openSource(*tarPath, *imageURL)
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	defer source.Close()

	xzr, err := xz.NewReader(source)
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
		kernel, err := readBootArtifact(*kernelPath, *kernelURL)
		if err != nil {
			_ = os.Remove(tmpRootfsPath)
			return fmt.Errorf("read kernel image: %w", err)
		}
		artifacts.kernel = kernel
	}
	if artifacts.initrd == nil || *initrdPath != "" || initrdURLSet {
		initrd, err := readBootArtifact(*initrdPath, *initrdURL)
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
kernel=vmlinux.bin
kernel_source=%s
initrd=initrd.img
initrd_source=%s
size=%s
ssh_authorized_keys=/etc/firedoze/authorized_keys
network=06:00:<guest-ip-octets> with guest /30 and host at guest_ip-1
builder=firedoze-image native-go
`, *release, *arch, source.Name(), artifacts.kernel.path, artifacts.initrd.path, *sizeText)
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

type namedReadCloser interface {
	io.ReadCloser
	Name() string
}

type sourceReader struct {
	io.ReadCloser
	name string
}

func (s sourceReader) Name() string {
	return s.name
}

func openSource(tarPath string, imageURL string) (namedReadCloser, error) {
	if tarPath != "" {
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, err
		}
		return sourceReader{ReadCloser: f, name: tarPath}, nil
	}
	resp, err := http.Get(imageURL)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", imageURL, resp.Status)
	}
	return sourceReader{ReadCloser: resp.Body, name: imageURL}, nil
}

func readBootArtifact(localPath string, url string) (*bootArtifact, error) {
	if localPath != "" {
		data, err := os.ReadFile(localPath)
		if err != nil {
			return nil, err
		}
		return &bootArtifact{path: localPath, data: data}, nil
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &bootArtifact{path: url, data: data}, nil
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

dev="${1:-eth0}"
mac="$(cat "/sys/class/net/$dev/address")"
IFS=: set -- $mac

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

ip addr flush dev "$dev"
ip addr add "$guest_ip/30" dev "$dev"
ip link set "$dev" up
ip route replace default via "$host_ip" dev "$dev"

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
ExecStart=/usr/local/sbin/firedoze-guest-network eth0
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`,
		},
		{
			path: "etc/cloud/cloud.cfg.d/99-firedoze.cfg",
			mode: 0o644,
			data: "datasource_list: [ None ]\npreserve_hostname: true\nmanage_etc_hosts: false\nssh_pwauth: false\ndisable_root: false\n",
		},
	}

	for _, dir := range []string{
		"etc/cloud/cloud.cfg.d",
		"etc/firedoze",
		"etc/ssh/sshd_config.d",
		"etc/systemd/system",
		"etc/systemd/system/multi-user.target.wants",
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

	if err := symlink(efs, "/etc/systemd/system/firedoze-network.service", "etc/systemd/system/multi-user.target.wants/firedoze-network.service", now); err != nil {
		return err
	}
	for _, unit := range []string{"cloud-init.service", "cloud-init-local.service", "cloud-config.service", "cloud-final.service"} {
		if err := symlink(efs, "/dev/null", "etc/systemd/system/"+unit, now); err != nil {
			return err
		}
	}
	return nil
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
