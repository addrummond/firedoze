package firecracker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"firedoze/internal/model"
	"firedoze/internal/store"

	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	fcops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
)

const linuxClockTicksPerSecond = 100

func (m *Manager) ListVMResourceUsage(ctx context.Context, namePatterns []string) ([]model.VMResourceUsage, error) {
	vms, err := m.store.ListVMsMatching(ctx, namePatterns)
	if err != nil {
		return nil, err
	}
	usages := make([]model.VMResourceUsage, 0, len(vms))
	for _, vm := range vms {
		usage := m.vmResourceUsage(ctx, vm)
		usages = append(usages, usage)
	}
	return usages, nil
}

func (m *Manager) VMResourceUsage(ctx context.Context, name string) (model.VMResourceUsage, error) {
	vm, err := m.store.GetVM(ctx, name)
	if err != nil {
		return model.VMResourceUsage{}, err
	}
	return m.vmResourceUsage(ctx, vm), nil
}

func (m *Manager) vmResourceUsage(ctx context.Context, vm store.VM) model.VMResourceUsage {
	usage := model.VMResourceUsage{
		Name:      vm.Name,
		State:     vm.State,
		VCPUs:     vm.VCPUs,
		MemoryMiB: vm.MemoryMiB,
		DiskBytes: vm.DiskBytes,
	}
	if diskPath, err := m.vmDiskPath(vm); err == nil {
		if _, allocated, err := fileDiskUsage(diskPath); err == nil {
			usage.DiskAllocatedBytes = allocated
		}
	}

	m.mu.Lock()
	proc, running := m.running[vm.Name]
	m.mu.Unlock()
	if !running || proc == nil || proc.Command == nil || proc.Command.Process == nil {
		return usage
	}

	pid := proc.Command.Process.Pid
	processUsage, err := readProcessResourceUsage(pid)
	if err != nil {
		m.logger.Debug("read firecracker process resource usage", "vm", vm.Name, "pid", pid, "error", err)
	} else {
		usage.Process = &processUsage
	}

	if m.cfg.Balloon.Enabled {
		balloonUsage, err := m.balloonResourceUsage(ctx, proc.SocketPath)
		if err != nil {
			m.logger.Debug("read firecracker balloon usage", "vm", vm.Name, "error", err)
		} else {
			usage.Balloon = &balloonUsage
		}
	}

	return usage
}

func fileDiskUsage(path string) (int64, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	allocated := info.Size()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		allocated = stat.Blocks * 512
	}
	return info.Size(), allocated, nil
}

func readProcessResourceUsage(pid int) (model.ProcessResourceUsage, error) {
	usage := model.ProcessResourceUsage{PID: pid}

	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return usage, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		switch key {
		case "VmRSS":
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				usage.RSSBytes = value * 1024
			}
		case "VmSize":
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				usage.VMSizeBytes = value * 1024
			}
		case "Threads":
			if value, err := strconv.Atoi(fields[1]); err == nil {
				usage.Threads = value
			}
		}
	}

	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if cpuSeconds, ok := parseProcStatCPUSeconds(string(stat)); ok {
			usage.CPUSeconds = cpuSeconds
		}
	}
	return usage, nil
}

func parseProcStatCPUSeconds(raw string) (float64, bool) {
	closingParen := strings.LastIndex(raw, ")")
	if closingParen < 0 || closingParen+2 > len(raw) {
		return 0, false
	}
	fields := strings.Fields(raw[closingParen+2:])
	if len(fields) <= 12 {
		return 0, false
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, false
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(utime+stime) / linuxClockTicksPerSecond, true
}

func (m *Manager) balloonResourceUsage(ctx context.Context, socketPath string) (model.BalloonResourceUsage, error) {
	stats, err := firecrackerDescribeBalloonStats(ctx, socketPath)
	if err != nil {
		return model.BalloonResourceUsage{}, err
	}
	usage := model.BalloonResourceUsage{
		Enabled:         true,
		AvailableBytes:  stats.AvailableMemory,
		FreeBytes:       stats.FreeMemory,
		DiskCachesBytes: stats.DiskCaches,
		TotalBytes:      stats.TotalMemory,
		SwapInBytes:     stats.SwapIn,
		SwapOutBytes:    stats.SwapOut,
	}
	if stats.TargetMib != nil {
		usage.TargetMiB = *stats.TargetMib
	}
	if stats.ActualMib != nil {
		usage.ActualMiB = *stats.ActualMib
	}
	return usage, nil
}

func firecrackerDescribeBalloonStats(ctx context.Context, socketPath string) (*fcmodels.BalloonStats, error) {
	params := fcops.NewDescribeBalloonStatsParamsWithContext(ctx)
	resp, err := firecrackerOperations(socketPath).DescribeBalloonStats(params)
	if err != nil {
		return nil, fmt.Errorf("firecracker describe balloon stats: %w", err)
	}
	return resp.GetPayload(), nil
}

func firecrackerSetBalloonTarget(ctx context.Context, socketPath string, targetMiB int64) error {
	params := fcops.NewPatchBalloonParamsWithContext(ctx)
	params.SetBody(&fcmodels.BalloonUpdate{
		AmountMib: int64Ptr(targetMiB),
	})
	if _, err := firecrackerOperations(socketPath).PatchBalloon(params); err != nil {
		return fmt.Errorf("firecracker set balloon target: %w", err)
	}
	return nil
}

func int64Ptr(value int64) *int64 {
	return &value
}
