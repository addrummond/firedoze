package firecracker

import (
	"context"
	"crypto/rand"
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

	fcclient "github.com/firecracker-microvm/firecracker-go-sdk/client"
	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	fcops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/vishvananda/netlink"
)

var ErrAlreadyRunning = errors.New("vm already running")
var ErrAlreadyExists = errors.New("already exists")
var ErrNotRunning = errors.New("vm is not running")

const tuntapModeTap netlink.TuntapMode = 0x0002

const ShutdownSleepTimeout = 2 * time.Minute

const (
	debugfsPath   = "/usr/sbin/debugfs"
	iptablesPath  = "/usr/sbin/iptables"
	sshKeygenPath = "/usr/bin/ssh-keygen"
)

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
	TapName    string
	GuestCIDR  string
	FinalState string
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
	if params.IdleSleepAfterSeconds < 0 {
		return store.VM{}, errors.New("idle_sleep_after_seconds cannot be negative")
	}
	if params.PrivateIP == "" {
		ip, err := m.nextPrivateIP(ctx)
		if err != nil {
			return store.VM{}, err
		}
		params.PrivateIP = ip.String()
	}
	return m.store.CreateVM(ctx, params)
}

func (m *Manager) RestoreSnapshot(ctx context.Context, snapshotName string, params store.CreateVMParams) (store.VM, error) {
	if snapshotName == "" {
		return store.VM{}, errors.New("snapshot name is required")
	}
	if params.Name == "" {
		return store.VM{}, errors.New("vm name is required")
	}
	exists, err := m.store.VMExists(ctx, params.Name)
	if err != nil {
		return store.VM{}, err
	}
	if exists {
		return store.VM{}, fmt.Errorf("%w: vm %q", ErrAlreadyExists, params.Name)
	}
	snapshot, err := m.store.GetSnapshot(ctx, snapshotName)
	if err != nil {
		return store.VM{}, err
	}
	diskInfo, err := os.Stat(snapshot.DiskPath)
	if err != nil {
		return store.VM{}, fmt.Errorf("snapshot disk: %w", err)
	}
	if params.DiskBytes == 0 {
		params.DiskBytes = diskInfo.Size()
	}
	if params.VCPUs == 0 {
		params.VCPUs = m.cfg.Firecracker.DefaultVCPUs
	}
	if params.MemoryMiB == 0 {
		params.MemoryMiB = m.cfg.Firecracker.DefaultMemoryMiB
	}
	if params.DefaultHTTPPort == 0 {
		params.DefaultHTTPPort = m.cfg.DefaultHTTPPort
	}
	if params.IdleSleepAfterSeconds < 0 {
		return store.VM{}, errors.New("idle_sleep_after_seconds cannot be negative")
	}
	if params.PrivateIP == "" {
		ip, err := m.nextPrivateIP(ctx)
		if err != nil {
			return store.VM{}, err
		}
		params.PrivateIP = ip.String()
	}

	layout := m.layout(params.Name)
	if err := os.MkdirAll(layout.vmDir, 0o755); err != nil {
		return store.VM{}, err
	}
	if err := copyFile(layout.diskPath, snapshot.DiskPath); err != nil {
		return store.VM{}, fmt.Errorf("copy snapshot disk: %w", err)
	}
	if params.DiskBytes > diskInfo.Size() {
		if err := os.Truncate(layout.diskPath, params.DiskBytes); err != nil {
			return store.VM{}, err
		}
	}
	if err := m.rewriteGuestIdentity(ctx, layout.diskPath, params.Name); err != nil {
		return store.VM{}, fmt.Errorf("rewrite guest identity: %w", err)
	}
	vm, err := m.store.CreateVM(ctx, params)
	if err != nil {
		return store.VM{}, err
	}
	m.logger.Info("restored snapshot to vm", "snapshot", snapshotName, "vm", vm.Name)
	return vm, nil
}

func (m *Manager) ListVMs(ctx context.Context) ([]store.VM, error) {
	return m.store.ListVMs(ctx)
}

func (m *Manager) ListSnapshots(ctx context.Context) ([]store.Snapshot, error) {
	return m.store.ListSnapshots(ctx)
}

func (m *Manager) UpdateVM(ctx context.Context, name string, params store.UpdateVMParams) (store.VM, error) {
	if params.DefaultHTTPPort != nil && (*params.DefaultHTTPPort <= 0 || *params.DefaultHTTPPort > 65535) {
		return store.VM{}, errors.New("default_http_port must be between 1 and 65535")
	}
	if params.IdleSleepAfterSeconds != nil && *params.IdleSleepAfterSeconds < 0 {
		return store.VM{}, errors.New("idle_sleep_after_seconds cannot be negative")
	}
	return m.store.UpdateVM(ctx, name, params)
}

func (m *Manager) DeleteVM(ctx context.Context, name string) error {
	if _, err := m.store.GetVM(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	_, running := m.running[name]
	m.mu.Unlock()
	if running {
		if err := m.StopVM(ctx, name); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(m.layout(name).vmDir); err != nil {
		return err
	}
	return m.store.DeleteVM(ctx, name)
}

func (m *Manager) DeleteSnapshot(ctx context.Context, name string) error {
	if _, err := m.store.GetSnapshot(ctx, name); err != nil {
		return err
	}
	if err := os.RemoveAll(m.snapshotLayout(name).dir); err != nil {
		return err
	}
	return m.store.DeleteSnapshot(ctx, name)
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
	if vm.State == "sleeping" {
		return m.resumeVM(ctx, vm)
	}

	layout := m.layout(name)
	if err := os.MkdirAll(layout.vmDir, 0o755); err != nil {
		return store.VM{}, err
	}
	if err := os.MkdirAll(layout.runtimeDir, 0o755); err != nil {
		return store.VM{}, err
	}
	diskCreated, err := ensureDisk(layout.diskPath, m.cfg.Firecracker.BaseRootfsPath, vm.DiskBytes)
	if err != nil {
		return store.VM{}, fmt.Errorf("prepare disk: %w", err)
	}
	if diskCreated {
		if err := m.rewriteGuestIdentity(ctx, layout.diskPath, vm.Name); err != nil {
			return store.VM{}, fmt.Errorf("rewrite guest identity: %w", err)
		}
	}
	if err := m.injectAuthorizedKeys(ctx, layout.diskPath); err != nil {
		return store.VM{}, fmt.Errorf("inject authorized keys: %w", err)
	}
	netdev, err := m.prepareNetwork(ctx, vm)
	if err != nil {
		return store.VM{}, fmt.Errorf("prepare network: %w", err)
	}
	if err := writeFirecrackerConfig(layout.configPath, firecrackerConfig{
		BootSource: bootSource{
			KernelImagePath: m.cfg.Firecracker.BaseKernelPath,
			InitrdPath:      m.cfg.Firecracker.BaseInitrdPath,
			BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off net.ifnames=0 root=/dev/vda rw",
		},
		Drives: []drive{{
			DriveID:      "rootfs",
			PathOnHost:   layout.diskPath,
			IsRootDevice: true,
			IsReadOnly:   false,
		}},
		NetworkInterfaces: []networkInterface{{
			IfaceID:     "eth0",
			HostDevName: netdev.tapName,
			GuestMAC:    netdev.guestMAC,
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

	proc, err := m.launchProcess(name, layout, netdev, "--config-file", layout.configPath)
	if err != nil {
		return store.VM{}, err
	}

	if err := waitForSocket(ctx, layout.socketPath, 5*time.Second); err != nil {
		_ = m.StopVM(context.Background(), name)
		return store.VM{}, err
	}
	if err := m.store.SetVMState(ctx, name, "running"); err != nil {
		return store.VM{}, err
	}

	m.logger.Info("started vm", "vm", name, "pid", proc.Command.Process.Pid)
	return m.store.GetVM(ctx, name)
}

func (m *Manager) resumeVM(ctx context.Context, vm store.VM) (store.VM, error) {
	layout := m.layout(vm.Name)
	if _, err := os.Stat(layout.sleepStatePath); err != nil {
		return store.VM{}, fmt.Errorf("sleep state: %w", err)
	}
	if _, err := os.Stat(layout.sleepMemPath); err != nil {
		return store.VM{}, fmt.Errorf("sleep memory: %w", err)
	}
	if err := os.MkdirAll(layout.runtimeDir, 0o755); err != nil {
		return store.VM{}, err
	}
	netdev, err := m.prepareNetwork(ctx, vm)
	if err != nil {
		return store.VM{}, fmt.Errorf("prepare network: %w", err)
	}
	_ = os.Remove(layout.socketPath)
	proc, err := m.launchProcess(vm.Name, layout, netdev)
	if err != nil {
		_ = m.cleanupNetwork(&Process{TapName: netdev.tapName, GuestCIDR: netdev.guestCIDR})
		return store.VM{}, err
	}
	if err := waitForSocket(ctx, layout.socketPath, 5*time.Second); err != nil {
		_ = m.stopProcess(context.Background(), proc, "stopped")
		return store.VM{}, err
	}
	if err := firecrackerLoadSnapshot(ctx, layout.socketPath, layout.sleepStatePath, layout.sleepMemPath); err != nil {
		_ = m.stopProcess(context.Background(), proc, "stopped")
		return store.VM{}, err
	}
	if err := m.store.SetVMState(ctx, vm.Name, "running"); err != nil {
		return store.VM{}, err
	}
	m.logger.Info("resumed vm", "vm", vm.Name, "pid", proc.Command.Process.Pid)
	return m.store.GetVM(ctx, vm.Name)
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

	if err := m.waitForProcessExit(ctx, proc, 5*time.Second); err != nil {
		return err
	}
	_ = m.cleanupAfterExit(proc)
	return m.store.SetVMState(ctx, name, "stopped")
}

func (m *Manager) SleepVM(ctx context.Context, name string) (store.VM, error) {
	vm, err := m.store.GetVM(ctx, name)
	if err != nil {
		return store.VM{}, err
	}
	m.mu.Lock()
	proc, ok := m.running[name]
	m.mu.Unlock()
	if !ok {
		if vm.State == "sleeping" {
			return vm, nil
		}
		return store.VM{}, fmt.Errorf("%w: %s", ErrNotRunning, name)
	}
	layout := m.layout(name)
	if err := os.MkdirAll(layout.sleepDir, 0o755); err != nil {
		return store.VM{}, err
	}
	if err := firecrackerSetVMState(ctx, proc.SocketPath, "Paused"); err != nil {
		return store.VM{}, err
	}
	if err := firecrackerCreateSnapshot(ctx, proc.SocketPath, layout.sleepStatePath, layout.sleepMemPath); err != nil {
		_ = firecrackerSetVMState(context.Background(), proc.SocketPath, "Resumed")
		return store.VM{}, err
	}
	if err := m.stopProcess(ctx, proc, "sleeping"); err != nil {
		return store.VM{}, err
	}
	if err := m.store.SetVMState(ctx, name, "sleeping"); err != nil {
		return store.VM{}, err
	}
	m.logger.Info("slept vm", "vm", name)
	return m.store.GetVM(ctx, name)
}

func (m *Manager) SleepRunningVMs(ctx context.Context) error {
	m.mu.Lock()
	names := make([]string, 0, len(m.running))
	for name := range m.running {
		names = append(names, name)
	}
	m.mu.Unlock()

	var errs []error
	for _, name := range names {
		if _, err := m.SleepVM(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("sleep %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) SaveSnapshot(ctx context.Context, params store.CreateSnapshotParams) (store.Snapshot, error) {
	if params.Name == "" {
		return store.Snapshot{}, errors.New("snapshot name is required")
	}
	if params.SourceVM == "" {
		return store.Snapshot{}, errors.New("source VM is required")
	}
	exists, err := m.store.SnapshotExists(ctx, params.Name)
	if err != nil {
		return store.Snapshot{}, err
	}
	if exists {
		return store.Snapshot{}, fmt.Errorf("snapshot %q already exists", params.Name)
	}
	vm, err := m.store.GetVM(ctx, params.SourceVM)
	if err != nil {
		return store.Snapshot{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	proc, ok := m.running[vm.Name]
	if !ok {
		return store.Snapshot{}, fmt.Errorf("%w: %s", ErrNotRunning, vm.Name)
	}

	vmLayout := m.layout(vm.Name)
	snapshotLayout := m.snapshotLayout(params.Name)
	if err := os.MkdirAll(snapshotLayout.dir, 0o755); err != nil {
		return store.Snapshot{}, err
	}

	if err := firecrackerSetVMState(ctx, proc.SocketPath, "Paused"); err != nil {
		return store.Snapshot{}, err
	}
	resume := true
	defer func() {
		if resume {
			if err := firecrackerSetVMState(context.Background(), proc.SocketPath, "Resumed"); err != nil {
				m.logger.Warn("resume vm after snapshot", "vm", vm.Name, "error", err)
			}
		}
	}()

	if err := firecrackerCreateSnapshot(ctx, proc.SocketPath, snapshotLayout.statePath, snapshotLayout.memPath); err != nil {
		return store.Snapshot{}, err
	}
	if err := copyFile(snapshotLayout.diskPath, vmLayout.diskPath); err != nil {
		return store.Snapshot{}, err
	}

	if err := firecrackerSetVMState(ctx, proc.SocketPath, "Resumed"); err != nil {
		return store.Snapshot{}, err
	}
	resume = false

	snapshot, err := m.store.CreateSnapshot(ctx, store.CreateSnapshotParams{
		Name:        params.Name,
		SourceVM:    vm.Name,
		StatePath:   snapshotLayout.statePath,
		MemPath:     snapshotLayout.memPath,
		DiskPath:    snapshotLayout.diskPath,
		BaseImageID: filepath.Base(m.cfg.Firecracker.BaseRootfsPath),
		KernelID:    filepath.Base(m.cfg.Firecracker.BaseKernelPath),
	})
	if err != nil {
		return store.Snapshot{}, err
	}
	m.logger.Info("saved snapshot", "snapshot", snapshot.Name, "vm", vm.Name)
	return snapshot, nil
}

type preparedNetwork struct {
	tapName   string
	guestMAC  string
	guestCIDR string
}

func (m *Manager) prepareNetwork(ctx context.Context, vm store.VM) (preparedNetwork, error) {
	if vm.PrivateIP == "" {
		return preparedNetwork{}, errors.New("vm has no private_ip")
	}
	guestIP := net.ParseIP(vm.PrivateIP).To4()
	if guestIP == nil {
		return preparedNetwork{}, fmt.Errorf("private_ip must be IPv4: %q", vm.PrivateIP)
	}
	hostIP := append(net.IP(nil), guestIP...)
	hostIP[3]--
	if hostIP[3] == 0 {
		return preparedNetwork{}, fmt.Errorf("private_ip %s has invalid /30 host peer", vm.PrivateIP)
	}

	tapName := tapName(vm.Name)
	if err := deleteTap(tapName); err != nil {
		m.logger.Debug("delete existing tap", "tap", tapName, "error", err)
	}
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: tapName},
		Mode:      tuntapModeTap,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return preparedNetwork{}, err
	}
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		_ = deleteTap(tapName)
		return preparedNetwork{}, err
	}
	addr, err := netlink.ParseAddr(hostIP.String() + "/30")
	if err != nil {
		_ = deleteTap(tapName)
		return preparedNetwork{}, err
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		_ = deleteTap(tapName)
		return preparedNetwork{}, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		_ = deleteTap(tapName)
		return preparedNetwork{}, err
	}
	if err := enableIPv4Forwarding(); err != nil {
		return preparedNetwork{}, err
	}
	guestCIDR := guestIP.String() + "/30"
	if err := m.ensureMasquerade(ctx, guestCIDR, tapName); err != nil {
		_ = deleteTap(tapName)
		return preparedNetwork{}, err
	}

	return preparedNetwork{
		tapName:   tapName,
		guestMAC:  macForGuestIP(guestIP),
		guestCIDR: guestCIDR,
	}, nil
}

func (m *Manager) ensureMasquerade(ctx context.Context, guestCIDR string, tapName string) error {
	_, wgNet, err := net.ParseCIDR(m.cfg.WireGuard.Address)
	if err != nil {
		return err
	}
	checkArgs := []string{"-t", "nat", "-C", "POSTROUTING", "-s", wgNet.String(), "-d", guestCIDR, "-o", tapName, "-j", "MASQUERADE"}
	if err := run(ctx, iptablesPath, checkArgs...); err == nil {
		return nil
	}
	addArgs := append([]string{"-t", "nat", "-A", "POSTROUTING"}, checkArgs[4:]...)
	return run(ctx, iptablesPath, addArgs...)
}

func (m *Manager) cleanupNetwork(proc *Process) error {
	var errs []error
	if proc.GuestCIDR != "" && proc.TapName != "" {
		if _, wgNet, err := net.ParseCIDR(m.cfg.WireGuard.Address); err == nil {
			errs = append(errs, run(context.Background(), iptablesPath, "-t", "nat", "-D", "POSTROUTING", "-s", wgNet.String(), "-d", proc.GuestCIDR, "-o", proc.TapName, "-j", "MASQUERADE"))
		}
	}
	errs = append(errs, deleteTap(proc.TapName))
	return errors.Join(errs...)
}

func (m *Manager) nextPrivateIP(ctx context.Context) (net.IP, error) {
	_, subnet, err := net.ParseCIDR(m.cfg.VMNetwork.Subnet)
	if err != nil {
		return nil, err
	}
	base := subnet.IP.To4()
	if base == nil {
		return nil, errors.New("vm_network.subnet must be IPv4 for v1")
	}
	count, err := m.store.CountVMs(ctx)
	if err != nil {
		return nil, err
	}
	ip := append(net.IP(nil), base...)
	offset := 2 + count*4
	ip[2] += byte(offset / 256)
	ip[3] += byte(offset % 256)
	if !subnet.Contains(ip) {
		return nil, fmt.Errorf("vm subnet exhausted: %s", subnet)
	}
	return ip, nil
}

func tapName(vmName string) string {
	name := "fdtap-" + vmName
	if len(name) > 15 {
		return name[:15]
	}
	return name
}

func macForGuestIP(ip net.IP) string {
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3])
}

func enableIPv4Forwarding() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644)
}

func deleteTap(name string) error {
	if name == "" {
		return nil
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return err
	}
	return netlink.LinkDel(link)
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, string(output))
	}
	return nil
}

func (m *Manager) launchProcess(name string, layout layout, netdev preparedNetwork, args ...string) (*Process, error) {
	stdout, err := os.OpenFile(layout.stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(layout.stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}

	cmdArgs := append([]string{"--api-sock", layout.socketPath}, args...)
	cmd := exec.Command(m.cfg.Firecracker.BinaryPath, cmdArgs...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}

	proc := &Process{
		Name:       name,
		RuntimeDir: layout.runtimeDir,
		SocketPath: layout.socketPath,
		TapName:    netdev.tapName,
		GuestCIDR:  netdev.guestCIDR,
		FinalState: "stopped",
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
		finalState := proc.FinalState
		if finalState == "" {
			finalState = "stopped"
		}
		delete(m.running, name)
		m.mu.Unlock()

		if err := m.store.SetVMState(context.Background(), name, finalState); err != nil {
			m.logger.Warn("set vm state after firecracker exit", "vm", name, "state", finalState, "error", err)
		}
		if err != nil {
			m.logger.Info("firecracker exited", "vm", name, "state", finalState, "error", err)
		} else {
			m.logger.Info("firecracker exited", "vm", name, "state", finalState)
		}
	}()

	return proc, nil
}

func (m *Manager) stopProcess(ctx context.Context, proc *Process, finalState string) error {
	m.mu.Lock()
	proc.FinalState = finalState
	m.mu.Unlock()
	if proc.Command.Process != nil {
		_ = proc.Command.Process.Kill()
	}
	if err := m.waitForProcessExit(ctx, proc, 5*time.Second); err != nil {
		return err
	}
	return m.cleanupAfterExit(proc)
}

func (m *Manager) waitForProcessExit(ctx context.Context, proc *Process, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		m.mu.Lock()
		_, stillRunning := m.running[proc.Name]
		m.mu.Unlock()
		if !stillRunning {
			return nil
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
}

func (m *Manager) cleanupAfterExit(proc *Process) error {
	_ = os.Remove(proc.SocketPath)
	return m.cleanupNetwork(proc)
}

type layout struct {
	vmDir          string
	runtimeDir     string
	sleepDir       string
	diskPath       string
	configPath     string
	socketPath     string
	stdoutPath     string
	stderrPath     string
	sleepStatePath string
	sleepMemPath   string
}

type snapshotLayout struct {
	dir       string
	statePath string
	memPath   string
	diskPath  string
}

func (m *Manager) layout(name string) layout {
	vmDir := filepath.Join(m.cfg.StateDir, "vms", name)
	runtimeDir := filepath.Join(vmDir, "run")
	sleepDir := filepath.Join(vmDir, "sleep")
	return layout{
		vmDir:          vmDir,
		runtimeDir:     runtimeDir,
		sleepDir:       sleepDir,
		diskPath:       filepath.Join(vmDir, "rootfs.ext4"),
		configPath:     filepath.Join(runtimeDir, "firecracker.json"),
		socketPath:     filepath.Join(runtimeDir, "firecracker.sock"),
		stdoutPath:     filepath.Join(runtimeDir, "stdout.log"),
		stderrPath:     filepath.Join(runtimeDir, "stderr.log"),
		sleepStatePath: filepath.Join(sleepDir, "vmstate"),
		sleepMemPath:   filepath.Join(sleepDir, "memory"),
	}
}

func (m *Manager) snapshotLayout(name string) snapshotLayout {
	dir := filepath.Join(m.cfg.StateDir, "snapshots", name)
	return snapshotLayout{
		dir:       dir,
		statePath: filepath.Join(dir, "vmstate"),
		memPath:   filepath.Join(dir, "memory"),
		diskPath:  filepath.Join(dir, "rootfs.ext4"),
	}
}

func ensureDisk(path string, source string, size int64) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	in, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return false, err
	}
	if err := out.Truncate(size); err != nil {
		_ = out.Close()
		return false, err
	}
	return true, out.Close()
}

func copyFile(dst string, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func (m *Manager) injectAuthorizedKeys(ctx context.Context, diskPath string) error {
	if len(m.cfg.SSH.AuthorizedKeyFiles) == 0 {
		return nil
	}
	var keys []byte
	for _, path := range m.cfg.SSH.AuthorizedKeyFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		keys = append(keys, data...)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			keys = append(keys, '\n')
		}
	}

	tmp, err := os.CreateTemp("", "firedoze-authorized-keys-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(keys); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := run(ctx, debugfsPath, "-w", "-R", "mkdir /etc/firedoze", diskPath); err != nil {
		m.logger.Debug("mkdir /etc/firedoze in guest disk", "error", err)
	}
	if err := run(ctx, debugfsPath, "-w", "-R", "rm /etc/firedoze/authorized_keys", diskPath); err != nil {
		m.logger.Debug("remove existing authorized_keys in guest disk", "error", err)
	}
	if err := run(ctx, debugfsPath, "-w", "-R", "write "+tmpPath+" /etc/firedoze/authorized_keys", diskPath); err != nil {
		return err
	}
	if err := run(ctx, debugfsPath, "-w", "-R", "set_inode_field /etc/firedoze/authorized_keys mode 0100644", diskPath); err != nil {
		return err
	}
	return nil
}

func (m *Manager) rewriteGuestIdentity(ctx context.Context, diskPath string, vmName string) error {
	if err := writeGuestFileMode(ctx, diskPath, "/etc/hostname", []byte(vmName+"\n"), "0100644"); err != nil {
		return err
	}
	hosts := fmt.Sprintf("127.0.0.1 localhost\n127.0.1.1 %s\n\n::1 localhost ip6-localhost ip6-loopback\nff02::1 ip6-allnodes\nff02::2 ip6-allrouters\n", vmName)
	if err := writeGuestFileMode(ctx, diskPath, "/etc/hosts", []byte(hosts), "0100644"); err != nil {
		return err
	}
	machineID, err := randomMachineID()
	if err != nil {
		return err
	}
	if err := writeGuestFileMode(ctx, diskPath, "/etc/machine-id", []byte(machineID+"\n"), "0100444"); err != nil {
		return err
	}
	if err := writeGuestFileMode(ctx, diskPath, "/var/lib/dbus/machine-id", []byte(machineID+"\n"), "0100444"); err != nil {
		m.logger.Debug("rewrite dbus machine-id in guest disk", "error", err)
	}

	return rewriteSSHHostKeys(ctx, diskPath)
}

func writeGuestFile(ctx context.Context, diskPath string, guestPath string, data []byte) error {
	return writeGuestFileMode(ctx, diskPath, guestPath, data, "")
}

func writeGuestFileMode(ctx context.Context, diskPath string, guestPath string, data []byte, mode string) error {
	tmp, err := os.CreateTemp("", "firedoze-guest-file-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := run(ctx, debugfsPath, "-w", "-R", "rm "+guestPath, diskPath); err != nil {
		// Missing files are fine; the following write creates the replacement.
	}
	if err := run(ctx, debugfsPath, "-w", "-R", "write "+tmpPath+" "+guestPath, diskPath); err != nil {
		return err
	}
	if mode == "" {
		return nil
	}
	return run(ctx, debugfsPath, "-w", "-R", "set_inode_field "+guestPath+" mode "+mode, diskPath)
}

func rewriteSSHHostKeys(ctx context.Context, diskPath string) error {
	tmpDir, err := os.MkdirTemp("", "firedoze-ssh-host-keys-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	keys := []struct {
		keyType string
		path    string
	}{
		{keyType: "rsa", path: "/etc/ssh/ssh_host_rsa_key"},
		{keyType: "ecdsa", path: "/etc/ssh/ssh_host_ecdsa_key"},
		{keyType: "ed25519", path: "/etc/ssh/ssh_host_ed25519_key"},
	}
	for _, key := range keys {
		localPath := filepath.Join(tmpDir, filepath.Base(key.path))
		if err := run(ctx, sshKeygenPath, "-q", "-N", "", "-t", key.keyType, "-f", localPath); err != nil {
			return err
		}
		if err := replaceGuestFile(ctx, diskPath, key.path, localPath, "0100600"); err != nil {
			return err
		}
		if err := replaceGuestFile(ctx, diskPath, key.path+".pub", localPath+".pub", "0100644"); err != nil {
			return err
		}
	}
	return nil
}

func replaceGuestFile(ctx context.Context, diskPath string, guestPath string, localPath string, mode string) error {
	if err := run(ctx, debugfsPath, "-w", "-R", "rm "+guestPath, diskPath); err != nil {
		// Missing files are fine; write creates the replacement.
	}
	if err := run(ctx, debugfsPath, "-w", "-R", "write "+localPath+" "+guestPath, diskPath); err != nil {
		return err
	}
	return run(ctx, debugfsPath, "-w", "-R", "set_inode_field "+guestPath+" mode "+mode, diskPath)
}

func randomMachineID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", id[:]), nil
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
	params := fcops.NewCreateSyncActionParamsWithContext(ctx)
	params.SetInfo(&fcmodels.InstanceActionInfo{
		ActionType: stringPtr(action),
	})
	if _, err := firecrackerOperations(socketPath).CreateSyncAction(params); err != nil {
		return fmt.Errorf("firecracker action %s: %w", action, err)
	}
	return nil
}

func firecrackerSetVMState(ctx context.Context, socketPath string, state string) error {
	params := fcops.NewPatchVMParamsWithContext(ctx)
	params.SetBody(&fcmodels.VM{
		State: stringPtr(state),
	})
	if _, err := firecrackerOperations(socketPath).PatchVM(params); err != nil {
		return fmt.Errorf("firecracker set vm state %s: %w", state, err)
	}
	return nil
}

func firecrackerCreateSnapshot(ctx context.Context, socketPath string, statePath string, memPath string) error {
	params := fcops.NewCreateSnapshotParamsWithContext(ctx)
	params.SetBody(&fcmodels.SnapshotCreateParams{
		SnapshotType: fcmodels.SnapshotCreateParamsSnapshotTypeFull,
		SnapshotPath: stringPtr(statePath),
		MemFilePath:  stringPtr(memPath),
	})
	if _, err := firecrackerOperations(socketPath).CreateSnapshot(params); err != nil {
		return fmt.Errorf("firecracker create snapshot: %w", err)
	}
	return nil
}

func firecrackerLoadSnapshot(ctx context.Context, socketPath string, statePath string, memPath string) error {
	params := fcops.NewLoadSnapshotParamsWithContext(ctx)
	params.SetBody(&fcmodels.SnapshotLoadParams{
		SnapshotPath:        stringPtr(statePath),
		MemFilePath:         stringPtr(memPath),
		EnableDiffSnapshots: true,
		ResumeVM:            true,
	})
	if _, err := firecrackerOperations(socketPath).LoadSnapshot(params); err != nil {
		return fmt.Errorf("firecracker load snapshot: %w", err)
	}
	return nil
}

func firecrackerOperations(socketPath string) fcops.ClientIface {
	transport := httptransport.New(fcclient.DefaultHost, fcclient.DefaultBasePath, fcclient.DefaultSchemes)
	transport.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return fcclient.New(transport, strfmt.Default).Operations
}

func stringPtr(value string) *string {
	return &value
}

type firecrackerConfig struct {
	BootSource        bootSource         `json:"boot-source"`
	Drives            []drive            `json:"drives"`
	NetworkInterfaces []networkInterface `json:"network-interfaces,omitempty"`
	MachineConfig     machineConfig      `json:"machine-config"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	InitrdPath      string `json:"initrd_path,omitempty"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type networkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac"`
}

type machineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}
