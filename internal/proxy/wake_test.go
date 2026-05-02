package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"firedoze/internal/config"
	"firedoze/internal/store"
)

type recordingStarter struct {
	starts int
}

func (s *recordingStarter) StartVM(context.Context, string) (store.VM, error) {
	s.starts++
	return store.VM{Name: "demo", State: "running", PrivateIP: "10.88.0.2", DefaultHTTPPort: 8080, AutoWake: true}, nil
}

func TestWakeProxyDoesNotWakeWhenAutoWakeDisabled(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "10.88.0.2", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), "demo", "sleeping"); err != nil {
		t.Fatal(err)
	}

	starter := &recordingStarter{}
	proxy := NewWakeProxy(testConfig(), st, starter, nil)
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	resp := httptest.NewRecorder()

	proxy.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusServiceUnavailable)
	}
	if starter.starts != 0 {
		t.Fatalf("starts = %d, want 0", starter.starts)
	}
}

func TestWakeProxyDoesNotWakeCrawler(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "10.88.0.2", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, AutoWake: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), "demo", "sleeping"); err != nil {
		t.Fatal(err)
	}

	starter := &recordingStarter{}
	proxy := NewWakeProxy(testConfig(), st, starter, nil)
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	req.Header.Set("User-Agent", "Mozilla/5.0 (l9scan/2.0; +https://leakix.net)")
	resp := httptest.NewRecorder()

	proxy.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusForbidden)
	}
	if starter.starts != 0 {
		t.Fatalf("starts = %d, want 0", starter.starts)
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.BaseDomain = "example.test"
	return cfg
}
