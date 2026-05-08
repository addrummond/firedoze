package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("metadata path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `pragma busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `pragma foreign_keys = on`); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `select strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`).Scan(&raw); err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, raw)
}

const schema = `
create table if not exists metadata (
	key text primary key,
	value text not null,
	updated_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

create table if not exists vms (
	uuid text primary key,
	name text not null unique,
	state text not null,
	private_ip text,
	vcpus integer not null,
	memory_min_mib integer not null,
	memory_max_mib integer not null,
	disk_bytes integer not null,
	default_http_port integer not null,
	idle_sleep_after_seconds integer not null default 0,
	last_started_at text not null default '',
	last_activity_at text not null default '',
	stopped_at text not null default '',
	archived_disk_path text not null default '',
	base_image_id text not null default '',
	kernel_id text not null default '',
	base_image_metadata text not null default '',
	auto_wake integer not null default 0,
	public_http integer not null default 0,
	created_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	updated_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

create table if not exists snapshots (
	name text primary key,
	source_vm_uuid text,
	source_vm text,
	state_path text not null,
	mem_path text not null,
	disk_path text not null,
	base_image_id text not null,
	kernel_id text not null,
	base_image_metadata text not null default '',
	created_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

create table if not exists routes (
	name text primary key,
	vm_uuid text not null references vms(uuid) on delete cascade,
	port integer not null,
	is_default integer not null default 0,
	created_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	check (port between 1 and 65535),
	check (is_default in (0, 1))
);

create unique index if not exists routes_one_default_per_vm
	on routes(vm_uuid)
	where is_default = 1;

create table if not exists route_protections (
	hostname text primary key,
	created_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
`
