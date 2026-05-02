package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrNotFound = errors.New("not found")

type VM struct {
	Name                  string   `json:"name"`
	State                 string   `json:"state"`
	PrivateIP             string   `json:"private_ip,omitempty"`
	VCPUs                 int      `json:"vcpus"`
	MemoryMiB             int      `json:"memory_mib"`
	DiskBytes             int64    `json:"disk_bytes"`
	DefaultHTTPPort       int      `json:"default_http_port"`
	IdleSleepAfterSeconds int      `json:"idle_sleep_after_seconds,omitempty"`
	LastStartedAt         string   `json:"last_started_at,omitempty"`
	BaseImageID           string   `json:"base_image_id,omitempty"`
	KernelID              string   `json:"kernel_id,omitempty"`
	BaseImageMetadata     JSONText `json:"base_image_metadata,omitempty"`
	AutoWake              bool     `json:"auto_wake"`
	PublicHTTP            bool     `json:"public_http"`
}

type CreateVMParams struct {
	Name                  string
	PrivateIP             string
	VCPUs                 int
	MemoryMiB             int
	DiskBytes             int64
	DefaultHTTPPort       int
	IdleSleepAfterSeconds int
	BaseImageID           string
	KernelID              string
	BaseImageMetadata     string
	AutoWake              bool
	PublicHTTP            bool
}

type UpdateVMParams struct {
	DefaultHTTPPort       *int
	IdleSleepAfterSeconds *int
	AutoWake              *bool
	PublicHTTP            *bool
}

func (s *Store) CreateVM(ctx context.Context, params CreateVMParams) (VM, error) {
	_, err := s.db.ExecContext(ctx, `
		insert into vms (name, state, private_ip, vcpus, memory_mib, disk_bytes, default_http_port, idle_sleep_after_seconds, base_image_id, kernel_id, base_image_metadata, auto_wake, public_http)
		values (?, 'stopped', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, params.Name, params.PrivateIP, params.VCPUs, params.MemoryMiB, params.DiskBytes, params.DefaultHTTPPort, params.IdleSleepAfterSeconds, params.BaseImageID, params.KernelID, params.BaseImageMetadata, boolToInt(params.AutoWake), boolToInt(params.PublicHTTP))
	if err != nil {
		return VM{}, err
	}
	return s.GetVM(ctx, params.Name)
}

func (s *Store) ListVMs(ctx context.Context) ([]VM, error) {
	return s.ListVMsMatching(ctx, nil)
}

func (s *Store) ListVMsMatching(ctx context.Context, namePatterns []string) ([]VM, error) {
	query := `
		select name, state, coalesce(private_ip, ''), vcpus, memory_mib, disk_bytes, default_http_port, idle_sleep_after_seconds, last_started_at, base_image_id, kernel_id, base_image_metadata, auto_wake, public_http
		from vms
	`
	args := []any{}
	if len(namePatterns) > 0 {
		clauses := make([]string, 0, len(namePatterns))
		for _, pattern := range namePatterns {
			clauses = append(clauses, `name like ? escape '\'`)
			args = append(args, GlobToLike(pattern))
		}
		query += " where " + strings.Join(clauses, " or ")
	}
	query += `
		order by name
	`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	vms := []VM{}
	for rows.Next() {
		var vm VM
		var autoWake int
		var publicHTTP int
		if err := rows.Scan(&vm.Name, &vm.State, &vm.PrivateIP, &vm.VCPUs, &vm.MemoryMiB, &vm.DiskBytes, &vm.DefaultHTTPPort, &vm.IdleSleepAfterSeconds, &vm.LastStartedAt, &vm.BaseImageID, &vm.KernelID, &vm.BaseImageMetadata, &autoWake, &publicHTTP); err != nil {
			return nil, err
		}
		vm.AutoWake = autoWake != 0
		vm.PublicHTTP = publicHTTP != 0
		vms = append(vms, vm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return vms, nil
}

func GlobToLike(pattern string) string {
	var b strings.Builder
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Store) GetVM(ctx context.Context, name string) (VM, error) {
	var vm VM
	var autoWake int
	var publicHTTP int
	err := s.db.QueryRowContext(ctx, `
		select name, state, coalesce(private_ip, ''), vcpus, memory_mib, disk_bytes, default_http_port, idle_sleep_after_seconds, last_started_at, base_image_id, kernel_id, base_image_metadata, auto_wake, public_http
		from vms
		where name = ?
	`, name).Scan(&vm.Name, &vm.State, &vm.PrivateIP, &vm.VCPUs, &vm.MemoryMiB, &vm.DiskBytes, &vm.DefaultHTTPPort, &vm.IdleSleepAfterSeconds, &vm.LastStartedAt, &vm.BaseImageID, &vm.KernelID, &vm.BaseImageMetadata, &autoWake, &publicHTTP)
	if errors.Is(err, sql.ErrNoRows) {
		return VM{}, ErrNotFound
	}
	if err != nil {
		return VM{}, err
	}
	vm.AutoWake = autoWake != 0
	vm.PublicHTTP = publicHTTP != 0
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
		set
			state = ?,
			last_started_at = case
				when ? = 'running' then strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
				else last_started_at
			end,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		where name = ?
	`, state, state, name)
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

func (s *Store) UpdateVM(ctx context.Context, name string, params UpdateVMParams) (VM, error) {
	vm, err := s.GetVM(ctx, name)
	if err != nil {
		return VM{}, err
	}
	if params.DefaultHTTPPort != nil {
		vm.DefaultHTTPPort = *params.DefaultHTTPPort
	}
	if params.IdleSleepAfterSeconds != nil {
		vm.IdleSleepAfterSeconds = *params.IdleSleepAfterSeconds
	}
	if params.AutoWake != nil {
		vm.AutoWake = *params.AutoWake
	}
	if params.PublicHTTP != nil {
		vm.PublicHTTP = *params.PublicHTTP
	}
	result, err := s.db.ExecContext(ctx, `
		update vms
		set default_http_port = ?, idle_sleep_after_seconds = ?, auto_wake = ?, public_http = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		where name = ?
	`, vm.DefaultHTTPPort, vm.IdleSleepAfterSeconds, boolToInt(vm.AutoWake), boolToInt(vm.PublicHTTP), name)
	if err != nil {
		return VM{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return VM{}, err
	}
	if rows == 0 {
		return VM{}, fmt.Errorf("%w: vm %q", ErrNotFound, name)
	}
	return s.GetVM(ctx, name)
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) DeleteVM(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `delete from vms where name = ?`, name)
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
