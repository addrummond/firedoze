package store

import (
	"context"
	"path/filepath"
	"testing"
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
		if _, err := st.CreateVM(ctx, CreateVMParams{Name: name, PrivateIP: "10.0.0.2", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
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
