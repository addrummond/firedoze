package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"math/bits"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/model"
	"firedoze/internal/store"

	fcclient "github.com/firecracker-microvm/firecracker-go-sdk/client"
	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	fcops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var ErrAlreadyRunning = errors.New("vm already running")
var ErrAlreadyExists = errors.New("already exists")
var ErrNotRunning = errors.New("vm is not running")
var ErrRunning = errors.New("vm is running")
var ErrNotStopped = errors.New("vm is not stopped")

const firecrackerVirtioMemBaseAddress = uint64(512) << 30

const tuntapModeTap netlink.TuntapMode = 0x0002

const ShutdownSleepTimeout = 2 * time.Minute

const (
	debugfsPath   = "/usr/sbin/debugfs"
	sshKeygenPath = "/usr/bin/ssh-keygen"
)

var (
	runCommand                  = run
	deleteTapCmd                = deleteTap
	hostPhysicalAddressBitsFunc = hostPhysicalAddressBits
)

type Manager struct {
	cfg    config.Config
	store  *store.Store
	logger *slog.Logger

	mu                       sync.Mutex
	running                  map[string]*Process
	vmOps                    map[string]struct{}
	coldArchives             map[string]*coldArchiveOperation
	guestMemoryReports       map[string]model.GuestMemoryReport
	copyColdFile             func(context.Context, string, string) error
	rewriteGuestIdentityFunc func(context.Context, string, string) error

	metadataMu    sync.Mutex
	baseMetadata  BaseImageMetadata
	baseSignature string
}

type coldArchiveOperation struct {
	cancel context.CancelFunc
	done   chan struct{}
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
		cfg:                cfg,
		store:              st,
		logger:             logger,
		running:            make(map[string]*Process),
		vmOps:              make(map[string]struct{}),
		coldArchives:       make(map[string]*coldArchiveOperation),
		guestMemoryReports: make(map[string]model.GuestMemoryReport),
		copyColdFile:       copyRegularFile,
	}
}

func (m *Manager) ReconcileStartup(ctx context.Context) error {
	vms, err := m.store.ListVMs(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, vm := range vms {
		proc := &Process{
			Name:      vm.Name,
			TapName:   tapName(vm.Name),
			GuestCIDR: vm.PrivateIP + "/127",
		}
		if err := m.cleanupNetwork(proc); err != nil {
			m.logger.Debug("cleanup stale vm network", "vm", vm.Name, "error", err)
		}
		if vm.State != "running" {
			continue
		}
		if err := m.store.SetVMState(ctx, vm.Name, "lost"); err != nil {
			errs = append(errs, fmt.Errorf("mark %s lost: %w", vm.Name, err))
			continue
		}
		m.logger.Warn("marked stale running vm lost after daemon restart", "vm", vm.Name)
	}
	return errors.Join(errs...)
}

func (m *Manager) CreateVM(ctx context.Context, params store.CreateVMParams) (store.VM, error) {
	routeExists, err := m.store.RouteExists(ctx, params.Name)
	if err != nil {
		return store.VM{}, err
	}
	if routeExists {
		return store.VM{}, fmt.Errorf("%w: route %q reserves VM name", ErrAlreadyExists, params.Name)
	}
	if params.VCPUs == 0 {
		params.VCPUs = m.cfg.Firecracker.DefaultVCPUs
	}
	if params.MemoryMinMiB == 0 {
		params.MemoryMinMiB = m.cfg.Firecracker.DefaultMemoryMinMiB
	}
	if params.MemoryMaxMiB == 0 {
		params.MemoryMaxMiB = m.cfg.Firecracker.DefaultMemoryMaxMiB
	}
	if params.MemoryMinMiB > params.MemoryMaxMiB {
		return store.VM{}, errors.New("memory_min_mib must be less than or equal to memory_max_mib")
	}
	if err := validateMemoryHotplugRange(params.MemoryMinMiB, params.MemoryMaxMiB); err != nil {
		return store.VM{}, err
	}
	if params.DiskBytes == 0 {
		params.DiskBytes = m.cfg.Firecracker.DefaultDiskBytes
	}
	if params.DefaultHTTPPort == 0 {
		params.DefaultHTTPPort = m.cfg.DefaultHTTPPort
	}
	if !params.AutoWakeSet {
		params.AutoWake = true
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
	metadata, err := m.baseImageMetadata()
	if err != nil {
		return store.VM{}, err
	}
	applyBaseImageMetadata(&params, metadata)
	vm, err := m.store.CreateVM(ctx, params)
	if errors.Is(err, store.ErrAlreadyExists) {
		return store.VM{}, fmt.Errorf("%w: %v", ErrAlreadyExists, err)
	}
	return vm, err
}

func (m *Manager) WarmBaseImageMetadata(ctx context.Context) (BaseImageMetadata, error) {
	select {
	case <-ctx.Done():
		return BaseImageMetadata{}, ctx.Err()
	default:
	}
	return m.baseImageMetadata()
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
	routeExists, err := m.store.RouteExists(ctx, params.Name)
	if err != nil {
		return store.VM{}, err
	}
	if routeExists {
		return store.VM{}, fmt.Errorf("%w: route %q reserves VM name", ErrAlreadyExists, params.Name)
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
	if params.MemoryMinMiB == 0 {
		params.MemoryMinMiB = m.cfg.Firecracker.DefaultMemoryMinMiB
	}
	if params.MemoryMaxMiB == 0 {
		params.MemoryMaxMiB = m.cfg.Firecracker.DefaultMemoryMaxMiB
	}
	if params.MemoryMinMiB > params.MemoryMaxMiB {
		return store.VM{}, errors.New("memory_min_mib must be less than or equal to memory_max_mib")
	}
	if err := validateMemoryHotplugRange(params.MemoryMinMiB, params.MemoryMaxMiB); err != nil {
		return store.VM{}, err
	}
	if params.DefaultHTTPPort == 0 {
		params.DefaultHTTPPort = m.cfg.DefaultHTTPPort
	}
	if !params.AutoWakeSet {
		params.AutoWake = true
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
	params.BaseImageID = snapshot.BaseImageID
	params.KernelID = snapshot.KernelID
	params.BaseImageMetadata = string(snapshot.BaseImageMetadata)

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
	if errors.Is(err, store.ErrAlreadyExists) {
		return store.VM{}, fmt.Errorf("%w: %v", ErrAlreadyExists, err)
	}
	if err != nil {
		return store.VM{}, err
	}
	m.logger.Info("restored snapshot to vm", "snapshot", snapshotName, "vm", vm.Name)
	return vm, nil
}

func (m *Manager) ListVMs(ctx context.Context) ([]store.VM, error) {
	return m.store.ListVMs(ctx)
}

func (m *Manager) ListVMsMatching(ctx context.Context, namePatterns []string) ([]store.VM, error) {
	return m.store.ListVMsMatching(ctx, namePatterns)
}

func (m *Manager) GetVM(ctx context.Context, name string) (store.VM, error) {
	return m.store.GetVM(ctx, name)
}

func (m *Manager) ListSnapshots(ctx context.Context) ([]store.Snapshot, error) {
	return m.store.ListSnapshots(ctx)
}

func (m *Manager) GetSnapshot(ctx context.Context, name string) (store.Snapshot, error) {
	return m.store.GetSnapshot(ctx, name)
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

func (m *Manager) RecordVMMemoryReportByPrivateIP(ctx context.Context, privateIP string, targetMiB *int, report model.GuestMemoryReport) (model.MemoryHotplugUsage, error) {
	vm, err := m.store.GetVMByPrivateIP(ctx, privateIP)
	if err != nil {
		return model.MemoryHotplugUsage{}, err
	}
	report.ReportedAt = time.Now().UTC().Format(time.RFC3339)
	if targetMiB != nil {
		report.LastTargetMiB = *targetMiB
	}
	m.mu.Lock()
	m.guestMemoryReports[vm.Name] = report
	m.mu.Unlock()
	if targetMiB == nil {
		return model.MemoryHotplugUsage{}, nil
	}
	return m.setVMMemoryTarget(ctx, vm, *targetMiB)
}

func (m *Manager) setVMMemoryTarget(ctx context.Context, vm store.VM, targetMiB int) (model.MemoryHotplugUsage, error) {
	if targetMiB < vm.MemoryMinMiB {
		targetMiB = vm.MemoryMinMiB
	}
	if targetMiB > vm.MemoryMaxMiB {
		targetMiB = vm.MemoryMaxMiB
	}
	requestedMiB := targetMiB - vm.MemoryMinMiB
	if requestedMiB < 0 {
		requestedMiB = 0
	}
	if requestedMiB > 0 {
		requestedMiB = ((requestedMiB + 127) / 128) * 128
	}
	if maxRequested := vm.MemoryMaxMiB - vm.MemoryMinMiB; requestedMiB > maxRequested {
		requestedMiB = maxRequested
	}
	m.mu.Lock()
	proc, running := m.running[vm.Name]
	m.mu.Unlock()
	if !running || proc == nil {
		return model.MemoryHotplugUsage{}, ErrNotRunning
	}
	if vm.MemoryMaxMiB == vm.MemoryMinMiB {
		return model.MemoryHotplugUsage{
			EffectiveMiB: vm.MemoryMinMiB,
		}, nil
	}
	if !virtioMemUsableForVM(vm) {
		return model.MemoryHotplugUsage{
			EffectiveMiB: vm.MemoryMaxMiB,
		}, nil
	}
	if err := firecrackerPatchMemoryHotplug(ctx, proc.SocketPath, requestedMiB); err != nil {
		return model.MemoryHotplugUsage{}, err
	}
	status, err := firecrackerGetMemoryHotplug(ctx, proc.SocketPath)
	if err != nil {
		return model.MemoryHotplugUsage{}, err
	}
	return model.MemoryHotplugUsage{
		TotalMiB:     status.TotalSizeMiB,
		RequestedMiB: status.RequestedSizeMiB,
		PluggedMiB:   status.PluggedSizeMiB,
		EffectiveMiB: vm.MemoryMinMiB + status.PluggedSizeMiB,
	}, nil
}

type BaseImageMetadata struct {
	Rootfs   ArtifactMetadata  `json:"rootfs"`
	Kernel   ArtifactMetadata  `json:"kernel"`
	Initrd   *ArtifactMetadata `json:"initrd,omitempty"`
	Manifest map[string]string `json:"manifest,omitempty"`
}

type ArtifactMetadata struct {
	Path     string `json:"path"`
	Basename string `json:"basename"`
	SHA256   string `json:"sha256,omitempty"`
	Size     int64  `json:"size,omitempty"`
	ModTime  string `json:"mod_time,omitempty"`
}

func (m *Manager) baseImageMetadata() (BaseImageMetadata, error) {
	signature, err := m.baseImageSignature()
	if err != nil {
		return BaseImageMetadata{}, err
	}
	m.metadataMu.Lock()
	defer m.metadataMu.Unlock()
	if m.baseSignature == signature {
		return m.baseMetadata, nil
	}

	metadata := BaseImageMetadata{}
	metadata.Rootfs, err = artifactMetadata(m.cfg.Firecracker.BaseRootfsPath)
	if err != nil {
		return BaseImageMetadata{}, fmt.Errorf("rootfs metadata: %w", err)
	}
	metadata.Kernel, err = artifactMetadata(m.cfg.Firecracker.BaseKernelPath)
	if err != nil {
		return BaseImageMetadata{}, fmt.Errorf("kernel metadata: %w", err)
	}
	if m.cfg.Firecracker.BaseInitrdPath != "" {
		initrd, err := artifactMetadata(m.cfg.Firecracker.BaseInitrdPath)
		if err != nil {
			return BaseImageMetadata{}, fmt.Errorf("initrd metadata: %w", err)
		}
		metadata.Initrd = &initrd
	}
	manifest, err := readImageManifest(filepath.Join(filepath.Dir(m.cfg.Firecracker.BaseRootfsPath), "manifest.txt"))
	if err != nil {
		m.logger.Debug("read base image manifest", "error", err)
	}
	metadata.Manifest = manifest
	m.baseSignature = signature
	m.baseMetadata = metadata
	return metadata, nil
}

func (m *Manager) baseImageSignature() (string, error) {
	paths := []string{m.cfg.Firecracker.BaseRootfsPath, m.cfg.Firecracker.BaseKernelPath}
	if m.cfg.Firecracker.BaseInitrdPath != "" {
		paths = append(paths, m.cfg.Firecracker.BaseInitrdPath)
	}
	var b strings.Builder
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s\x00%d\x00%d\x00", p, info.Size(), info.ModTime().UnixNano())
	}
	return b.String(), nil
}

func applyBaseImageMetadata(params *store.CreateVMParams, metadata BaseImageMetadata) {
	params.BaseImageID = metadata.Rootfs.SHA256
	if params.BaseImageID == "" {
		params.BaseImageID = metadata.Rootfs.Basename
	}
	params.KernelID = metadata.Kernel.SHA256
	if params.KernelID == "" {
		params.KernelID = metadata.Kernel.Basename
	}
	if data, err := json.Marshal(metadata); err == nil {
		params.BaseImageMetadata = string(data)
	}
}

func artifactMetadata(p string) (ArtifactMetadata, error) {
	info, err := os.Stat(p)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	sum, err := fileSHA256(p)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	return ArtifactMetadata{
		Path:     p,
		Basename: filepath.Base(p),
		SHA256:   sum,
		Size:     info.Size(),
		ModTime:  info.ModTime().UTC().Format(time.RFC3339Nano),
	}, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readImageManifest(p string) (map[string]string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	manifest := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		manifest[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return manifest, nil
}

func (m *Manager) DeleteVM(ctx context.Context, name string) error {
	if err := m.beginVMOperationCancelingColdArchive(ctx, name); err != nil {
		return err
	}
	defer m.endVMOperation(name)

	vm, err := m.store.GetVM(ctx, name)
	if err != nil {
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
	if vm.ArchivedDiskPath != "" {
		if err := os.Remove(vm.ArchivedDiskPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if coldDir := m.coldVMDir(name); coldDir != "" {
		if err := os.RemoveAll(coldDir); err != nil {
			return err
		}
	}
	if err := m.store.DeleteRoutesForVM(ctx, name); err != nil {
		return err
	}
	if err := m.store.DeleteVM(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.guestMemoryReports, name)
	m.mu.Unlock()
	return nil
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

func (m *Manager) beginVMOperation(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vmOps[name]; ok {
		return ErrAlreadyRunning
	}
	m.vmOps[name] = struct{}{}
	return nil
}

func (m *Manager) endVMOperation(name string) {
	m.mu.Lock()
	delete(m.vmOps, name)
	m.mu.Unlock()
}

func (m *Manager) beginVMOperationCancelingColdArchive(ctx context.Context, name string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		if archive := m.coldArchives[name]; archive != nil {
			archive.cancel()
			done := archive.done
			m.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if _, ok := m.vmOps[name]; ok {
			m.mu.Unlock()
			return ErrAlreadyRunning
		}
		m.vmOps[name] = struct{}{}
		m.mu.Unlock()
		return nil
	}
}

func (m *Manager) beginStartOperationCancelingColdArchive(ctx context.Context, name string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		if _, ok := m.running[name]; ok {
			m.mu.Unlock()
			return ErrAlreadyRunning
		}
		if archive := m.coldArchives[name]; archive != nil {
			archive.cancel()
			done := archive.done
			m.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if _, ok := m.vmOps[name]; ok {
			m.mu.Unlock()
			return ErrAlreadyRunning
		}
		m.vmOps[name] = struct{}{}
		m.mu.Unlock()
		return nil
	}
}

func (m *Manager) beginColdArchiveOperation(ctx context.Context, name string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	archiveCtx, cancel := context.WithCancel(ctx)
	archive := &coldArchiveOperation{
		cancel: cancel,
		done:   make(chan struct{}),
	}

	m.mu.Lock()
	if _, ok := m.vmOps[name]; ok {
		m.mu.Unlock()
		cancel()
		return nil, nil, ErrAlreadyRunning
	}
	m.vmOps[name] = struct{}{}
	m.coldArchives[name] = archive
	m.mu.Unlock()

	end := func() {
		cancel()
		m.mu.Lock()
		if current := m.coldArchives[name]; current == archive {
			delete(m.coldArchives, name)
		}
		delete(m.vmOps, name)
		close(archive.done)
		m.mu.Unlock()
	}
	return archiveCtx, end, nil
}

func (m *Manager) StartVM(ctx context.Context, name string) (store.VM, error) {
	if err := m.beginStartOperationCancelingColdArchive(ctx, name); err != nil {
		return store.VM{}, err
	}
	defer m.endVMOperation(name)

	vm, err := m.store.GetVM(ctx, name)
	if err != nil {
		return store.VM{}, err
	}
	memoryConfig, err := memoryConfigForHost(vm)
	if err != nil {
		return store.VM{}, err
	}
	if memoryConfig.degraded {
		m.logger.Warn("virtio-mem unavailable on this host; starting VM with fixed max memory", "vm", vm.Name, "memory_mib", memoryConfig.bootMiB)
	}
	if vm.State == "sleeping" {
		return m.resumeVM(ctx, vm)
	}

	if err := m.hydrateColdDisk(ctx, vm); err != nil {
		return store.VM{}, fmt.Errorf("restore cold disk: %w", err)
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
	netdev, err := m.prepareNetwork(ctx, vm)
	if err != nil {
		return store.VM{}, fmt.Errorf("prepare network: %w", err)
	}
	fcConfig := firecrackerConfig{
		BootSource: bootSource{
			KernelImagePath: m.cfg.Firecracker.BaseKernelPath,
			InitrdPath:      m.cfg.Firecracker.BaseInitrdPath,
			BootArgs:        m.bootArgs(netdev),
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
			MemSizeMiB: memoryConfig.bootMiB,
			SMT:        false,
		},
	}
	fcConfig.MemoryHotplug = memoryConfig.hotplug
	if err := writeFirecrackerConfig(layout.configPath, fcConfig); err != nil {
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

func (m *Manager) RebootVM(ctx context.Context, name string) (store.VM, error) {
	vm, err := m.store.GetVM(ctx, name)
	if err != nil {
		return store.VM{}, err
	}

	m.mu.Lock()
	_, running := m.running[name]
	m.mu.Unlock()
	if running {
		if err := m.StopVM(ctx, name); err != nil {
			return store.VM{}, err
		}
		return m.StartVM(ctx, name)
	}

	if vm.State == "sleeping" {
		if err := os.RemoveAll(m.layout(name).sleepDir); err != nil {
			return store.VM{}, err
		}
		if err := m.store.SetVMState(ctx, name, "stopped"); err != nil {
			return store.VM{}, err
		}
	}
	return m.StartVM(ctx, name)
}

func (m *Manager) bootArgs(netdev preparedNetwork) string {
	args := "console=ttyS0 reboot=k panic=1 net.ifnames=0 root=/dev/vda rw memhp_default_state=online_movable quiet loglevel=3 systemd.show_status=false rd.systemd.show_status=false"
	args += " firedoze.guest_ip=" + netdev.guestIP.String()
	args += " firedoze.host_ip=" + netdev.hostIP.String()
	args += fmt.Sprintf(" firedoze.memory_port=%d", m.cfg.GuestControl.MemoryPort)
	if m.cfg.DNS.Enabled {
		args += " firedoze.dns_ip=" + m.cfg.DNS.ListenIP
		args += " firedoze.dns_domain=" + m.cfg.DNS.Domain
	}
	return args
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
	names := m.RunningVMNames()

	var errs []error
	for _, name := range names {
		if _, err := m.SleepVM(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("sleep %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) RunningVMNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.running))
	for name := range m.running {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) StartVMs(ctx context.Context, names []string) error {
	var errs []error
	for _, name := range names {
		if _, err := m.StartVM(ctx, name); err != nil {
			if errors.Is(err, ErrAlreadyRunning) {
				continue
			}
			errs = append(errs, fmt.Errorf("start %s: %w", name, err))
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
	if err := m.beginVMOperationCancelingColdArchive(ctx, params.SourceVM); err != nil {
		return store.Snapshot{}, err
	}
	defer m.endVMOperation(params.SourceVM)

	vm, err := m.store.GetVM(ctx, params.SourceVM)
	if err != nil {
		return store.Snapshot{}, err
	}

	m.mu.Lock()
	_, running := m.running[vm.Name]
	m.mu.Unlock()
	if vm.State == "running" || running {
		return store.Snapshot{}, fmt.Errorf("%w: cannot snapshot running VM %q; run `firedoze vm stop %s` first", ErrRunning, vm.Name, vm.Name)
	}
	if vm.State != "stopped" {
		return store.Snapshot{}, fmt.Errorf("%w: cannot snapshot VM %q in state %q; run `firedoze vm stop %s` first", ErrNotStopped, vm.Name, vm.State, vm.Name)
	}

	vmDiskPath, err := m.vmDiskPath(vm)
	if err != nil {
		return store.Snapshot{}, err
	}
	snapshotLayout := m.snapshotLayout(params.Name)
	if err := os.MkdirAll(snapshotLayout.dir, 0o755); err != nil {
		return store.Snapshot{}, err
	}

	if err := copyFile(snapshotLayout.diskPath, vmDiskPath); err != nil {
		return store.Snapshot{}, fmt.Errorf("copy vm disk: %w", err)
	}

	snapshot, err := m.store.CreateSnapshot(ctx, store.CreateSnapshotParams{
		Name:              params.Name,
		SourceVM:          vm.Name,
		StatePath:         "",
		MemPath:           "",
		DiskPath:          snapshotLayout.diskPath,
		BaseImageID:       vm.BaseImageID,
		KernelID:          vm.KernelID,
		BaseImageMetadata: string(vm.BaseImageMetadata),
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
	hostIP    net.IP
	guestIP   net.IP
}

func (m *Manager) prepareNetwork(ctx context.Context, vm store.VM) (preparedNetwork, error) {
	_ = ctx
	if vm.PrivateIP == "" {
		return preparedNetwork{}, errors.New("vm has no private_ip")
	}
	guestIP := net.ParseIP(vm.PrivateIP)
	if guestIP == nil {
		return preparedNetwork{}, fmt.Errorf("private_ip must be an IP address: %q", vm.PrivateIP)
	}
	guestIP = guestIP.To16()
	if guestIP == nil || guestIP.To4() != nil {
		return preparedNetwork{}, fmt.Errorf("private_ip must be IPv6: %q", vm.PrivateIP)
	}
	hostIP, err := decrementIP(guestIP)
	if err != nil {
		return preparedNetwork{}, fmt.Errorf("private_ip %s has invalid /127 host peer: %w", vm.PrivateIP, err)
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
	addr, err := netlink.ParseAddr(hostIP.String() + "/127")
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
	if err := enableIPv6Forwarding(); err != nil {
		return preparedNetwork{}, err
	}
	guestCIDR := guestIP.String() + "/127"

	return preparedNetwork{
		tapName:   tapName,
		guestMAC:  macForVMName(vm.Name),
		guestCIDR: guestCIDR,
		hostIP:    hostIP,
		guestIP:   guestIP,
	}, nil
}

func (m *Manager) cleanupNetwork(proc *Process) error {
	return deleteTapCmd(proc.TapName)
}

func (m *Manager) nextPrivateIP(ctx context.Context) (net.IP, error) {
	_, subnet, err := net.ParseCIDR(m.cfg.VMNetwork.Subnet)
	if err != nil {
		return nil, err
	}
	base := subnet.IP.To16()
	if base == nil || base.To4() != nil {
		return nil, errors.New("vm_network.subnet must be IPv6")
	}
	vms, err := m.store.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	used := make(map[string]struct{}, len(vms))
	for _, vm := range vms {
		if vm.PrivateIP != "" {
			used[vm.PrivateIP] = struct{}{}
		}
	}
	// Reserve ::1 for the DNS listener. Each VM gets a /127 pair:
	// even address on the host TAP, odd address in the guest.
	for offset := int64(3); ; offset += 2 {
		ip, err := addToIP(base, offset)
		if err != nil {
			return nil, err
		}
		if !subnet.Contains(ip) {
			return nil, fmt.Errorf("vm subnet exhausted: %s", subnet)
		}
		if _, ok := used[ip.String()]; ok {
			continue
		}
		return ip, nil
	}
}

func tapName(vmName string) string {
	name := "fdtap-" + vmName
	if len(name) > 15 {
		return name[:15]
	}
	return name
}

func macForVMName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3])
}

func enableIPv6Forwarding() error {
	return os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1\n"), 0o644)
}

func addToIP(ip net.IP, offset int64) (net.IP, error) {
	value := new(big.Int).SetBytes(ip.To16())
	value.Add(value, big.NewInt(offset))
	if value.Sign() < 0 {
		return nil, fmt.Errorf("IP underflow")
	}
	bytes := value.Bytes()
	if len(bytes) > net.IPv6len {
		return nil, fmt.Errorf("IP overflow")
	}
	out := make(net.IP, net.IPv6len)
	copy(out[net.IPv6len-len(bytes):], bytes)
	return out, nil
}

func decrementIP(ip net.IP) (net.IP, error) {
	return addToIP(ip, -1)
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

	cmdArgs := append([]string{"--api-sock", layout.socketPath, "--enable-pci"}, args...)
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
		m.mu.Unlock()

		if err := m.cleanupAfterExit(proc); err != nil {
			m.logger.Warn("cleanup after firecracker exit", "vm", name, "error", err)
		}

		if err := m.store.SetVMState(context.Background(), name, finalState); err != nil {
			m.logger.Warn("set vm state after firecracker exit", "vm", name, "state", finalState, "error", err)
		}
		if err != nil {
			m.logger.Info("firecracker exited", "vm", name, "state", finalState, "error", err)
		} else {
			m.logger.Info("firecracker exited", "vm", name, "state", finalState)
		}

		m.mu.Lock()
		delete(m.running, name)
		m.mu.Unlock()
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

func (m *Manager) coldVMDir(name string) string {
	if m.cfg.ColdStorage.Dir == "" {
		return ""
	}
	return filepath.Join(m.cfg.ColdStorage.Dir, "vms", name)
}

func (m *Manager) coldDiskPath(name string) string {
	dir := m.coldVMDir(name)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "rootfs.ext4")
}

func (m *Manager) vmDiskPath(vm store.VM) (string, error) {
	hot := m.layout(vm.Name).diskPath
	if _, err := os.Stat(hot); err == nil {
		return hot, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	cold := vm.ArchivedDiskPath
	archived := cold != ""
	if cold == "" {
		cold = m.coldDiskPath(vm.Name)
	}
	if cold != "" {
		if _, err := os.Stat(cold); err == nil {
			return cold, nil
		} else if !os.IsNotExist(err) {
			return "", err
		} else if archived {
			return "", fmt.Errorf("archived disk %s: %w", cold, err)
		}
	}
	return hot, nil
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
	if cloned, err := tryCloneFile(out, in); err != nil {
		_ = out.Close()
		return false, err
	} else if cloned {
		if err := out.Truncate(size); err != nil {
			_ = out.Close()
			return false, err
		}
		return true, out.Close()
	}
	if err := copySparseFile(out, in); err != nil {
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
	if cloned, err := tryCloneFile(out, in); err != nil {
		_ = out.Close()
		return err
	} else if cloned {
		return out.Close()
	}
	if err := copySparseFile(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func copyRegularFile(ctx context.Context, dst string, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	copyErr := copyFileDenseContext(ctx, out, in)
	syncErr := error(nil)
	if copyErr == nil {
		syncErr = out.Sync()
	}
	closeErr := out.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

func copyFileDenseContext(ctx context.Context, out *os.File, in *os.File) error {
	buf := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func copySparseFile(out *os.File, in *os.File) error {
	info, err := in.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	if !info.Mode().IsRegular() {
		_, err := io.Copy(out, in)
		return err
	}

	offset := int64(0)
	buf := make([]byte, 1024*1024)
	for offset < size {
		data, err := unix.Seek(int(in.Fd()), offset, unix.SEEK_DATA)
		if err != nil {
			if err == unix.ENXIO {
				break
			}
			if isSparseSeekUnsupported(err) {
				return copyFileDense(out, in)
			}
			return err
		}
		hole, err := unix.Seek(int(in.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			if isSparseSeekUnsupported(err) {
				return copyFileDense(out, in)
			}
			return err
		}
		if hole > size {
			hole = size
		}
		if _, err := out.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := in.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyBuffer(out, io.LimitReader(in, hole-data), buf); err != nil {
			return err
		}
		offset = hole
	}
	return out.Truncate(size)
}

func isSparseSeekUnsupported(err error) bool {
	return err == unix.EINVAL || err == unix.ENOTTY || err == unix.EOPNOTSUPP
}

func copyFileDense(out *os.File, in *os.File) error {
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := io.Copy(out, in)
	return err
}

func (m *Manager) rewriteGuestIdentity(ctx context.Context, diskPath string, vmName string) error {
	if m.rewriteGuestIdentityFunc != nil {
		return m.rewriteGuestIdentityFunc(ctx, diskPath, vmName)
	}
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
	if err := runCommand(ctx, debugfsPath, "-w", "-R", "rm "+guestPath, diskPath); err != nil {
		// Missing files are fine; the following write creates the replacement.
	}
	if err := runCommand(ctx, debugfsPath, "-w", "-R", "write "+tmpPath+" "+guestPath, diskPath); err != nil {
		return err
	}
	if mode == "" {
		return nil
	}
	return runCommand(ctx, debugfsPath, "-w", "-R", "set_inode_field "+guestPath+" mode "+mode, diskPath)
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
		if err := runCommand(ctx, sshKeygenPath, "-q", "-N", "", "-t", key.keyType, "-f", localPath); err != nil {
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
	if err := runCommand(ctx, debugfsPath, "-w", "-R", "rm "+guestPath, diskPath); err != nil {
		// Missing files are fine; write creates the replacement.
	}
	if err := runCommand(ctx, debugfsPath, "-w", "-R", "write "+localPath+" "+guestPath, diskPath); err != nil {
		return err
	}
	return runCommand(ctx, debugfsPath, "-w", "-R", "set_inode_field "+guestPath+" mode "+mode, diskPath)
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

type memoryHotplugSizeUpdate struct {
	RequestedSizeMiB int `json:"requested_size_mib"`
}

type memoryHotplugStatus struct {
	TotalSizeMiB     int `json:"total_size_mib"`
	SlotSizeMiB      int `json:"slot_size_mib"`
	BlockSizeMiB     int `json:"block_size_mib"`
	PluggedSizeMiB   int `json:"plugged_size_mib"`
	RequestedSizeMiB int `json:"requested_size_mib"`
}

func firecrackerPatchMemoryHotplug(ctx context.Context, socketPath string, requestedMiB int) error {
	return firecrackerREST(ctx, socketPath, http.MethodPatch, "/hotplug/memory", memoryHotplugSizeUpdate{RequestedSizeMiB: requestedMiB}, nil)
}

func firecrackerGetMemoryHotplug(ctx context.Context, socketPath string) (memoryHotplugStatus, error) {
	var status memoryHotplugStatus
	err := firecrackerREST(ctx, socketPath, http.MethodGet, "/hotplug/memory", nil, &status)
	return status, err
}

func firecrackerREST(ctx context.Context, socketPath string, method string, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
	BootSource        bootSource           `json:"boot-source"`
	Drives            []drive              `json:"drives"`
	NetworkInterfaces []networkInterface   `json:"network-interfaces,omitempty"`
	MachineConfig     machineConfig        `json:"machine-config"`
	MemoryHotplug     *memoryHotplugConfig `json:"memory-hotplug,omitempty"`
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

type memoryHotplugConfig struct {
	TotalSizeMiB int `json:"total_size_mib"`
	SlotSizeMiB  int `json:"slot_size_mib,omitempty"`
	BlockSizeMiB int `json:"block_size_mib,omitempty"`
}

type hostMemoryConfig struct {
	bootMiB  int
	hotplug  *memoryHotplugConfig
	degraded bool
}

func memoryConfigForHost(vm store.VM) (hostMemoryConfig, error) {
	if err := validateMemoryHotplugRange(vm.MemoryMinMiB, vm.MemoryMaxMiB); err != nil {
		return hostMemoryConfig{}, err
	}
	if vm.MemoryMaxMiB <= vm.MemoryMinMiB {
		return hostMemoryConfig{bootMiB: vm.MemoryMaxMiB}, nil
	}
	if !virtioMemUsableForVM(vm) {
		return hostMemoryConfig{bootMiB: vm.MemoryMaxMiB, degraded: true}, nil
	}
	return hostMemoryConfig{
		bootMiB: vm.MemoryMinMiB,
		hotplug: &memoryHotplugConfig{
			TotalSizeMiB: vm.MemoryMaxMiB - vm.MemoryMinMiB,
			SlotSizeMiB:  128,
			BlockSizeMiB: 2,
		},
	}, nil
}

func virtioMemUsableForVM(vm store.VM) bool {
	if vm.MemoryMaxMiB <= vm.MemoryMinMiB {
		return false
	}
	return virtioMemSupportedForRange(vm.MemoryMinMiB, vm.MemoryMaxMiB)
}

func validateMemoryHotplugRange(minMiB int, maxMiB int) error {
	if minMiB <= 0 || maxMiB <= 0 {
		return errors.New("memory_min_mib and memory_max_mib must be positive")
	}
	delta := maxMiB - minMiB
	if delta == 0 {
		return nil
	}
	if delta < 128 || delta%128 != 0 {
		return errors.New("memory_max_mib - memory_min_mib must be zero or a positive multiple of 128")
	}
	return nil
}

func virtioMemSupportedForRange(minMiB int, maxMiB int) bool {
	if err := validateMemoryHotplugRange(minMiB, maxMiB); err != nil {
		return false
	}
	if maxMiB == minMiB {
		return false
	}
	physicalBits, ok := hostPhysicalAddressBitsFunc()
	if !ok {
		return true
	}
	requiredBits := requiredVirtioMemPhysicalAddressBits(maxMiB - minMiB)
	return physicalBits >= requiredBits
}

func requiredVirtioMemPhysicalAddressBits(hotplugMiB int) int {
	if hotplugMiB <= 0 {
		return 0
	}
	hotplugEnd := firecrackerVirtioMemBaseAddress + (uint64(hotplugMiB) << 20) - 1
	return bits.Len64(hotplugEnd)
}

func hostPhysicalAddressBits() (int, bool) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0, false
	}
	return parsePhysicalAddressBits(data)
}

func parsePhysicalAddressBits(data []byte) (int, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "address sizes") {
			continue
		}
		fields := strings.Fields(line)
		for i, field := range fields {
			if strings.TrimSuffix(field, ",") != "physical" || i < 2 || fields[i-1] != "bits" {
				continue
			}
			value, err := strconv.Atoi(fields[i-2])
			if err == nil && value > 0 {
				return value, true
			}
		}
	}
	return 0, false
}
