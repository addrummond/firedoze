package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/store"
)

var ErrAlreadyRunning = errors.New("vm already running")

type Manager struct {
	cfg    config.Config
	store  *store.Store
	logger *slog.Logger

	mu      sync.Mutex
	running map[string]*Process
}

type Process struct {
	Name       string
	RuntimeDir string
	SocketPath string
	Command    *exec.Cmd
}

func NewManager(cfg config.Config, st *store.Store, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:     cfg,
		store:   st,
		logger:  logger,
		running: make(map[string]*Process),
	}
}

func (m *Manager) CreateVM(ctx context.Context, params store.CreateVMParams) (store.VM, error) {
	if params.VCPUs == 0 {
		params.VCPUs = m.cfg.Firecracker.DefaultVCPUs
	}
	if params.MemoryMiB == 0 {
		params.MemoryMiB = m.cfg.Firecracker.DefaultMemoryMiB
	}
	if params.DiskBytes == 0 {
		params.DiskBytes = m.cfg.Firecracker.DefaultDiskBytes
	}
	if params.DefaultHTTPPort == 0 {
		params.DefaultHTTPPort = m.cfg.DefaultHTTPPort
	}
	return m.store.CreateVM(ctx, params)
}

func (m *Manager) ListVMs(ctx context.Context) ([]store.VM, error) {
	return m.store.ListVMs(ctx)
}

func (m *Manager) StartVM(ctx context.Context, name string) (store.VM, error) {
	vm, err := m.store.GetVM(ctx, name)
	if err != nil {
		return store.VM{}, err
	}

	m.mu.Lock()
	if _, ok := m.running[name]; ok {
		m.mu.Unlock()
		return store.VM{}, ErrAlreadyRunning
	}
	m.mu.Unlock()

	layout := m.layout(name)
	if err := os.MkdirAll(layout.vmDir, 0o755); err != nil {
		return store.VM{}, err
	}
	if err := os.MkdirAll(layout.runtimeDir, 0o755); err != nil {
		return store.VM{}, err
	}
	if err := ensureDisk(layout.diskPath, m.cfg.Firecracker.BaseRootfsPath, vm.DiskBytes); err != nil {
		return store.VM{}, fmt.Errorf("prepare disk: %w", err)
	}
	if err := writeFirecrackerConfig(layout.configPath, firecrackerConfig{
		BootSource: bootSource{
			KernelImagePath: m.cfg.Firecracker.BaseKernelPath,
			BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw",
		},
		Drives: []drive{{
			DriveID:      "rootfs",
			PathOnHost:   layout.diskPath,
			IsRootDevice: true,
			IsReadOnly:   false,
		}},
		MachineConfig: machineConfig{
			VCPUCount:  vm.VCPUs,
			MemSizeMiB: vm.MemoryMiB,
			SMT:        false,
		},
	}); err != nil {
		return store.VM{}, err
	}

	_ = os.Remove(layout.socketPath)

	stdout, err := os.OpenFile(layout.stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return store.VM{}, err
	}
	stderr, err := os.OpenFile(layout.stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = stdout.Close()
		return store.VM{}, err
	}

	cmd := exec.Command(m.cfg.Firecracker.BinaryPath, "--api-sock", layout.socketPath, "--config-file", layout.configPath)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return store.VM{}, err
	}

	proc := &Process{
		Name:       name,
		RuntimeDir: layout.runtimeDir,
		SocketPath: layout.socketPath,
		Command:    cmd,
	}

	m.mu.Lock()
	m.running[name] = proc
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		m.mu.Lock()
		delete(m.running, name)
		m.mu.Unlock()
		if err := m.store.SetVMState(context.Background(), name, "stopped"); err != nil {
			m.logger.Warn("set vm stopped after firecracker exit", "vm", name, "error", err)
		}
		if err != nil {
			m.logger.Info("firecracker exited", "vm", name, "error", err)
		} else {
			m.logger.Info("firecracker exited", "vm", name)
		}
	}()

	if err := waitForSocket(ctx, layout.socketPath, 5*time.Second); err != nil {
		_ = m.StopVM(context.Background(), name)
		return store.VM{}, err
	}
	if err := m.store.SetVMState(ctx, name, "running"); err != nil {
		return store.VM{}, err
	}

	m.logger.Info("started vm", "vm", name, "pid", cmd.Process.Pid)
	return m.store.GetVM(ctx, name)
}

func (m *Manager) StopVM(ctx context.Context, name string) error {
	m.mu.Lock()
	proc, ok := m.running[name]
	m.mu.Unlock()
	if !ok {
		return m.store.SetVMState(ctx, name, "stopped")
	}

	if err := firecrackerAction(ctx, proc.SocketPath, "SendCtrlAltDel"); err != nil {
		m.logger.Warn("graceful firecracker stop failed; killing process", "vm", name, "error", err)
		if proc.Command.Process != nil {
			_ = proc.Command.Process.Kill()
		}
	}

	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		m.mu.Lock()
		_, stillRunning := m.running[name]
		m.mu.Unlock()
		if !stillRunning {
			break
		}
		select {
		case <-deadline:
			if proc.Command.Process != nil {
				_ = proc.Command.Process.Kill()
			}
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	_ = os.Remove(proc.SocketPath)
	return m.store.SetVMState(ctx, name, "stopped")
}

type layout struct {
	vmDir      string
	runtimeDir string
	diskPath   string
	configPath string
	socketPath string
	stdoutPath string
	stderrPath string
}

func (m *Manager) layout(name string) layout {
	vmDir := filepath.Join(m.cfg.StateDir, "vms", name)
	runtimeDir := filepath.Join(vmDir, "run")
	return layout{
		vmDir:      vmDir,
		runtimeDir: runtimeDir,
		diskPath:   filepath.Join(vmDir, "rootfs.ext4"),
		configPath: filepath.Join(runtimeDir, "firecracker.json"),
		socketPath: filepath.Join(runtimeDir, "firecracker.sock"),
		stdoutPath: filepath.Join(runtimeDir, "stdout.log"),
		stderrPath: filepath.Join(runtimeDir, "stderr.log"),
	}
}

func ensureDisk(path string, source string, size int64) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Truncate(size); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeFirecrackerConfig(path string, cfg firecrackerConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for firecracker socket %s", path)
		case <-ticker.C:
		}
	}
}

func firecrackerAction(ctx context.Context, socketPath string, action string) error {
	body, err := json.Marshal(map[string]string{"action_type": action})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://firecracker/actions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker action %s returned %s: %s", action, resp.Status, string(data))
	}
	return nil
}

type firecrackerConfig struct {
	BootSource    bootSource    `json:"boot-source"`
	Drives        []drive       `json:"drives"`
	MachineConfig machineConfig `json:"machine-config"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type machineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}
