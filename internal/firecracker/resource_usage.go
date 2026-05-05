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
		Name:         vm.Name,
		State:        vm.State,
		VCPUs:        vm.VCPUs,
		MemoryMinMiB: vm.MemoryMinMiB,
		MemoryMaxMiB: vm.MemoryMaxMiB,
		DiskBytes:    vm.DiskBytes,
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
	if vm.MemoryMaxMiB > vm.MemoryMinMiB {
		status, err := firecrackerGetMemoryHotplug(ctx, proc.SocketPath)
		if err != nil {
			m.logger.Debug("read firecracker memory hotplug status", "vm", vm.Name, "error", err)
		} else {
			usage.MemoryHotplug = &model.MemoryHotplugUsage{
				TotalMiB:     status.TotalSizeMiB,
				RequestedMiB: status.RequestedSizeMiB,
				PluggedMiB:   status.PluggedSizeMiB,
				EffectiveMiB: vm.MemoryMinMiB + status.PluggedSizeMiB,
			}
		}
	}
	processUsage, err := readProcessResourceUsage(pid)
	if err != nil {
		m.logger.Debug("read firecracker process resource usage", "vm", vm.Name, "pid", pid, "error", err)
	} else {
		usage.Process = &processUsage
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
