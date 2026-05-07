package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"firedoze/internal/model"
)

var ErrNotFound = errors.New("not found")
var ErrAlreadyExists = errors.New("already exists")

type VM = model.VM

type CreateVMParams struct {
	Name                  string
	PrivateIP             string
	VCPUs                 int
	MemoryMinMiB          int
	MemoryMaxMiB          int
	DiskBytes             int64
	DefaultHTTPPort       int
	IdleSleepAfterSeconds int
	BaseImageID           string
	KernelID              string
	BaseImageMetadata     string
	AutoWake              bool
	AutoWakeSet           bool
	PublicHTTP            bool
}

type UpdateVMParams struct {
	DefaultHTTPPort       *int
	IdleSleepAfterSeconds *int
	AutoWake              *bool
	PublicHTTP            *bool
}

func (s *Store) CreateVM(ctx context.Context, params CreateVMParams) (VM, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return VM{}, err
	}
	defer tx.Rollback()

	var routeExists bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from routes where name = ?)`, params.Name).Scan(&routeExists); err != nil {
		return VM{}, err
	}
	if routeExists {
		return VM{}, fmt.Errorf("%w: route %q reserves VM name", ErrAlreadyExists, params.Name)
	}
	_, err = tx.ExecContext(ctx, `
		insert into vms (name, state, private_ip, vcpus, memory_min_mib, memory_max_mib, disk_bytes, default_http_port, idle_sleep_after_seconds, stopped_at, base_image_id, kernel_id, base_image_metadata, auto_wake, public_http)
		values (?, 'stopped', ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), ?, ?, ?, ?, ?)
	`, params.Name, params.PrivateIP, params.VCPUs, params.MemoryMinMiB, params.MemoryMaxMiB, params.DiskBytes, params.DefaultHTTPPort, params.IdleSleepAfterSeconds, params.BaseImageID, params.KernelID, params.BaseImageMetadata, boolToInt(params.AutoWake), boolToInt(params.PublicHTTP))
	if err != nil {
		return VM{}, err
	}
	if err := tx.Commit(); err != nil {
		return VM{}, err
	}
	return s.GetVM(ctx, params.Name)
}

func (s *Store) ListVMs(ctx context.Context) ([]VM, error) {
	return s.ListVMsMatching(ctx, nil)
}

func (s *Store) ListVMsMatching(ctx context.Context, namePatterns []string) ([]VM, error) {
	query := `
		select name, state, coalesce(private_ip, ''), vcpus, memory_min_mib, memory_max_mib, disk_bytes, default_http_port, idle_sleep_after_seconds, last_started_at, last_activity_at, stopped_at, archived_disk_path, base_image_id, kernel_id, base_image_metadata, auto_wake, public_http
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
		if err := rows.Scan(&vm.Name, &vm.State, &vm.PrivateIP, &vm.VCPUs, &vm.MemoryMinMiB, &vm.MemoryMaxMiB, &vm.DiskBytes, &vm.DefaultHTTPPort, &vm.IdleSleepAfterSeconds, &vm.LastStartedAt, &vm.LastActivityAt, &vm.StoppedAt, &vm.ArchivedDiskPath, &vm.BaseImageID, &vm.KernelID, &vm.BaseImageMetadata, &autoWake, &publicHTTP); err != nil {
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
		select name, state, coalesce(private_ip, ''), vcpus, memory_min_mib, memory_max_mib, disk_bytes, default_http_port, idle_sleep_after_seconds, last_started_at, last_activity_at, stopped_at, archived_disk_path, base_image_id, kernel_id, base_image_metadata, auto_wake, public_http
		from vms
		where name = ?
	`, name).Scan(&vm.Name, &vm.State, &vm.PrivateIP, &vm.VCPUs, &vm.MemoryMinMiB, &vm.MemoryMaxMiB, &vm.DiskBytes, &vm.DefaultHTTPPort, &vm.IdleSleepAfterSeconds, &vm.LastStartedAt, &vm.LastActivityAt, &vm.StoppedAt, &vm.ArchivedDiskPath, &vm.BaseImageID, &vm.KernelID, &vm.BaseImageMetadata, &autoWake, &publicHTTP)
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

func (s *Store) GetVMByPrivateIP(ctx context.Context, privateIP string) (VM, error) {
	var vm VM
	var autoWake int
	var publicHTTP int
	err := s.db.QueryRowContext(ctx, `
		select name, state, coalesce(private_ip, ''), vcpus, memory_min_mib, memory_max_mib, disk_bytes, default_http_port, idle_sleep_after_seconds, last_started_at, last_activity_at, stopped_at, archived_disk_path, base_image_id, kernel_id, base_image_metadata, auto_wake, public_http
		from vms
		where private_ip = ?
	`, privateIP).Scan(&vm.Name, &vm.State, &vm.PrivateIP, &vm.VCPUs, &vm.MemoryMinMiB, &vm.MemoryMaxMiB, &vm.DiskBytes, &vm.DefaultHTTPPort, &vm.IdleSleepAfterSeconds, &vm.LastStartedAt, &vm.LastActivityAt, &vm.StoppedAt, &vm.ArchivedDiskPath, &vm.BaseImageID, &vm.KernelID, &vm.BaseImageMetadata, &autoWake, &publicHTTP)
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
			last_activity_at = case
				when ? = 'running' then strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
				else last_activity_at
			end,
			stopped_at = case
				when ? = 'stopped' then strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
				else ''
			end,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		where name = ?
	`, state, state, state, state, name)
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

func (s *Store) TouchVMActivity(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `
		update vms
		set last_activity_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		where name = ?
	`, name)
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

func (s *Store) SetVMArchivedDiskPath(ctx context.Context, name string, path string) error {
	result, err := s.db.ExecContext(ctx, `
		update vms
		set archived_disk_path = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		where name = ?
	`, path, name)
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
