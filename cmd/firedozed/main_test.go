package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"firedoze/internal/config"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestRunInitConfigWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firedoze.toml")
	code, stdout, stderr := captureRun(t, "-config", path, "-init-config", "-init-host", "firedoze.example")
	if code != 0 {
		t.Fatalf("run exit = %d, stderr = %s", code, stderr)
	}
	if strings.TrimSpace(stdout) != path {
		t.Fatalf("stdout = %q, want config path", stdout)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `base_domain = "firedoze.example"`) {
		t.Fatalf("config did not use init host as base domain:\n%s", text)
	}
	if !strings.Contains(text, `endpoint = "firedoze.example:51820"`) {
		t.Fatalf("config did not use init host as endpoint:\n%s", text)
	}
}

func TestRunPrintHelpers(t *testing.T) {
	path, cfg, serverKey := writeTestConfig(t, nil)

	code, stdout, stderr := captureRun(t, "-config", path, "-print-api-env")
	if code != 0 {
		t.Fatalf("-print-api-env exit = %d, stderr = %s", code, stderr)
	}
	if got, want := stdout, "export FIREDOZE_API=\"http://[fd7a:115c:a1e1::1]\"\n"; got != want {
		t.Fatalf("-print-api-env stdout = %q, want %q", got, want)
	}

	code, stdout, stderr = captureRun(t, "-config", path, "-wg-server-public-key")
	if code != 0 {
		t.Fatalf("-wg-server-public-key exit = %d, stderr = %s", code, stderr)
	}
	if got, want := strings.TrimSpace(stdout), serverKey.PublicKey().String(); got != want {
		t.Fatalf("server public key = %q, want %q", got, want)
	}

	code, stdout, stderr = captureRun(t, "-config", path, "-print-config")
	if code != 0 {
		t.Fatalf("-print-config exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "state_dir = ") || !strings.Contains(stdout, cfg.StateDir) {
		t.Fatalf("-print-config did not include state dir %q:\n%s", cfg.StateDir, stdout)
	}
	if !strings.Contains(stdout, "listen_ip = ") || !strings.Contains(stdout, "fd7a:115c:a1e0::1") {
		t.Fatalf("-print-config did not include derived DNS listen IP:\n%s", stdout)
	}
}

func TestRunWireGuardPeerConfigAndAddPeer(t *testing.T) {
	aliceKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	path, _, serverKey := writeTestConfig(t, []config.WGPeer{
		{
			Name:       "alice-laptop",
			PublicKey:  aliceKey.PublicKey().String(),
			AllowedIPs: []string{"fd7a:115c:a1e1::2/128"},
		},
	})

	code, stdout, stderr := captureRun(t, "-config", path, "-wg-peer-config", "alice-laptop")
	if code != 0 {
		t.Fatalf("-wg-peer-config exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"# WireGuard client config template for alice-laptop.",
		"Address = fd7a:115c:a1e1::2/128",
		"PublicKey = " + serverKey.PublicKey().String(),
		"Endpoint = firedoze.example:51820",
		"AllowedIPs = fd7a:115c:a1e1::1/128, fd7a:115c:a1e0::/64",
		"#   firedoze server add firedoze http://[fd7a:115c:a1e1::1] -default",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("-wg-peer-config output missing %q:\n%s", want, stdout)
		}
	}

	bobKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = captureRun(t, "-config", path, "-wg-add-peer", "bob-laptop", bobKey.PublicKey().String())
	if code != 0 {
		t.Fatalf("-wg-add-peer exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "# WireGuard client config template for bob-laptop.") {
		t.Fatalf("-wg-add-peer did not print client template:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Address = fd7a:115c:a1e1::3/128") {
		t.Fatalf("-wg-add-peer did not allocate next free address:\n%s", stdout)
	}
	updated, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.WireGuard.Peers) != 2 {
		t.Fatalf("peers = %d, want 2", len(updated.WireGuard.Peers))
	}
	if got := updated.WireGuard.Peers[1]; got.Name != "bob-laptop" || got.PublicKey != bobKey.PublicKey().String() || strings.Join(got.AllowedIPs, ",") != "fd7a:115c:a1e1::3/128" {
		t.Fatalf("appended peer = %#v", got)
	}
}

func TestRunServeRequiresSetupWireGuard(t *testing.T) {
	path, _, _ := writeTestConfig(t, nil)
	code, _, stderr := captureRun(t, "-config", path, "-serve")
	if code != 1 {
		t.Fatalf("-serve without -setup-wireguard exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "refusing to serve API without -setup-wireguard") {
		t.Fatalf("stderr missing refusal message:\n%s", stderr)
	}
}

func TestRestartWakeFileLifecycle(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := writeRestartWakeFile(cfg, []string{"one", "two"}); err != nil {
		t.Fatalf("writeRestartWakeFile: %v", err)
	}
	path := restartWakePath(cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"one"`) || !strings.Contains(string(data), `"two"`) {
		t.Fatalf("restart wake file = %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("restart wake file mode = %v, want 0600", got)
	}
	if err := writeRestartWakeFile(cfg, nil); err != nil {
		t.Fatalf("clear restart wake file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("restart wake file still exists after clear: %v", err)
	}
}

func TestWakeRestartVMs(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := writeRestartWakeFile(cfg, []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	starter := &fakeRestartStarter{}
	if err := wakeRestartVMs(context.Background(), cfg, starter, discardLogger()); err != nil {
		t.Fatalf("wakeRestartVMs: %v", err)
	}
	if got := strings.Join(starter.names, ","); got != "one,two" {
		t.Fatalf("started names = %q, want one,two", got)
	}
	if _, err := os.Stat(restartWakePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("restart wake file still exists after wake: %v", err)
	}
}

func TestWakeRestartVMsMalformedJSON(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(restartWakePath(cfg), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := wakeRestartVMs(context.Background(), cfg, &fakeRestartStarter{}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("wakeRestartVMs malformed error = %v, want parse error", err)
	}
	if _, statErr := os.Stat(restartWakePath(cfg)); !os.IsNotExist(statErr) {
		t.Fatalf("malformed restart wake file should be removed before parse: %v", statErr)
	}
}

func TestWakeRestartVMsPropagatesStartFailure(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := writeRestartWakeFile(cfg, []string{"one"}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("start failed")
	err := wakeRestartVMs(context.Background(), cfg, &fakeRestartStarter{err: want}, discardLogger())
	if !errors.Is(err, want) {
		t.Fatalf("wakeRestartVMs error = %v, want %v", err, want)
	}
}

func TestWireGuardBindIP(t *testing.T) {
	tests := []struct {
		address string
		want    string
		ok      bool
	}{
		{address: "fd7a:115c:a1e1::1/64", want: "fd7a:115c:a1e1::1", ok: true},
		{address: "10.77.0.1/24", want: "10.77.0.1", ok: true},
		{address: "not-cidr", ok: false},
	}
	for _, tt := range tests {
		got, err := wireGuardBindIP(tt.address)
		if tt.ok && err != nil {
			t.Fatalf("wireGuardBindIP(%q): %v", tt.address, err)
		}
		if !tt.ok && err == nil {
			t.Fatalf("wireGuardBindIP(%q) succeeded, want error", tt.address)
		}
		if tt.ok && !got.Equal(net.ParseIP(tt.want)) {
			t.Fatalf("wireGuardBindIP(%q) = %s, want %s", tt.address, got, tt.want)
		}
	}
}

type fakeRestartStarter struct {
	names []string
	err   error
}

func (f *fakeRestartStarter) StartVMs(ctx context.Context, names []string) error {
	f.names = append(f.names, names...)
	return f.err
}

func writeTestConfig(t *testing.T, peers []config.WGPeer) (string, config.Config, wgtypes.Key) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.Metadata.Path = filepath.Join(cfg.StateDir, "firedoze.db")
	cfg.WireGuard.PrivateKeyFile = filepath.Join(dir, "etc", "wg.key")
	cfg.WireGuard.Endpoint = "firedoze.example:51820"
	cfg.WireGuard.Address = "fd7a:115c:a1e1::1/64"
	cfg.WireGuard.Peers = peers
	cfg.VMNetwork.Subnet = "fd7a:115c:a1e0::/64"

	serverKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.WireGuard.PrivateKeyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.WireGuard.PrivateKeyFile, []byte(serverKey.String()+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "firedoze.toml")
	if err := os.WriteFile(path, []byte(cfg.TOML()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	return path, cfg, serverKey
}

func captureRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = stdoutW
	os.Stderr = stderrW
	code := run(args)
	_ = stdoutW.Close()
	_ = stderrW.Close()
	stdout, readOutErr := io.ReadAll(stdoutR)
	stderr, readErrErr := io.ReadAll(stderrR)
	os.Stdout = oldStdout
	os.Stderr = oldStderr
	if readOutErr != nil {
		t.Fatal(readOutErr)
	}
	if readErrErr != nil {
		t.Fatal(readErrErr)
	}
	return code, string(stdout), string(stderr)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
