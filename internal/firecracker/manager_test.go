package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/store"
)

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.Metadata.Path = filepath.Join(dir, "firedoze.db")
	return NewManager(cfg, st, slog.New(slog.NewTextHandler(os.Stderr, nil))), st
}

func configureTestBaseImage(t *testing.T, m *Manager) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	kernel := filepath.Join(dir, "vmlinux.bin")
	initrd := filepath.Join(dir, "initrd.img")
	if err := os.WriteFile(rootfs, []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.txt"), []byte("# firedoze image\nubuntu_version=24.04\nimage_id=test-image\nbad-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.cfg.Firecracker.BaseRootfsPath = rootfs
	m.cfg.Firecracker.BaseKernelPath = kernel
	m.cfg.Firecracker.BaseInitrdPath = initrd
}

func TestCreateVMDefaultsAndBaseImageMetadata(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t)
	configureTestBaseImage(t, m)
	m.cfg.VMNetwork.Subnet = "fd00::/126"

	vm, err := m.CreateVM(ctx, store.CreateVMParams{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if vm.PrivateIP != "fd00::3" {
		t.Fatalf("PrivateIP = %q, want fd00::3", vm.PrivateIP)
	}
	if vm.VCPUs != m.cfg.Firecracker.DefaultVCPUs || vm.MemoryMinMiB != m.cfg.Firecracker.DefaultMemoryMinMiB || vm.MemoryMaxMiB != m.cfg.Firecracker.DefaultMemoryMaxMiB || vm.DiskBytes != m.cfg.Firecracker.DefaultDiskBytes {
		t.Fatalf("VM defaults = %#v", vm)
	}
	if vm.DefaultHTTPPort != m.cfg.DefaultHTTPPort {
		t.Fatalf("DefaultHTTPPort = %d, want %d", vm.DefaultHTTPPort, m.cfg.DefaultHTTPPort)
	}
	if !vm.AutoWake {
		t.Fatal("AutoWake = false, want default true")
	}
	if vm.BaseImageID == "" || vm.KernelID == "" {
		t.Fatalf("metadata IDs missing: base=%q kernel=%q", vm.BaseImageID, vm.KernelID)
	}
	var metadata BaseImageMetadata
	if err := json.Unmarshal([]byte(vm.BaseImageMetadata), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Manifest["ubuntu_version"] != "24.04" || metadata.Manifest["image_id"] != "test-image" {
		t.Fatalf("manifest metadata = %#v", metadata.Manifest)
	}
	if metadata.Initrd == nil || metadata.Initrd.Basename != "initrd.img" {
		t.Fatalf("initrd metadata = %#v", metadata.Initrd)
	}
}

func TestCreateVMValidationAndExplicitAutoWake(t *testing.T) {
	m, _ := newTestManager(t)
	configureTestBaseImage(t, m)

	_, err := m.CreateVM(context.Background(), store.CreateVMParams{Name: "bad", IdleSleepAfterSeconds: -1})
	if err == nil {
		t.Fatal("CreateVM accepted negative idle sleep timeout")
	}
	vm, err := m.CreateVM(context.Background(), store.CreateVMParams{
		Name:        "demo",
		PrivateIP:   "fd00::99",
		AutoWake:    false,
		AutoWakeSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vm.PrivateIP != "fd00::99" || vm.AutoWake {
		t.Fatalf("VM = %#v", vm)
	}
}

func TestCreateAndRestoreRejectRouteNameConflict(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "owner", PrivateIP: "fd00::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoute(ctx, store.CreateRouteParams{Name: "alias", VMName: "owner", Port: 8080}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.CreateVM(ctx, store.CreateVMParams{Name: "alias"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateVM route-name conflict error = %v, want ErrAlreadyExists", err)
	}
	if _, err := m.RestoreSnapshot(ctx, "missing-snapshot", store.CreateVMParams{Name: "alias"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("RestoreSnapshot route-name conflict error = %v, want ErrAlreadyExists", err)
	}
}

func TestBaseImageMetadataCacheAndManifestParsing(t *testing.T) {
	m, _ := newTestManager(t)
	configureTestBaseImage(t, m)

	first, err := m.baseImageMetadata()
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.baseImageMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if first.Rootfs.SHA256 != second.Rootfs.SHA256 {
		t.Fatalf("cached metadata changed: %q vs %q", first.Rootfs.SHA256, second.Rootfs.SHA256)
	}
	if first.Manifest["ubuntu_version"] != "24.04" {
		t.Fatalf("manifest = %#v", first.Manifest)
	}

	if err := os.WriteFile(m.cfg.Firecracker.BaseRootfsPath, []byte("rootfs changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := m.baseImageMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if third.Rootfs.SHA256 == first.Rootfs.SHA256 {
		t.Fatalf("rootfs SHA did not change after artifact update: %s", third.Rootfs.SHA256)
	}
}

func TestWarmBaseImageMetadataHonorsCanceledContext(t *testing.T) {
	m, _ := newTestManager(t)
	configureTestBaseImage(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.WarmBaseImageMetadata(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WarmBaseImageMetadata error = %v, want context.Canceled", err)
	}
}

func TestApplyBaseImageMetadataFallbacks(t *testing.T) {
	params := store.CreateVMParams{}
	applyBaseImageMetadata(&params, BaseImageMetadata{
		Rootfs: ArtifactMetadata{Basename: "rootfs.ext4"},
		Kernel: ArtifactMetadata{Basename: "vmlinux.bin"},
	})
	if params.BaseImageID != "rootfs.ext4" || params.KernelID != "vmlinux.bin" {
		t.Fatalf("fallback IDs = %q/%q", params.BaseImageID, params.KernelID)
	}
	if params.BaseImageMetadata == "" {
		t.Fatal("BaseImageMetadata was not populated")
	}
}

func TestNextPrivateIPSkipsUsedAndDetectsExhaustion(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	m.cfg.VMNetwork.Subnet = "fd00::/125"
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "first", PrivateIP: "fd00::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	ip, err := m.nextPrivateIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ip.String() != "fd00::5" {
		t.Fatalf("next IP = %s, want fd00::5", ip)
	}

	m.cfg.VMNetwork.Subnet = "fd00::/126"
	_, err = m.nextPrivateIP(ctx)
	if err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("exhausted nextPrivateIP error = %v", err)
	}

	m.cfg.VMNetwork.Subnet = "10.0.0.0/24"
	_, err = m.nextPrivateIP(ctx)
	if err == nil || !strings.Contains(err.Error(), "IPv6") {
		t.Fatalf("IPv4 nextPrivateIP error = %v", err)
	}
}

func TestUpdateVMValidation(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "demo", PrivateIP: "fd00::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	badPort := 70000
	if _, err := m.UpdateVM(ctx, "demo", store.UpdateVMParams{DefaultHTTPPort: &badPort}); err == nil {
		t.Fatal("UpdateVM accepted bad default_http_port")
	}
	badIdle := -1
	if _, err := m.UpdateVM(ctx, "demo", store.UpdateVMParams{IdleSleepAfterSeconds: &badIdle}); err == nil {
		t.Fatal("UpdateVM accepted negative idle timeout")
	}
	goodPort := 3000
	vm, err := m.UpdateVM(ctx, "demo", store.UpdateVMParams{DefaultHTTPPort: &goodPort})
	if err != nil {
		t.Fatal(err)
	}
	if vm.DefaultHTTPPort != 3000 {
		t.Fatalf("DefaultHTTPPort = %d, want 3000", vm.DefaultHTTPPort)
	}
}

func TestRestoreSnapshotCopiesDiskAndMetadata(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	snapshotDir := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshotDisk := filepath.Join(snapshotDir, "rootfs.ext4")
	if err := os.WriteFile(snapshotDisk, []byte("snapshot disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSnapshot(ctx, store.CreateSnapshotParams{
		Name:              "snap",
		SourceVM:          "source",
		DiskPath:          snapshotDisk,
		BaseImageID:       "base-id",
		KernelID:          "kernel-id",
		BaseImageMetadata: `{"image":"metadata"}`,
	}); err != nil {
		t.Fatal(err)
	}
	var rewrittenDisk, rewrittenName string
	m.rewriteGuestIdentityFunc = func(_ context.Context, diskPath string, vmName string) error {
		rewrittenDisk = diskPath
		rewrittenName = vmName
		return nil
	}

	vm, err := m.RestoreSnapshot(ctx, "snap", store.CreateVMParams{Name: "copy", MemoryMinMiB: 256, MemoryMaxMiB: 256, PublicHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name != "copy" || vm.MemoryMinMiB != 256 || vm.MemoryMaxMiB != 256 || !vm.AutoWake || !vm.PublicHTTP || vm.BaseImageID != "base-id" || vm.KernelID != "kernel-id" || string(vm.BaseImageMetadata) != `{"image":"metadata"}` {
		t.Fatalf("restored VM = %#v", vm)
	}
	data, err := os.ReadFile(m.layout("copy").diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "snapshot disk" {
		t.Fatalf("restored disk = %q, want snapshot disk", data)
	}
	if rewrittenDisk != m.layout("copy").diskPath || rewrittenName != "copy" {
		t.Fatalf("rewrite called with %q/%q", rewrittenDisk, rewrittenName)
	}

	_, err = m.RestoreSnapshot(ctx, "snap", store.CreateVMParams{Name: "copy"})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate restore error = %v, want ErrAlreadyExists", err)
	}
}

func TestRestoreSnapshotValidation(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.RestoreSnapshot(context.Background(), "", store.CreateVMParams{Name: "copy"}); err == nil {
		t.Fatal("RestoreSnapshot accepted empty snapshot name")
	}
	if _, err := m.RestoreSnapshot(context.Background(), "snap", store.CreateVMParams{}); err == nil {
		t.Fatal("RestoreSnapshot accepted empty VM name")
	}
}

func TestManagerListAndGetWrappers(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "alpha", PrivateIP: "fd00::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "beta", PrivateIP: "fd00::5", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSnapshot(ctx, store.CreateSnapshotParams{Name: "snap", DiskPath: "/disk"}); err != nil {
		t.Fatal(err)
	}

	vms, err := m.ListVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("ListVMs len = %d, want 2", len(vms))
	}
	vms, err = m.ListVMsMatching(ctx, []string{"a*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].Name != "alpha" {
		t.Fatalf("ListVMsMatching = %#v", vms)
	}
	vm, err := m.GetVM(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name != "beta" {
		t.Fatalf("GetVM = %#v", vm)
	}
	snapshots, err := m.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Name != "snap" {
		t.Fatalf("ListSnapshots = %#v", snapshots)
	}
	snapshot, err := m.GetSnapshot(ctx, "snap")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Name != "snap" {
		t.Fatalf("GetSnapshot = %#v", snapshot)
	}
}

func TestReconcileStartupMarksRunningVMsLostAndIgnoresCleanupErrors(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "stopped", PrivateIP: "fd00::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "running", PrivateIP: "fd00::5", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(ctx, "running", "running"); err != nil {
		t.Fatal(err)
	}
	var cleaned []string
	restore := stubDeleteTap(t, func(name string) error {
		cleaned = append(cleaned, name)
		return errors.New("cleanup failed")
	})
	defer restore()

	if err := m.ReconcileStartup(ctx); err != nil {
		t.Fatal(err)
	}
	running, err := st.GetVM(ctx, "running")
	if err != nil {
		t.Fatal(err)
	}
	if running.State != "lost" {
		t.Fatalf("running state = %q, want lost", running.State)
	}
	stopped, err := st.GetVM(ctx, "stopped")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "stopped" {
		t.Fatalf("stopped state = %q, want stopped", stopped.State)
	}
	if strings.Join(cleaned, ",") != "fdtap-running,fdtap-stopped" {
		t.Fatalf("cleanup taps = %#v", cleaned)
	}
}

func TestStopVMWithoutRunningProcessMarksStopped(t *testing.T) {
	m, st := newTestManager(t)
	createSnapshotTestVM(t, m, st, "demo", "running")

	if err := m.StopVM(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	vm, err := st.GetVM(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != "stopped" {
		t.Fatalf("state = %q, want stopped", vm.State)
	}
}

func TestFirecrackerProcessExitMarksStoppedAndCleansUp(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	vm := createSnapshotTestVM(t, m, st, "demo", "running")
	layout := m.layout(vm.Name)
	if err := os.MkdirAll(layout.runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.socketPath, []byte("socket"), 0o644); err != nil {
		t.Fatal(err)
	}
	var cleaned []string
	restore := stubDeleteTap(t, func(name string) error {
		cleaned = append(cleaned, name)
		return nil
	})
	defer restore()
	m.cfg.Firecracker.BinaryPath = "/usr/bin/true"

	proc, err := m.launchProcess(vm.Name, layout, preparedNetwork{
		tapName:   tapName(vm.Name),
		guestCIDR: vm.PrivateIP + "/127",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.waitForProcessExit(ctx, proc, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	updated, err := st.GetVM(ctx, vm.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "stopped" {
		t.Fatalf("state = %q, want stopped", updated.State)
	}
	if _, err := os.Stat(layout.socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket stat = %v, want removed", err)
	}
	if strings.Join(cleaned, ",") != "fdtap-demo" {
		t.Fatalf("cleanup taps = %#v", cleaned)
	}
}

func TestSleepRunningVMsWithNoRunningVMs(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.SleepRunningVMs(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBeginVMOperationGuardsSameName(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.beginVMOperation("demo"); err != nil {
		t.Fatal(err)
	}
	if err := m.beginVMOperation("demo"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second beginVMOperation error = %v, want ErrAlreadyRunning", err)
	}
	m.endVMOperation("demo")
	if err := m.beginVMOperation("demo"); err != nil {
		t.Fatal(err)
	}
	m.endVMOperation("demo")
}

func TestCopySparseFilePreservesSparseRegions(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src")
	dstPath := filepath.Join(dir, "dst")
	src, err := os.OpenFile(srcPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.WriteAt([]byte("begin"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := src.WriteAt([]byte("end"), 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := src.Truncate(128 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := copySparseFile(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if dstInfo.Size() != srcInfo.Size() {
		t.Fatalf("dst size = %d, want %d", dstInfo.Size(), srcInfo.Size())
	}
	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:5]) != "begin" || string(data[64<<20:int64(64<<20)+3]) != "end" {
		t.Fatal("copied sparse file content mismatch")
	}
	srcBlocks := allocatedBlocks(t, srcInfo)
	dstBlocks := allocatedBlocks(t, dstInfo)
	if dstBlocks > srcBlocks*2 {
		t.Fatalf("dst allocated blocks = %d, source = %d; copy did not preserve sparse regions", dstBlocks, srcBlocks)
	}
}

func allocatedBlocks(t *testing.T, info os.FileInfo) int64 {
	t.Helper()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("file info has no syscall.Stat_t")
	}
	return stat.Blocks
}

func createSnapshotTestVM(t *testing.T, m *Manager, st *store.Store, name string, state string) store.VM {
	t.Helper()
	vm, err := st.CreateVM(context.Background(), store.CreateVMParams{
		Name:         name,
		PrivateIP:    "fd7a:115c:a1e0::3",
		VCPUs:        1,
		MemoryMinMiB: 128, MemoryMaxMiB: 128,
		DiskBytes:       1024,
		DefaultHTTPPort: 8080,
		BaseImageID:     "base",
		KernelID:        "kernel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), name, state); err != nil {
		t.Fatal(err)
	}
	layout := m.layout(name)
	if err := os.MkdirAll(layout.vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.diskPath, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	vm.State = state
	return vm
}

func TestSaveSnapshotRejectsRunningVM(t *testing.T) {
	m, st := newTestManager(t)
	createSnapshotTestVM(t, m, st, "demo", "running")

	_, err := m.SaveSnapshot(context.Background(), store.CreateSnapshotParams{Name: "snap", SourceVM: "demo"})
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("SaveSnapshot error = %v, want ErrRunning", err)
	}
}

func TestSaveSnapshotRejectsVMOperationInProgress(t *testing.T) {
	m, st := newTestManager(t)
	createSnapshotTestVM(t, m, st, "demo", "sleeping")
	m.vmOps["demo"] = struct{}{}

	_, err := m.SaveSnapshot(context.Background(), store.CreateSnapshotParams{Name: "snap", SourceVM: "demo"})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("SaveSnapshot error = %v, want ErrAlreadyRunning", err)
	}
}

func TestSaveSnapshotFromStoppedVMCopiesDiskOnly(t *testing.T) {
	m, st := newTestManager(t)
	createSnapshotTestVM(t, m, st, "demo", "stopped")

	snapshot, err := m.SaveSnapshot(context.Background(), store.CreateSnapshotParams{Name: "snap", SourceVM: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StatePath != "" || snapshot.MemPath != "" {
		t.Fatalf("stopped snapshot state=%q mem=%q, want disk-only", snapshot.StatePath, snapshot.MemPath)
	}
	data, err := os.ReadFile(snapshot.DiskPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "disk" {
		t.Fatalf("snapshot disk = %q, want disk", data)
	}
}

func TestSnapshotExportImportBundlePreservesDiskAndMetadata(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	layout := m.snapshotLayout("snap")
	if err := os.MkdirAll(layout.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.diskPath, []byte("snapshot disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSnapshot(ctx, store.CreateSnapshotParams{
		Name:              "snap",
		SourceVM:          "source",
		DiskPath:          layout.diskPath,
		BaseImageID:       "base-id",
		KernelID:          "kernel-id",
		BaseImageMetadata: `{"image":"metadata"}`,
	}); err != nil {
		t.Fatal(err)
	}

	var bundle bytes.Buffer
	if err := m.ExportSnapshot(ctx, "snap", &bundle); err != nil {
		t.Fatal(err)
	}
	imported, err := m.ImportSnapshot(ctx, "imported", bytes.NewReader(bundle.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if imported.Name != "imported" || imported.SourceVM != "source" || imported.BaseImageID != "base-id" || imported.KernelID != "kernel-id" {
		t.Fatalf("imported snapshot = %#v", imported)
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(imported.BaseImageMetadata), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["image"] != "metadata" {
		t.Fatalf("imported metadata = %#v", metadata)
	}
	data, err := os.ReadFile(m.snapshotLayout("imported").diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "snapshot disk" {
		t.Fatalf("imported disk = %q, want snapshot disk", data)
	}

	_, err = m.ImportSnapshot(ctx, "imported", bytes.NewReader(bundle.Bytes()))
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate import error = %v, want ErrAlreadyExists", err)
	}
}

func TestSnapshotImportRejectsInvalidBundleAndCleansDirectory(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.ImportSnapshot(context.Background(), "bad", strings.NewReader("not a bundle"))
	if !errors.Is(err, ErrInvalidSnapshotBundle) {
		t.Fatalf("ImportSnapshot error = %v, want ErrInvalidSnapshotBundle", err)
	}
	if _, err := os.Stat(m.snapshotLayout("bad").dir); !os.IsNotExist(err) {
		t.Fatalf("bad snapshot dir stat error = %v, want not exist", err)
	}
}

func TestSaveSnapshotRejectsSleepingVM(t *testing.T) {
	m, st := newTestManager(t)
	createSnapshotTestVM(t, m, st, "demo", "sleeping")

	_, err := m.SaveSnapshot(context.Background(), store.CreateSnapshotParams{Name: "snap", SourceVM: "demo"})
	if !errors.Is(err, ErrNotStopped) {
		t.Fatalf("SaveSnapshot error = %v, want ErrNotStopped", err)
	}
}

func TestArchiveStoppedVMMovesDiskToColdStorage(t *testing.T) {
	m, st := newTestManager(t)
	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	m.cfg.ColdStorage.ArchiveStoppedAfterSeconds = 1
	createSnapshotTestVM(t, m, st, "demo", "stopped")

	if err := m.ArchiveStoppedVM(context.Background(), "demo", time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(m.layout("demo").diskPath); !os.IsNotExist(err) {
		t.Fatalf("hot disk stat error = %v, want not exist", err)
	}
	data, err := os.ReadFile(m.coldDiskPath("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "disk" {
		t.Fatalf("cold disk = %q, want disk", data)
	}
	vm, err := st.GetVM(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if vm.ArchivedDiskPath != m.coldDiskPath("demo") {
		t.Fatalf("archived_disk_path = %q, want %q", vm.ArchivedDiskPath, m.coldDiskPath("demo"))
	}
}

func TestHydrateColdDiskRestoresArchivedDisk(t *testing.T) {
	m, st := newTestManager(t)
	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	m.cfg.ColdStorage.ArchiveStoppedAfterSeconds = 1
	createSnapshotTestVM(t, m, st, "demo", "stopped")
	if err := m.ArchiveStoppedVM(context.Background(), "demo", time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	vm, err := st.GetVM(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if vm.ArchivedDiskPath == "" {
		t.Fatal("archived VM has empty archived_disk_path")
	}
	if err := m.hydrateColdDisk(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.layout("demo").diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "disk" {
		t.Fatalf("hot disk = %q, want disk", data)
	}
	if _, err := os.Stat(m.coldDiskPath("demo")); !os.IsNotExist(err) {
		t.Fatalf("cold disk stat error = %v, want not exist", err)
	}
	vm, err = st.GetVM(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if vm.ArchivedDiskPath != "" {
		t.Fatalf("archived_disk_path = %q, want empty", vm.ArchivedDiskPath)
	}
}

func TestSaveSnapshotFromArchivedStoppedVMCopiesColdDisk(t *testing.T) {
	m, st := newTestManager(t)
	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	m.cfg.ColdStorage.ArchiveStoppedAfterSeconds = 1
	createSnapshotTestVM(t, m, st, "demo", "stopped")
	if err := m.ArchiveStoppedVM(context.Background(), "demo", time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	snapshot, err := m.SaveSnapshot(context.Background(), store.CreateSnapshotParams{Name: "snap", SourceVM: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot.DiskPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "disk" {
		t.Fatalf("snapshot disk = %q, want disk", data)
	}
}

func TestHydrateColdDiskFailsWhenRecordedArchiveIsMissing(t *testing.T) {
	m, st := newTestManager(t)
	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	vm := createSnapshotTestVM(t, m, st, "demo", "stopped")
	missingPath := filepath.Join(m.cfg.ColdStorage.Dir, "vms", "demo", "rootfs.ext4")
	if err := st.SetVMArchivedDiskPath(context.Background(), "demo", missingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.layout("demo").diskPath); err != nil {
		t.Fatal(err)
	}
	vm.ArchivedDiskPath = missingPath

	err := m.hydrateColdDisk(context.Background(), vm)
	if err == nil {
		t.Fatal("hydrateColdDisk succeeded with missing archived disk")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hydrateColdDisk error = %v, want not exist", err)
	}
}

func TestBeginStartOperationCancelsColdArchive(t *testing.T) {
	m, _ := newTestManager(t)
	archiveCtx, endArchive, err := m.beginColdArchiveOperation(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	canceled := make(chan struct{})
	go func() {
		<-archiveCtx.Done()
		close(canceled)
		endArchive()
	}()

	if err := m.beginStartOperationCancelingColdArchive(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	defer m.endVMOperation("demo")

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("start operation did not cancel cold archive")
	}
}

func TestDeleteVMCancelsInProgressColdArchive(t *testing.T) {
	m, st := newTestManager(t)
	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	m.cfg.ColdStorage.ArchiveStoppedAfterSeconds = 1
	createSnapshotTestVM(t, m, st, "demo", "stopped")

	copyStarted := make(chan struct{})
	m.copyColdFile = func(ctx context.Context, dst string, src string) error {
		close(copyStarted)
		<-ctx.Done()
		return ctx.Err()
	}

	archiveDone := make(chan error, 1)
	go func() {
		archiveDone <- m.ArchiveStoppedVM(context.Background(), "demo", time.Now().Add(2*time.Second))
	}()

	select {
	case <-copyStarted:
	case <-time.After(time.Second):
		t.Fatal("archive copy did not start")
	}

	if err := m.DeleteVM(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-archiveDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("archive error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete did not wait for archive cancellation")
	}

	if _, err := st.GetVM(context.Background(), "demo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetVM after delete error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(m.coldDiskPath("demo")); !os.IsNotExist(err) {
		t.Fatalf("cold disk stat error = %v, want not exist", err)
	}
}

func TestDeleteStoppedVMRemovesDiskArchivesColdDirAndRoutes(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	createSnapshotTestVM(t, m, st, "demo", "stopped")
	archivedPath := filepath.Join(t.TempDir(), "archived-rootfs.ext4")
	if err := os.WriteFile(archivedPath, []byte("archived"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMArchivedDiskPath(ctx, "demo", archivedPath); err != nil {
		t.Fatal(err)
	}
	coldDir := m.coldVMDir("demo")
	if err := os.MkdirAll(coldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coldDir, "stale"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoute(ctx, store.CreateRouteParams{Name: "web", VMName: "demo", Port: 8080}); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteVM(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetVM(ctx, "demo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetVM after delete error = %v, want ErrNotFound", err)
	}
	if _, err := st.GetRoute(ctx, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRoute after delete error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(m.layout("demo").vmDir); !os.IsNotExist(err) {
		t.Fatalf("hot vm dir stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(archivedPath); !os.IsNotExist(err) {
		t.Fatalf("archived disk stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(coldDir); !os.IsNotExist(err) {
		t.Fatalf("cold dir stat error = %v, want not exist", err)
	}
}

func TestDeleteSnapshotRemovesDirectoryAndRow(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	snapshotLayout := m.snapshotLayout("snap")
	if err := os.MkdirAll(snapshotLayout.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotLayout.diskPath, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSnapshot(ctx, store.CreateSnapshotParams{Name: "snap", DiskPath: snapshotLayout.diskPath}); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteSnapshot(ctx, "snap"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSnapshot(ctx, "snap"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSnapshot after delete error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(snapshotLayout.dir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir stat error = %v, want not exist", err)
	}
	if err := m.DeleteSnapshot(ctx, "snap"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteSnapshot missing error = %v, want ErrNotFound", err)
	}
}

func TestSleepVMStoppedAndAlreadySleeping(t *testing.T) {
	m, st := newTestManager(t)
	createSnapshotTestVM(t, m, st, "demo", "stopped")
	if _, err := m.SleepVM(context.Background(), "demo"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("SleepVM stopped error = %v, want ErrNotRunning", err)
	}
	if err := st.SetVMState(context.Background(), "demo", "sleeping"); err != nil {
		t.Fatal(err)
	}
	vm, err := m.SleepVM(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != "sleeping" {
		t.Fatalf("sleeping VM state = %q", vm.State)
	}
}

func TestRunningVMNamesSortedAndStartVMsIgnoresAlreadyRunning(t *testing.T) {
	m, _ := newTestManager(t)
	m.running["beta"] = &Process{Name: "beta"}
	m.running["alpha"] = &Process{Name: "alpha"}

	names := m.RunningVMNames()
	if strings.Join(names, ",") != "alpha,beta" {
		t.Fatalf("RunningVMNames = %#v", names)
	}
	if err := m.StartVMs(context.Background(), []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
}

func TestBootArgsDNSAndHelpers(t *testing.T) {
	m, _ := newTestManager(t)
	netdev := preparedNetwork{
		hostIP:  net.ParseIP("fd00::2"),
		guestIP: net.ParseIP("fd00::3"),
	}
	m.cfg.DNS.Enabled = true
	m.cfg.DNS.ListenIP = "fd00::1"
	m.cfg.DNS.Domain = "firedoze"
	args := m.bootArgs(netdev)
	for _, want := range []string{"quiet", "loglevel=3", "systemd.show_status=false", "rd.systemd.show_status=false", "firedoze.guest_ip=fd00::3", "firedoze.host_ip=fd00::2", "firedoze.dns_ip=fd00::1", "firedoze.dns_domain=firedoze"} {
		if !strings.Contains(args, want) {
			t.Fatalf("boot args %q missing %q", args, want)
		}
	}
	m.cfg.DNS.Enabled = false
	if args := m.bootArgs(netdev); strings.Contains(args, "firedoze.dns_ip") {
		t.Fatalf("DNS disabled boot args include DNS: %s", args)
	}
	if got := tapName("short"); got != "fdtap-short" {
		t.Fatalf("tapName(short) = %q", got)
	}
	if got := tapName("very-long-vm-name"); len(got) > 15 {
		t.Fatalf("tapName too long: %q", got)
	}
	if mac := macForVMName("demo"); !strings.HasPrefix(mac, "06:00:") || mac != macForVMName("demo") {
		t.Fatalf("macForVMName = %q", mac)
	}
}

func TestIPMathOverflowUnderflow(t *testing.T) {
	ip, err := addToIP(net.ParseIP("fd00::1"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if ip.String() != "fd00::3" {
		t.Fatalf("addToIP = %s, want fd00::3", ip)
	}
	if _, err := decrementIP(net.ParseIP("::")); err == nil {
		t.Fatal("decrementIP accepted underflow")
	}
	if _, err := addToIP(net.ParseIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"), 1); err == nil {
		t.Fatal("addToIP accepted overflow")
	}
}

func TestDiskPathResolutionAndColdPaths(t *testing.T) {
	m, _ := newTestManager(t)
	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	vm := store.VM{Name: "demo"}
	if got := m.coldVMDir("demo"); got != filepath.Join(m.cfg.ColdStorage.Dir, "vms", "demo") {
		t.Fatalf("coldVMDir = %q", got)
	}
	if got := m.coldDiskPath("demo"); got != filepath.Join(m.cfg.ColdStorage.Dir, "vms", "demo", "rootfs.ext4") {
		t.Fatalf("coldDiskPath = %q", got)
	}

	if err := os.MkdirAll(m.layout("demo").vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.layout("demo").diskPath, []byte("hot"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := m.vmDiskPath(vm)
	if err != nil {
		t.Fatal(err)
	}
	if path != m.layout("demo").diskPath {
		t.Fatalf("vmDiskPath hot = %q", path)
	}
	if err := os.Remove(m.layout("demo").diskPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.coldVMDir("demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.coldDiskPath("demo"), []byte("cold"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err = m.vmDiskPath(vm)
	if err != nil {
		t.Fatal(err)
	}
	if path != m.coldDiskPath("demo") {
		t.Fatalf("vmDiskPath cold = %q", path)
	}
	vm.ArchivedDiskPath = filepath.Join(t.TempDir(), "missing.ext4")
	if _, err := m.vmDiskPath(vm); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing recorded archive error = %v, want not exist", err)
	}
}

func TestEnsureDiskCopyFileAndConfigHelpers(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.ext4")
	disk := filepath.Join(dir, "disk.ext4")
	if err := os.WriteFile(source, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := ensureDisk(disk, source, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("ensureDisk created = false, want true")
	}
	info, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 16 {
		t.Fatalf("disk size = %d, want 16", info.Size())
	}
	created, err = ensureDisk(disk, source, 32)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("ensureDisk existing created = true, want false")
	}

	copyPath := filepath.Join(dir, "copy.ext4")
	if err := copyFile(copyPath, source); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base" {
		t.Fatalf("copyFile data = %q", data)
	}

	configPath := filepath.Join(dir, "firecracker.json")
	if err := writeFirecrackerConfig(configPath, firecrackerConfig{
		BootSource:    bootSource{KernelImagePath: "kernel", BootArgs: "args"},
		Drives:        []drive{{DriveID: "rootfs", PathOnHost: disk, IsRootDevice: true}},
		MachineConfig: machineConfig{VCPUCount: 1, MemSizeMiB: 128},
	}); err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configData), `"kernel_image_path": "kernel"`) {
		t.Fatalf("firecracker config = %s", configData)
	}
	if strings.Contains(string(configData), `"balloon"`) {
		t.Fatalf("firecracker config should not include balloon device = %s", configData)
	}
	if err := waitForSocket(context.Background(), configPath, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestCopyRegularFileHonorsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(source, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := copyRegularFile(ctx, dst, source)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyRegularFile error = %v, want context.Canceled", err)
	}
}

func TestColdStorageMonitorDisabledAndCheckArchivesStoppedVM(t *testing.T) {
	m, st := newTestManager(t)
	monitor := NewColdStorageMonitor(m, nil)
	if monitor.manager != m {
		t.Fatal("cold storage monitor manager not set")
	}
	monitor.Run(context.Background())

	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	m.cfg.ColdStorage.ArchiveStoppedAfterSeconds = 1
	createSnapshotTestVM(t, m, st, "stopped", "stopped")
	createSnapshotTestVM(t, m, st, "running", "running")

	monitor.check(context.Background(), time.Now().Add(2*time.Second))
	if _, err := os.Stat(m.layout("stopped").diskPath); !os.IsNotExist(err) {
		t.Fatalf("stopped hot disk stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(m.coldDiskPath("stopped")); err != nil {
		t.Fatalf("stopped cold disk: %v", err)
	}
	if _, err := os.Stat(m.layout("running").diskPath); err != nil {
		t.Fatalf("running hot disk should remain: %v", err)
	}
}

func TestShouldArchiveStoppedVM(t *testing.T) {
	m, _ := newTestManager(t)
	m.cfg.ColdStorage.Dir = filepath.Join(t.TempDir(), "cold")
	m.cfg.ColdStorage.ArchiveStoppedAfterSeconds = 60
	now := time.Now().UTC()
	vm := store.VM{Name: "demo", State: "stopped", StoppedAt: now.Add(-61 * time.Second).Format(time.RFC3339Nano)}
	if !m.shouldArchiveStoppedVM(vm, now) {
		t.Fatal("old stopped VM was not archive-eligible")
	}
	vm.StoppedAt = now.Add(-59 * time.Second).Format(time.RFC3339Nano)
	if m.shouldArchiveStoppedVM(vm, now) {
		t.Fatal("recently stopped VM was archive-eligible")
	}
	vm.State = "running"
	if m.shouldArchiveStoppedVM(vm, now) {
		t.Fatal("running VM was archive-eligible")
	}
	vm.State = "stopped"
	vm.StoppedAt = "not-a-time"
	if m.shouldArchiveStoppedVM(vm, now) {
		t.Fatal("VM with bad stopped_at was archive-eligible")
	}
}

func TestIdleMonitorThresholdAndCleanup(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t)
	monitor := NewIdleMonitor(m, nil, nil)
	if monitor.manager != m {
		t.Fatal("idle monitor not initialized")
	}
	m.cfg.Idle.DefaultSleepAfterSeconds = 30
	if got := monitor.threshold(store.VM{Name: "default"}); got != 30*time.Second {
		t.Fatalf("default threshold = %s", got)
	}
	if got := monitor.threshold(store.VM{Name: "override", IdleSleepAfterSeconds: 5}); got != 5*time.Second {
		t.Fatalf("override threshold = %s", got)
	}
	now := time.Now().UTC()
	recent := now.Add(-29 * time.Second).Format(time.RFC3339Nano)
	if last, ok := vmLastActivity(store.VM{Name: "recent", LastActivityAt: recent}); !ok || !last.Equal(now.Add(-29*time.Second)) {
		t.Fatalf("last activity = %s %v, want recent timestamp", last, ok)
	}
	fallback := now.Add(-31 * time.Second).Format(time.RFC3339Nano)
	if last, ok := vmLastActivity(store.VM{Name: "fallback", LastStartedAt: fallback}); !ok || !last.Equal(now.Add(-31*time.Second)) {
		t.Fatalf("fallback activity = %s %v, want last_started_at", last, ok)
	}
	if _, ok := vmLastActivity(store.VM{Name: "missing"}); ok {
		t.Fatal("vmLastActivity accepted missing timestamps")
	}

	m.cfg.Idle.DefaultSleepAfterSeconds = 0
	monitor.Run(ctx)
}

func TestCopyDenseAndSmallHelpers(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(source, []byte("dense"), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFileDense(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "dense" {
		t.Fatalf("dense copy = %q", data)
	}
	if !isSparseSeekUnsupported(syscall.EINVAL) || isSparseSeekUnsupported(syscall.EIO) {
		t.Fatal("isSparseSeekUnsupported classification mismatch")
	}
	id, err := randomMachineID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 {
		t.Fatalf("machine ID length = %d, want 32", len(id))
	}
	value := stringPtr("x")
	if value == nil || *value != "x" {
		t.Fatalf("stringPtr = %#v", value)
	}
}

func TestRunAndDeleteTapSmallHelpers(t *testing.T) {
	if err := run(context.Background(), "/bin/sh", "-c", "exit 0"); err != nil {
		t.Fatalf("run success: %v", err)
	}
	err := run(context.Background(), "/bin/sh", "-c", "printf firedoze-error; exit 7")
	if err == nil || !strings.Contains(err.Error(), "firedoze-error") {
		t.Fatalf("run failure error = %v, want command output", err)
	}
	if err := deleteTap(""); err != nil {
		t.Fatalf("deleteTap empty: %v", err)
	}
}

func TestRewriteGuestIdentityWritesGuestFilesAndSSHKeys(t *testing.T) {
	m, _ := newTestManager(t)
	diskPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	writes := map[string]string{}
	modes := map[string]string{}
	keyTypes := map[string]bool{}
	restore := stubRunCommand(t, func(_ context.Context, name string, args ...string) error {
		switch name {
		case sshKeygenPath:
			keyType, outPath := sshKeygenArgs(t, args)
			keyTypes[keyType] = true
			if err := os.WriteFile(outPath, []byte("private-"+keyType), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outPath+".pub", []byte("public-"+keyType), 0o644); err != nil {
				t.Fatal(err)
			}
			return nil
		case debugfsPath:
			op, guestPath := debugfsOperation(t, args, diskPath)
			switch {
			case strings.HasPrefix(op, "rm "):
				return errors.New("missing guest file")
			case strings.HasPrefix(op, "write "):
				fields := strings.Fields(op)
				if len(fields) != 3 {
					t.Fatalf("debugfs write op = %q", op)
				}
				data, err := os.ReadFile(fields[1])
				if err != nil {
					t.Fatal(err)
				}
				writes[guestPath] = string(data)
				return nil
			case strings.HasPrefix(op, "set_inode_field "):
				fields := strings.Fields(op)
				if len(fields) != 4 || fields[2] != "mode" {
					t.Fatalf("debugfs mode op = %q", op)
				}
				modes[guestPath] = fields[3]
				return nil
			default:
				t.Fatalf("unexpected debugfs op %q", op)
			}
		default:
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return nil
	})
	defer restore()

	if err := m.rewriteGuestIdentity(context.Background(), diskPath, "demo-vm"); err != nil {
		t.Fatal(err)
	}
	if got := writes["/etc/hostname"]; got != "demo-vm\n" {
		t.Fatalf("/etc/hostname = %q, want demo-vm newline", got)
	}
	if hosts := writes["/etc/hosts"]; !strings.Contains(hosts, "127.0.1.1 demo-vm") || !strings.Contains(hosts, "::1 localhost") {
		t.Fatalf("/etc/hosts = %q", hosts)
	}
	machineID := writes["/etc/machine-id"]
	if len(strings.TrimSpace(machineID)) != 32 || !strings.HasSuffix(machineID, "\n") {
		t.Fatalf("/etc/machine-id = %q, want 32 hex chars plus newline", machineID)
	}
	if writes["/var/lib/dbus/machine-id"] != machineID {
		t.Fatalf("dbus machine-id = %q, want %q", writes["/var/lib/dbus/machine-id"], machineID)
	}
	for _, guestPath := range []string{"/etc/hostname", "/etc/hosts"} {
		if got := modes[guestPath]; got != "0100644" {
			t.Fatalf("%s mode = %q, want 0100644", guestPath, got)
		}
	}
	for _, guestPath := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if got := modes[guestPath]; got != "0100444" {
			t.Fatalf("%s mode = %q, want 0100444", guestPath, got)
		}
	}
	for _, keyType := range []string{"rsa", "ecdsa", "ed25519"} {
		if !keyTypes[keyType] {
			t.Fatalf("ssh-keygen was not called for %s", keyType)
		}
		privatePath := "/etc/ssh/ssh_host_" + keyType + "_key"
		publicPath := privatePath + ".pub"
		if got := writes[privatePath]; got != "private-"+keyType {
			t.Fatalf("%s = %q", privatePath, got)
		}
		if got := writes[publicPath]; got != "public-"+keyType {
			t.Fatalf("%s = %q", publicPath, got)
		}
		if got := modes[privatePath]; got != "0100600" {
			t.Fatalf("%s mode = %q, want 0100600", privatePath, got)
		}
		if got := modes[publicPath]; got != "0100644" {
			t.Fatalf("%s mode = %q, want 0100644", publicPath, got)
		}
	}
}

func TestGuestFileCommandErrors(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeErr := errors.New("debugfs write failed")
	restore := stubRunCommand(t, func(_ context.Context, name string, args ...string) error {
		if name != debugfsPath {
			t.Fatalf("unexpected command %s", name)
		}
		op, _ := debugfsOperation(t, args, diskPath)
		if strings.HasPrefix(op, "write ") {
			return writeErr
		}
		return nil
	})
	err := writeGuestFile(context.Background(), diskPath, "/etc/example", []byte("data"))
	restore()
	if !errors.Is(err, writeErr) {
		t.Fatalf("writeGuestFile error = %v, want %v", err, writeErr)
	}

	modeErr := errors.New("debugfs mode failed")
	restore = stubRunCommand(t, func(_ context.Context, name string, args ...string) error {
		if name != debugfsPath {
			t.Fatalf("unexpected command %s", name)
		}
		op, _ := debugfsOperation(t, args, diskPath)
		if strings.HasPrefix(op, "set_inode_field ") {
			return modeErr
		}
		return nil
	})
	err = replaceGuestFile(context.Background(), diskPath, "/etc/example", filepath.Join(t.TempDir(), "missing-local"), "0100644")
	restore()
	if !errors.Is(err, modeErr) {
		t.Fatalf("replaceGuestFile error = %v, want %v", err, modeErr)
	}

	keygenErr := errors.New("ssh-keygen failed")
	restore = stubRunCommand(t, func(_ context.Context, name string, args ...string) error {
		if name == sshKeygenPath {
			return keygenErr
		}
		return nil
	})
	err = rewriteSSHHostKeys(context.Background(), diskPath)
	restore()
	if !errors.Is(err, keygenErr) {
		t.Fatalf("rewriteSSHHostKeys error = %v, want %v", err, keygenErr)
	}
}

func TestStartAndRebootEarlyErrorPaths(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t)
	configureTestBaseImage(t, m)
	m.rewriteGuestIdentityFunc = func(context.Context, string, string) error {
		return nil
	}

	if _, err := m.StartVM(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("StartVM missing error = %v, want ErrNotFound", err)
	}
	if _, err := m.RebootVM(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RebootVM missing error = %v, want ErrNotFound", err)
	}

	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "badip", PrivateIP: "not-an-ip", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	_, err := m.StartVM(ctx, "badip")
	if err == nil || !strings.Contains(err.Error(), "prepare network") || !strings.Contains(err.Error(), "private_ip must be an IP address") {
		t.Fatalf("StartVM bad IP error = %v", err)
	}

	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "sleepy", PrivateIP: "not-an-ip", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(ctx, "sleepy", "sleeping"); err != nil {
		t.Fatal(err)
	}
	layout := m.layout("sleepy")
	if err := os.MkdirAll(layout.sleepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.sleepStatePath, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.sleepMemPath, []byte("mem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RebootVM(ctx, "sleepy"); err == nil {
		t.Fatal("RebootVM sleeping VM succeeded despite missing base rootfs setup for start")
	}
	updated, err := st.GetVM(ctx, "sleepy")
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "stopped" {
		t.Fatalf("sleepy state = %q, want stopped after reboot converts sleeping VM", updated.State)
	}
	if _, err := os.Stat(layout.sleepDir); !os.IsNotExist(err) {
		t.Fatalf("sleep dir stat = %v, want removed", err)
	}
}

func TestPrepareNetworkInputValidation(t *testing.T) {
	m, _ := newTestManager(t)
	tests := []struct {
		name string
		vm   store.VM
		want string
	}{
		{name: "missing", vm: store.VM{Name: "demo"}, want: "no private_ip"},
		{name: "bad ip", vm: store.VM{Name: "demo", PrivateIP: "bad"}, want: "must be an IP address"},
		{name: "ipv4", vm: store.VM{Name: "demo", PrivateIP: "10.0.0.2"}, want: "must be IPv6"},
		{name: "underflow", vm: store.VM{Name: "demo", PrivateIP: "::"}, want: "invalid /127 host peer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := m.prepareNetwork(context.Background(), tt.vm)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("prepareNetwork error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParsePhysicalAddressBits(t *testing.T) {
	data := []byte("processor: 0\naddress sizes\t: 39 bits physical, 48 bits virtual\n")
	got, ok := parsePhysicalAddressBits(data)
	if !ok || got != 39 {
		t.Fatalf("parsePhysicalAddressBits = %d, %v; want 39, true", got, ok)
	}
}

func TestRequiredVirtioMemPhysicalAddressBits(t *testing.T) {
	if got := requiredVirtioMemPhysicalAddressBits(128); got != 40 {
		t.Fatalf("required physical bits = %d, want 40", got)
	}
}

func TestMemoryConfigForHostFallsBackWhenVirtioMemUnsupported(t *testing.T) {
	restore := stubHostPhysicalAddressBits(t, func() (int, bool) { return 39, true })
	defer restore()

	cfg, err := memoryConfigForHost(store.VM{Name: "demo", MemoryMinMiB: 512, MemoryMaxMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.bootMiB != 1024 || cfg.hotplug != nil || !cfg.degraded {
		t.Fatalf("memory config = %#v, want fixed 1024MiB degraded fallback", cfg)
	}
}

func TestMemoryConfigForHostUsesVirtioMemWhenSupported(t *testing.T) {
	restore := stubHostPhysicalAddressBits(t, func() (int, bool) { return 40, true })
	defer restore()

	cfg, err := memoryConfigForHost(store.VM{Name: "demo", MemoryMinMiB: 512, MemoryMaxMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.bootMiB != 512 || cfg.hotplug == nil || cfg.hotplug.TotalSizeMiB != 512 || cfg.degraded {
		t.Fatalf("memory config = %#v, want 512MiB boot plus 512MiB hotplug", cfg)
	}
}

func TestMemoryConfigForHostUsesFixedMemoryForFixedRange(t *testing.T) {
	restore := stubHostPhysicalAddressBits(t, func() (int, bool) { return 39, true })
	defer restore()

	cfg, err := memoryConfigForHost(store.VM{Name: "demo", MemoryMinMiB: 512, MemoryMaxMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.bootMiB != 512 || cfg.hotplug != nil || cfg.degraded {
		t.Fatalf("memory config = %#v, want fixed 512MiB", cfg)
	}
}

func stubRunCommand(t *testing.T, fn func(context.Context, string, ...string) error) func() {
	t.Helper()
	old := runCommand
	runCommand = fn
	return func() {
		runCommand = old
	}
}

func stubHostPhysicalAddressBits(t *testing.T, fn func() (int, bool)) func() {
	t.Helper()
	old := hostPhysicalAddressBitsFunc
	hostPhysicalAddressBitsFunc = fn
	return func() {
		hostPhysicalAddressBitsFunc = old
	}
}

func stubDeleteTap(t *testing.T, fn func(string) error) func() {
	t.Helper()
	old := deleteTapCmd
	deleteTapCmd = fn
	return func() {
		deleteTapCmd = old
	}
}

func sshKeygenArgs(t *testing.T, args []string) (keyType string, outPath string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "-t":
			keyType = args[i+1]
		case "-f":
			outPath = args[i+1]
		}
	}
	if keyType == "" || outPath == "" {
		t.Fatalf("ssh-keygen args missing type or output path: %v", args)
	}
	return keyType, outPath
}

func debugfsOperation(t *testing.T, args []string, diskPath string) (op string, guestPath string) {
	t.Helper()
	if len(args) != 4 || args[0] != "-w" || args[1] != "-R" || args[3] != diskPath {
		t.Fatalf("debugfs args = %v, want -w -R <op> %s", args, diskPath)
	}
	op = args[2]
	fields := strings.Fields(op)
	if len(fields) < 2 {
		t.Fatalf("debugfs op = %q", op)
	}
	switch fields[0] {
	case "rm":
		guestPath = fields[1]
	case "write":
		if len(fields) != 3 {
			t.Fatalf("debugfs write op = %q", op)
		}
		guestPath = fields[2]
	case "set_inode_field":
		if len(fields) != 4 {
			t.Fatalf("debugfs mode op = %q", op)
		}
		guestPath = fields[1]
	default:
		t.Fatalf("unexpected debugfs op = %q", op)
	}
	return op, guestPath
}
