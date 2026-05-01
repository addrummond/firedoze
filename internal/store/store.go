package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
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

	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	return s.ensureColumn(ctx, "snapshots", "mem_path", "text not null default ''")
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
	name text primary key,
	state text not null,
	private_ip text,
	vcpus integer not null,
	memory_mib integer not null,
	disk_bytes integer not null,
	default_http_port integer not null,
	created_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	updated_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

create table if not exists snapshots (
	name text primary key,
	source_vm text,
	state_path text not null,
	mem_path text not null,
	disk_path text not null,
	base_image_id text not null,
	kernel_id text not null,
	created_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

create table if not exists routes (
	name text primary key,
	vm_name text not null references vms(name) on delete cascade,
	port integer not null,
	is_default integer not null default 0,
	created_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	check (port between 1 and 65535),
	check (is_default in (0, 1))
);

create unique index if not exists routes_one_default_per_vm
	on routes(vm_name)
	where is_default = 1;
`

func (s *Store) ensureColumn(ctx context.Context, table string, column string, definition string) error {
	rows, err := s.db.QueryContext(ctx, `pragma table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, fmt.Sprintf("alter table %s add column %s %s", table, column, definition))
	return err
}
