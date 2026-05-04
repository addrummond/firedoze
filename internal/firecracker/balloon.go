package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"firedoze/internal/store"
)

const minBalloonAdjustmentMiB = 8

type BalloonMonitor struct {
	manager *Manager
	logger  *slog.Logger
}

func NewBalloonMonitor(manager *Manager, logger *slog.Logger) *BalloonMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &BalloonMonitor{
		manager: manager,
		logger:  logger,
	}
}

func (m *BalloonMonitor) Run(ctx context.Context) {
	if !m.manager.cfg.Balloon.Enabled {
		m.logger.Info("balloon monitor disabled")
		return
	}

	interval := time.Duration(m.manager.cfg.Balloon.ReclaimIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.logger.Info("balloon monitor started", "check_interval", interval, "min_free_mib", m.manager.cfg.Balloon.ReclaimMinFreeMiB)
	m.check(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.check(ctx)
		}
	}
}

func (m *BalloonMonitor) check(ctx context.Context) {
	if err := m.manager.ReclaimRunningVMMemory(ctx); err != nil {
		m.logger.Warn("balloon reclaim failed", "error", err)
	}
}

func (m *Manager) ReclaimRunningVMMemory(ctx context.Context) error {
	if !m.cfg.Balloon.Enabled {
		return nil
	}

	m.mu.Lock()
	procs := make(map[string]*Process, len(m.running))
	for name, proc := range m.running {
		procs[name] = proc
	}
	m.mu.Unlock()

	var errs []error
	for name, proc := range procs {
		if proc == nil {
			continue
		}
		if err := m.reclaimVMMemory(ctx, name, proc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) reclaimVMMemory(ctx context.Context, name string, proc *Process) error {
	vm, err := m.store.GetVM(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	stats, err := firecrackerDescribeBalloonStats(ctx, proc.SocketPath)
	if err != nil {
		m.logger.Debug("skip balloon reclaim", "vm", name, "error", err)
		return nil
	}
	if stats == nil || stats.ActualMib == nil || stats.TargetMib == nil {
		return nil
	}
	if stats.AvailableMemory == 0 && stats.TotalMemory == 0 {
		return nil
	}

	targetMiB := balloonTargetMiB(vm.MemoryMiB, *stats.ActualMib, stats.AvailableMemory, m.cfg.Balloon.ReclaimMinFreeMiB)
	if absInt64(targetMiB-*stats.TargetMib) < minBalloonAdjustmentMiB {
		return nil
	}
	if err := firecrackerSetBalloonTarget(ctx, proc.SocketPath, targetMiB); err != nil {
		return err
	}
	m.logger.Debug("updated balloon target", "vm", name, "target_mib", targetMiB, "actual_mib", *stats.ActualMib, "available_bytes", stats.AvailableMemory)
	return nil
}

func balloonTargetMiB(memoryMiB int, actualMiB int64, availableBytes int64, minFreeMiB int) int64 {
	maxTarget := int64(memoryMiB - minFreeMiB)
	if maxTarget < 0 {
		maxTarget = 0
	}
	availableMiB := availableBytes / (1024 * 1024)
	target := actualMiB + availableMiB - int64(minFreeMiB)
	if target < 0 {
		target = 0
	}
	if target > maxTarget {
		target = maxTarget
	}
	return target
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
