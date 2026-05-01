package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"firedoze/internal/config"
	"firedoze/internal/store"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

type Manager struct {
	cfg    config.Config
	store  *store.Store
	logger *slog.Logger
	mu     sync.Mutex
}

func NewManager(cfg config.Config, st *store.Store, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:    cfg,
		store:  st,
		logger: logger,
	}
}

func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vms, err := m.store.ListVMs(ctx)
	if err != nil {
		return err
	}

	cfg, routeCount := m.caddyConfig(vms)
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := caddy.Load(data, true); err != nil {
		return err
	}
	m.logger.Info("reconciled caddy routes", "routes", routeCount, "http_port", m.cfg.Caddy.HTTPPort)
	return nil
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return caddy.Stop()
}

func (m *Manager) caddyConfig(vms []store.VM) (map[string]any, int) {
	routes := make([]map[string]any, 0, len(vms)+1)
	for _, vm := range vms {
		if vm.PrivateIP == "" {
			continue
		}
		host := vm.Name + "." + m.cfg.BaseDomain
		upstream := net.JoinHostPort(vm.PrivateIP, strconv.Itoa(vm.DefaultHTTPPort))
		routes = append(routes, map[string]any{
			"match": []map[string]any{{
				"host": []string{host},
			}},
			"handle": []map[string]any{{
				"handler": "reverse_proxy",
				"upstreams": []map[string]any{{
					"dial": upstream,
				}},
			}},
		})
	}
	routes = append(routes, map[string]any{
		"handle": []map[string]any{{
			"handler":     "static_response",
			"status_code": "404",
			"body":        "firedoze route not found\n",
		}},
	})

	return map[string]any{
		"admin": map[string]any{
			"disabled": true,
		},
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"firedoze": map[string]any{
						"listen": []string{":" + strconv.Itoa(m.cfg.Caddy.HTTPPort)},
						"routes": routes,
						"automatic_https": map[string]any{
							"disable": true,
						},
					},
				},
			},
		},
	}, len(routes) - 1
}

func DefaultHost(vmName string, baseDomain string) string {
	return fmt.Sprintf("%s.%s", vmName, baseDomain)
}
