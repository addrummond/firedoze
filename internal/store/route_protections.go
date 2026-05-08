package store

import (
	"context"
	"strings"
)

func NormalizeHostname(hostname string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
}

func (s *Store) ProtectRouteHostname(ctx context.Context, hostname string) error {
	hostname = NormalizeHostname(hostname)
	_, err := s.db.ExecContext(ctx, `
		insert into route_protections (hostname)
		values (?)
		on conflict(hostname) do nothing
	`, hostname)
	return err
}

func (s *Store) UnprotectRouteHostname(ctx context.Context, hostname string) error {
	hostname = NormalizeHostname(hostname)
	_, err := s.db.ExecContext(ctx, `delete from route_protections where hostname = ?`, hostname)
	return err
}

func (s *Store) IsRouteHostnameProtected(ctx context.Context, hostname string) (bool, error) {
	hostname = NormalizeHostname(hostname)
	var protected bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from route_protections where hostname = ?)`, hostname).Scan(&protected); err != nil {
		return false, err
	}
	return protected, nil
}

func (s *Store) ListRouteProtections(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `select hostname from route_protections order by hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hostnames := []string{}
	for rows.Next() {
		var hostname string
		if err := rows.Scan(&hostname); err != nil {
			return nil, err
		}
		hostnames = append(hostnames, hostname)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hostnames, nil
}
