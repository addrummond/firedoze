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
	aliases, err := m.store.ListRoutes(ctx)
	if err != nil {
		return err
	}

	cfg, routeCount := m.caddyConfig(vms, aliases)
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := caddy.Load(data, true); err != nil {
		return err
	}
	m.logger.Info("reconciled caddy routes", "routes", routeCount, "http_port", m.cfg.Caddy.HTTPPort, "https_port", m.cfg.Caddy.HTTPSPort)
	return nil
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return caddy.Stop()
}

func (m *Manager) caddyConfig(vms []store.VM, aliases []store.Route) (map[string]any, int) {
	routes := make([]map[string]any, 0, len(vms)+len(aliases)+1)
	vmsByName := make(map[string]store.VM, len(vms))
	for _, vm := range vms {
		vmsByName[vm.Name] = vm
		if vm.PrivateIP == "" || !vm.PublicHTTP {
			continue
		}
		host := vm.Name + "." + m.cfg.BaseDomain
		routes = append(routes, reverseProxyRoute(host, m.wakeProxyUpstream()))
	}
	for _, alias := range aliases {
		vm, ok := vmsByName[alias.VMName]
		if !ok || vm.PrivateIP == "" || !vm.PublicHTTP {
			continue
		}
		host := alias.Name + "." + m.cfg.BaseDomain
		routes = append(routes, reverseProxyRoute(host, m.wakeProxyUpstream()))
	}
	routes = append(routes, map[string]any{
		"handle": []map[string]any{{
			"handler":     "static_response",
			"status_code": "404",
			"body":        "firedoze route not found\n",
		}},
	})

	servers := map[string]any{}
	if m.cfg.Caddy.TLSMode == "behind_proxy" {
		servers["firedoze_http"] = map[string]any{
			"listen": []string{":" + strconv.Itoa(m.cfg.Caddy.HTTPPort)},
			"routes": routes,
		}
	} else {
		servers["firedoze_http"] = map[string]any{
			"listen": []string{":" + strconv.Itoa(m.cfg.Caddy.HTTPPort)},
			"routes": []map[string]any{redirectToHTTPSRoute()},
		}
		servers["firedoze_https"] = map[string]any{
			"listen":                  []string{":" + strconv.Itoa(m.cfg.Caddy.HTTPSPort)},
			"routes":                  routes,
			"tls_connection_policies": []map[string]any{{}},
		}
	}

	return map[string]any{
		"admin": map[string]any{
			"disabled": true,
		},
		"apps": map[string]any{
			"http": map[string]any{
				"http_port":  m.cfg.Caddy.HTTPPort,
				"https_port": m.cfg.Caddy.HTTPSPort,
				"servers":    servers,
			},
		},
	}, len(routes) - 1
}

func (m *Manager) wakeProxyUpstream() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(m.cfg.Caddy.InternalProxyPort))
}

func DefaultHost(vmName string, baseDomain string) string {
	return fmt.Sprintf("%s.%s", vmName, baseDomain)
}

func reverseProxyRoute(host string, upstream string) map[string]any {
	return map[string]any{
		"match": []map[string]any{{
			"host": []string{host},
		}},
		"handle": []map[string]any{{
			"handler": "reverse_proxy",
			"upstreams": []map[string]any{{
				"dial": upstream,
			}},
		}},
	}
}

func redirectToHTTPSRoute() map[string]any {
	return map[string]any{
		"handle": []map[string]any{{
			"handler":     "static_response",
			"status_code": "308",
			"headers": map[string][]string{
				"Location": {"https://{http.request.host}{http.request.uri}"},
			},
		}},
	}
}
