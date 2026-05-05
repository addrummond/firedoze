package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"firedoze/internal/firecracker"
	"firedoze/internal/model"
	"firedoze/internal/store"
)

type GuestMemoryManager interface {
	SetVMMemoryTargetByPrivateIP(context.Context, string, int) (model.MemoryHotplugUsage, error)
}

type GuestServer struct {
	manager GuestMemoryManager
	mux     *http.ServeMux
}

func NewGuestServer(manager GuestMemoryManager) *GuestServer {
	s := &GuestServer{
		manager: manager,
		mux:     http.NewServeMux(),
	}
	s.mux.HandleFunc("POST /memory-hint", s.handleMemoryHint)
	return s
}

func (s *GuestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *GuestServer) handleMemoryHint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetMiB int `json:"target_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.TargetMiB <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("target_mib must be positive"))
		return
	}
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	remoteIP = strings.Trim(remoteIP, "[]")
	usage, err := s.manager.SetVMMemoryTargetByPrivateIP(r.Context(), remoteIP, req.TargetMiB)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusForbidden, err)
		case errors.Is(err, firecracker.ErrNotRunning):
			writeError(w, http.StatusConflict, err)
		default:
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory": usage})
}
