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

var (
	caddyLoad = caddy.Load
	caddyStop = caddy.Stop
)

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
	protectedHosts, err := m.store.ListRouteProtections(ctx)
	if err != nil {
		return err
	}

	cfg, routeCount := m.caddyConfig(vms, aliases, protectedHosts)
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := caddyLoad(data, true); err != nil {
		return err
	}
	m.logger.Info("reconciled caddy routes", "routes", routeCount, "http_port", m.cfg.Caddy.HTTPPort, "https_port", m.cfg.Caddy.HTTPSPort)
	return nil
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return caddyStop()
}

func (m *Manager) caddyConfig(vms []store.VM, aliases []store.Route, protectedHosts []string) (map[string]any, int) {
	routes := make([]map[string]any, 0, len(vms)+len(aliases)+3)
	publicHosts := []string{m.cfg.BaseDomain}
	publicHostSet := map[string]struct{}{m.cfg.BaseDomain: {}}
	routeCount := 0
	routes = append(routes, routeAuthRoute(m.wakeProxyUpstream()))
	routes = append(routes, baseDomainRoute(m.cfg.BaseDomain))
	vmsByName := make(map[string]store.VM, len(vms))
	for _, vm := range vms {
		vmsByName[vm.Name] = vm
		if vm.PrivateIP == "" || !vm.PublicHTTP {
			continue
		}
		host := vm.Name + "." + m.cfg.BaseDomain
		if _, ok := publicHostSet[host]; !ok {
			publicHosts = append(publicHosts, host)
			publicHostSet[host] = struct{}{}
		}
		routes = append(routes, reverseProxyRoute(host, m.wakeProxyUpstream()))
		routeCount++
	}
	for _, alias := range aliases {
		vm, ok := vmsByName[alias.VMName]
		if !ok || vm.PrivateIP == "" || !vm.PublicHTTP {
			continue
		}
		host := alias.Name + "." + m.cfg.BaseDomain
		if _, ok := publicHostSet[host]; !ok {
			publicHosts = append(publicHosts, host)
			publicHostSet[host] = struct{}{}
		}
		routes = append(routes, reverseProxyRoute(host, m.wakeProxyUpstream()))
		routeCount++
	}
	for _, host := range protectedHosts {
		if _, ok := publicHostSet[host]; !ok {
			publicHosts = append(publicHosts, host)
			publicHostSet[host] = struct{}{}
			routes = append(routes, reverseProxyRoute(host, m.wakeProxyUpstream()))
		}
	}
	routes = append(routes, routeNotFoundRoute())

	servers := map[string]any{}
	if m.cfg.Caddy.TLSMode == "behind_proxy" {
		servers["firedoze_http"] = map[string]any{
			"listen": []string{":" + strconv.Itoa(m.cfg.Caddy.HTTPPort)},
			"routes": routes,
		}
	} else {
		redirectRoutes := make([]map[string]any, 0, len(publicHosts)+1)
		for _, host := range publicHosts {
			redirectRoutes = append(redirectRoutes, redirectToHTTPSRoute(host))
		}
		redirectRoutes = append(redirectRoutes, routeNotFoundRoute())
		servers["firedoze_http"] = map[string]any{
			"listen": []string{":" + strconv.Itoa(m.cfg.Caddy.HTTPPort)},
			"routes": redirectRoutes,
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
	}, routeCount
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

func routeAuthRoute(upstream string) map[string]any {
	return map[string]any{
		"match": []map[string]any{{
			"path": []string{"/_firedoze/auth"},
		}},
		"handle": []map[string]any{{
			"handler": "reverse_proxy",
			"upstreams": []map[string]any{{
				"dial": upstream,
			}},
		}},
	}
}

func baseDomainRoute(host string) map[string]any {
	return map[string]any{
		"match": []map[string]any{{
			"host": []string{host},
		}},
		"handle": []map[string]any{{
			"handler":     "static_response",
			"status_code": "200",
			"body":        "firedoze is running\n",
		}},
	}
}

func routeNotFoundRoute() map[string]any {
	return map[string]any{
		"handle": []map[string]any{{
			"handler":     "static_response",
			"status_code": "404",
			"body":        "firedoze route not found\n",
		}},
	}
}

func redirectToHTTPSRoute(host string) map[string]any {
	return map[string]any{
		"match": []map[string]any{{
			"host": []string{host},
		}},
		"handle": []map[string]any{{
			"handler":     "static_response",
			"status_code": "308",
			"headers": map[string][]string{
				"Location": {"https://{http.request.host}{http.request.uri}"},
			},
		}},
	}
}
