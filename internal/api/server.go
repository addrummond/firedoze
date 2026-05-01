package api

import (
	"encoding/json"
	"net/http"
	"time"

	"firedoze/internal/config"
)

const ShutdownTimeout = 5 * time.Second

type Server struct {
	cfg config.Config
	mux *http.ServeMux
}

func NewServer(cfg config.Config) http.Handler {
	server := &Server{
		cfg: cfg,
		mux: http.NewServeMux(),
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
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
