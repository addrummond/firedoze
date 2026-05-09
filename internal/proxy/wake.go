package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/firecracker"
	"firedoze/internal/routeauth"
	"firedoze/internal/store"
)

type VMStarter interface {
	StartVM(context.Context, string) (store.VM, error)
}

type WakeProxy struct {
	cfg     config.Config
	store   *store.Store
	manager VMStarter
	logger  *slog.Logger
	gate    *wakeGate
}

var wakeProxyTransport http.RoundTripper = http.DefaultTransport

func NewWakeProxy(cfg config.Config, st *store.Store, manager VMStarter, logger *slog.Logger) *WakeProxy {
	return NewWakeProxyWithAuth(cfg, st, manager, routeauth.NewManager(routeauth.KeyPath(cfg.StateDir), logger), logger)
}

func NewWakeProxyWithAuth(cfg config.Config, st *store.Store, manager VMStarter, auth *routeauth.Manager, logger *slog.Logger) *WakeProxy {
	if logger == nil {
		logger = slog.Default()
	}
	return &WakeProxy{
		cfg:     cfg,
		store:   st,
		manager: manager,
		logger:  logger,
		gate:    newWakeGate(cfg, auth, logger),
	}
}

func (p *WakeProxy) Run(ctx context.Context) error {
	server := &http.Server{
		Addr:    net.JoinHostPort("127.0.0.1", strconv.Itoa(p.cfg.Caddy.InternalProxyPort)),
		Handler: p,
	}

	errCh := make(chan error, 1)
	go func() {
		p.logger.Info("wake proxy listening", "addr", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (p *WakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := routeHost(r.Host)
	if r.Method == http.MethodGet && r.URL.Path == "/_firedoze/auth" {
		p.handleSignedAuthURL(w, r, host)
		return
	}
	vm, port, ok := p.routeForHost(r.Context(), r.Host)
	if !ok {
		http.Error(w, "firedoze route not found", http.StatusNotFound)
		return
	}
	if vm.State != "running" && vm.State != "sleeping" {
		http.Error(w, "firedoze route not found", http.StatusNotFound)
		return
	}
	if !vm.PublicHTTP {
		http.Error(w, "firedoze route not found", http.StatusNotFound)
		return
	}
	protected, err := p.store.IsRouteHostnameProtected(r.Context(), host)
	if err != nil {
		p.logger.Warn("check route protection", "host", host, "error", err)
		http.Error(w, "firedoze route auth unavailable", http.StatusServiceUnavailable)
		return
	}
	if protected && !p.gate.approved(r, host) {
		http.Error(w, "firedoze route is protected", http.StatusForbidden)
		return
	}
	if vm.State == "sleeping" {
		if !vm.AutoWake {
			p.logger.Info(
				"ignored http wake because vm auto_wake is disabled",
				"vm", vm.Name,
				"host", r.Host,
				"method", r.Method,
				"path", r.URL.RequestURI(),
				"remote_addr", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			)
			http.Error(w, "firedoze vm auto wake is disabled", http.StatusServiceUnavailable)
			return
		}
		if !p.gate.approved(r, host) {
			p.gate.handle(w, r, host)
			return
		}
		started, err := p.manager.StartVM(r.Context(), vm.UUID)
		if err != nil && !errors.Is(err, firecracker.ErrAlreadyRunning) {
			p.logger.Warn("wake vm for http route", "vm", vm.Name, "host", r.Host, "error", err)
			http.Error(w, "firedoze wake failed", http.StatusServiceUnavailable)
			return
		}
		if err == nil {
			vm = started
		} else if refreshed, refreshErr := p.store.GetVM(r.Context(), vm.UUID); refreshErr == nil {
			vm = refreshed
		}
		p.logger.Info(
			"woke vm for http route",
			"vm", vm.Name,
			"host", r.Host,
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		)
	}
	if vm.State != "running" {
		http.Error(w, "firedoze vm is not running", http.StatusServiceUnavailable)
		return
	}
	if vm.PrivateIP == "" {
		http.Error(w, "firedoze vm has no private ip", http.StatusServiceUnavailable)
		return
	}
	if err := p.store.TouchVMActivity(r.Context(), vm.UUID); err != nil {
		p.logger.Warn("touch vm activity for http route", "vm", vm.Name, "host", r.Host, "error", err)
	}

	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(vm.PrivateIP, strconv.Itoa(port)),
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = wakeProxyTransport
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		p.logger.Warn("proxy vm http route", "vm", vm.Name, "host", req.Host, "target", target.Host, "error", err)
		http.Error(w, "firedoze proxy failed (is your service running?)", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

func (p *WakeProxy) handleSignedAuthURL(w http.ResponseWriter, r *http.Request, host string) {
	token := r.URL.Query().Get("token")
	expires, ok := p.gate.auth.Validate(token, host)
	if !ok {
		http.Error(w, "firedoze auth token invalid or expired", http.StatusForbidden)
		return
	}
	if err := p.gate.auth.SetCookie(w, host, expires); err != nil {
		p.logger.Warn("set route auth cookie", "host", host, "error", err)
		http.Error(w, "firedoze route auth unavailable", http.StatusServiceUnavailable)
		return
	}
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (p *WakeProxy) routeForHost(ctx context.Context, hostport string) (store.VM, int, bool) {
	host := routeHost(hostport)
	base := strings.TrimSuffix(strings.ToLower(p.cfg.BaseDomain), ".")
	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return store.VM{}, 0, false
	}
	name := strings.TrimSuffix(host, suffix)
	if name == "" {
		return store.VM{}, 0, false
	}

	if vm, err := p.store.GetVMByName(ctx, name); err == nil {
		return vm, vm.DefaultHTTPPort, true
	}

	route, err := p.store.GetRoute(ctx, name)
	if err != nil {
		return store.VM{}, 0, false
	}
	vm, err := p.store.GetVM(ctx, route.VMUUID)
	if err != nil {
		return store.VM{}, 0, false
	}
	return vm, route.Port, true
}

func routeHost(hostport string) string {
	host := strings.ToLower(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}
