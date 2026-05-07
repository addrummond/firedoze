package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestGlobToLike(t *testing.T) {
	tests := []struct {
		glob string
		like string
	}{
		{glob: "dev-*", like: "dev-%"},
		{glob: "dev-?", like: "dev-_"},
		{glob: "literal_%", like: `literal\_\%`},
		{glob: `slash\name`, like: `slash\\name`},
	}
	for _, tt := range tests {
		if got := GlobToLike(tt.glob); got != tt.like {
			t.Fatalf("GlobToLike(%q) = %q, want %q", tt.glob, got, tt.like)
		}
	}
}

func TestListVMsMatching(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"alice-one", "alice-two", "bob-one", "literal_%"} {
		if _, err := st.CreateVM(ctx, CreateVMParams{Name: name, PrivateIP: "10.0.0.2", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
			t.Fatal(err)
		}
	}

	vms, err := st.ListVMsMatching(ctx, []string{"alice-*", "literal_%"})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, vm := range vms {
		got = append(got, vm.Name)
	}
	want := []string{"alice-one", "alice-two", "literal_%"}
	if len(got) != len(want) {
		t.Fatalf("names = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %#v, want %#v", got, want)
		}
	}
}

func TestSetVMStateAllowsLost(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateVM(ctx, CreateVMParams{Name: "demo", PrivateIP: "10.0.0.2", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(ctx, created.UUID, "lost"); err != nil {
		t.Fatal(err)
	}
	vm, err := st.GetVM(ctx, created.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != "lost" {
		t.Fatalf("state = %q, want lost", vm.State)
	}
}

func TestVMStoppedAtTracksStoppedState(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	vm, err := st.CreateVM(ctx, CreateVMParams{Name: "demo", PrivateIP: "10.0.0.2", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if vm.StoppedAt == "" {
		t.Fatal("created stopped VM has empty stopped_at")
	}
	if _, err := time.Parse(time.RFC3339Nano, vm.StoppedAt); err != nil {
		t.Fatalf("parse created stopped_at: %v", err)
	}
	if err := st.SetVMState(ctx, vm.UUID, "running"); err != nil {
		t.Fatal(err)
	}
	vm, err = st.GetVM(ctx, vm.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.StoppedAt != "" {
		t.Fatalf("running VM stopped_at = %q, want empty", vm.StoppedAt)
	}
	if err := st.SetVMState(ctx, vm.UUID, "stopped"); err != nil {
		t.Fatal(err)
	}
	vm, err = st.GetVM(ctx, vm.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.StoppedAt == "" {
		t.Fatal("stopped VM has empty stopped_at")
	}
}
