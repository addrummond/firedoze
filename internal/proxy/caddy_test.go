package proxy

import (
	"encoding/json"
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
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}

	var cfgJSON struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen                []string         `json:"listen"`
					TLSConnectionPolicies []map[string]any `json:"tls_connection_policies"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(data, &cfgJSON); err != nil {
		t.Fatal(err)
	}
	httpsServer, ok := cfgJSON.Apps.HTTP.Servers["firedoze_https"]
	if !ok {
		t.Fatal("missing firedoze_https server")
	}
	if len(httpsServer.Listen) != 1 || httpsServer.Listen[0] != ":443" {
		t.Fatalf("https listen = %v, want [:443]", httpsServer.Listen)
	}
	if httpsServer.TLSConnectionPolicies == nil {
		t.Fatal("https server has no tls_connection_policies")
	}
	httpServer, ok := cfgJSON.Apps.HTTP.Servers["firedoze_http"]
	if !ok {
		t.Fatal("missing firedoze_http server")
	}
	if len(httpServer.Listen) != 1 || httpServer.Listen[0] != ":80" {
		t.Fatalf("http listen = %v, want [:80]", httpServer.Listen)
	}
	if httpServer.TLSConnectionPolicies != nil {
		t.Fatal("http server unexpectedly has tls_connection_policies")
	}
}
