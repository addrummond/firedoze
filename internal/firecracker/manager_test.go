package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
		Name:            name,
		PrivateIP:       "fd7a:115c:a1e0::3",
		VCPUs:           1,
		MemoryMiB:       128,
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
