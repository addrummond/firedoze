package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"firedoze/internal/model"
)

type fakeGuestMemoryManager struct {
	privateIP string
	target    *int
	report    model.GuestMemoryReport
}

func (m *fakeGuestMemoryManager) RecordVMMemoryReportByPrivateIP(_ context.Context, privateIP string, target *int, report model.GuestMemoryReport) (model.MemoryHotplugUsage, error) {
	m.privateIP = privateIP
	m.target = target
	m.report = report
	return model.MemoryHotplugUsage{EffectiveMiB: report.TotalMiB}, nil
}

func TestGuestMemoryHintAcceptsReportWithoutTarget(t *testing.T) {
	manager := &fakeGuestMemoryManager{}
	handler := NewGuestServer(manager)
	req := httptest.NewRequest(http.MethodPost, "/memory-hint", strings.NewReader(`{"total_mib":512,"available_mib":384,"swap_total_mib":256,"swap_free_mib":200,"load1":0.25}`))
	req.RemoteAddr = "[fd00::3]:1234"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assertStatus(t, rec, http.StatusOK)
	if manager.privateIP != "fd00::3" {
		t.Fatalf("privateIP = %q, want fd00::3", manager.privateIP)
	}
	if manager.target != nil {
		t.Fatalf("target = %#v, want nil", manager.target)
	}
	if manager.report.TotalMiB != 512 || manager.report.AvailableMiB != 384 || manager.report.Load1 != 0.25 {
		t.Fatalf("report = %#v", manager.report)
	}
}
