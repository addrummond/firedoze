package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/firecracker"
	"firedoze/internal/store"

	"github.com/dchest/captcha"
)

type fakeStarter struct {
	vm     store.VM
	err    error
	starts []string
}

type proxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f proxyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (s *fakeStarter) StartVM(_ context.Context, name string) (store.VM, error) {
	s.starts = append(s.starts, name)
	return s.vm, s.err
}

func TestDefaultHostAndCaddyFallbackRoute(t *testing.T) {
	if got, want := DefaultHost("demo", "dev.example.test"), "demo.dev.example.test"; got != want {
		t.Fatalf("DefaultHost = %q, want %q", got, want)
	}
	cfg := config.Default()
	cfg.BaseDomain = "example.test"
	cfg.Caddy.InternalProxyPort = 18082
	manager := NewManager(cfg, nil, nil)
	raw, routeCount := manager.caddyConfig([]store.VM{
		{Name: "public", PrivateIP: "fd00::3", PublicHTTP: true},
		{Name: "empty-ip", PublicHTTP: true},
	}, nil)
	if routeCount != 1 {
		t.Fatalf("routeCount = %d, want 1", routeCount)
	}
	servers := caddyServers(t, raw)
	httpsServer := servers["firedoze_https"]
	last := httpsServer.Routes[len(httpsServer.Routes)-1]
	data := mustJSON(t, last)
	if !strings.Contains(string(data), "firedoze route not found") {
		t.Fatalf("fallback route = %s", data)
	}
	if !strings.Contains(string(mustJSON(t, httpsServer.Routes[0])), "127.0.0.1:18082") {
		t.Fatalf("public route does not use wake proxy upstream: %s", mustJSON(t, httpsServer.Routes[0]))
	}
}

func TestCaddyReconcileAndStopUseAdapter(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{
		Name: "demo", PrivateIP: "fd00::3", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, PublicHTTP: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoute(context.Background(), store.CreateRouteParams{Name: "api", VMName: "demo", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	var loaded []byte
	var force bool
	stopped := false
	oldLoad := caddyLoad
	oldStop := caddyStop
	caddyLoad = func(data []byte, forceReload bool) error {
		loaded = append([]byte(nil), data...)
		force = forceReload
		return nil
	}
	caddyStop = func() error {
		stopped = true
		return nil
	}
	t.Cleanup(func() {
		caddyLoad = oldLoad
		caddyStop = oldStop
	})

	manager := NewManager(testConfig(), st, nil)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !force {
		t.Fatal("caddyLoad forceReload = false, want true")
	}
	if !strings.Contains(string(loaded), "demo.example.test") || !strings.Contains(string(loaded), "api.example.test") {
		t.Fatalf("loaded Caddy config missing routes: %s", loaded)
	}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("caddyStop was not called")
	}
}

func TestWakeProxyRouteForHostDefaultAliasAndHostNormalization(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{
		Name: "demo", PrivateIP: "127.0.0.1", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, PublicHTTP: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoute(context.Background(), store.CreateRouteParams{Name: "api", VMName: "demo", Port: 9000}); err != nil {
		t.Fatal(err)
	}
	proxy := NewWakeProxy(testConfig(), st, &fakeStarter{}, nil)

	vm, port, ok := proxy.routeForHost(context.Background(), "DEMO.EXAMPLE.TEST:443")
	if !ok || vm.Name != "demo" || port != 8080 {
		t.Fatalf("default route = %#v/%d/%v", vm, port, ok)
	}
	vm, port, ok = proxy.routeForHost(context.Background(), "api.example.test.")
	if !ok || vm.Name != "demo" || port != 9000 {
		t.Fatalf("alias route = %#v/%d/%v", vm, port, ok)
	}
	for _, host := range []string{"demo.other.test", "nested.demo.example.test", "example.test"} {
		if _, _, ok := proxy.routeForHost(context.Background(), host); ok {
			t.Fatalf("routeForHost(%q) matched unexpectedly", host)
		}
	}
	if got := routeHost("DEMO.EXAMPLE.TEST:443."); got != "demo.example.test" {
		t.Fatalf("routeHost with malformed trailing dot = %q", got)
	}
	if got := routeHost("DEMO.EXAMPLE.TEST:443"); got != "demo.example.test" {
		t.Fatalf("routeHost = %q, want demo.example.test", got)
	}
}

func TestWakeProxyProxiesRunningDefaultRouteAndAlias(t *testing.T) {
	oldTransport := wakeProxyTransport
	wakeProxyTransport = proxyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Host != "demo.example.test" && r.Host != "api.example.test" {
			t.Fatalf("unexpected forwarded host: %s", r.Host)
		}
		if r.URL.Scheme != "http" || r.URL.Host != "192.0.2.10:8080" || r.URL.Path != "/path" {
			t.Fatalf("unexpected upstream URL: %s", r.URL.String())
		}
		resp := httptest.NewRecorder()
		resp.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(resp, "hello from upstream")
		return resp.Result(), nil
	})
	t.Cleanup(func() {
		wakeProxyTransport = oldTransport
	})

	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{
		Name: "demo", PrivateIP: "192.0.2.10", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, AutoWake: true, PublicHTTP: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), "demo", "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoute(context.Background(), store.CreateRouteParams{Name: "api", VMName: "demo", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	proxy := NewWakeProxy(testConfig(), st, &fakeStarter{}, nil)

	for _, host := range []string{"demo.example.test", "api.example.test"} {
		req := httptest.NewRequest(http.MethodGet, "https://"+host+"/path", nil)
		req.Host = host
		resp := httptest.NewRecorder()
		proxy.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body: %s", host, resp.Code, resp.Body.String())
		}
		if resp.Header().Get("X-Upstream") != "yes" || resp.Body.String() != "hello from upstream" {
			t.Fatalf("%s response = headers %v body %q", host, resp.Header(), resp.Body.String())
		}
	}
}

func TestWakeProxySleepingStartFailuresAndPostCaptcha(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{
		Name: "demo", PrivateIP: "127.0.0.1", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 1, AutoWake: true, PublicHTTP: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVMState(context.Background(), "demo", "sleeping"); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	key, err := ensureWakeGateKey(filepath.Join(cfg.StateDir, "wake_gate.key"))
	if err != nil {
		t.Fatal(err)
	}

	starter := &fakeStarter{err: errors.New("boom")}
	proxy := NewWakeProxy(cfg, st, starter, nil)
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	req.AddCookie(&http.Cookie{Name: wakeGateCookieName, Value: signedWakeCookie(key, "demo.example.test", time.Now().Add(time.Hour))})
	resp := httptest.NewRecorder()
	proxy.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("start failure status = %d, want 503; body: %s", resp.Code, resp.Body.String())
	}

	starter.err = firecracker.ErrAlreadyRunning
	resp = httptest.NewRecorder()
	proxy.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("already-running stale state status = %d, want 503; body: %s", resp.Code, resp.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "https://demo.example.test/not-wake", nil)
	req.Host = "demo.example.test"
	resp = httptest.NewRecorder()
	proxy.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("captcha-required POST status = %d, want 403", resp.Code)
	}
}

func TestWakeGateKeysCookiesAndHandlers(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "wake_gate.key")
	key, err := ensureWakeGateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != wakeGateKeySize {
		t.Fatalf("key len = %d, want %d", len(key), wakeGateKeySize)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o, want 600", info.Mode().Perm())
	}
	keyAgain, err := ensureWakeGateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(key, keyAgain) {
		t.Fatal("ensureWakeGateKey did not reuse existing key")
	}
	badKeyPath := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(badKeyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureWakeGateKey(badKeyPath); err == nil {
		t.Fatal("ensureWakeGateKey accepted short key")
	}

	gate := &wakeGate{keyPath: keyPath}
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.Host = "demo.example.test"
	if gate.approved(req, "demo.example.test") {
		t.Fatal("request without cookie was approved")
	}
	req.AddCookie(&http.Cookie{Name: wakeGateCookieName, Value: signedWakeCookie(key, "other.example.test", time.Now().Add(time.Hour))})
	if gate.approved(req, "demo.example.test") {
		t.Fatal("wrong-host cookie was approved")
	}
	req = httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.AddCookie(&http.Cookie{Name: wakeGateCookieName, Value: signedWakeCookie(key, "demo.example.test", time.Now().Add(-time.Hour))})
	if gate.approved(req, "demo.example.test") {
		t.Fatal("expired cookie was approved")
	}
	req = httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.AddCookie(&http.Cookie{Name: wakeGateCookieName, Value: "bad.cookie.parts"})
	if gate.approved(req, "demo.example.test") {
		t.Fatal("malformed cookie was approved")
	}

	resp := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "https://demo.example.test/_firedoze/wake-captcha/missing.png", nil)
	if !gate.handle(resp, req, "demo.example.test") {
		t.Fatal("gate did not handle captcha route")
	}
	if resp.Code != http.StatusNotFound {
		t.Fatalf("missing captcha status = %d, want 404", resp.Code)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "https://demo.example.test/_firedoze/wake", strings.NewReader("id=missing&answer=wrong&next=//evil.test"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gate.verify(resp, req, "demo.example.test")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("bad captcha verify status = %d, want 403", resp.Code)
	}

	if decoded, err := decodeCookiePart(encodeCookiePart("demo")); err != nil || decoded != "demo" {
		t.Fatalf("cookie part round trip = %q/%v", decoded, err)
	}
	if _, err := decodeCookiePart("not valid base64"); err == nil {
		t.Fatal("decodeCookiePart accepted invalid base64")
	}
	if validSignature(key, "message", "not valid") {
		t.Fatal("validSignature accepted invalid signature encoding")
	}
}

func TestWakeGateVerifySuccessSetsCookieAndRedirects(t *testing.T) {
	store := captcha.NewMemoryStore(10, time.Minute)
	store.Set("known", []byte{1, 2, 3, 4, 5})
	captcha.SetCustomStore(store)
	defer captcha.SetCustomStore(captcha.NewMemoryStore(captcha.CollectNum, captcha.Expiration))

	keyPath := filepath.Join(t.TempDir(), "wake_gate.key")
	gate := &wakeGate{keyPath: keyPath}
	body := strings.NewReader("id=known&answer=12345&next=/hello")
	req := httptest.NewRequest(http.MethodPost, "https://demo.example.test/_firedoze/wake", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := httptest.NewRecorder()

	gate.verify(resp, req, "demo.example.test")

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", resp.Code, resp.Body.String())
	}
	if location := resp.Header().Get("Location"); location != "/hello" {
		t.Fatalf("Location = %q, want /hello", location)
	}
	cookies := resp.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != wakeGateCookieName {
		t.Fatalf("cookies = %#v", cookies)
	}
	approvedReq := httptest.NewRequest(http.MethodGet, "https://demo.example.test/hello", nil)
	approvedReq.AddCookie(cookies[0])
	if !gate.approved(approvedReq, "demo.example.test") {
		t.Fatal("fresh verify cookie was not approved")
	}
}

func TestTCPWakeHelpers(t *testing.T) {
	cfg := testConfig()
	cfg.WireGuard.Interface = "fdwg-test"
	cfg.VMNetwork.Subnet = "10.88.0.0/16"
	cfg.SSH.WakeProxyPort = 18022
	proxy := NewTCPWakeProxy(cfg, testStore(t), &fakeStarter{}, nil)

	if !isIPv4CIDR("10.88.0.0/16") {
		t.Fatal("isIPv4CIDR returned false for IPv4 CIDR")
	}
	if isIPv4CIDR("fd00::/64") || isIPv4CIDR("not-cidr") {
		t.Fatal("isIPv4CIDR accepted non-IPv4 CIDR")
	}
	wantRule := []string{
		"-t", "nat", "-A", "PREROUTING",
		"-i", "fdwg-test",
		"-p", "tcp",
		"-d", "10.88.0.0/16",
		"--dport", "22",
		"-j", "REDIRECT",
		"--to-ports", "18022",
	}
	if got := proxy.sshRedirectRule("-A"); !reflect.DeepEqual(got, wantRule) {
		t.Fatalf("sshRedirectRule = %#v, want %#v", got, wantRule)
	}
}

func TestTCPWakeRedirectCommands(t *testing.T) {
	cfg := testConfig()
	cfg.WireGuard.Interface = "fdwg-test"
	cfg.VMNetwork.Subnet = "10.88.0.0/16"
	cfg.SSH.WakeProxyPort = 18022
	proxy := NewTCPWakeProxy(cfg, testStore(t), &fakeStarter{}, nil)

	var ops []string
	restore := stubTCPWakeCommands(t, func(_ context.Context, name string, args ...string) error {
		if name != "/usr/sbin/iptables" {
			t.Fatalf("command = %q, want iptables", name)
		}
		op := args[2]
		ops = append(ops, op)
		if op == "-C" {
			return errors.New("missing rule")
		}
		return nil
	})
	defer restore()

	if err := proxy.ensureSSHRedirect(context.Background()); err != nil {
		t.Fatalf("ensureSSHRedirect: %v", err)
	}
	if err := proxy.deleteSSHRedirect(context.Background()); err != nil {
		t.Fatalf("deleteSSHRedirect: %v", err)
	}
	if got := strings.Join(ops, ","); got != "-C,-A,-D" {
		t.Fatalf("iptables ops = %q, want -C,-A,-D", got)
	}

	ops = nil
	restore()
	restore = stubTCPWakeCommands(t, func(_ context.Context, name string, args ...string) error {
		ops = append(ops, args[2])
		return nil
	})
	defer restore()
	if err := proxy.ensureSSHRedirect(context.Background()); err != nil {
		t.Fatalf("ensureSSHRedirect existing rule: %v", err)
	}
	if got := strings.Join(ops, ","); got != "-C" {
		t.Fatalf("existing-rule iptables ops = %q, want -C", got)
	}
}

func TestTCPWakeVMByPrivateIPAndRunSSHIPv6Noop(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{Name: "demo", PrivateIP: "fd00::3", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.VMNetwork.Subnet = "fd00::/64"
	proxy := NewTCPWakeProxy(cfg, st, &fakeStarter{}, nil)
	vm, err := proxy.vmByPrivateIP(context.Background(), "fd00::3")
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name != "demo" {
		t.Fatalf("vmByPrivateIP = %#v", vm)
	}
	if _, err := proxy.vmByPrivateIP(context.Background(), "fd00::5"); err == nil {
		t.Fatal("vmByPrivateIP accepted missing IP")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := proxy.RunSSH(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTCPWakeHandleSSHConn(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		autoWake   bool
		startErr   error
		waitErr    error
		dialErr    error
		wantStarts int
		wantWait   bool
		wantDial   bool
	}{
		{name: "auto wake disabled", state: "sleeping", autoWake: false},
		{name: "start failure", state: "sleeping", autoWake: true, startErr: errors.New("start failed"), wantStarts: 1},
		{name: "wait failure", state: "sleeping", autoWake: true, waitErr: errors.New("ssh not ready"), wantStarts: 1, wantWait: true},
		{name: "dial failure", state: "running", autoWake: true, dialErr: errors.New("dial failed"), wantWait: true, wantDial: true},
		{name: "running proxy", state: "running", autoWake: true, wantWait: true, wantDial: true},
		{name: "sleeping starts and proxies", state: "sleeping", autoWake: true, wantStarts: 1, wantWait: true, wantDial: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := testStore(t)
			if _, err := st.CreateVM(context.Background(), store.CreateVMParams{
				Name: "demo", PrivateIP: "fd00::3", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080, AutoWake: tt.autoWake, AutoWakeSet: true,
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.SetVMState(context.Background(), "demo", tt.state); err != nil {
				t.Fatal(err)
			}
			starter := &fakeStarter{
				vm:  store.VM{Name: "demo", State: "running", PrivateIP: "fd00::3", AutoWake: tt.autoWake},
				err: tt.startErr,
			}
			proxy := NewTCPWakeProxy(testConfig(), st, starter, nil)
			client := &bufferConn{reader: strings.NewReader("from-client")}
			upstream := &bufferConn{reader: strings.NewReader("from-upstream")}
			waited := false
			dialed := false

			restore := stubTCPWakeNetwork(t, tcpWakeNetworkStubs{
				originalDestination: func(net.Conn) (*net.TCPAddr, error) {
					return &net.TCPAddr{IP: net.ParseIP("fd00::3"), Port: 22}, nil
				},
				waitForTCP: func(_ context.Context, address string, timeout time.Duration) error {
					waited = true
					if address != "[fd00::3]:22" {
						t.Fatalf("wait address = %q, want [fd00::3]:22", address)
					}
					if timeout != sshReadyTimeout {
						t.Fatalf("wait timeout = %s, want %s", timeout, sshReadyTimeout)
					}
					return tt.waitErr
				},
				dialTimeout: func(network string, address string, timeout time.Duration) (net.Conn, error) {
					dialed = true
					if network != "tcp" || address != "[fd00::3]:22" {
						t.Fatalf("dial = %s %s, want tcp [fd00::3]:22", network, address)
					}
					if timeout != 10*time.Second {
						t.Fatalf("dial timeout = %s, want 10s", timeout)
					}
					if tt.dialErr != nil {
						return nil, tt.dialErr
					}
					return upstream, nil
				},
			})
			defer restore()

			proxy.handleSSHConn(context.Background(), client)

			if len(starter.starts) != tt.wantStarts {
				t.Fatalf("starts = %#v, want %d", starter.starts, tt.wantStarts)
			}
			if waited != tt.wantWait {
				t.Fatalf("waited = %v, want %v", waited, tt.wantWait)
			}
			if dialed != tt.wantDial {
				t.Fatalf("dialed = %v, want %v", dialed, tt.wantDial)
			}
			if !client.closed {
				t.Fatal("client connection was not closed")
			}
		})
	}
}

func TestTCPWakeHandleSSHConnOriginalDestinationAndLookupFailures(t *testing.T) {
	st := testStore(t)
	proxy := NewTCPWakeProxy(testConfig(), st, &fakeStarter{}, nil)

	client := &bufferConn{}
	restore := stubTCPWakeNetwork(t, tcpWakeNetworkStubs{
		originalDestination: func(net.Conn) (*net.TCPAddr, error) {
			return nil, errors.New("no original destination")
		},
	})
	proxy.handleSSHConn(context.Background(), client)
	restore()
	if !client.closed {
		t.Fatal("client was not closed after original destination failure")
	}

	client = &bufferConn{}
	restore = stubTCPWakeNetwork(t, tcpWakeNetworkStubs{
		originalDestination: func(net.Conn) (*net.TCPAddr, error) {
			return &net.TCPAddr{IP: net.ParseIP("fd00::99"), Port: 22}, nil
		},
	})
	proxy.handleSSHConn(context.Background(), client)
	restore()
	if !client.closed {
		t.Fatal("client was not closed after vm lookup failure")
	}
}

func TestWaitForTCPAndProxyCopy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := waitForTCP(ctx, "127.0.0.1:1", 10*time.Millisecond); err == nil {
		t.Fatal("waitForTCP succeeded for closed port")
	}

	src := &bufferConn{reader: strings.NewReader("hello")}
	dst := &bufferConn{}
	errCh := make(chan error, 1)
	go proxyCopy(errCh, dst, src)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxyCopy did not finish")
	}
	if dst.writer.String() != "hello" {
		t.Fatalf("proxyCopy wrote %q, want hello", dst.writer.String())
	}
}

type tcpWakeNetworkStubs struct {
	originalDestination func(net.Conn) (*net.TCPAddr, error)
	waitForTCP          func(context.Context, string, time.Duration) error
	dialTimeout         func(string, string, time.Duration) (net.Conn, error)
}

func stubTCPWakeNetwork(t *testing.T, stubs tcpWakeNetworkStubs) func() {
	t.Helper()
	oldOriginalDestination := originalDestinationFunc
	oldWaitForTCP := waitForTCPFunc
	oldDialTimeout := dialTimeoutFunc
	if stubs.originalDestination != nil {
		originalDestinationFunc = stubs.originalDestination
	}
	if stubs.waitForTCP != nil {
		waitForTCPFunc = stubs.waitForTCP
	}
	if stubs.dialTimeout != nil {
		dialTimeoutFunc = stubs.dialTimeout
	}
	return func() {
		originalDestinationFunc = oldOriginalDestination
		waitForTCPFunc = oldWaitForTCP
		dialTimeoutFunc = oldDialTimeout
	}
}

func stubTCPWakeCommands(t *testing.T, fn func(context.Context, string, ...string) error) func() {
	t.Helper()
	old := runCommand
	runCommand = fn
	return func() {
		runCommand = old
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type bufferConn struct {
	reader *strings.Reader
	writer bytes.Buffer
	closed bool
}

func (c *bufferConn) Read(p []byte) (int, error) {
	if c.reader == nil {
		return 0, io.EOF
	}
	return c.reader.Read(p)
}

func (c *bufferConn) Write(p []byte) (int, error) {
	return c.writer.Write(p)
}

func (c *bufferConn) Close() error {
	c.closed = true
	return nil
}

func (c *bufferConn) LocalAddr() net.Addr {
	return dummyAddr("local")
}

func (c *bufferConn) RemoteAddr() net.Addr {
	return dummyAddr("remote")
}

func (c *bufferConn) SetDeadline(time.Time) error {
	return nil
}

func (c *bufferConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *bufferConn) SetWriteDeadline(time.Time) error {
	return nil
}

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }
