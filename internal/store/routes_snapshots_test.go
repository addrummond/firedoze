package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRouteCRUDAndVMExists(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	createTestVM(t, st, "dev")
	createTestVM(t, st, "api")

	exists, err := st.VMExists(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("VMExists(dev) = false, want true")
	}
	exists, err = st.VMExists(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("VMExists(missing) = true, want false")
	}

	route, err := st.CreateRoute(ctx, CreateRouteParams{Name: "web", VMName: "dev", Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if route.Name != "web" || route.VMName != "dev" || route.Port != 8080 {
		t.Fatalf("route = %#v", route)
	}
	exists, err = st.RouteExists(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("RouteExists(web) = false, want true")
	}
	exists, err = st.RouteExists(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("RouteExists(missing) = true, want false")
	}
	if _, err := st.CreateRoute(ctx, CreateRouteParams{Name: "web", VMName: "dev", Port: 8081}); err == nil {
		t.Fatal("duplicate route create succeeded")
	}
	if _, err := st.CreateRoute(ctx, CreateRouteParams{Name: "dev", VMName: "api", Port: 8080}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateRoute with VM name error = %v, want ErrAlreadyExists", err)
	}
	if _, err := st.CreateVM(ctx, CreateVMParams{Name: "web", PrivateIP: "fd00::4", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateVM with route name error = %v, want ErrAlreadyExists", err)
	}

	routes, err := st.ListRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Name != "web" {
		t.Fatalf("routes = %#v", routes)
	}

	if err := st.DeleteRoutesForVM(ctx, "api"); err != nil {
		t.Fatal(err)
	}
	routes, err = st.ListRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("DeleteRoutesForVM(api) removed wrong routes: %#v", routes)
	}

	if err := st.DeleteRoute(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRoute(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRoute deleted route error = %v, want ErrNotFound", err)
	}
	if err := st.DeleteRoute(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteRoute missing error = %v, want ErrNotFound", err)
	}
}

func TestSnapshotCRUDAndMetadata(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	createTestVM(t, st, "dev")

	metadata := `{"rootfs":{"basename":"rootfs.ext4","sha256":"abc"}}`
	snapshot, err := st.CreateSnapshot(ctx, CreateSnapshotParams{
		Name:              "snap.1",
		SourceVM:          "dev",
		StatePath:         "/snap/state",
		MemPath:           "/snap/mem",
		DiskPath:          "/snap/rootfs.ext4",
		BaseImageID:       "base-id",
		KernelID:          "kernel-id",
		BaseImageMetadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Name != "snap.1" || string(snapshot.BaseImageMetadata) != metadata {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, err := st.CreateSnapshot(ctx, CreateSnapshotParams{Name: "snap.1"}); err == nil {
		t.Fatal("duplicate snapshot create succeeded")
	}

	exists, err := st.SnapshotExists(ctx, "snap.1")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("SnapshotExists(snap.1) = false, want true")
	}

	snapshots, err := st.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Name != "snap.1" || string(snapshots[0].BaseImageMetadata) != metadata {
		t.Fatalf("snapshots = %#v", snapshots)
	}

	if err := st.DeleteSnapshot(ctx, "snap.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSnapshot(ctx, "snap.1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSnapshot deleted snapshot error = %v, want ErrNotFound", err)
	}
	if err := st.DeleteSnapshot(ctx, "snap.1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteSnapshot missing error = %v, want ErrNotFound", err)
	}
}

func TestVMUpdateArchiveDeleteAndCount(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	createTestVM(t, st, "dev")
	createTestVM(t, st, "other")
	if _, err := st.CreateSnapshot(ctx, CreateSnapshotParams{Name: "dev-snap", SourceVM: "dev", StatePath: "/state", MemPath: "/mem", DiskPath: "/disk"}); err != nil {
		t.Fatal(err)
	}

	count, err := st.CountVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("CountVMs = %d, want 2", count)
	}

	httpPort := 3000
	idleSeconds := 99
	autoWake := true
	publicHTTP := true
	vm, err := st.UpdateVM(ctx, "dev", UpdateVMParams{
		DefaultHTTPPort:       &httpPort,
		IdleSleepAfterSeconds: &idleSeconds,
		AutoWake:              &autoWake,
		PublicHTTP:            &publicHTTP,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vm.DefaultHTTPPort != 3000 || vm.IdleSleepAfterSeconds != 99 || !vm.AutoWake || !vm.PublicHTTP {
		t.Fatalf("updated VM = %#v", vm)
	}
	if _, err := st.UpdateVM(ctx, "missing", UpdateVMParams{DefaultHTTPPort: &httpPort}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateVM missing error = %v, want ErrNotFound", err)
	}

	if err := st.SetVMArchivedDiskPath(ctx, "dev", "/cold/dev/rootfs.ext4"); err != nil {
		t.Fatal(err)
	}
	vm, err = st.GetVM(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if vm.ArchivedDiskPath != "/cold/dev/rootfs.ext4" {
		t.Fatalf("ArchivedDiskPath = %q", vm.ArchivedDiskPath)
	}
	if err := st.SetVMArchivedDiskPath(ctx, "dev", ""); err != nil {
		t.Fatal(err)
	}
	vm, err = st.GetVM(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if vm.ArchivedDiskPath != "" {
		t.Fatalf("cleared ArchivedDiskPath = %q", vm.ArchivedDiskPath)
	}
	if err := st.SetVMArchivedDiskPath(ctx, "missing", "/cold"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetVMArchivedDiskPath missing error = %v, want ErrNotFound", err)
	}

	if err := st.DeleteVM(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetVM(ctx, "dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetVM deleted VM error = %v, want ErrNotFound", err)
	}
	if err := st.DeleteVM(ctx, "dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteVM missing error = %v, want ErrNotFound", err)
	}
	if _, err := st.GetSnapshot(ctx, "dev-snap"); err != nil {
		t.Fatalf("DeleteVM removed snapshot: %v", err)
	}
	count, err = st.CountVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("CountVMs after delete = %d, want 1", count)
	}
}

func TestMigrateOldSchemaAddsColumnsAndDefaults(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	if _, err := st.db.ExecContext(ctx, `
		create table vms (
			name text primary key,
			state text not null,
			private_ip text,
			vcpus integer not null,
			memory_min_mib integer not null,
			memory_max_mib integer not null,
			disk_bytes integer not null,
			default_http_port integer not null,
			created_at text not null default '',
			updated_at text not null default ''
		);
		create table snapshots (
			name text primary key,
			source_vm text,
			state_path text not null,
			disk_path text not null,
			base_image_id text not null,
			kernel_id text not null,
			created_at text not null default ''
		);
		insert into vms (name, state, private_ip, vcpus, memory_min_mib, memory_max_mib, disk_bytes, default_http_port, updated_at)
		values ('old', 'stopped', 'fd00::2', 1, 128, 512, 1024, 8080, '2026-05-04T00:00:00Z');
		insert into snapshots (name, source_vm, state_path, disk_path, base_image_id, kernel_id)
		values ('snap-old', 'old', '/state', '/disk', 'base', 'kernel');
	`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	vm, err := st.GetVM(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if vm.StoppedAt != "2026-05-04T00:00:00Z" || vm.IdleSleepAfterSeconds != 0 || vm.AutoWake || vm.PublicHTTP {
		t.Fatalf("migrated VM = %#v", vm)
	}
	snapshot, err := st.GetSnapshot(ctx, "snap-old")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MemPath != "" || snapshot.BaseImageMetadata != "" {
		t.Fatalf("migrated snapshot = %#v", snapshot)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func createTestVM(t *testing.T, st *Store, name string) VM {
	t.Helper()
	vm, err := st.CreateVM(context.Background(), CreateVMParams{
		Name:                  name,
		PrivateIP:             "fd00::2",
		VCPUs:                 1,
		MemoryMinMiB:          128,
		MemoryMaxMiB:          128,
		DiskBytes:             1024,
		DefaultHTTPPort:       8080,
		IdleSleepAfterSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	return vm
}
