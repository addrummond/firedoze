package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrNotFound = errors.New("not found")

type VM struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	PrivateIP       string `json:"private_ip,omitempty"`
	VCPUs           int    `json:"vcpus"`
	MemoryMiB       int    `json:"memory_mib"`
	DiskBytes       int64  `json:"disk_bytes"`
	DefaultHTTPPort int    `json:"default_http_port"`
}

type CreateVMParams struct {
	Name            string
	PrivateIP       string
	VCPUs           int
	MemoryMiB       int
	DiskBytes       int64
	DefaultHTTPPort int
}

func (s *Store) CreateVM(ctx context.Context, params CreateVMParams) (VM, error) {
	_, err := s.db.ExecContext(ctx, `
		insert into vms (name, state, private_ip, vcpus, memory_mib, disk_bytes, default_http_port)
		values (?, 'stopped', ?, ?, ?, ?, ?)
	`, params.Name, params.PrivateIP, params.VCPUs, params.MemoryMiB, params.DiskBytes, params.DefaultHTTPPort)
	if err != nil {
		return VM{}, err
	}
	return s.GetVM(ctx, params.Name)
}

func (s *Store) ListVMs(ctx context.Context) ([]VM, error) {
	rows, err := s.db.QueryContext(ctx, `
		select name, state, coalesce(private_ip, ''), vcpus, memory_mib, disk_bytes, default_http_port
		from vms
		order by name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vms []VM
	for rows.Next() {
		var vm VM
		if err := rows.Scan(&vm.Name, &vm.State, &vm.PrivateIP, &vm.VCPUs, &vm.MemoryMiB, &vm.DiskBytes, &vm.DefaultHTTPPort); err != nil {
			return nil, err
		}
		vms = append(vms, vm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return vms, nil
}

func (s *Store) GetVM(ctx context.Context, name string) (VM, error) {
	var vm VM
	err := s.db.QueryRowContext(ctx, `
		select name, state, coalesce(private_ip, ''), vcpus, memory_mib, disk_bytes, default_http_port
		from vms
		where name = ?
	`, name).Scan(&vm.Name, &vm.State, &vm.PrivateIP, &vm.VCPUs, &vm.MemoryMiB, &vm.DiskBytes, &vm.DefaultHTTPPort)
	if errors.Is(err, sql.ErrNoRows) {
		return VM{}, ErrNotFound
	}
	if err != nil {
		return VM{}, err
	}
	return vm, nil
}

func (s *Store) CountVMs(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `select count(*) from vms`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) SetVMState(ctx context.Context, name string, state string) error {
	result, err := s.db.ExecContext(ctx, `
		update vms
		set state = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		where name = ?
	`, state, name)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: vm %q", ErrNotFound, name)
	}
	return nil
}
