package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/firecracker"
	"firedoze/internal/store"
	wgconfig "firedoze/internal/wireguard"
)

const ShutdownTimeout = 5 * time.Second

type Server struct {
	cfg     config.Config
	manager *firecracker.Manager
	store   *store.Store
	proxy   Proxy
	mux     *http.ServeMux
}

type Proxy interface {
	Reconcile(context.Context) error
}

func NewServer(cfg config.Config, manager *firecracker.Manager, st *store.Store, proxy Proxy) http.Handler {
	server := &Server{
		cfg:     cfg,
		manager: manager,
		store:   st,
		proxy:   proxy,
		mux:     http.NewServeMux(),
	}
	server.routes()
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handleHelp)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /config", s.handleConfig)
	s.mux.HandleFunc("GET /vms", s.handleListVMs)
	s.mux.HandleFunc("POST /vms", s.handleCreateVM)
	s.mux.HandleFunc("GET /vms/{name}", s.handleGetVM)
	s.mux.HandleFunc("PATCH /vms/{name}/settings", s.handleUpdateVMSettings)
	s.mux.HandleFunc("DELETE /vms/{name}", s.handleDeleteVM)
	s.mux.HandleFunc("POST /vms/{name}/start", s.handleStartVM)
	s.mux.HandleFunc("POST /vms/{name}/stop", s.handleStopVM)
	s.mux.HandleFunc("POST /vms/{name}/sleep", s.handleSleepVM)
	s.mux.HandleFunc("GET /routes", s.handleListRoutes)
	s.mux.HandleFunc("POST /routes", s.handleCreateRoute)
	s.mux.HandleFunc("DELETE /routes/{name}", s.handleDeleteRoute)
	s.mux.HandleFunc("GET /snapshots", s.handleListSnapshots)
	s.mux.HandleFunc("POST /snapshots", s.handleCreateSnapshot)
	s.mux.HandleFunc("GET /snapshots/{name}", s.handleGetSnapshot)
	s.mux.HandleFunc("DELETE /snapshots/{name}", s.handleDeleteSnapshot)
	s.mux.HandleFunc("POST /snapshots/{name}/restore", s.handleRestoreSnapshot)
	s.mux.HandleFunc("GET /wireguard/peers", s.handleListWireGuardPeers)
	s.mux.HandleFunc("GET /wireguard/peers/{name}/config", s.handleWireGuardPeerConfig)
}

func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     "firedoze",
		"api_version": 1,
		"resources": map[string][]string{
			"health":          {"GET /health"},
			"config":          {"GET /config"},
			"vms":             {"GET /vms", "POST /vms", "GET /vms/{name}", "PATCH /vms/{name}/settings", "DELETE /vms/{name}", "POST /vms/{name}/start", "POST /vms/{name}/stop", "POST /vms/{name}/sleep"},
			"routes":          {"GET /routes", "POST /routes", "DELETE /routes/{name}"},
			"snapshots":       {"GET /snapshots", "POST /snapshots", "GET /snapshots/{name}", "DELETE /snapshots/{name}", "POST /snapshots/{name}/restore"},
			"wireguard_peers": {"GET /wireguard/peers", "GET /wireguard/peers/{name}/config"},
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"base_domain":       s.cfg.BaseDomain,
		"default_http_port": s.cfg.DefaultHTTPPort,
		"api": map[string]any{
			"port": s.cfg.API.Port,
		},
		"caddy": map[string]any{
			"http_port":           s.cfg.Caddy.HTTPPort,
			"https_port":          s.cfg.Caddy.HTTPSPort,
			"internal_proxy_port": s.cfg.Caddy.InternalProxyPort,
		},
		"wireguard": map[string]any{
			"interface":   s.cfg.WireGuard.Interface,
			"listen_port": s.cfg.WireGuard.ListenPort,
			"address":     s.cfg.WireGuard.Address,
			"endpoint":    wgconfig.Endpoint(s.cfg),
			"peers":       len(s.cfg.WireGuard.Peers),
		},
		"vm_network": map[string]any{
			"subnet": s.cfg.VMNetwork.Subnet,
		},
		"dns": map[string]any{
			"enabled":          s.cfg.DNS.Enabled,
			"domain":           s.cfg.DNS.Domain,
			"listen_ip":        s.cfg.DNS.ListenIP,
			"port":             s.cfg.DNS.Port,
			"ttl_seconds":      s.cfg.DNS.TTLSeconds,
			"upstream_servers": s.cfg.DNS.UpstreamServers,
		},
		"ssh": map[string]any{
			"user": s.cfg.SSH.User,
		},
		"idle": map[string]any{
			"check_interval_seconds":      s.cfg.Idle.CheckIntervalSeconds,
			"default_sleep_after_seconds": s.cfg.Idle.DefaultSleepAfterSeconds,
		},
		"firecracker": map[string]any{
			"binary_path":        s.cfg.Firecracker.BinaryPath,
			"base_kernel_path":   s.cfg.Firecracker.BaseKernelPath,
			"base_initrd_path":   s.cfg.Firecracker.BaseInitrdPath,
			"base_rootfs_path":   s.cfg.Firecracker.BaseRootfsPath,
			"default_vcpus":      s.cfg.Firecracker.DefaultVCPUs,
			"default_memory_mib": s.cfg.Firecracker.DefaultMemoryMiB,
			"default_disk_bytes": s.cfg.Firecracker.DefaultDiskBytes,
		},
	})
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.manager.ListVMsMatching(r.Context(), r.URL.Query()["name"])
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": s.vmInfos(vms)})
}

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	vm, err := s.manager.GetVM(r.Context(), name)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": s.vmInfo(vm)})
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                  string `json:"name"`
		VCPUs                 int    `json:"vcpus"`
		MemoryMiB             int    `json:"memory_mib"`
		DiskBytes             int64  `json:"disk_bytes"`
		DefaultHTTPPort       int    `json:"default_http_port"`
		IdleSleepAfterSeconds int    `json:"idle_sleep_after_seconds"`
		AutoWake              bool   `json:"auto_wake"`
		PublicHTTP            bool   `json:"public_http"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !validVMName(req.Name) {
		writeError(w, http.StatusBadRequest, errors.New("name must contain only lowercase letters, numbers, and hyphens"))
		return
	}
	vm, err := s.manager.CreateVM(r.Context(), store.CreateVMParams{
		Name:                  req.Name,
		VCPUs:                 req.VCPUs,
		MemoryMiB:             req.MemoryMiB,
		DiskBytes:             req.DiskBytes,
		DefaultHTTPPort:       req.DefaultHTTPPort,
		IdleSleepAfterSeconds: req.IdleSleepAfterSeconds,
		AutoWake:              req.AutoWake,
		PublicHTTP:            req.PublicHTTP,
	})
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"vm": s.vmInfo(vm)})
}

func (s *Server) handleStartVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	vm, err := s.manager.StartVM(r.Context(), name)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, firecracker.ErrAlreadyRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": s.vmInfo(vm)})
}

func (s *Server) handleUpdateVMSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		DefaultHTTPPort       *int  `json:"default_http_port"`
		IdleSleepAfterSeconds *int  `json:"idle_sleep_after_seconds"`
		AutoWake              *bool `json:"auto_wake"`
		PublicHTTP            *bool `json:"public_http"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	vm, err := s.manager.UpdateVM(r.Context(), name, store.UpdateVMParams{
		DefaultHTTPPort:       req.DefaultHTTPPort,
		IdleSleepAfterSeconds: req.IdleSleepAfterSeconds,
		AutoWake:              req.AutoWake,
		PublicHTTP:            req.PublicHTTP,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": s.vmInfo(vm)})
}

func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.manager.DeleteVM(r.Context(), name); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleStopVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.manager.StopVM(r.Context(), name); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleSleepVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	vm, err := s.manager.SleepVM(r.Context(), name)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, firecracker.ErrNotRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": s.vmInfo(vm)})
}

func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := s.store.ListRoutes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": s.routeInfos(routes)})
}

func (s *Server) handleCreateRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		VMName string `json:"vm_name"`
		VM     string `json:"vm"`
		Port   int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.VMName == "" {
		req.VMName = req.VM
	}
	if !validVMName(req.Name) {
		writeError(w, http.StatusBadRequest, errors.New("name must contain only lowercase letters, numbers, and hyphens"))
		return
	}
	if !validVMName(req.VMName) {
		writeError(w, http.StatusBadRequest, errors.New("vm_name must contain only lowercase letters, numbers, and hyphens"))
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, errors.New("port must be between 1 and 65535"))
		return
	}
	reserved, err := s.store.VMExists(r.Context(), req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if reserved {
		writeError(w, http.StatusConflict, errors.New("route name is reserved by a VM default hostname"))
		return
	}
	route, err := s.store.CreateRoute(r.Context(), store.CreateRouteParams{
		Name:   req.Name,
		VMName: req.VMName,
		Port:   req.Port,
	})
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"route": s.routeInfo(route)})
}

func (s *Server) handleDeleteRoute(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.DeleteRoute(r.Context(), name); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	snapshots, err := s.manager.ListSnapshots(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snapshots})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.manager.GetSnapshot(r.Context(), r.PathValue("name"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot})
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		VMName string `json:"vm_name"`
		VM     string `json:"vm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.VMName == "" {
		req.VMName = req.VM
	}
	if !validSnapshotName(req.Name) {
		writeError(w, http.StatusBadRequest, errors.New("snapshot name must contain only letters, numbers, dots, underscores, and hyphens"))
		return
	}
	if !validVMName(req.VMName) {
		writeError(w, http.StatusBadRequest, errors.New("vm_name must contain only lowercase letters, numbers, and hyphens"))
		return
	}
	snapshot, err := s.manager.SaveSnapshot(r.Context(), store.CreateSnapshotParams{
		Name:     req.Name,
		SourceVM: req.VMName,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, firecracker.ErrNotRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"snapshot": snapshot,
	})
}

func (s *Server) handleRestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotName := r.PathValue("name")
	var req struct {
		VMName                string `json:"vm_name"`
		VM                    string `json:"vm"`
		VCPUs                 int    `json:"vcpus"`
		MemoryMiB             int    `json:"memory_mib"`
		DiskBytes             int64  `json:"disk_bytes"`
		DefaultHTTPPort       int    `json:"default_http_port"`
		IdleSleepAfterSeconds int    `json:"idle_sleep_after_seconds"`
		AutoWake              bool   `json:"auto_wake"`
		PublicHTTP            bool   `json:"public_http"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.VMName == "" {
		req.VMName = req.VM
	}
	if !validSnapshotName(snapshotName) {
		writeError(w, http.StatusBadRequest, errors.New("snapshot name must contain only letters, numbers, dots, underscores, and hyphens"))
		return
	}
	if !validVMName(req.VMName) {
		writeError(w, http.StatusBadRequest, errors.New("vm_name must contain only lowercase letters, numbers, and hyphens"))
		return
	}
	vm, err := s.manager.RestoreSnapshot(r.Context(), snapshotName, store.CreateVMParams{
		Name:                  req.VMName,
		VCPUs:                 req.VCPUs,
		MemoryMiB:             req.MemoryMiB,
		DiskBytes:             req.DiskBytes,
		DefaultHTTPPort:       req.DefaultHTTPPort,
		IdleSleepAfterSeconds: req.IdleSleepAfterSeconds,
		AutoWake:              req.AutoWake,
		PublicHTTP:            req.PublicHTTP,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, firecracker.ErrAlreadyExists) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"vm": s.vmInfo(vm)})
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSnapshotName(name) {
		writeError(w, http.StatusBadRequest, errors.New("snapshot name must contain only letters, numbers, dots, underscores, and hyphens"))
		return
	}
	if err := s.manager.DeleteSnapshot(r.Context(), name); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListWireGuardPeers(w http.ResponseWriter, r *http.Request) {
	peers := make([]map[string]any, 0, len(s.cfg.WireGuard.Peers))
	for _, peer := range s.cfg.WireGuard.Peers {
		peers = append(peers, map[string]any{
			"name":        peer.Name,
			"allowed_ips": peer.AllowedIPs,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": peers})
}

func (s *Server) handleWireGuardPeerConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, peer := range s.cfg.WireGuard.Peers {
		if peer.Name != name {
			continue
		}
		cfg, err := wgconfig.PeerConfig(s.cfg, peer)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"config": cfg})
		return
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("%w: wireguard peer %q", store.ErrNotFound, name))
}

func (s *Server) reconcileProxy(r *http.Request) error {
	if s.proxy == nil {
		return nil
	}
	return s.proxy.Reconcile(r.Context())
}

type vmInfo struct {
	store.VM
	Hostname string            `json:"hostname"`
	SSH      string            `json:"ssh"`
	URLs     map[string]string `json:"urls"`
}

type routeInfo struct {
	store.Route
	Hostname string `json:"hostname"`
	URL      string `json:"url"`
}

func (s *Server) vmInfos(vms []store.VM) []vmInfo {
	infos := make([]vmInfo, 0, len(vms))
	for _, vm := range vms {
		infos = append(infos, s.vmInfo(vm))
	}
	return infos
}

func (s *Server) vmInfo(vm store.VM) vmInfo {
	hostname := s.defaultHostname(vm.Name)
	return vmInfo{
		VM:       vm,
		Hostname: hostname,
		SSH:      "ssh " + s.cfg.SSH.User + "@" + hostname,
		URLs: map[string]string{
			"default": s.publicURL(hostname),
		},
	}
}

func (s *Server) routeInfos(routes []store.Route) []routeInfo {
	infos := make([]routeInfo, 0, len(routes))
	for _, route := range routes {
		infos = append(infos, s.routeInfo(route))
	}
	return infos
}

func (s *Server) routeInfo(route store.Route) routeInfo {
	hostname := s.defaultHostname(route.Name)
	return routeInfo{
		Route:    route,
		Hostname: hostname,
		URL:      s.publicURL(hostname),
	}
}

func (s *Server) defaultHostname(name string) string {
	return name + "." + strings.TrimSuffix(s.cfg.BaseDomain, ".")
}

func (s *Server) publicURL(hostname string) string {
	if s.cfg.Caddy.HTTPSPort == 443 {
		return "https://" + hostname
	}
	return "https://" + net.JoinHostPort(hostname, fmt.Sprint(s.cfg.Caddy.HTTPSPort))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

var vmNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var snapshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validVMName(name string) bool {
	return vmNamePattern.MatchString(name)
}

func validSnapshotName(name string) bool {
	return snapshotNamePattern.MatchString(name)
}
