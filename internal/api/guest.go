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
	RecordVMMemoryReportByPrivateIP(context.Context, string, *int, model.GuestMemoryReport) (model.MemoryHotplugUsage, error)
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
		TargetMiB    *int    `json:"target_mib,omitempty"`
		TotalMiB     int     `json:"total_mib,omitempty"`
		AvailableMiB int     `json:"available_mib,omitempty"`
		FreeMiB      int     `json:"free_mib,omitempty"`
		BuffersMiB   int     `json:"buffers_mib,omitempty"`
		CachedMiB    int     `json:"cached_mib,omitempty"`
		SwapTotalMiB int     `json:"swap_total_mib,omitempty"`
		SwapFreeMiB  int     `json:"swap_free_mib,omitempty"`
		Load1        float64 `json:"load1,omitempty"`
		Load5        float64 `json:"load5,omitempty"`
		Load15       float64 `json:"load15,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.TargetMiB != nil && *req.TargetMiB <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("target_mib must be positive"))
		return
	}
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	remoteIP = strings.Trim(remoteIP, "[]")
	report := model.GuestMemoryReport{
		TotalMiB:     req.TotalMiB,
		AvailableMiB: req.AvailableMiB,
		FreeMiB:      req.FreeMiB,
		BuffersMiB:   req.BuffersMiB,
		CachedMiB:    req.CachedMiB,
		SwapTotalMiB: req.SwapTotalMiB,
		SwapFreeMiB:  req.SwapFreeMiB,
		Load1:        req.Load1,
		Load5:        req.Load5,
		Load15:       req.Load15,
	}
	usage, err := s.manager.RecordVMMemoryReportByPrivateIP(r.Context(), remoteIP, req.TargetMiB, report)
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
