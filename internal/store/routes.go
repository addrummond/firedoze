package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type Route struct {
	Name      string `json:"name"`
	VMName    string `json:"vm_name"`
	Port      int    `json:"port"`
	IsDefault bool   `json:"is_default"`
}

type CreateRouteParams struct {
	Name      string
	VMName    string
	Port      int
	IsDefault bool
}

func (s *Store) CreateRoute(ctx context.Context, params CreateRouteParams) (Route, error) {
	_, err := s.db.ExecContext(ctx, `
		insert into routes (name, vm_name, port, is_default)
		values (?, ?, ?, ?)
	`, params.Name, params.VMName, params.Port, boolInt(params.IsDefault))
	if err != nil {
		return Route{}, err
	}
	return s.GetRoute(ctx, params.Name)
}

func (s *Store) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx, `
		select name, vm_name, port, is_default
		from routes
		order by name
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
		select name, vm_name, port, is_default
		from routes
		where name = ?
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

func (s *Store) VMExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from vms where name = ?)`, name).Scan(&exists); err != nil {
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
	if err := row.Scan(&route.Name, &route.VMName, &route.Port, &isDefault); err != nil {
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
