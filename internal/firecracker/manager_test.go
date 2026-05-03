package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
