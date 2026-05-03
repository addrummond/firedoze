package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

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

func TestSaveSnapshotFromSleepingVMCopiesSleepState(t *testing.T) {
	m, st := newTestManager(t)
	createSnapshotTestVM(t, m, st, "demo", "sleeping")
	layout := m.layout("demo")
	if err := os.MkdirAll(layout.sleepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.sleepStatePath, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.sleepMemPath, []byte("memory"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot, err := m.SaveSnapshot(context.Background(), store.CreateSnapshotParams{Name: "snap", SourceVM: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		snapshot.DiskPath:  "disk",
		snapshot.StatePath: "state",
		snapshot.MemPath:   "memory",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", path, data, want)
		}
	}
}
