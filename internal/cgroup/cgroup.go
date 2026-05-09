package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultRoot    = "/sys/fs/cgroup"
	vmGroupName    = "firedoze-vms"
	daemonName     = "firedoze-daemon"
	cpuWeightValue = "100\n"
	ioWeightValue  = "default 100\n"
)

var ErrUnavailable = errors.New("cgroup v2 unavailable")

type Manager struct {
	root       string
	service    string
	serviceDir string
	daemonDir  string
	vmsDir     string
}

type Usage struct {
	MemoryCurrentBytes  uint64
	MemoryPeakBytes     uint64
	CPUUsageSeconds     float64
	CPUUserSeconds      float64
	CPUSystemSeconds    float64
	CPUThrottledSeconds float64
	CPUThrottledEvents  uint64
	CPUWeight           int
	IOReadBytes         uint64
	IOWriteBytes        uint64
	IOWeight            int
}

func New(root string) (*Manager, error) {
	if root == "" {
		root = DefaultRoot
	}
	service, err := currentUnifiedCgroupPath("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	return newManager(root, service), nil
}

func newManager(root string, service string) *Manager {
	service = cleanCgroupPath(service)
	serviceDir := filepath.Join(root, service)
	return &Manager{
		root:       root,
		service:    service,
		serviceDir: serviceDir,
		daemonDir:  filepath.Join(serviceDir, daemonName),
		vmsDir:     filepath.Join(serviceDir, vmGroupName),
	}
}

func (m *Manager) Setup(ctx context.Context) error {
	if err := requireUnified(m.root); err != nil {
		return err
	}
	if err := os.MkdirAll(m.daemonDir, 0o755); err != nil {
		return fmt.Errorf("create daemon cgroup: %w", err)
	}
	if err := os.MkdirAll(m.vmsDir, 0o755); err != nil {
		return fmt.Errorf("create vm cgroup parent: %w", err)
	}
	if err := writeFile(filepath.Join(m.daemonDir, "cgroup.procs"), strconv.Itoa(os.Getpid())+"\n"); err != nil {
		return fmt.Errorf("move daemon to child cgroup: %w", err)
	}
	if err := enableControllers(m.serviceDir, "cpu", "memory", "io"); err != nil {
		return fmt.Errorf("enable service cgroup controllers: %w", err)
	}
	if err := enableControllers(m.vmsDir, "cpu", "memory", "io"); err != nil {
		return fmt.Errorf("enable vm cgroup controllers: %w", err)
	}
	_ = ctx
	return nil
}

func (m *Manager) AttachVM(ctx context.Context, uuid string, pid int) (string, error) {
	dir, err := m.PrepareVM(ctx, uuid)
	if err != nil {
		return "", err
	}
	if err := writeFile(filepath.Join(dir, "cgroup.procs"), strconv.Itoa(pid)+"\n"); err != nil {
		return "", fmt.Errorf("move firecracker process to vm cgroup: %w", err)
	}
	_ = ctx
	return dir, nil
}

func (m *Manager) PrepareVM(ctx context.Context, uuid string) (string, error) {
	dir := filepath.Join(m.vmsDir, vmCgroupName(uuid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create vm cgroup: %w", err)
	}
	if err := writeOptionalFile(filepath.Join(dir, "cpu.weight"), cpuWeightValue); err != nil {
		return "", fmt.Errorf("set vm cpu weight: %w", err)
	}
	if err := writeOptionalFile(filepath.Join(dir, "io.weight"), ioWeightValue); err != nil {
		return "", fmt.Errorf("set vm io weight: %w", err)
	}
	_ = ctx
	return dir, nil
}

func Remove(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func ReadUsage(path string) (Usage, error) {
	if path == "" {
		return Usage{}, ErrUnavailable
	}
	var usage Usage
	if value, err := readUintFile(filepath.Join(path, "memory.current")); err == nil {
		usage.MemoryCurrentBytes = value
	}
	if value, err := readUintFile(filepath.Join(path, "memory.peak")); err == nil {
		usage.MemoryPeakBytes = value
	}
	if err := readCPUStat(filepath.Join(path, "cpu.stat"), &usage); err != nil && !os.IsNotExist(err) {
		return usage, err
	}
	if weight, err := readWeight(filepath.Join(path, "cpu.weight")); err == nil {
		usage.CPUWeight = weight
	}
	if err := readIOStat(filepath.Join(path, "io.stat"), &usage); err != nil && !os.IsNotExist(err) {
		return usage, err
	}
	if weight, err := readWeight(filepath.Join(path, "io.weight")); err == nil {
		usage.IOWeight = weight
	}
	return usage, nil
}

func currentUnifiedCgroupPath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			return fields[2], nil
		}
	}
	return "", ErrUnavailable
}

func requireUnified(root string) error {
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		if os.IsNotExist(err) {
			return ErrUnavailable
		}
		return err
	}
	return nil
}

func enableControllers(dir string, requested ...string) error {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return err
	}
	available := map[string]bool{}
	for _, controller := range strings.Fields(string(data)) {
		available[controller] = true
	}
	var values []string
	for _, controller := range requested {
		if available[controller] {
			values = append(values, "+"+controller)
		}
	}
	if len(values) == 0 {
		return nil
	}
	return writeFile(filepath.Join(dir, "cgroup.subtree_control"), strings.Join(values, " ")+"\n")
}

func vmCgroupName(uuid string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", "\x00", "-")
	return "vm-" + replacer.Replace(uuid)
}

func cleanCgroupPath(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "/")
	return filepath.Clean(value)
}

func writeOptionalFile(path string, data string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return writeFile(path, data)
}

func writeFile(path string, data string) error {
	return os.WriteFile(path, []byte(data), 0o644)
}

func readUintFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func readCPUStat(path string, usage *Usage) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "usage_usec":
			usage.CPUUsageSeconds = float64(value) / 1_000_000
		case "user_usec":
			usage.CPUUserSeconds = float64(value) / 1_000_000
		case "system_usec":
			usage.CPUSystemSeconds = float64(value) / 1_000_000
		case "nr_throttled":
			usage.CPUThrottledEvents = value
		case "throttled_usec":
			usage.CPUThrottledSeconds = float64(value) / 1_000_000
		}
	}
	return nil
}

func readIOStat(path string, usage *Usage) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		for _, field := range fields[1:] {
			key, valueText, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			value, err := strconv.ParseUint(valueText, 10, 64)
			if err != nil {
				continue
			}
			switch key {
			case "rbytes":
				usage.IOReadBytes += value
			case "wbytes":
				usage.IOWriteBytes += value
			}
		}
	}
	return nil
}

func readWeight(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "default" {
			return strconv.Atoi(fields[1])
		}
		if len(fields) == 1 {
			return strconv.Atoi(fields[0])
		}
	}
	return 0, ErrUnavailable
}
