package firecracker

import (
	"context"
	"log/slog"
	"time"

	"firedoze/internal/store"
)

type Reconciler interface {
	Reconcile(context.Context) error
}

type IdleMonitor struct {
	manager    *Manager
	reconciler Reconciler
	logger     *slog.Logger
}

func NewIdleMonitor(manager *Manager, reconciler Reconciler, logger *slog.Logger) *IdleMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &IdleMonitor{
		manager:    manager,
		reconciler: reconciler,
		logger:     logger,
	}
}

func (m *IdleMonitor) Run(ctx context.Context) {
	interval := time.Duration(m.manager.cfg.Idle.CheckIntervalSeconds) * time.Second
	if m.manager.cfg.Idle.DefaultSleepAfterSeconds == 0 {
		m.logger.Info("idle monitor disabled")
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.logger.Info("idle monitor started", "check_interval", interval)
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

func (m *IdleMonitor) check(ctx context.Context, now time.Time) {
	vms, err := m.manager.ListVMs(ctx)
	if err != nil {
		m.logger.Warn("idle monitor list vms", "error", err)
		return
	}

	for _, vm := range vms {
		if vm.State != "running" {
			continue
		}

		threshold := m.threshold(vm)
		if threshold <= 0 {
			continue
		}

		lastActive, ok := vmLastActivity(vm)
		if !ok || now.Sub(lastActive) < threshold {
			continue
		}

		m.sleepIdleVM(ctx, vm, now.Sub(lastActive))
	}
}

func (m *IdleMonitor) threshold(vm store.VM) time.Duration {
	seconds := vm.IdleSleepAfterSeconds
	if seconds == 0 {
		seconds = m.manager.cfg.Idle.DefaultSleepAfterSeconds
	}
	return time.Duration(seconds) * time.Second
}

func vmLastActivity(vm store.VM) (time.Time, bool) {
	for _, raw := range []string{vm.LastActivityAt, vm.LastStartedAt} {
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (m *IdleMonitor) sleepIdleVM(ctx context.Context, vm store.VM, idleFor time.Duration) {
	sleepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if _, err := m.manager.SleepVM(sleepCtx, vm.UUID); err != nil {
		m.logger.Warn("idle sleep failed", "vm", vm.Name, "idle_for", idleFor, "error", err)
		return
	}
	m.logger.Info("idle slept vm", "vm", vm.Name, "idle_for", idleFor)

	if m.reconciler == nil {
		return
	}
	if err := m.reconciler.Reconcile(ctx); err != nil {
		m.logger.Warn("reconcile proxy after idle sleep", "vm", vm.Name, "error", err)
	}
}
