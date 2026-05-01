package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type Snapshot struct {
	Name        string `json:"name"`
	SourceVM    string `json:"source_vm,omitempty"`
	StatePath   string `json:"state_path"`
	MemPath     string `json:"mem_path"`
	DiskPath    string `json:"disk_path"`
	BaseImageID string `json:"base_image_id"`
	KernelID    string `json:"kernel_id"`
	CreatedAt   string `json:"created_at"`
}

type CreateSnapshotParams struct {
	Name        string
	SourceVM    string
	StatePath   string
	MemPath     string
	DiskPath    string
	BaseImageID string
	KernelID    string
}

func (s *Store) CreateSnapshot(ctx context.Context, params CreateSnapshotParams) (Snapshot, error) {
	_, err := s.db.ExecContext(ctx, `
		insert into snapshots (name, source_vm, state_path, mem_path, disk_path, base_image_id, kernel_id)
		values (?, ?, ?, ?, ?, ?, ?)
	`, params.Name, params.SourceVM, params.StatePath, params.MemPath, params.DiskPath, params.BaseImageID, params.KernelID)
	if err != nil {
		return Snapshot{}, err
	}
	return s.GetSnapshot(ctx, params.Name)
}

func (s *Store) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		select name, coalesce(source_vm, ''), state_path, mem_path, disk_path, base_image_id, kernel_id, created_at
		from snapshots
		order by name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	snapshots := []Snapshot{}
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return snapshots, nil
}

func (s *Store) GetSnapshot(ctx context.Context, name string) (Snapshot, error) {
	snapshot, err := scanSnapshot(s.db.QueryRowContext(ctx, `
		select name, coalesce(source_vm, ''), state_path, mem_path, disk_path, base_image_id, kernel_id, created_at
		from snapshots
		where name = ?
	`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) SnapshotExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from snapshots where name = ?)`, name).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Store) DeleteSnapshot(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `delete from snapshots where name = ?`, name)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: snapshot %q", ErrNotFound, name)
	}
	return nil
}

type snapshotScanner interface {
	Scan(dest ...any) error
}

func scanSnapshot(row snapshotScanner) (Snapshot, error) {
	var snapshot Snapshot
	err := row.Scan(
		&snapshot.Name,
		&snapshot.SourceVM,
		&snapshot.StatePath,
		&snapshot.MemPath,
		&snapshot.DiskPath,
		&snapshot.BaseImageID,
		&snapshot.KernelID,
		&snapshot.CreatedAt,
	)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}
