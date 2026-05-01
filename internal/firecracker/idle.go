package firecracker

import (
	"context"
	"log/slog"
	"time"

	"firedoze/internal/store"

	"github.com/vishvananda/netlink"
)

type Reconciler interface {
	Reconcile(context.Context) error
}

type IdleMonitor struct {
	manager    *Manager
	reconciler Reconciler
	logger     *slog.Logger
	seen       map[string]idleObservation
}

type idleObservation struct {
	bytes      uint64
	lastActive time.Time
}

func NewIdleMonitor(manager *Manager, reconciler Reconciler, logger *slog.Logger) *IdleMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &IdleMonitor{
		manager:    manager,
		reconciler: reconciler,
		logger:     logger,
		seen:       make(map[string]idleObservation),
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

	running := make(map[string]struct{}, len(vms))
	for _, vm := range vms {
		if vm.State != "running" {
			delete(m.seen, vm.Name)
			continue
		}
		running[vm.Name] = struct{}{}

		threshold := m.threshold(vm)
		if threshold <= 0 {
			delete(m.seen, vm.Name)
			continue
		}

		total, ok := tapTrafficBytes(vm.Name)
		if !ok {
			continue
		}
		obs, ok := m.seen[vm.Name]
		if !ok || obs.bytes != total {
			m.seen[vm.Name] = idleObservation{
				bytes:      total,
				lastActive: now,
			}
			continue
		}
		if now.Sub(obs.lastActive) < threshold {
			continue
		}

		m.sleepIdleVM(ctx, vm, now.Sub(obs.lastActive))
		delete(m.seen, vm.Name)
	}

	for name := range m.seen {
		if _, ok := running[name]; !ok {
			delete(m.seen, name)
		}
	}
}

func (m *IdleMonitor) threshold(vm store.VM) time.Duration {
	seconds := vm.IdleSleepAfterSeconds
	if seconds == 0 {
		seconds = m.manager.cfg.Idle.DefaultSleepAfterSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (m *IdleMonitor) sleepIdleVM(ctx context.Context, vm store.VM, idleFor time.Duration) {
	sleepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if _, err := m.manager.SleepVM(sleepCtx, vm.Name); err != nil {
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

func tapTrafficBytes(vmName string) (uint64, bool) {
	link, err := netlink.LinkByName(tapName(vmName))
	if err != nil {
		return 0, false
	}
	stats := link.Attrs().Statistics
	if stats == nil {
		return 0, false
	}
	return stats.RxBytes + stats.TxBytes, true
}
