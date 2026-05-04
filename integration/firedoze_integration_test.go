//go:build integration && linux

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type commandResult struct {
	stdout string
	stderr string
}

type firedozeCommand struct {
	name string
	args []string
}

type integrationEnv struct {
	apiURL  string
	command firedozeCommand
	home    string
	root    string
}

func TestFiredozeClientSmokeIntegration(t *testing.T) {
	env := newIntegrationEnv(t)

	health := env.firedoze(t, 30*time.Second, "health")
	if got := strings.TrimSpace(health.stdout); got != "ok" {
		t.Fatalf("firedoze health stdout = %q, want ok; stderr:\n%s", got, health.stderr)
	}

	env.firedoze(t, 30*time.Second, "config")
	env.firedoze(t, 30*time.Second, "vm", "list", "-names")
}

func TestFiredozeVMLifecycleIntegration(t *testing.T) {
	env := newIntegrationEnv(t)
	if os.Getenv("FIREDOZE_INTEGRATION_LIFECYCLE") != "1" {
		t.Skip("set FIREDOZE_INTEGRATION_LIFECYCLE=1 to create, start, snapshot, restore, and delete real VMs")
	}

	suffix := fmt.Sprintf("%d-%d", time.Now().Unix(), os.Getpid())
	vmName := "it-" + suffix
	cloneName := vmName + "-clone"
	snapshotName := vmName + "-snap"

	t.Cleanup(func() {
		env.firedozeAllowFailure(t, 2*time.Minute, "snapshot", "delete", snapshotName)
		env.firedozeAllowFailure(t, 5*time.Minute, "vm", "delete", vmName, cloneName)
	})

	env.firedoze(t, 10*time.Minute, "vm", "create", vmName, "-memory-mib", "512")
	names := env.firedoze(t, 30*time.Second, "vm", "list", "-names", vmName)
	if !lineSetContains(names.stdout, vmName) {
		t.Fatalf("firedoze vm list -names %s did not include VM; stdout:\n%s\nstderr:\n%s", vmName, names.stdout, names.stderr)
	}

	env.firedoze(t, 5*time.Minute, "vm", "start", vmName)
	env.firedoze(t, 5*time.Minute, "exec", vmName, "--", "true")
	env.firedoze(t, 5*time.Minute, "vm", "sleep", vmName)
	env.firedoze(t, 5*time.Minute, "vm", "start", vmName)
	env.firedoze(t, 5*time.Minute, "exec", vmName, "--", "true")
	env.firedoze(t, 5*time.Minute, "vm", "stop", vmName)
	env.firedoze(t, 5*time.Minute, "snapshot", "save", snapshotName, vmName)
	env.firedoze(t, 5*time.Minute, "snapshot", "restore", snapshotName, cloneName, "-memory-mib", "512")
	env.firedoze(t, 5*time.Minute, "vm", "start", cloneName)
	env.firedoze(t, 5*time.Minute, "exec", cloneName, "--", "true")
	env.firedoze(t, 5*time.Minute, "vm", "stop", cloneName)
	env.firedoze(t, 5*time.Minute, "vm", "delete", vmName, cloneName)
	env.firedoze(t, 2*time.Minute, "snapshot", "delete", snapshotName)
}

func newIntegrationEnv(t *testing.T) integrationEnv {
	t.Helper()
	if os.Getenv("FIREDOZE_INTEGRATION") != "1" {
		t.Skip("set FIREDOZE_INTEGRATION=1 on a configured Linux host to run Firedoze integration tests")
	}
	apiURL := strings.TrimSpace(os.Getenv("FIREDOZE_API"))
	if apiURL == "" {
		t.Skip("set FIREDOZE_API to the WireGuard-only Firedoze API URL")
	}

	root := repoRoot(t)
	home := t.TempDir()
	return integrationEnv{
		apiURL:  apiURL,
		command: resolveFiredozeCommand(t, root),
		home:    home,
		root:    root,
	}
}

func resolveFiredozeCommand(t *testing.T, root string) firedozeCommand {
	t.Helper()
	if bin := strings.TrimSpace(os.Getenv("FIREDOZE_BIN")); bin != "" {
		return firedozeCommand{name: bin}
	}
	if bin := filepath.Join(root, "firedoze"); fileExists(bin) {
		return firedozeCommand{name: bin}
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("FIREDOZE_BIN is unset, ./firedoze does not exist, and go is unavailable: %v", err)
	}
	return firedozeCommand{name: "go", args: []string{"run", "./cmd/firedoze"}}
}

func (e integrationEnv) firedoze(t *testing.T, timeout time.Duration, args ...string) commandResult {
	t.Helper()
	result, err := e.runFiredoze(timeout, args...)
	if err != nil {
		t.Fatalf("firedoze %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, result.stdout, result.stderr)
	}
	return result
}

func (e integrationEnv) firedozeAllowFailure(t *testing.T, timeout time.Duration, args ...string) {
	t.Helper()
	result, err := e.runFiredoze(timeout, args...)
	if err != nil {
		t.Logf("cleanup firedoze %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, result.stdout, result.stderr)
	}
}

func (e integrationEnv) runFiredoze(timeout time.Duration, args ...string) (commandResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmdArgs := append([]string{}, e.command.args...)
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, e.command.name, cmdArgs...)
	cmd.Dir = e.root
	cmd.Env = append(os.Environ(),
		"FIREDOZE_API="+e.apiURL,
		"HOME="+e.home,
		"XDG_CONFIG_HOME="+filepath.Join(e.home, ".config"),
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("timed out after %s", timeout)
	}
	return result, err
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate integration test file")
	}
	dir := filepath.Dir(file)
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root from %s", file)
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lineSetContains(output string, want string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
