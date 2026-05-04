package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"firedoze/internal/config"
	"firedoze/internal/store"
)

func TestCaddyConfigOnlyRoutesPublicVMs(t *testing.T) {
	cfg := config.Default()
	cfg.BaseDomain = "example.test"
	manager := NewManager(cfg, nil, nil)

	_, routeCount := manager.caddyConfig([]store.VM{
		{Name: "hidden", PrivateIP: "10.88.0.2", PublicHTTP: false},
		{Name: "public", PrivateIP: "10.88.0.3", PublicHTTP: true},
	}, []store.Route{
		{Name: "hidden-alias", VMName: "hidden", Port: 8080},
		{Name: "public-alias", VMName: "public", Port: 8080},
	})

	if routeCount != 2 {
		t.Fatalf("routeCount = %d, want 2", routeCount)
	}
}

func TestCaddyConfigServesTLSOnHTTPSPort(t *testing.T) {
	cfg := config.Default()
	cfg.BaseDomain = "example.test"
	manager := NewManager(cfg, nil, nil)

	raw, _ := manager.caddyConfig([]store.VM{
		{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", PublicHTTP: true},
	}, nil)
	servers := caddyServers(t, raw)

	httpsServer, ok := servers["firedoze_https"]
	if !ok {
		t.Fatal("missing firedoze_https server")
	}
	if len(httpsServer.Listen) != 1 || httpsServer.Listen[0] != ":443" {
		t.Fatalf("https listen = %v, want [:443]", httpsServer.Listen)
	}
	if httpsServer.TLSConnectionPolicies == nil {
		t.Fatal("https server has no tls_connection_policies")
	}
	httpServer, ok := servers["firedoze_http"]
	if !ok {
		t.Fatal("missing firedoze_http server")
	}
	if len(httpServer.Listen) != 1 || httpServer.Listen[0] != ":80" {
		t.Fatalf("http listen = %v, want [:80]", httpServer.Listen)
	}
	if httpServer.TLSConnectionPolicies != nil {
		t.Fatal("http server unexpectedly has tls_connection_policies")
	}
	httpRoutes := string(mustJSON(t, httpServer.Routes))
	for _, want := range []string{"example.test", "demo.example.test", "firedoze route not found"} {
		if !strings.Contains(httpRoutes, want) {
			t.Fatalf("http routes = %s, want substring %q", httpRoutes, want)
		}
	}
}

func TestCaddyConfigBehindProxyServesRoutesOnHTTP(t *testing.T) {
	cfg := config.Default()
	cfg.BaseDomain = "example.test"
	cfg.Caddy.TLSMode = "behind_proxy"
	manager := NewManager(cfg, nil, nil)

	raw, routeCount := manager.caddyConfig([]store.VM{
		{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", PublicHTTP: true},
	}, nil)
	if routeCount != 1 {
		t.Fatalf("routeCount = %d, want 1", routeCount)
	}
	servers := caddyServers(t, raw)
	if _, ok := servers["firedoze_https"]; ok {
		t.Fatal("behind_proxy mode unexpectedly configured an HTTPS server")
	}
	httpServer, ok := servers["firedoze_http"]
	if !ok {
		t.Fatal("missing firedoze_http server")
	}
	if len(httpServer.Listen) != 1 || httpServer.Listen[0] != ":80" {
		t.Fatalf("http listen = %v, want [:80]", httpServer.Listen)
	}
	if httpServer.TLSConnectionPolicies != nil {
		t.Fatal("http server unexpectedly has tls_connection_policies")
	}
	if len(httpServer.Routes) == 0 {
		t.Fatal("http server has no routes")
	}
}

type caddyServerConfig struct {
	Listen                []string         `json:"listen"`
	Routes                []map[string]any `json:"routes"`
	TLSConnectionPolicies []map[string]any `json:"tls_connection_policies"`
}

func caddyServers(t *testing.T, raw map[string]any) map[string]caddyServerConfig {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var cfgJSON struct {
		Apps struct {
			HTTP struct {
				Servers map[string]caddyServerConfig `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(data, &cfgJSON); err != nil {
		t.Fatal(err)
	}
	return cfgJSON.Apps.HTTP.Servers
}
