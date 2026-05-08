package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/routeauth"
	"firedoze/internal/store"
)

type recordingStarter struct {
	starts int
}

func (s *recordingStarter) StartVM(context.Context, string) (store.VM, error) {
	s.starts++
	return store.VM{Name: "demo", State: "running", PrivateIP: "fd7a:115c:a1e0::3", DefaultHTTPPort: 8080, AutoWake: true}, nil
}

func TestWakeProxyDoesNotWakeWhenAutoWakeDisabled(t *testing.T) {
	st := testStore(t)
	vm, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, PublicHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), vm.UUID, "sleeping"); err != nil {
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

func TestWakeProxyRequiresCaptchaBeforeWaking(t *testing.T) {
	st := testStore(t)
	vm, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, AutoWake: true, PublicHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), vm.UUID, "sleeping"); err != nil {
		t.Fatal(err)
	}

	starter := &recordingStarter{}
	proxy := NewWakeProxy(testConfig(), st, starter, nil)
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	resp := httptest.NewRecorder()

	proxy.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if starter.starts != 0 {
		t.Fatalf("starts = %d, want 0", starter.starts)
	}
	if !strings.Contains(resp.Body.String(), "Are you human?") {
		t.Fatalf("response missing captcha page:\n%s", resp.Body.String())
	}
}

func TestWakeProxyReturnsNotFoundForStoppedVM(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, AutoWake: true, PublicHTTP: true}); err != nil {
		t.Fatal(err)
	}

	starter := &recordingStarter{}
	proxy := NewWakeProxy(testConfig(), st, starter, nil)
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	resp := httptest.NewRecorder()

	proxy.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusNotFound)
	}
	if starter.starts != 0 {
		t.Fatalf("starts = %d, want 0", starter.starts)
	}
	if strings.Contains(resp.Body.String(), "Are you human?") {
		t.Fatalf("stopped VM unexpectedly returned captcha page:\n%s", resp.Body.String())
	}
}

func TestWakeProxyWakesWithSignedCookie(t *testing.T) {
	st := testStore(t)
	vm, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, AutoWake: true, PublicHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), vm.UUID, "sleeping"); err != nil {
		t.Fatal(err)
	}

	starter := &recordingStarter{}
	cfg := testConfig()
	auth := routeauth.NewManager(routeauth.KeyPath(cfg.StateDir), nil)
	if err := auth.Load(); err != nil {
		t.Fatal(err)
	}
	proxy := NewWakeProxyWithAuth(cfg, st, starter, auth, nil)
	token, err := auth.Token("demo.example.test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	req.AddCookie(&http.Cookie{
		Name:  wakeGateCookieName,
		Value: token,
	})
	resp := httptest.NewRecorder()

	proxy.ServeHTTP(resp, req)

	if starter.starts == 0 {
		t.Fatalf("starts = %d, want non-zero", starter.starts)
	}
}

func TestWakeProxyRouteProtectionDoesNotWakeUnauthenticatedTraffic(t *testing.T) {
	st := testStore(t)
	vm, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, AutoWake: true, PublicHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), vm.UUID, "sleeping"); err != nil {
		t.Fatal(err)
	}
	if err := st.ProtectRouteHostname(context.Background(), "demo.example.test"); err != nil {
		t.Fatal(err)
	}

	starter := &recordingStarter{}
	proxy := NewWakeProxy(testConfig(), st, starter, nil)
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	resp := httptest.NewRecorder()

	proxy.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusForbidden)
	}
	if starter.starts != 0 {
		t.Fatalf("starts = %d, want 0", starter.starts)
	}
}

func TestWakeProxyProtectedHostnameDoesNotPublishHiddenVM(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, AutoWake: true, PublicHTTP: false}); err != nil {
		t.Fatal(err)
	}
	if err := st.ProtectRouteHostname(context.Background(), "demo.example.test"); err != nil {
		t.Fatal(err)
	}

	proxy := NewWakeProxy(testConfig(), st, &recordingStarter{}, nil)
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	resp := httptest.NewRecorder()

	proxy.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusNotFound)
	}
}

func TestWakeProxySignedAuthURLSetsCookieWithoutRoute(t *testing.T) {
	cfg := testConfig()
	auth := routeauth.NewManager(routeauth.KeyPath(cfg.StateDir), nil)
	if err := auth.Load(); err != nil {
		t.Fatal(err)
	}
	proxy := NewWakeProxyWithAuth(cfg, testStore(t), &recordingStarter{}, auth, nil)
	token, err := auth.Token("future.example.test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://future.example.test/_firedoze/auth?token="+url.QueryEscape(token), nil)
	req.Host = "future.example.test"
	resp := httptest.NewRecorder()

	proxy.ServeHTTP(resp, req)

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusSeeOther)
	}
	cookies := resp.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != wakeGateCookieName {
		t.Fatalf("cookies = %#v", cookies)
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
	cfg.StateDir = filepath.Join(os.TempDir(), "firedoze-wake-test")
	return cfg
}
