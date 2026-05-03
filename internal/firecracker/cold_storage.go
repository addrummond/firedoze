package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"firedoze/internal/store"
)

type ColdStorageMonitor struct {
	manager *Manager
	logger  *slog.Logger
}

func NewColdStorageMonitor(manager *Manager, logger *slog.Logger) *ColdStorageMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &ColdStorageMonitor{
		manager: manager,
		logger:  logger,
	}
}

func (m *ColdStorageMonitor) Run(ctx context.Context) {
	if !m.manager.coldStorageEnabled() {
		m.logger.Info("cold storage monitor disabled")
		return
	}

	interval := time.Duration(m.manager.cfg.Idle.CheckIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.logger.Info("cold storage monitor started", "check_interval", interval, "dir", m.manager.cfg.ColdStorage.Dir)
	m.check(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.check(ctx, now)
		}
	}
}

func (m *ColdStorageMonitor) check(ctx context.Context, now time.Time) {
	vms, err := m.manager.ListVMs(ctx)
	if err != nil {
		m.logger.Warn("cold storage monitor list vms", "error", err)
		return
	}
	for _, vm := range vms {
		if vm.State != "stopped" {
			continue
		}
		if err := m.manager.ArchiveStoppedVM(ctx, vm.Name, now); err != nil {
			if errors.Is(err, ErrAlreadyRunning) {
				m.logger.Debug("cold storage skipped busy vm", "vm", vm.Name)
				continue
			}
			m.logger.Warn("cold storage archive failed", "vm", vm.Name, "error", err)
		}
	}
}

func (m *Manager) coldStorageEnabled() bool {
	return m.cfg.ColdStorage.Dir != "" && m.cfg.ColdStorage.ArchiveStoppedAfterSeconds > 0
}

func (m *Manager) ArchiveStoppedVM(ctx context.Context, name string, now time.Time) error {
	if !m.coldStorageEnabled() {
		return nil
	}
	if err := m.beginVMOperation(name); err != nil {
		return err
	}
	defer m.endVMOperation(name)

	vm, err := m.store.GetVM(ctx, name)
	if err != nil {
		return err
	}
	if !m.shouldArchiveStoppedVM(vm, now) {
		return nil
	}

	m.mu.Lock()
	_, running := m.running[name]
	m.mu.Unlock()
	if running {
		return nil
	}

	layout := m.layout(name)
	if _, err := os.Stat(layout.diskPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	coldDir := m.coldVMDir(name)
	coldPath := filepath.Join(coldDir, "rootfs.ext4")
	if err := os.MkdirAll(coldDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(coldDir, "rootfs.ext4.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if err := copyRegularFile(tmpPath, layout.diskPath); err != nil {
		return err
	}
	if err := os.Remove(coldPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpPath, coldPath); err != nil {
		return err
	}
	if err := m.store.SetVMArchivedDiskPath(ctx, name, coldPath); err != nil {
		return err
	}
	if err := os.Remove(layout.diskPath); err != nil {
		return err
	}
	m.logger.Info("archived stopped vm disk to cold storage", "vm", name, "path", coldPath)
	return nil
}

func (m *Manager) shouldArchiveStoppedVM(vm store.VM, now time.Time) bool {
	if vm.State != "stopped" || vm.StoppedAt == "" {
		return false
	}
	stoppedAt, err := time.Parse(time.RFC3339Nano, vm.StoppedAt)
	if err != nil {
		m.logger.Warn("cannot parse vm stopped_at", "vm", vm.Name, "stopped_at", vm.StoppedAt, "error", err)
		return false
	}
	threshold := time.Duration(m.cfg.ColdStorage.ArchiveStoppedAfterSeconds) * time.Second
	return !now.Before(stoppedAt.Add(threshold))
}

func (m *Manager) hydrateColdDisk(ctx context.Context, vm store.VM) error {
	if m.cfg.ColdStorage.Dir == "" && vm.ArchivedDiskPath == "" {
		return nil
	}

	layout := m.layout(vm.Name)
	if _, err := os.Stat(layout.diskPath); err == nil {
		if vm.ArchivedDiskPath != "" {
			if err := m.store.SetVMArchivedDiskPath(ctx, vm.Name, ""); err != nil {
				return err
			}
			if err := os.Remove(vm.ArchivedDiskPath); err != nil && !os.IsNotExist(err) {
				m.logger.Warn("remove stale archived disk after hot disk won", "vm", vm.Name, "path", vm.ArchivedDiskPath, "error", err)
			}
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	archived := vm.ArchivedDiskPath != ""
	coldPath := vm.ArchivedDiskPath
	if coldPath == "" {
		coldPath = m.coldDiskPath(vm.Name)
	}
	if coldPath == "" {
		return nil
	}
	if _, err := os.Stat(coldPath); err != nil {
		if os.IsNotExist(err) {
			if archived {
				return fmt.Errorf("archived disk %s: %w", coldPath, err)
			}
			return nil
		}
		return err
	}

	if err := os.MkdirAll(layout.vmDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(layout.vmDir, "rootfs.ext4.*.restore.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if err := copyRegularFile(tmpPath, coldPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, layout.diskPath); err != nil {
		return err
	}
	if err := m.store.SetVMArchivedDiskPath(ctx, vm.Name, ""); err != nil {
		return err
	}
	if err := os.Remove(coldPath); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("remove hydrated cold disk", "vm", vm.Name, "path", coldPath, "error", err)
	}
	if err := os.Remove(filepath.Dir(coldPath)); err != nil && !os.IsNotExist(err) {
		m.logger.Debug("remove cold vm directory", "vm", vm.Name, "error", err)
	}
	m.logger.Info("restored vm disk from cold storage", "vm", vm.Name, "path", coldPath)
	return nil
}
