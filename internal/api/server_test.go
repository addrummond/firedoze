package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"firedoze/internal/model"
	"firedoze/internal/store"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type fakeManager struct {
	warmBaseImageMetadataFunc func(context.Context) (firecracker.BaseImageMetadata, error)
	listVMsMatchingFunc       func(context.Context, []string) ([]store.VM, error)
	listVMResourceUsageFunc   func(context.Context, []string) ([]model.VMResourceUsage, error)
	getVMFunc                 func(context.Context, string) (store.VM, error)
	getVMByNameFunc           func(context.Context, string) (store.VM, error)
	vmResourceUsageFunc       func(context.Context, string) (model.VMResourceUsage, error)
	createVMFunc              func(context.Context, store.CreateVMParams) (store.VM, error)
	updateVMFunc              func(context.Context, string, store.UpdateVMParams) (store.VM, error)
	deleteVMFunc              func(context.Context, string) error
	startVMFunc               func(context.Context, string) (store.VM, error)
	stopVMFunc                func(context.Context, string) error
	sleepVMFunc               func(context.Context, string) (store.VM, error)
	rebootVMFunc              func(context.Context, string) (store.VM, error)
	listSnapshotsFunc         func(context.Context) ([]store.Snapshot, error)
	getSnapshotFunc           func(context.Context, string) (store.Snapshot, error)
	saveSnapshotFunc          func(context.Context, store.CreateSnapshotParams) (store.Snapshot, error)
	restoreSnapshotFunc       func(context.Context, string, store.CreateVMParams) (store.VM, error)
	exportSnapshotFunc        func(context.Context, string, io.Writer) error
	importSnapshotFunc        func(context.Context, string, io.Reader) (store.Snapshot, error)
	deleteSnapshotFunc        func(context.Context, string) error
}

func (m *fakeManager) WarmBaseImageMetadata(ctx context.Context) (firecracker.BaseImageMetadata, error) {
	if m.warmBaseImageMetadataFunc != nil {
		return m.warmBaseImageMetadataFunc(ctx)
	}
	return firecracker.BaseImageMetadata{}, nil
}

func (m *fakeManager) ListVMsMatching(ctx context.Context, patterns []string) ([]store.VM, error) {
	if m.listVMsMatchingFunc != nil {
		return m.listVMsMatchingFunc(ctx, patterns)
	}
	return nil, nil
}

func (m *fakeManager) ListVMResourceUsage(ctx context.Context, patterns []string) ([]model.VMResourceUsage, error) {
	if m.listVMResourceUsageFunc != nil {
		return m.listVMResourceUsageFunc(ctx, patterns)
	}
	return nil, nil
}

func (m *fakeManager) GetVM(ctx context.Context, name string) (store.VM, error) {
	if m.getVMFunc != nil {
		return m.getVMFunc(ctx, name)
	}
	return testVM(name), nil
}

func (m *fakeManager) GetVMByName(ctx context.Context, name string) (store.VM, error) {
	if m.getVMByNameFunc != nil {
		return m.getVMByNameFunc(ctx, name)
	}
	return testVM(name), nil
}

func (m *fakeManager) VMResourceUsage(ctx context.Context, name string) (model.VMResourceUsage, error) {
	if m.vmResourceUsageFunc != nil {
		return m.vmResourceUsageFunc(ctx, name)
	}
	return model.VMResourceUsage{Name: name, State: "running", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128}, nil
}

func (m *fakeManager) CreateVM(ctx context.Context, params store.CreateVMParams) (store.VM, error) {
	if m.createVMFunc != nil {
		return m.createVMFunc(ctx, params)
	}
	return vmFromParams(params), nil
}

func (m *fakeManager) UpdateVM(ctx context.Context, name string, params store.UpdateVMParams) (store.VM, error) {
	if m.updateVMFunc != nil {
		return m.updateVMFunc(ctx, name, params)
	}
	vm := testVM(name)
	if params.DefaultHTTPPort != nil {
		vm.DefaultHTTPPort = *params.DefaultHTTPPort
	}
	if params.IdleSleepAfterSeconds != nil {
		vm.IdleSleepAfterSeconds = *params.IdleSleepAfterSeconds
	}
	if params.AutoWake != nil {
		vm.AutoWake = *params.AutoWake
	}
	if params.PublicHTTP != nil {
		vm.PublicHTTP = *params.PublicHTTP
	}
	return vm, nil
}

func (m *fakeManager) DeleteVM(ctx context.Context, name string) error {
	if m.deleteVMFunc != nil {
		return m.deleteVMFunc(ctx, name)
	}
	return nil
}

func (m *fakeManager) StartVM(ctx context.Context, name string) (store.VM, error) {
	if m.startVMFunc != nil {
		return m.startVMFunc(ctx, name)
	}
	vm := testVM(name)
	vm.State = "running"
	return vm, nil
}

func (m *fakeManager) StopVM(ctx context.Context, name string) error {
	if m.stopVMFunc != nil {
		return m.stopVMFunc(ctx, name)
	}
	return nil
}

func (m *fakeManager) SleepVM(ctx context.Context, name string) (store.VM, error) {
	if m.sleepVMFunc != nil {
		return m.sleepVMFunc(ctx, name)
	}
	vm := testVM(name)
	vm.State = "sleeping"
	return vm, nil
}

func (m *fakeManager) RebootVM(ctx context.Context, name string) (store.VM, error) {
	if m.rebootVMFunc != nil {
		return m.rebootVMFunc(ctx, name)
	}
	vm := testVM(name)
	vm.State = "running"
	return vm, nil
}

func (m *fakeManager) ListSnapshots(ctx context.Context) ([]store.Snapshot, error) {
	if m.listSnapshotsFunc != nil {
		return m.listSnapshotsFunc(ctx)
	}
	return nil, nil
}

func (m *fakeManager) GetSnapshot(ctx context.Context, name string) (store.Snapshot, error) {
	if m.getSnapshotFunc != nil {
		return m.getSnapshotFunc(ctx, name)
	}
	return testSnapshot(name), nil
}

func (m *fakeManager) SaveSnapshot(ctx context.Context, params store.CreateSnapshotParams) (store.Snapshot, error) {
	if m.saveSnapshotFunc != nil {
		return m.saveSnapshotFunc(ctx, params)
	}
	return testSnapshot(params.Name), nil
}

func (m *fakeManager) RestoreSnapshot(ctx context.Context, name string, params store.CreateVMParams) (store.VM, error) {
	if m.restoreSnapshotFunc != nil {
		return m.restoreSnapshotFunc(ctx, name, params)
	}
	return vmFromParams(params), nil
}

func (m *fakeManager) ExportSnapshot(ctx context.Context, name string, w io.Writer) error {
	if m.exportSnapshotFunc != nil {
		return m.exportSnapshotFunc(ctx, name, w)
	}
	_, err := io.WriteString(w, "snapshot bundle")
	return err
}

func (m *fakeManager) ImportSnapshot(ctx context.Context, name string, r io.Reader) (store.Snapshot, error) {
	if m.importSnapshotFunc != nil {
		return m.importSnapshotFunc(ctx, name, r)
	}
	return testSnapshot(name), nil
}

func (m *fakeManager) DeleteSnapshot(ctx context.Context, name string) error {
	if m.deleteSnapshotFunc != nil {
		return m.deleteSnapshotFunc(ctx, name)
	}
	return nil
}

type fakeProxy struct {
	calls int
	err   error
}

func (p *fakeProxy) Reconcile(context.Context) error {
	p.calls++
	return p.err
}

type fakeRouteAuth struct {
	host string
	ttl  time.Duration
	err  error
}

func (a *fakeRouteAuth) SignedURL(host string, ttl time.Duration) (string, error) {
	a.host = host
	a.ttl = ttl
	if a.err != nil {
		return "", a.err
	}
	return "https://" + host + "/_firedoze/auth?token=test", nil
}

func TestBasicEndpointsAndConfigHideSecrets(t *testing.T) {
	cfg := testConfig(t)
	cfg.WireGuard.Peers = []config.WGPeer{
		{Name: "alice", PublicKey: "public-key", AllowedIPs: []string{"fd00::2/128"}},
	}
	manager := &fakeManager{}
	proxy := &fakeProxy{}
	handler := NewServer(cfg, manager, testStore(t), proxy)

	rec := request(t, handler, http.MethodGet, "/health", nil)
	assertStatus(t, rec, http.StatusOK)
	var health map[string]string
	decode(t, rec, &health)
	if health["status"] != "ok" {
		t.Fatalf("status = %q, want ok", health["status"])
	}

	rec = request(t, handler, http.MethodGet, "/config", nil)
	assertStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, secret := range []string{"private_key", "private_key_file", "public-key"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response exposed %q: %s", secret, body)
		}
	}
	var configResponse map[string]any
	decode(t, rec, &configResponse)
	if configResponse["base_domain"] != cfg.BaseDomain {
		t.Fatalf("base_domain = %v, want %q", configResponse["base_domain"], cfg.BaseDomain)
	}

	rec = request(t, handler, http.MethodGet, "/wireguard/peers", nil)
	assertStatus(t, rec, http.StatusOK)
	body = rec.Body.String()
	if strings.Contains(body, "public-key") {
		t.Fatalf("peer list exposed public key: %s", body)
	}
	if !strings.Contains(body, "fd00::2/128") {
		t.Fatalf("peer list did not include allowed IP: %s", body)
	}
}

func TestHelpWarmBaseImageAndGetVM(t *testing.T) {
	manager := &fakeManager{
		warmBaseImageMetadataFunc: func(context.Context) (firecracker.BaseImageMetadata, error) {
			return firecracker.BaseImageMetadata{
				Rootfs: firecracker.ArtifactMetadata{Path: "/images/rootfs.ext4", SHA256: "root-sha"},
				Kernel: firecracker.ArtifactMetadata{Path: "/images/vmlinux.bin", SHA256: "kernel-sha"},
			}, nil
		},
		getVMFunc: func(_ context.Context, name string) (store.VM, error) {
			if name == "missing" {
				return store.VM{}, store.ErrNotFound
			}
			return testVM(name), nil
		},
	}
	handler := NewServer(testConfig(t), manager, testStore(t), nil)

	rec := request(t, handler, http.MethodGet, "/", nil)
	assertStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"service": "firedoze"`) || !strings.Contains(body, "POST /vms/{uuid}/reboot") {
		t.Fatalf("help response missing expected commands: %s", body)
	}

	rec = request(t, handler, http.MethodPost, "/base-image/warmup", nil)
	assertStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, "root-sha") || !strings.Contains(body, "kernel-sha") {
		t.Fatalf("warmup response missing metadata: %s", body)
	}

	rec = request(t, handler, http.MethodGet, "/vms/dev", nil)
	assertStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"name": "dev"`) {
		t.Fatalf("get vm response missing VM: %s", body)
	}

	rec = request(t, handler, http.MethodGet, "/vms/missing", nil)
	assertStatus(t, rec, http.StatusNotFound)
}

func TestWarmBaseImageError(t *testing.T) {
	manager := &fakeManager{
		warmBaseImageMetadataFunc: func(context.Context) (firecracker.BaseImageMetadata, error) {
			return firecracker.BaseImageMetadata{}, errors.New("metadata failed")
		},
	}
	handler := NewServer(testConfig(t), manager, testStore(t), nil)

	rec := request(t, handler, http.MethodPost, "/base-image/warmup", nil)
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestResourceUsageEndpoints(t *testing.T) {
	var patterns []string
	manager := &fakeManager{
		listVMResourceUsageFunc: func(_ context.Context, got []string) ([]model.VMResourceUsage, error) {
			patterns = append([]string(nil), got...)
			return []model.VMResourceUsage{{
				Name:         "demo",
				State:        "running",
				VCPUs:        2,
				MemoryMinMiB: 512, MemoryMaxMiB: 512,
				Process: &model.ProcessResourceUsage{PID: 123, RSSBytes: 64 << 20},
			}}, nil
		},
		vmResourceUsageFunc: func(_ context.Context, name string) (model.VMResourceUsage, error) {
			if name == "missing" {
				return model.VMResourceUsage{}, store.ErrNotFound
			}
			return model.VMResourceUsage{Name: name, State: "stopped"}, nil
		},
	}
	handler := NewServer(testConfig(t), manager, testStore(t), &fakeProxy{})

	rec := request(t, handler, http.MethodGet, "/usage?name=demo*", nil)
	assertStatus(t, rec, http.StatusOK)
	if !reflect.DeepEqual(patterns, []string{"demo*"}) {
		t.Fatalf("patterns = %#v", patterns)
	}
	var listOut struct {
		VMs []model.VMResourceUsage `json:"vms"`
	}
	decode(t, rec, &listOut)
	if len(listOut.VMs) != 1 || listOut.VMs[0].Name != "demo" || listOut.VMs[0].Process.RSSBytes != 64<<20 {
		t.Fatalf("usage list = %#v", listOut)
	}

	rec = request(t, handler, http.MethodGet, "/vms/demo/usage", nil)
	assertStatus(t, rec, http.StatusOK)
	var getOut struct {
		Usage model.VMResourceUsage `json:"usage"`
	}
	decode(t, rec, &getOut)
	if getOut.Usage.Name != "demo" {
		t.Fatalf("usage = %#v", getOut.Usage)
	}

	rec = request(t, handler, http.MethodGet, "/vms/missing/usage", nil)
	assertStatus(t, rec, http.StatusNotFound)
}

func TestWireGuardPeerConfigReturnsClientImportConfig(t *testing.T) {
	cfg := testConfig(t)
	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.WireGuard.PrivateKeyFile, []byte(privateKey.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clientKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg.WireGuard.Peers = []config.WGPeer{
		{Name: "alice", PublicKey: clientKey.PublicKey().String(), AllowedIPs: []string{"fd00::2/128"}},
	}
	handler := NewServer(cfg, &fakeManager{}, testStore(t), nil)

	rec := request(t, handler, http.MethodGet, "/wireguard/peers/alice/config", nil)
	assertStatus(t, rec, http.StatusOK)
	var response map[string]string
	decode(t, rec, &response)
	template := response["config"]
	if !strings.Contains(template, "# Firedoze client import config.") || !strings.Contains(template, `client_public_key = "`+clientKey.PublicKey().String()+`"`) {
		t.Fatalf("client import config missing expected values: %s", template)
	}
	if strings.Contains(template, privateKey.String()) || strings.Contains(template, "private_key") || strings.Contains(template, "<client-private-key>") {
		t.Fatalf("client import config exposed or requested a private key: %s", template)
	}

	rec = request(t, handler, http.MethodGet, "/wireguard/peers/bob/config", nil)
	assertStatus(t, rec, http.StatusNotFound)
}

func TestListVMsPassesNamePatternsAndFormatsURLs(t *testing.T) {
	var gotPatterns []string
	manager := &fakeManager{
		listVMsMatchingFunc: func(_ context.Context, patterns []string) ([]store.VM, error) {
			gotPatterns = append([]string(nil), patterns...)
			return []store.VM{testVM("alpha")}, nil
		},
	}
	cfg := testConfig(t)
	cfg.Caddy.HTTPSPort = 8443
	handler := NewServer(cfg, manager, testStore(t), nil)

	rec := request(t, handler, http.MethodGet, "/vms?name=a*&name=b?", nil)
	assertStatus(t, rec, http.StatusOK)
	if !reflect.DeepEqual(gotPatterns, []string{"a*", "b?"}) {
		t.Fatalf("name patterns = %#v", gotPatterns)
	}
	if body := rec.Body.String(); !strings.Contains(body, "https://alpha.dev.test:8443") {
		t.Fatalf("response did not include formatted URL: %s", body)
	}
}

func TestListVMsManagerError(t *testing.T) {
	manager := &fakeManager{
		listVMsMatchingFunc: func(context.Context, []string) ([]store.VM, error) {
			return nil, errors.New("list failed")
		},
	}
	handler := NewServer(testConfig(t), manager, testStore(t), nil)

	rec := request(t, handler, http.MethodGet, "/vms", nil)
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestCreateAndUpdateVMUseManagerParamsAndReconcileProxy(t *testing.T) {
	autoWake := true
	var createParams store.CreateVMParams
	var updateName string
	var updateParams store.UpdateVMParams
	proxy := &fakeProxy{}
	manager := &fakeManager{
		createVMFunc: func(_ context.Context, params store.CreateVMParams) (store.VM, error) {
			createParams = params
			return vmFromParams(params), nil
		},
		updateVMFunc: func(_ context.Context, name string, params store.UpdateVMParams) (store.VM, error) {
			updateName = name
			updateParams = params
			return (&fakeManager{}).UpdateVM(context.Background(), name, params)
		},
	}
	handler := NewServer(testConfig(t), manager, testStore(t), proxy)

	rec := request(t, handler, http.MethodPost, "/vms", map[string]any{
		"name":                     "dev-a",
		"vcpus":                    2,
		"memory_min_mib":           256,
		"memory_max_mib":           1024,
		"disk_bytes":               42,
		"default_http_port":        3000,
		"idle_sleep_after_seconds": 12,
		"auto_wake":                autoWake,
		"public_http":              true,
	})
	assertStatus(t, rec, http.StatusCreated)
	if createParams.Name != "dev-a" || createParams.VCPUs != 2 || createParams.MemoryMinMiB != 256 || createParams.MemoryMaxMiB != 1024 || createParams.DiskBytes != 42 || createParams.DefaultHTTPPort != 3000 {
		t.Fatalf("unexpected create params: %#v", createParams)
	}
	if !createParams.AutoWakeSet || !createParams.AutoWake || !createParams.PublicHTTP {
		t.Fatalf("unexpected create booleans: %#v", createParams)
	}
	if proxy.calls != 1 {
		t.Fatalf("proxy reconcile calls = %d, want 1", proxy.calls)
	}

	rec = request(t, handler, http.MethodPatch, "/vms/dev-a-uuid/settings", map[string]any{
		"default_http_port":        8088,
		"idle_sleep_after_seconds": 60,
		"auto_wake":                false,
		"public_http":              false,
	})
	assertStatus(t, rec, http.StatusOK)
	if updateName != "dev-a-uuid" {
		t.Fatalf("update uuid = %q", updateName)
	}
	if updateParams.DefaultHTTPPort == nil || *updateParams.DefaultHTTPPort != 8088 {
		t.Fatalf("default_http_port params = %#v", updateParams.DefaultHTTPPort)
	}
	if updateParams.AutoWake == nil || *updateParams.AutoWake {
		t.Fatalf("auto_wake params = %#v", updateParams.AutoWake)
	}
	if proxy.calls != 2 {
		t.Fatalf("proxy reconcile calls = %d, want 2", proxy.calls)
	}
}

func TestVMValidationAndLifecycleStatusCodes(t *testing.T) {
	handler := NewServer(testConfig(t), &fakeManager{}, testStore(t), nil)

	rec := request(t, handler, http.MethodPost, "/vms", map[string]any{"name": "Bad_Name"})
	assertStatus(t, rec, http.StatusBadRequest)

	rec = request(t, handler, http.MethodPost, "/vms", map[string]any{"name": "550e8400-e29b-41d4-a716-446655440000"})
	assertStatus(t, rec, http.StatusBadRequest)

	rec = request(t, handler, http.MethodPost, "/vms", "{")
	assertStatus(t, rec, http.StatusBadRequest)

	rec = request(t, handler, http.MethodPatch, "/vms/dev/settings", "{")
	assertStatus(t, rec, http.StatusBadRequest)

	tests := []struct {
		name   string
		method string
		path   string
		setup  func(*fakeManager)
		want   int
	}{
		{
			name: "start not found", method: http.MethodPost, path: "/vms/missing/start", want: http.StatusNotFound,
			setup: func(m *fakeManager) {
				m.startVMFunc = func(context.Context, string) (store.VM, error) { return store.VM{}, store.ErrNotFound }
			},
		},
		{
			name: "start already running", method: http.MethodPost, path: "/vms/dev/start", want: http.StatusConflict,
			setup: func(m *fakeManager) {
				m.startVMFunc = func(context.Context, string) (store.VM, error) { return store.VM{}, firecracker.ErrAlreadyRunning }
			},
		},
		{
			name: "sleep not running", method: http.MethodPost, path: "/vms/dev/sleep", want: http.StatusConflict,
			setup: func(m *fakeManager) {
				m.sleepVMFunc = func(context.Context, string) (store.VM, error) { return store.VM{}, firecracker.ErrNotRunning }
			},
		},
		{
			name: "sleep not found", method: http.MethodPost, path: "/vms/missing/sleep", want: http.StatusNotFound,
			setup: func(m *fakeManager) {
				m.sleepVMFunc = func(context.Context, string) (store.VM, error) { return store.VM{}, store.ErrNotFound }
			},
		},
		{
			name: "delete not found", method: http.MethodDelete, path: "/vms/missing", want: http.StatusNotFound,
			setup: func(m *fakeManager) {
				m.deleteVMFunc = func(context.Context, string) error { return store.ErrNotFound }
			},
		},
		{
			name: "stop not found", method: http.MethodPost, path: "/vms/missing/stop", want: http.StatusNotFound,
			setup: func(m *fakeManager) {
				m.stopVMFunc = func(context.Context, string) error { return store.ErrNotFound }
			},
		},
		{
			name: "reboot not found", method: http.MethodPost, path: "/vms/missing/reboot", want: http.StatusNotFound,
			setup: func(m *fakeManager) {
				m.rebootVMFunc = func(context.Context, string) (store.VM, error) { return store.VM{}, store.ErrNotFound }
			},
		},
		{
			name: "reboot already running", method: http.MethodPost, path: "/vms/dev/reboot", want: http.StatusConflict,
			setup: func(m *fakeManager) {
				m.rebootVMFunc = func(context.Context, string) (store.VM, error) { return store.VM{}, firecracker.ErrAlreadyRunning }
			},
		},
		{
			name: "stop success", method: http.MethodPost, path: "/vms/dev/stop", want: http.StatusOK,
		},
		{
			name: "reboot success", method: http.MethodPost, path: "/vms/dev/reboot", want: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &fakeManager{}
			if tt.setup != nil {
				tt.setup(manager)
			}
			proxy := &fakeProxy{}
			rec := request(t, NewServer(testConfig(t), manager, testStore(t), proxy), tt.method, tt.path, nil)
			assertStatus(t, rec, tt.want)
			if tt.want >= 200 && tt.want < 300 && proxy.calls != 1 {
				t.Fatalf("proxy reconcile calls = %d, want 1", proxy.calls)
			}
		})
	}
}

func TestCreateUpdateAndProxyErrorMappings(t *testing.T) {
	manager := &fakeManager{
		createVMFunc: func(context.Context, store.CreateVMParams) (store.VM, error) {
			return store.VM{}, firecracker.ErrAlreadyExists
		},
		updateVMFunc: func(context.Context, string, store.UpdateVMParams) (store.VM, error) {
			return store.VM{}, errors.New("bad settings")
		},
	}
	handler := NewServer(testConfig(t), manager, testStore(t), nil)

	rec := request(t, handler, http.MethodPost, "/vms", map[string]any{"name": "dev"})
	assertStatus(t, rec, http.StatusConflict)

	rec = request(t, handler, http.MethodPatch, "/vms/dev/settings", map[string]any{"default_http_port": 70000})
	assertStatus(t, rec, http.StatusBadRequest)

	manager.updateVMFunc = func(context.Context, string, store.UpdateVMParams) (store.VM, error) {
		return store.VM{}, store.ErrNotFound
	}
	rec = request(t, handler, http.MethodPatch, "/vms/missing/settings", map[string]any{"default_http_port": 8080})
	assertStatus(t, rec, http.StatusNotFound)
}

func TestRoutesUseStoreValidationAndProxyReconcile(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateVM(context.Background(), store.CreateVMParams{
		Name: "dev", PrivateIP: "fd00::2", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1, DefaultHTTPPort: 8080,
	}); err != nil {
		t.Fatal(err)
	}
	dev, err := st.GetVMByName(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &fakeProxy{}
	handler := NewServer(testConfig(t), &fakeManager{}, st, proxy)

	rec := request(t, handler, http.MethodPost, "/routes", map[string]any{
		"name":    "dev",
		"vm_uuid": dev.UUID,
		"port":    8080,
	})
	assertStatus(t, rec, http.StatusConflict)

	rec = request(t, handler, http.MethodPost, "/routes", map[string]any{
		"name":    "api",
		"vm_uuid": dev.UUID,
		"port":    70000,
	})
	assertStatus(t, rec, http.StatusBadRequest)

	rec = request(t, handler, http.MethodPost, "/routes", map[string]any{
		"name":    "api.preview",
		"vm_uuid": dev.UUID,
		"port":    9000,
	})
	assertStatus(t, rec, http.StatusCreated)
	if proxy.calls != 1 {
		t.Fatalf("proxy reconcile calls = %d, want 1", proxy.calls)
	}
	if body := rec.Body.String(); !strings.Contains(body, "https://api.preview.dev.test") {
		t.Fatalf("route response did not include URL: %s", body)
	}

	rec = request(t, handler, http.MethodGet, "/routes", nil)
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"name": "api.preview"`) {
		t.Fatalf("route list did not include route: %s", rec.Body.String())
	}

	rec = request(t, handler, http.MethodDelete, "/routes/api.preview", nil)
	assertStatus(t, rec, http.StatusOK)
	if proxy.calls != 2 {
		t.Fatalf("proxy reconcile calls = %d, want 2", proxy.calls)
	}

	rec = request(t, handler, http.MethodDelete, "/routes/api.preview", nil)
	assertStatus(t, rec, http.StatusNotFound)
}

func TestRouteProtectionAndSignedURLHandlers(t *testing.T) {
	st := testStore(t)
	proxy := &fakeProxy{}
	auth := &fakeRouteAuth{}
	handler := NewServerWithRouteAuth(testConfig(t), &fakeManager{}, st, proxy, auth)

	rec := request(t, handler, http.MethodPost, "/route-protections", map[string]any{
		"hostname": "Secret.Dev.Test.",
	})
	assertStatus(t, rec, http.StatusOK)
	if proxy.calls != 1 {
		t.Fatalf("proxy reconcile calls = %d, want 1", proxy.calls)
	}
	protected, err := st.IsRouteHostnameProtected(context.Background(), "secret.dev.test")
	if err != nil {
		t.Fatal(err)
	}
	if !protected {
		t.Fatal("hostname was not protected")
	}

	rec = request(t, handler, http.MethodPost, "/route-auth/signed-url", map[string]any{
		"hostname": "secret.dev.test",
	})
	assertStatus(t, rec, http.StatusOK)
	if auth.host != "secret.dev.test" || auth.ttl != 24*time.Hour {
		t.Fatalf("signed URL args = %q/%s", auth.host, auth.ttl)
	}
	if !strings.Contains(rec.Body.String(), "https://secret.dev.test/_firedoze/auth") {
		t.Fatalf("signed URL response = %s", rec.Body.String())
	}

	rec = request(t, handler, http.MethodPost, "/route-auth/signed-url", map[string]any{
		"hostname":    "secret.dev.test",
		"ttl_seconds": 60,
	})
	assertStatus(t, rec, http.StatusOK)
	if auth.ttl != time.Minute {
		t.Fatalf("signed URL ttl = %s, want 1m", auth.ttl)
	}

	rec = request(t, handler, http.MethodDelete, "/route-protections/secret.dev.test", nil)
	assertStatus(t, rec, http.StatusOK)
	if proxy.calls != 2 {
		t.Fatalf("proxy reconcile calls = %d, want 2", proxy.calls)
	}
	protected, err = st.IsRouteHostnameProtected(context.Background(), "secret.dev.test")
	if err != nil {
		t.Fatal(err)
	}
	if protected {
		t.Fatal("hostname was still protected")
	}

	rec = request(t, handler, http.MethodPost, "/route-protections", map[string]any{
		"hostname": "outside.example.test",
	})
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestSnapshotHandlersValidateMapErrorsAndReconcileRestore(t *testing.T) {
	var saveParams store.CreateSnapshotParams
	var restoreName string
	var restoreParams store.CreateVMParams
	proxy := &fakeProxy{}
	manager := &fakeManager{
		saveSnapshotFunc: func(_ context.Context, params store.CreateSnapshotParams) (store.Snapshot, error) {
			saveParams = params
			return testSnapshot(params.Name), nil
		},
		restoreSnapshotFunc: func(_ context.Context, name string, params store.CreateVMParams) (store.VM, error) {
			restoreName = name
			restoreParams = params
			return vmFromParams(params), nil
		},
	}
	handler := NewServer(testConfig(t), manager, testStore(t), proxy)

	manager.listSnapshotsFunc = func(context.Context) ([]store.Snapshot, error) {
		return []store.Snapshot{testSnapshot("snap.0")}, nil
	}
	rec := request(t, handler, http.MethodGet, "/snapshots", nil)
	assertStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"name": "snap.0"`) {
		t.Fatalf("snapshot list missing snapshot: %s", body)
	}

	rec = request(t, handler, http.MethodPost, "/snapshots", map[string]any{"name": "snap.1", "vm_uuid": "dev-uuid"})
	assertStatus(t, rec, http.StatusCreated)
	if saveParams.Name != "snap.1" || saveParams.SourceVMUUID != "dev-uuid" {
		t.Fatalf("save params = %#v", saveParams)
	}
	if proxy.calls != 0 {
		t.Fatalf("snapshot create reconciled proxy unexpectedly")
	}

	rec = request(t, handler, http.MethodPost, "/snapshots/bad%20name/restore", map[string]any{"vm_name": "copy"})
	assertStatus(t, rec, http.StatusBadRequest)

	rec = request(t, handler, http.MethodPost, "/snapshots/snap.1/restore", map[string]any{
		"vm_name":                  "copy",
		"vcpus":                    3,
		"memory_min_mib":           512,
		"memory_max_mib":           2048,
		"default_http_port":        9090,
		"idle_sleep_after_seconds": 120,
		"auto_wake":                true,
		"public_http":              true,
	})
	assertStatus(t, rec, http.StatusCreated)
	if restoreName != "snap.1" || restoreParams.Name != "copy" || restoreParams.VCPUs != 3 || restoreParams.MemoryMinMiB != 512 || restoreParams.MemoryMaxMiB != 2048 || restoreParams.DefaultHTTPPort != 9090 {
		t.Fatalf("restore params = %q %#v", restoreName, restoreParams)
	}
	if !restoreParams.AutoWakeSet || !restoreParams.AutoWake || !restoreParams.PublicHTTP {
		t.Fatalf("restore booleans = %#v", restoreParams)
	}
	if proxy.calls != 1 {
		t.Fatalf("restore proxy reconcile calls = %d, want 1", proxy.calls)
	}

	manager.restoreSnapshotFunc = func(context.Context, string, store.CreateVMParams) (store.VM, error) {
		return store.VM{}, firecracker.ErrAlreadyExists
	}
	rec = request(t, handler, http.MethodPost, "/snapshots/snap.1/restore", map[string]any{"vm_name": "copy"})
	assertStatus(t, rec, http.StatusConflict)

	manager.restoreSnapshotFunc = func(context.Context, string, store.CreateVMParams) (store.VM, error) {
		return store.VM{}, store.ErrNotFound
	}
	rec = request(t, handler, http.MethodPost, "/snapshots/missing/restore", map[string]any{"vm_name": "copy"})
	assertStatus(t, rec, http.StatusNotFound)

	manager.saveSnapshotFunc = func(context.Context, store.CreateSnapshotParams) (store.Snapshot, error) {
		return store.Snapshot{}, firecracker.ErrNotStopped
	}
	rec = request(t, handler, http.MethodPost, "/snapshots", map[string]any{"name": "snap.2", "vm_uuid": "dev-uuid"})
	assertStatus(t, rec, http.StatusConflict)

	rec = request(t, handler, http.MethodPost, "/snapshots", "{")
	assertStatus(t, rec, http.StatusBadRequest)

	rec = request(t, handler, http.MethodPost, "/snapshots", map[string]any{"name": "bad name", "vm_uuid": "dev-uuid"})
	assertStatus(t, rec, http.StatusBadRequest)

	manager.getSnapshotFunc = func(context.Context, string) (store.Snapshot, error) {
		return store.Snapshot{}, store.ErrNotFound
	}
	rec = request(t, handler, http.MethodGet, "/snapshots/missing", nil)
	assertStatus(t, rec, http.StatusNotFound)

	manager.deleteSnapshotFunc = nil
	rec = request(t, handler, http.MethodDelete, "/snapshots/snap.1", nil)
	assertStatus(t, rec, http.StatusOK)

	manager.deleteSnapshotFunc = func(context.Context, string) error { return store.ErrNotFound }
	rec = request(t, handler, http.MethodDelete, "/snapshots/missing", nil)
	assertStatus(t, rec, http.StatusNotFound)

	rec = request(t, handler, http.MethodDelete, "/snapshots/bad%20name", nil)
	assertStatus(t, rec, http.StatusBadRequest)

	var exportName string
	manager.exportSnapshotFunc = func(_ context.Context, name string, w io.Writer) error {
		exportName = name
		_, err := io.WriteString(w, "bundle")
		return err
	}
	rec = request(t, handler, http.MethodGet, "/snapshots/snap.1/export", nil)
	assertStatus(t, rec, http.StatusOK)
	if exportName != "snap.1" || rec.Body.String() != "bundle" {
		t.Fatalf("export = %q %q", exportName, rec.Body.String())
	}

	var importName, importBody string
	manager.importSnapshotFunc = func(_ context.Context, name string, r io.Reader) (store.Snapshot, error) {
		data, err := io.ReadAll(r)
		if err != nil {
			return store.Snapshot{}, err
		}
		importName = name
		importBody = string(data)
		return testSnapshot(name), nil
	}
	rec = request(t, handler, http.MethodPost, "/snapshots/imported/import", "bundle")
	assertStatus(t, rec, http.StatusCreated)
	if importName != "imported" || importBody != "bundle" {
		t.Fatalf("import = %q %q", importName, importBody)
	}

	manager.importSnapshotFunc = func(context.Context, string, io.Reader) (store.Snapshot, error) {
		return store.Snapshot{}, firecracker.ErrInvalidSnapshotBundle
	}
	rec = request(t, handler, http.MethodPost, "/snapshots/imported/import", "not-a-bundle")
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestProxyReconcileErrorReturnsServerError(t *testing.T) {
	proxy := &fakeProxy{err: errors.New("reconcile failed")}
	handler := NewServer(testConfig(t), &fakeManager{}, testStore(t), proxy)

	rec := request(t, handler, http.MethodPost, "/vms", map[string]any{"name": "dev"})
	assertStatus(t, rec, http.StatusInternalServerError)
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.BaseDomain = "dev.test"
	cfg.Caddy.HTTPSPort = 443
	cfg.WireGuard.Address = "fd00::1/64"
	cfg.WireGuard.Endpoint = "firedoze.test:51820"
	cfg.WireGuard.PrivateKeyFile = filepath.Join(t.TempDir(), "wg.key")
	cfg.VMNetwork.Subnet = "fd01::/64"
	return cfg
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func testVM(name string) store.VM {
	return store.VM{
		UUID:                  name + "-uuid",
		Name:                  name,
		State:                 "stopped",
		PrivateIP:             "fd01::2",
		VCPUs:                 1,
		MemoryMinMiB:          128,
		MemoryMaxMiB:          128,
		DiskBytes:             512,
		DefaultHTTPPort:       8080,
		IdleSleepAfterSeconds: 60,
		PublicHTTP:            true,
	}
}

func vmFromParams(params store.CreateVMParams) store.VM {
	vm := testVM(params.Name)
	vm.VCPUs = params.VCPUs
	vm.MemoryMinMiB = params.MemoryMinMiB
	vm.MemoryMaxMiB = params.MemoryMaxMiB
	vm.DiskBytes = params.DiskBytes
	vm.DefaultHTTPPort = params.DefaultHTTPPort
	vm.IdleSleepAfterSeconds = params.IdleSleepAfterSeconds
	vm.AutoWake = params.AutoWake
	vm.PublicHTTP = params.PublicHTTP
	return vm
}

func testSnapshot(name string) store.Snapshot {
	return store.Snapshot{
		Name:         name,
		SourceVMUUID: "dev-uuid",
		SourceVM:     "dev",
		StatePath:    "/state",
		MemPath:      "/mem",
		DiskPath:     "/disk",
		BaseImageID:  "base",
		KernelID:     "kernel",
		CreatedAt:    "2026-05-04T00:00:00Z",
	}
}

func request(t *testing.T, handler http.Handler, method string, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	switch body := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(body))
	default:
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, want, rec.Body.String())
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rec.Body.String())
	}
}
