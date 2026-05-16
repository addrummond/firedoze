package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateSelfBaselinesExistingSchema(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	initialSchema, err := migrationFS.ReadFile("migrations/000001_initial_schema.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, string(initialSchema)); err != nil {
		t.Fatal(err)
	}

	existing, err := st.CreateVM(ctx, CreateVMParams{
		Name:            "existing",
		PrivateIP:       "fd00::2",
		VCPUs:           1,
		MemoryMinMiB:    128,
		MemoryMaxMiB:    128,
		DiskBytes:       1024,
		DefaultHTTPPort: 8080,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var version int
	var dirty bool
	if err := st.db.QueryRowContext(ctx, `select version, dirty from schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatal(err)
	}
	if version != 1 || dirty {
		t.Fatalf("schema_migrations = version %d dirty %t, want version 1 dirty false", version, dirty)
	}

	got, err := st.GetVM(ctx, existing.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "existing" {
		t.Fatalf("vm name = %q, want existing", got.Name)
	}
}
