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
	"os/exec"
	osuser "os/user"
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
	baseImageRelease       = "resolute"
	baseImageVersion       = "26.04"
	baseImageArch          = "amd64"
	defaultSize            = "4G"
	defaultOutDir          = "dist/base-image"
	defaultImageInstallDir = "/var/lib/firedoze/images"

	baseRootSHA256   = "f5e922907ca6da7de57ee22044a399f497c89e84cf6eaa4ca5cff342bd286582"
	baseKernelSHA256 = "6f11cd68fc0d181bb4f9a72e2d0ec6a1c2a69a59f7b9bd6bf9538630d1102456"
	baseInitrdSHA256 = "b47e3fcd0a1409b1720e68262e88535c685189ba94a0fa931978901b806a80bc"

	baseBusyBoxStaticURL    = "https://ubuntu.mirror.constant.com/pool/main/b/busybox/busybox-static_1.37.0-7ubuntu1_amd64.deb"
	baseBusyBoxStaticSHA256 = "fd605342f62268753076aa7d9321ff098b36ba53e47434145a9aedc28fd141a4"

	baseKernelModulesURL    = "https://ubuntu.mirror.constant.com/pool/main/l/linux/linux-modules-7.0.0-14-generic_7.0.0-14.14_amd64.deb"
	baseKernelModulesSHA256 = "5bbbd6bd424b38a9927a897259f02de1b3caff94e9dc27b931b54d65bf005b8d"
)

var packagedGuestHelloBinaries = map[string]string{
	"amd64": "/usr/lib/firedoze/firedoze-hello-linux-amd64",
}

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
	case "install":
		if err := installImage(args[1:]); err != nil {
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
  firedoze-image-builder build [options]
  firedoze-image-builder install [options]

Build a Firecracker-ready Ubuntu root filesystem and matching boot artifacts.

Build options:
  -out DIR        Output directory. Default: dist/base-image
  -size SIZE      Root filesystem image size. Default: 4G
  -h               Show this help

Install options:
  -src DIR        Built artifact directory. Default: dist/base-image
  -dst DIR        Image install directory. Default: /var/lib/firedoze/images
  -user USER      Installed file owner. Default: firedoze
  -group GROUP    Installed file group. Default: firedoze

The builder is native Go. It does not require Docker, Podman, root, mounting,
or host ext4 support. Release packages include the small Linux guest helper
binary needed by the generated image. When running from a source checkout, the
builder can also compile that helper itself. Default Ubuntu artifacts are pinned
and SHA-256 verified.
`)
}

func build(args []string) error {
	fs := flag.NewFlagSet("firedoze-image-builder build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	sizeText := fs.String("size", defaultSize, "root filesystem image size")
	outDir := fs.String("out", defaultOutDir, "output directory")

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

	source, err := readArtifact("", defaultImageURL(), baseRootSHA256, false)
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

	overlay := newGuestOverlay()
	artifacts, err := populateRootfs(efs, tar.NewReader(xzr), overlay)
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	kernelModulesDeb, err := readKernelModulesDeb()
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	if err := installKernelModulesDeb(efs, kernelModulesDeb); err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	helloBinary, err := buildGuestHelloBinary(baseImageArch)
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	busyBoxBinary, err := readBusyBoxStatic()
	if err != nil {
		_ = backend.Close()
		_ = os.Remove(tmpRootfsPath)
		return err
	}
	if err := customizeGuest(efs, overlay, helloBinary, busyBoxBinary); err != nil {
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

	if artifacts.kernel == nil {
		kernel, err := readBootArtifact("", defaultKernelURL(), baseKernelSHA256, false)
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
	if artifacts.initrd == nil {
		initrd, err := readBootArtifact("", defaultInitrdURL(), baseInitrdSHA256, false)
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
ubuntu_version=%s
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
kernel_modules_source=%s
kernel_modules_sha256=%s
size=%s
ssh_auth=passwordless-ubuntu-over-wireguard
network=IPv6-only private VM network configured from firedoze kernel args
builder=firedoze-image-builder native-go
`, baseImageRelease, baseImageVersion, baseImageArch, source.name, baseRootSHA256, artifacts.kernel.path, baseKernelSHA256, artifacts.initrd.path, baseInitrdSHA256, baseKernelModulesURL, baseKernelModulesSHA256, *sizeText)
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

func installImage(args []string) error {
	fs := flag.NewFlagSet("firedoze-image-builder install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	srcDir := fs.String("src", defaultOutDir, "built artifact directory")
	dstDir := fs.String("dst", defaultImageInstallDir, "image install directory")
	userName := fs.String("user", "firedoze", "installed file owner")
	groupName := fs.String("group", "firedoze", "installed file group")

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

	uid, gid, err := lookupInstallOwner(*userName, *groupName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dstDir, 0o755); err != nil {
		return fmt.Errorf("create image install directory: %w", err)
	}
	if err := os.Chown(*dstDir, uid, gid); err != nil {
		return fmt.Errorf("set ownership on %s: %w", *dstDir, err)
	}
	if err := os.Chmod(*dstDir, 0o755); err != nil {
		return fmt.Errorf("set permissions on %s: %w", *dstDir, err)
	}

	for _, name := range []string{"vmlinux.bin", "initrd.img", "rootfs.ext4", "manifest.txt"} {
		src := filepath.Join(*srcDir, name)
		dst := filepath.Join(*dstDir, name)
		if err := copyInstallArtifact(src, dst, 0o644, uid, gid); err != nil {
			return err
		}
	}

	fmt.Printf("Installed firedoze base image artifacts in %s:\n", *dstDir)
	fmt.Println("  vmlinux.bin")
	fmt.Println("  initrd.img")
	fmt.Println("  rootfs.ext4")
	fmt.Println("  manifest.txt")
	return nil
}

func lookupInstallOwner(userName string, groupName string) (int, int, error) {
	uid := -1
	gid := -1
	if userName != "" {
		userInfo, err := osuser.Lookup(userName)
		if err != nil {
			return 0, 0, fmt.Errorf("lookup user %q: %w", userName, err)
		}
		parsed, err := strconv.Atoi(userInfo.Uid)
		if err != nil {
			return 0, 0, fmt.Errorf("parse uid for %q: %w", userName, err)
		}
		uid = parsed
	}
	if groupName != "" {
		groupInfo, err := osuser.LookupGroup(groupName)
		if err != nil {
			return 0, 0, fmt.Errorf("lookup group %q: %w", groupName, err)
		}
		parsed, err := strconv.Atoi(groupInfo.Gid)
		if err != nil {
			return 0, 0, fmt.Errorf("parse gid for %q: %w", groupName, err)
		}
		gid = parsed
	}
	return uid, gid, nil
}

func copyInstallArtifact(src string, dst string, mode os.FileMode, uid int, gid int) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create install directory for %s: %w", dst, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary install file for %s: %w", dst, err)
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set permissions on %s: %w", tmpPath, err)
	}
	if err := tmp.Chown(uid, gid); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set ownership on %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("install %s to %s: %w", src, dst, err)
	}
	removeTmp = false
	return nil
}

func defaultImageURL() string {
	return "https://cloud-images.ubuntu.com/resolute/20260421/resolute-server-cloudimg-amd64-root.tar.xz"
}

func defaultKernelURL() string {
	return "https://cloud-images.ubuntu.com/resolute/20260421/unpacked/resolute-server-cloudimg-amd64-vmlinuz-generic"
}

func defaultInitrdURL() string {
	return "https://cloud-images.ubuntu.com/resolute/20260421/unpacked/resolute-server-cloudimg-amd64-initrd-generic"
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

func readBusyBoxStatic() ([]byte, error) {
	artifact, err := readArtifact("", baseBusyBoxStaticURL, baseBusyBoxStaticSHA256, false)
	if err != nil {
		return nil, fmt.Errorf("read busybox-static package: %w", err)
	}
	binary, err := extractBusyBoxFromDeb(artifact.data)
	if err != nil {
		return nil, fmt.Errorf("extract busybox-static package: %w", err)
	}
	return binary, nil
}

func readKernelModulesDeb() ([]byte, error) {
	artifact, err := readArtifact("", baseKernelModulesURL, baseKernelModulesSHA256, false)
	if err != nil {
		return nil, fmt.Errorf("read kernel modules package: %w", err)
	}
	return artifact.data, nil
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

func extractBusyBoxFromDeb(deb []byte) ([]byte, error) {
	const globalHeader = "!<arch>\n"
	if !bytes.HasPrefix(deb, []byte(globalHeader)) {
		return nil, errors.New("invalid deb ar header")
	}
	offset := len(globalHeader)
	for offset < len(deb) {
		if offset+60 > len(deb) {
			return nil, errors.New("truncated deb ar member header")
		}
		header := deb[offset : offset+60]
		offset += 60
		name := strings.TrimSpace(string(header[:16]))
		name = strings.TrimSuffix(name, "/")
		sizeText := strings.TrimSpace(string(header[48:58]))
		size, err := strconv.ParseInt(sizeText, 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("invalid deb ar member size %q", sizeText)
		}
		if string(header[58:60]) != "`\n" {
			return nil, fmt.Errorf("invalid deb ar member trailer for %s", name)
		}
		end := offset + int(size)
		if end < offset || end > len(deb) {
			return nil, fmt.Errorf("truncated deb ar member %s", name)
		}
		data := deb[offset:end]
		offset = end
		if size%2 != 0 {
			offset++
		}
		if strings.HasPrefix(name, "data.tar.") {
			return extractBusyBoxFromDataTar(name, data)
		}
	}
	return nil, errors.New("deb package has no data.tar member")
}

func extractBusyBoxFromDataTar(name string, data []byte) ([]byte, error) {
	var r io.Reader = bytes.NewReader(data)
	switch {
	case strings.HasSuffix(name, ".zst"):
		dec, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		r = dec
	case strings.HasSuffix(name, ".xz"):
		dec, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		r = dec
	case strings.HasSuffix(name, ".gz"):
		dec, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		r = dec
	default:
		return nil, fmt.Errorf("unsupported deb data member compression: %s", name)
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		clean, ok := cleanTarPath(hdr.Name)
		if !ok {
			continue
		}
		if clean != "bin/busybox" && clean != "usr/bin/busybox" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("%s is not a regular file", hdr.Name)
		}
		return io.ReadAll(tr)
	}
	return nil, errors.New("data.tar does not contain busybox")
}

func installKernelModulesDeb(efs *ext4.FileSystem, deb []byte) error {
	if err := extractDebDataTarToRootfs(efs, deb, func(clean string) bool {
		return clean == "lib/modules" || strings.HasPrefix(clean, "lib/modules/")
	}); err != nil {
		return fmt.Errorf("extract kernel modules package: %w", err)
	}
	return nil
}

func extractDebDataTarToRootfs(efs *ext4.FileSystem, deb []byte, include func(string) bool) error {
	const globalHeader = "!<arch>\n"
	if !bytes.HasPrefix(deb, []byte(globalHeader)) {
		return errors.New("invalid deb ar header")
	}
	offset := len(globalHeader)
	for offset < len(deb) {
		if offset+60 > len(deb) {
			return errors.New("truncated deb ar member header")
		}
		header := deb[offset : offset+60]
		offset += 60
		name := strings.TrimSpace(string(header[:16]))
		name = strings.TrimSuffix(name, "/")
		sizeText := strings.TrimSpace(string(header[48:58]))
		size, err := strconv.ParseInt(sizeText, 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("invalid deb ar member size %q", sizeText)
		}
		if string(header[58:60]) != "`\n" {
			return fmt.Errorf("invalid deb ar member trailer for %s", name)
		}
		end := offset + int(size)
		if end < offset || end > len(deb) {
			return fmt.Errorf("truncated deb ar member %s", name)
		}
		data := deb[offset:end]
		offset = end
		if size%2 != 0 {
			offset++
		}
		if strings.HasPrefix(name, "data.tar.") {
			return extractDataTarToRootfs(efs, name, data, include)
		}
	}
	return errors.New("deb package has no data.tar member")
}

func extractDataTarToRootfs(efs *ext4.FileSystem, name string, data []byte, include func(string) bool) error {
	r, closeFn, err := compressedTarReader(name, data)
	if err != nil {
		return err
	}
	defer closeFn()

	tr := tar.NewReader(r)
	var hardlinks []pendingHardlink
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		clean, ok := cleanTarPath(hdr.Name)
		if !ok || !include(clean) {
			continue
		}
		mode := tarFileMode(hdr)
		if hdr.FileInfo().IsDir() {
			if err := mkdirAll(efs, clean, mode, hdr.Uid, hdr.Gid, hdr.ModTime); err != nil {
				return fmt.Errorf("create dir /%s: %w", clean, err)
			}
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			fileData, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("read /%s: %w", clean, err)
			}
			if err := writeFile(efs, clean, fileData, mode, hdr.Uid, hdr.Gid, hdr.ModTime); err != nil {
				return fmt.Errorf("write /%s: %w", clean, err)
			}
		case tar.TypeDir:
			if err := mkdirAll(efs, clean, mode, hdr.Uid, hdr.Gid, hdr.ModTime); err != nil {
				return fmt.Errorf("create dir /%s: %w", clean, err)
			}
		case tar.TypeSymlink:
			if err := symlink(efs, hdr.Linkname, clean, hdr.ModTime); err != nil {
				return fmt.Errorf("symlink /%s: %w", clean, err)
			}
		case tar.TypeLink:
			target, ok := cleanTarPath(hdr.Linkname)
			if !ok {
				continue
			}
			hcopy := *hdr
			hardlinks = append(hardlinks, pendingHardlink{path: clean, target: target, header: &hcopy})
		default:
			// Ignore device nodes and other special entries. They are not needed for
			// kernel module loading in the guest.
		}
	}

	for _, link := range hardlinks {
		data, err := efs.ReadFile(link.target)
		if err != nil {
			return fmt.Errorf("read hardlink target /%s for /%s: %w", link.target, link.path, err)
		}
		if err := writeFile(efs, link.path, data, tarFileMode(link.header), link.header.Uid, link.header.Gid, link.header.ModTime); err != nil {
			return fmt.Errorf("write hardlink copy /%s: %w", link.path, err)
		}
	}
	return nil
}

func compressedTarReader(name string, data []byte) (io.Reader, func(), error) {
	switch {
	case strings.HasSuffix(name, ".zst"):
		dec, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		return dec, dec.Close, nil
	case strings.HasSuffix(name, ".xz"):
		dec, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		return dec, func() {}, nil
	case strings.HasSuffix(name, ".gz"):
		dec, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		return dec, func() { _ = dec.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unsupported deb data member compression: %s", name)
	}
}

func buildGuestHelloBinary(arch string) ([]byte, error) {
	if path := packagedGuestHelloBinaries[arch]; path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read packaged firedoze-hello guest binary %s: %w", path, err)
		}
	}
	return buildGuestHelloBinaryFromSource(arch)
}

func buildGuestHelloBinaryFromSource(arch string) ([]byte, error) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "firedoze-hello-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	defer os.Remove(tmpPath)

	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", tmpPath, "./cmd/firedoze-hello")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("build firedoze-hello guest binary: %w\n%s", err, strings.TrimSpace(string(output)))
	}
	return os.ReadFile(tmpPath)
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module firedoze") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("could not find firedoze repo root; run firedoze-image-builder from a firedoze source checkout")
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

type fileOverlay struct {
	mode      os.FileMode
	uid       int
	gid       int
	transform func([]byte) []byte
	data      []byte
}

type guestOverlay struct {
	files    map[string]fileOverlay
	symlinks map[string]string
}

func newGuestOverlay() *guestOverlay {
	return &guestOverlay{
		files: map[string]fileOverlay{
			"etc/passwd": {
				mode: 0o644, uid: 0, gid: 0,
				transform: func(data []byte) []byte {
					return []byte(ensureNamedLineText(string(data), "ubuntu", "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash"))
				},
			},
			"etc/shadow": {
				mode: 0o640, uid: 0, gid: 42,
				transform: func(data []byte) []byte {
					return []byte(ensureNamedLineText(string(data), "ubuntu", "ubuntu::19723:0:99999:7:::"))
				},
			},
			"etc/group": {
				mode: 0o644, uid: 0, gid: 0,
				transform: func(data []byte) []byte {
					text := ensureGroupMemberText(string(data), "ubuntu", "x", "1000", "ubuntu")
					text = ensureGroupMemberText(text, "sudo", "x", "27", "ubuntu")
					return []byte(text)
				},
			},
			"etc/gshadow": {
				mode: 0o640, uid: 0, gid: 42,
				transform: func(data []byte) []byte {
					text := ensureGroupMemberText(string(data), "ubuntu", "!", "", "ubuntu")
					text = ensureGroupMemberText(text, "sudo", "*", "", "ubuntu")
					return []byte(text)
				},
			},
			"etc/fstab": {
				mode: 0o644, uid: 0, gid: 0,
				transform: func([]byte) []byte {
					return []byte("/dev/vda / ext4 defaults,errors=remount-ro 0 1\n")
				},
			},
		},
		symlinks: map[string]string{
			"etc/systemd/system/ssh.socket":                                       "/dev/null",
			"etc/systemd/system/cloud-init.service":                               "/dev/null",
			"etc/systemd/system/cloud-init-local.service":                         "/dev/null",
			"etc/systemd/system/cloud-config.service":                             "/dev/null",
			"etc/systemd/system/cloud-final.service":                              "/dev/null",
			"etc/systemd/system/systemd-networkd-wait-online.service":             "/dev/null",
			"etc/systemd/system/multipathd.service":                               "/dev/null",
			"etc/systemd/system/multipathd.socket":                                "/dev/null",
			"etc/ssh/sshd_config.d/60-cloudimg-settings.conf":                     "",
			"etc/systemd/system/sockets.target.wants/ssh.socket":                  "",
			"etc/systemd/system/multi-user.target.wants/firedoze-network.service": "/etc/systemd/system/firedoze-network.service",
			"etc/systemd/system/multi-user.target.wants/firedoze-sshd.service":    "/etc/systemd/system/firedoze-sshd.service",
		},
	}
}

func (o *guestOverlay) shouldSkip(p string) bool {
	if _, ok := o.files[p]; ok {
		return true
	}
	_, ok := o.symlinks[p]
	return ok
}

func (o *guestOverlay) captureFile(p string, data []byte) bool {
	file, ok := o.files[p]
	if !ok {
		return false
	}
	file.data = file.transform(data)
	o.files[p] = file
	return true
}

func (o *guestOverlay) apply(efs *ext4.FileSystem, modTime time.Time) error {
	for p, file := range o.files {
		data := file.data
		if data == nil {
			data = file.transform(nil)
		}
		if err := writeFile(efs, p, data, file.mode, file.uid, file.gid, modTime); err != nil {
			return fmt.Errorf("write overlay /%s: %w", p, err)
		}
	}
	for p, target := range o.symlinks {
		if target == "" {
			continue
		}
		if err := symlink(efs, target, p, modTime); err != nil {
			return fmt.Errorf("symlink overlay /%s: %w", p, err)
		}
	}
	return nil
}

func populateRootfs(efs *ext4.FileSystem, tr *tar.Reader, overlay *guestOverlay) (bootArtifacts, error) {
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
		mode := tarFileMode(hdr)
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
			if overlay.captureFile(clean, data) {
				artifacts.remember(clean, data)
				continue
			}
			if overlay.shouldSkip(clean) {
				continue
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
			if overlay.shouldSkip(clean) {
				continue
			}
			if err := symlink(efs, hdr.Linkname, clean, hdr.ModTime); err != nil {
				return artifacts, fmt.Errorf("symlink /%s: %w", clean, err)
			}
		case tar.TypeLink:
			target, ok := cleanTarPath(hdr.Linkname)
			if !ok {
				continue
			}
			if overlay.shouldSkip(clean) {
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
		if err := writeFile(efs, link.path, data, tarFileMode(link.header), link.header.Uid, link.header.Gid, link.header.ModTime); err != nil {
			return artifacts, fmt.Errorf("write hardlink copy /%s: %w", link.path, err)
		}
		artifacts.remember(link.path, data)
	}

	return artifacts, nil
}

func tarFileMode(hdr *tar.Header) os.FileMode {
	mode := os.FileMode(hdr.Mode & 0o777)
	if hdr.Mode&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if hdr.Mode&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if hdr.Mode&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
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

func customizeGuest(efs *ext4.FileSystem, overlay *guestOverlay, helloBinary []byte, busyBoxBinary []byte) error {
	now := time.Now()
	files := []struct {
		path string
		mode os.FileMode
		data string
	}{
		{
			path: "etc/ssh/sshd_config.d/99-firedoze.conf",
			mode: 0o644,
			data: "PubkeyAuthentication no\nPasswordAuthentication yes\nPermitEmptyPasswords yes\nKbdInteractiveAuthentication no\nUsePAM no\n",
		},
		{
			path: "etc/profile.d/firedoze-prompt.sh",
			mode: 0o644,
			data: `# firedoze VM shell prompt.
# shellcheck shell=sh

case "$-" in
  *i*) ;;
  *) return 0 2>/dev/null || exit 0 ;;
esac

firedoze_prompt_host="$(hostname 2>/dev/null || printf vm)"

if [ "$(id -u 2>/dev/null || printf 1)" = "0" ]; then
  firedoze_prompt_char="#"
else
  firedoze_prompt_char="$"
fi

case "${TERM:-}" in
  xterm*|screen*|tmux*|rxvt*|linux)
    PS1='\[\033[1;36m\]firedoze\[\033[0m\] \[\033[1;33m\]'"$firedoze_prompt_host"'\[\033[0m\] \w '"$firedoze_prompt_char"' '
    ;;
  *)
    PS1='firedoze '"$firedoze_prompt_host"' \w '"$firedoze_prompt_char"' '
    ;;
esac

unset firedoze_prompt_host firedoze_prompt_char
`,
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

guest_ip=""
host_ip=""
dns_ip=""
dns_domain="firedoze"
for arg in $(cat /proc/cmdline 2>/dev/null || true); do
  case "$arg" in
    firedoze.guest_ip=*) guest_ip="${arg#firedoze.guest_ip=}" ;;
    firedoze.host_ip=*) host_ip="${arg#firedoze.host_ip=}" ;;
    firedoze.dns_ip=*) dns_ip="${arg#firedoze.dns_ip=}" ;;
    firedoze.dns_domain=*) dns_domain="${arg#firedoze.dns_domain=}" ;;
  esac
done

if [ -z "$guest_ip" ] || [ -z "$host_ip" ]; then
  echo "missing firedoze.guest_ip or firedoze.host_ip kernel arg" >&2
  exit 1
fi

/bin/ip addr flush dev "$dev"
/bin/ip -6 addr flush dev "$dev" scope global || true
/bin/ip link set "$dev" up
/bin/ip -6 addr add "$guest_ip/127" dev "$dev"
/bin/ip -6 route replace default via "$host_ip" dev "$dev"
if [ -n "$dns_ip" ]; then
  /bin/ip -6 route replace "$dns_ip/128" via "$host_ip" dev "$dev"
fi

rm -f /etc/resolv.conf
if [ -n "$dns_ip" ]; then
  cat >/etc/resolv.conf <<RESOLV
search $dns_domain
nameserver $dns_ip
RESOLV
else
  cat >/etc/resolv.conf <<RESOLV
nameserver 1.1.1.1
nameserver 8.8.8.8
RESOLV
fi
`,
		},
		{
			path: "usr/local/bin/firedoze-hello",
			mode: 0o755,
			data: `#!/bin/sh
set -eu

port="${1:-8080}"
case "$port" in
  ''|*[!0-9]*)
    echo "usage: firedoze-hello [port]" >&2
    exit 2
    ;;
esac
if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
  echo "port must be between 1 and 65535" >&2
  exit 2
fi

if ! command -v nc >/dev/null 2>&1; then
  echo "firedoze-hello requires nc/netcat in the guest image" >&2
  exit 1
fi

echo "firedoze-hello listening on [::]:$port" >&2
while :; do
  fifo="/tmp/firedoze-hello.$$.fifo"
  rm -f "$fifo"
  mkfifo "$fifo"
  exec 3<>"$fifo"
  nc -6 -l -p "$port" -q 1 <"$fifo" | {
    IFS= read -r request_line || true
    request_line="$(printf '%s\n' "$request_line" | tr -d '\r')"
    request_path="$(printf '%s\n' "$request_line" | awk '{print $2}')"
    if [ "$request_path" = "/favicon.ico" ] || [ "$request_path" = "/favicon.svg" ]; then
      favicon='<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#111827"/><text x="32" y="43" text-anchor="middle" font-size="38">😴</text></svg>'
      {
        printf 'HTTP/1.1 200 OK\r\n'
        printf 'Content-Type: image/svg+xml; charset=utf-8\r\n'
        printf 'Cache-Control: public, max-age=86400\r\n'
        printf 'Connection: close\r\n'
        printf '\r\n'
        printf '%s\n' "$favicon"
      } >&3
      exit 0
    fi

    uptime_seconds="$(cut -d' ' -f1 /proc/uptime 2>/dev/null | cut -d. -f1 || true)"
    if [ -n "$uptime_seconds" ]; then
      days=$((uptime_seconds / 86400))
      hours=$(((uptime_seconds % 86400) / 3600))
      minutes=$(((uptime_seconds % 3600) / 60))
      if [ "$days" -gt 0 ]; then
        uptime_text="${days}d ${hours}h ${minutes}m"
      elif [ "$hours" -gt 0 ]; then
        uptime_text="${hours}h ${minutes}m"
      else
        uptime_text="${minutes}m"
      fi
    else
      uptime_text="unknown"
    fi
    load_text="$(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null || printf 'unknown')"

    printf 'HTTP/1.1 200 OK\r\n'
    printf 'Content-Type: text/plain; charset=utf-8\r\n'
    printf 'Connection: close\r\n'
    printf '\r\n'
    printf 'firedoze hello\n'
    printf '==============\n'
    printf '\n'
    printf 'Host\n'
    printf '  time:     %s\n' "$(date -Iseconds 2>/dev/null || date)"
    printf '  hostname: %s\n' "$(hostname)"
    printf '  user:     %s (uid %s)\n' "$(id -un)" "$(id -u)"
    printf '  kernel:   %s %s\n' "$(uname -s)" "$(uname -r)"
    printf '  uptime:   %s\n' "$uptime_text"
    printf '  load:     %s\n' "$load_text"
    printf '\n'
    printf 'Network\n'
    ip -brief -6 addr show scope global 2>/dev/null | awk '{printf "  %-8s %s\n", $1, $3}' || true
    printf '\n'
    printf 'Routes\n'
    ip -6 route 2>/dev/null | awk '/^default / {print "  default via " $3 " dev " $5; next} {print "  " $0}' || true
  } >&3
  exec 3>&-
  rm -f "$fifo"
done
`,
		},
		{
			path: "usr/local/bin/firedoze-stop",
			mode: 0o755,
			data: `#!/bin/sh
set -eu

reboot_bin="$(command -v reboot || true)"
if [ -z "$reboot_bin" ]; then
  echo "firedoze-stop requires the guest reboot command" >&2
  exit 1
fi

echo "firedoze-stop: stopping this Firedoze VM" >&2
if [ "$(id -u)" = "0" ]; then
  exec "$reboot_bin"
fi
if command -v sudo >/dev/null 2>&1; then
  exec sudo "$reboot_bin"
fi

echo "firedoze-stop requires root privileges or sudo" >&2
exit 1
`,
		},
		{
			path: "usr/local/bin/firedoze-hello-service",
			mode: 0o755,
			data: `#!/bin/sh
set -eu

usage() {
  echo "usage: firedoze-hello-service <install|start|stop|restart|status|disable> [port] [-verbose]" >&2
}

cmd="${1:-}"
shift || true
case "$cmd" in
  install|start|stop|restart|status|disable) ;;
  -h|--help|"")
    usage
    exit 2
    ;;
  *)
    usage
    exit 2
    ;;
esac

port=8080
verbose=
for arg in "$@"; do
  case "$arg" in
    -verbose)
      verbose=" -verbose"
      ;;
    ''|*[!0-9]*)
      echo "unexpected argument: $arg" >&2
      usage
      exit 2
      ;;
    *)
      port="$arg"
      ;;
  esac
done

case "$port" in
  ''|*[!0-9]*)
    echo "port must be numeric" >&2
    exit 2
    ;;
esac
if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
  echo "port must be between 1 and 65535" >&2
  exit 2
fi

unit=/etc/systemd/system/firedoze-hello.service

install_unit() {
  cat >"$unit" <<UNIT
[Unit]
Description=firedoze hello web server
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/firedoze-hello $port$verbose
Restart=always
RestartSec=1s
User=ubuntu
Group=ubuntu

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable firedoze-hello.service >/dev/null
}

case "$cmd" in
  install)
    install_unit
    systemctl restart firedoze-hello.service
    systemctl --no-pager --full status firedoze-hello.service
    ;;
  start)
    systemctl start firedoze-hello.service
    ;;
  stop)
    systemctl stop firedoze-hello.service
    ;;
  restart)
    systemctl restart firedoze-hello.service
    ;;
  status)
    systemctl --no-pager --full status firedoze-hello.service
    ;;
  disable)
    systemctl disable --now firedoze-hello.service >/dev/null || true
    rm -f "$unit"
    systemctl daemon-reload
    ;;
esac
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
			data: "datasource_list: [ None ]\npreserve_hostname: true\nmanage_etc_hosts: false\nssh_pwauth: true\ndisable_root: false\n",
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
		"usr/local/bin",
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
	if err := writeFile(efs, "usr/local/bin/firedoze-hello", helloBinary, 0o755, 0, 0, now); err != nil {
		return err
	}
	if err := writeFile(efs, "usr/bin/busybox", busyBoxBinary, 0o755, 0, 0, now); err != nil {
		return err
	}
	if err := overlay.apply(efs, now); err != nil {
		return err
	}
	if err := writeFile(efs, "etc/sudoers.d/90-firedoze-ubuntu", []byte("ubuntu ALL=(ALL) NOPASSWD:ALL\n"), 0o440, 0, 0, now); err != nil {
		return err
	}
	_ = efs.Chown("home/ubuntu", 1000, 1000)

	return nil
}

func ensureNamedLineText(text string, name string, line string) string {
	lines := nonemptyLines(text)
	found := false
	for i, current := range lines {
		if strings.HasPrefix(current, name+":") {
			lines[i] = line
			found = true
		}
	}
	if !found {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n") + "\n"
}

func ensureGroupMemberText(text string, name string, password string, gid string, member string) string {
	lines := nonemptyLines(text)
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
	return strings.Join(lines, "\n") + "\n"
}

func nonemptyLines(text string) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
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
		if info, err := efs.Stat(current); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("/%s exists and is not a directory", current)
			}
			continue
		}
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
	flags := os.O_RDWR
	if _, err := efs.Stat(full); err != nil {
		flags |= os.O_CREATE
	} else if err := efs.Truncate(full, 0); err != nil {
		return err
	}
	f, err := efs.OpenFile(full, flags)
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
	if current, err := efs.ReadLink(full); err == nil {
		if current != target {
			return fmt.Errorf("/%s already points to %q, not %q", full, current, target)
		}
		_ = efs.Chtimes(full, modTime, modTime, modTime)
		return nil
	}
	if _, err := efs.Stat(full); err == nil {
		return fmt.Errorf("/%s already exists and is not a symlink", full)
	}
	return efs.Symlink(target, full)
}

func replaceFile(p string, data []byte, mode os.FileMode) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
