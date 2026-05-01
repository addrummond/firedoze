package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/store"
	"firedoze/internal/vmm"
)

const ShutdownTimeout = 5 * time.Second

type Server struct {
	cfg     config.Config
	manager *vmm.Manager
	proxy   Proxy
	mux     *http.ServeMux
}

type Proxy interface {
	Reconcile(context.Context) error
}

func NewServer(cfg config.Config, manager *vmm.Manager, proxy Proxy) http.Handler {
	server := &Server{
		cfg:     cfg,
		manager: manager,
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
	s.mux.HandleFunc("POST /vms/{name}/start", s.handleStartVM)
	s.mux.HandleFunc("POST /vms/{name}/stop", s.handleStopVM)
}

func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "firedoze",
		"commands": []map[string]string{
			{
				"method":      "GET",
				"path":        "/",
				"description": "show this help response",
				"curl":        "curl http://" + r.Host + "/",
			},
			{
				"method":      "GET",
				"path":        "/health",
				"description": "check daemon health",
				"curl":        "curl http://" + r.Host + "/health",
			},
			{
				"method":      "GET",
				"path":        "/config",
				"description": "show non-secret runtime configuration",
				"curl":        "curl http://" + r.Host + "/config",
			},
			{
				"method":      "GET",
				"path":        "/vms",
				"description": "list VMs",
				"curl":        "curl http://" + r.Host + "/vms",
			},
			{
				"method":      "POST",
				"path":        "/vms",
				"description": "create a VM record",
				"curl":        "curl -X POST http://" + r.Host + `/vms -d '{"name":"demo"}'`,
			},
			{
				"method":      "POST",
				"path":        "/vms/{name}/start",
				"description": "start a VM",
				"curl":        "curl -X POST http://" + r.Host + "/vms/demo/start",
			},
			{
				"method":      "POST",
				"path":        "/vms/{name}/stop",
				"description": "stop a VM",
				"curl":        "curl -X POST http://" + r.Host + "/vms/demo/stop",
			},
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
			"http_port": s.cfg.Caddy.HTTPPort,
		},
		"wireguard": map[string]any{
			"interface":   s.cfg.WireGuard.Interface,
			"listen_port": s.cfg.WireGuard.ListenPort,
			"address":     s.cfg.WireGuard.Address,
			"peers":       len(s.cfg.WireGuard.Peers),
		},
		"vm_network": map[string]any{
			"subnet": s.cfg.VMNetwork.Subnet,
		},
		"ssh": map[string]any{
			"user": s.cfg.SSH.User,
		},
		"firecracker": map[string]any{
			"binary_path":        s.cfg.Firecracker.BinaryPath,
			"base_kernel_path":   s.cfg.Firecracker.BaseKernelPath,
			"base_rootfs_path":   s.cfg.Firecracker.BaseRootfsPath,
			"default_vcpus":      s.cfg.Firecracker.DefaultVCPUs,
			"default_memory_mib": s.cfg.Firecracker.DefaultMemoryMiB,
			"default_disk_bytes": s.cfg.Firecracker.DefaultDiskBytes,
		},
	})
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.manager.ListVMs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": vms})
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string `json:"name"`
		VCPUs           int    `json:"vcpus"`
		MemoryMiB       int    `json:"memory_mib"`
		DiskBytes       int64  `json:"disk_bytes"`
		DefaultHTTPPort int    `json:"default_http_port"`
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
		Name:            req.Name,
		VCPUs:           req.VCPUs,
		MemoryMiB:       req.MemoryMiB,
		DiskBytes:       req.DiskBytes,
		DefaultHTTPPort: req.DefaultHTTPPort,
	})
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"vm": vm})
}

func (s *Server) handleStartVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	vm, err := s.manager.StartVM(r.Context(), name)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, vmm.ErrAlreadyRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	if err := s.reconcileProxy(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": vm})
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

func (s *Server) reconcileProxy(r *http.Request) error {
	if s.proxy == nil {
		return nil
	}
	return s.proxy.Reconcile(r.Context())
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

func validVMName(name string) bool {
	return vmNamePattern.MatchString(name)
}
