package cgroup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentUnifiedCgroupPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(path, []byte("11:memory:/legacy\n0::/system.slice/firedozed.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := currentUnifiedCgroupPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/system.slice/firedozed.service" {
		t.Fatalf("path = %q", got)
	}
}

func TestSetupCreatesDelegatedTreeAndMovesDaemon(t *testing.T) {
	root := t.TempDir()
	writeFileForTest(t, filepath.Join(root, "cgroup.controllers"), "cpu memory io\n")
	serviceDir := filepath.Join(root, "system.slice", "firedozed.service")
	writeFileForTest(t, filepath.Join(serviceDir, "cgroup.controllers"), "cpu memory io\n")
	writeFileForTest(t, filepath.Join(serviceDir, "cgroup.subtree_control"), "")

	m := newManager(root, "/system.slice/firedozed.service")
	writeFileForTest(t, filepath.Join(m.vmsDir, "cgroup.controllers"), "cpu memory io\n")
	writeFileForTest(t, filepath.Join(m.vmsDir, "cgroup.subtree_control"), "")
	if err := m.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}

	assertFileContains(t, filepath.Join(m.daemonDir, "cgroup.procs"), "")
	assertFileContains(t, filepath.Join(m.serviceDir, "cgroup.subtree_control"), "+cpu +memory +io")
	assertFileContains(t, filepath.Join(m.vmsDir, "cgroup.subtree_control"), "+cpu +memory +io")
}

func TestAttachVMCreatesCgroupAndSetsEqualIOWeight(t *testing.T) {
	root := t.TempDir()
	m := newManager(root, "/firedozed.service")
	if err := os.MkdirAll(m.vmsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vmDir := filepath.Join(m.vmsDir, "vm-abc-123")
	writeFileForTest(t, filepath.Join(vmDir, "cpu.weight"), "50\n")
	writeFileForTest(t, filepath.Join(vmDir, "io.weight"), "default 50\n")

	path, err := m.AttachVM(context.Background(), "abc-123", 42)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(path, "firedoze-vms/vm-abc-123") {
		t.Fatalf("vm cgroup path = %q", path)
	}
	assertFileContains(t, filepath.Join(path, "cgroup.procs"), "42")
	assertFileContains(t, filepath.Join(path, "cpu.weight"), "100")
	assertFileContains(t, filepath.Join(path, "io.weight"), "default 100")
}

func TestCommandWrapsProcessIntoCgroupBeforeExec(t *testing.T) {
	cmd := Command("/usr/bin/firecracker", []string{"--api-sock", "/tmp/fc.sock"}, "/sys/fs/cgroup/firedoze-vms/vm-demo")
	if cmd.Path != "/bin/sh" {
		t.Fatalf("path = %q, want /bin/sh", cmd.Path)
	}
	if strings.Join(cmd.Args[:4], "\x00") != strings.Join([]string{"/bin/sh", "-c", `printf '%s\n' "$$" > "$FIREDOZE_CGROUP_PROCS"; exec "$@"`, "firedoze-cgroup-exec"}, "\x00") {
		t.Fatalf("unexpected shell wrapper args: %#v", cmd.Args)
	}
	if cmd.Args[4] != "/usr/bin/firecracker" || cmd.Args[5] != "--api-sock" || cmd.Args[6] != "/tmp/fc.sock" {
		t.Fatalf("wrapped firecracker args = %#v", cmd.Args)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "FIREDOZE_CGROUP_PROCS=/sys/fs/cgroup/firedoze-vms/vm-demo/cgroup.procs") {
		t.Fatalf("env missing cgroup.procs: %#v", cmd.Env)
	}
}

func TestReadUsageAggregatesCgroupStats(t *testing.T) {
	dir := t.TempDir()
	writeFileForTest(t, filepath.Join(dir, "memory.current"), "1048576\n")
	writeFileForTest(t, filepath.Join(dir, "memory.peak"), "2097152\n")
	writeFileForTest(t, filepath.Join(dir, "cpu.stat"), "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\nnr_throttled 2\nthrottled_usec 3000\n")
	writeFileForTest(t, filepath.Join(dir, "cpu.weight"), "100\n")
	writeFileForTest(t, filepath.Join(dir, "io.stat"), "8:0 rbytes=100 wbytes=200 rios=1 wios=2\n8:16 rbytes=300 wbytes=400\n")
	writeFileForTest(t, filepath.Join(dir, "io.weight"), "default 100\n")

	usage, err := ReadUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if usage.MemoryCurrentBytes != 1048576 || usage.MemoryPeakBytes != 2097152 {
		t.Fatalf("memory usage = %#v", usage)
	}
	if usage.CPUUsageSeconds != 1.5 || usage.CPUUserSeconds != 1 || usage.CPUSystemSeconds != 0.5 {
		t.Fatalf("cpu usage = %#v", usage)
	}
	if usage.CPUThrottledEvents != 2 || usage.CPUThrottledSeconds != 0.003 {
		t.Fatalf("throttling usage = %#v", usage)
	}
	if usage.CPUWeight != 100 {
		t.Fatalf("cpu weight = %d, want 100", usage.CPUWeight)
	}
	if usage.IOReadBytes != 400 || usage.IOWriteBytes != 600 || usage.IOWeight != 100 {
		t.Fatalf("io usage = %#v", usage)
	}
}

func writeFileForTest(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFileContains(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want containing %q", path, data, want)
	}
}
