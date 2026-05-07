package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"firedoze/internal/model"
)

type Route = model.Route

type CreateRouteParams struct {
	Name      string
	VMUUID    string
	Port      int
	IsDefault bool
}

func (s *Store) CreateRoute(ctx context.Context, params CreateRouteParams) (Route, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Route{}, err
	}
	defer tx.Rollback()

	var vmExists bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from vms where name = ?)`, params.Name).Scan(&vmExists); err != nil {
		return Route{}, err
	}
	if vmExists {
		return Route{}, fmt.Errorf("%w: VM %q reserves route name", ErrAlreadyExists, params.Name)
	}
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from vms where uuid = ?)`, params.VMUUID).Scan(&vmExists); err != nil {
		return Route{}, err
	}
	if !vmExists {
		return Route{}, fmt.Errorf("%w: vm %q", ErrNotFound, params.VMUUID)
	}
	_, err = tx.ExecContext(ctx, `
		insert into routes (name, vm_uuid, port, is_default)
		values (?, ?, ?, ?)
	`, params.Name, params.VMUUID, params.Port, boolInt(params.IsDefault))
	if err != nil {
		return Route{}, err
	}
	if err := tx.Commit(); err != nil {
		return Route{}, err
	}
	return s.GetRoute(ctx, params.Name)
}

func (s *Store) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx, `
		select routes.name, routes.vm_uuid, vms.name, routes.port, routes.is_default
		from routes
		join vms on vms.uuid = routes.vm_uuid
		order by routes.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	routes := []Route{}
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return routes, nil
}

func (s *Store) GetRoute(ctx context.Context, name string) (Route, error) {
	route, err := scanRoute(s.db.QueryRowContext(ctx, `
		select routes.name, routes.vm_uuid, vms.name, routes.port, routes.is_default
		from routes
		join vms on vms.uuid = routes.vm_uuid
		where routes.name = ?
	`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Route{}, ErrNotFound
	}
	if err != nil {
		return Route{}, err
	}
	return route, nil
}

func (s *Store) DeleteRoute(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `delete from routes where name = ?`, name)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: route %q", ErrNotFound, name)
	}
	return nil
}

func (s *Store) DeleteRoutesForVM(ctx context.Context, vmUUID string) error {
	_, err := s.db.ExecContext(ctx, `delete from routes where vm_uuid = ?`, vmUUID)
	return err
}

func (s *Store) VMExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from vms where name = ?)`, name).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Store) RouteExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from routes where name = ?)`, name).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

type routeScanner interface {
	Scan(dest ...any) error
}

func scanRoute(row routeScanner) (Route, error) {
	var route Route
	var isDefault int
	if err := row.Scan(&route.Name, &route.VMUUID, &route.VMName, &route.Port, &isDefault); err != nil {
		return Route{}, err
	}
	route.IsDefault = isDefault != 0
	return route, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
