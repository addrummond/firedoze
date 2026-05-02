package proxy

import (
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
