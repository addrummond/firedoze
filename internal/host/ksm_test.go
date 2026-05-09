package host

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureKSMEnablesAndTunesHostKSM(t *testing.T) {
	root := t.TempDir()
	writeTestKSMFile(t, root, "run", "0\n")
	writeTestKSMFile(t, root, "pages_to_scan", "100\n")
	writeTestKSMFile(t, root, "sleep_millisecs", "20\n")
	writeTestKSMFile(t, root, "pages_sharing", "0\n")
	restore := setTestKSMRoot(root)
	defer restore()

	if err := newSilentLinuxOps().EnsureKSM(context.Background()); err != nil {
		t.Fatalf("EnsureKSM: %v", err)
	}

	if got := readTestKSMFile(t, root, "run"); got != "1" {
		t.Fatalf("run = %q, want 1", got)
	}
	if got := readTestKSMFile(t, root, "pages_to_scan"); got != ksmPagesToScan {
		t.Fatalf("pages_to_scan = %q, want %s", got, ksmPagesToScan)
	}
	if got := readTestKSMFile(t, root, "sleep_millisecs"); got != ksmSleepMillis {
		t.Fatalf("sleep_millisecs = %q, want %s", got, ksmSleepMillis)
	}
}

func TestEnsureKSMAlreadyEnabledDoesNotRequireOptionalTuningFiles(t *testing.T) {
	root := t.TempDir()
	writeTestKSMFile(t, root, "run", "1\n")
	restore := setTestKSMRoot(root)
	defer restore()

	if err := newSilentLinuxOps().EnsureKSM(context.Background()); err != nil {
		t.Fatalf("EnsureKSM already enabled: %v", err)
	}
}

func TestEnsureKSMMissingSysfsIsNonFatal(t *testing.T) {
	restore := setTestKSMRoot(filepath.Join(t.TempDir(), "missing"))
	defer restore()

	if err := newSilentLinuxOps().EnsureKSM(context.Background()); err != nil {
		t.Fatalf("EnsureKSM missing sysfs: %v", err)
	}
}

func setTestKSMRoot(root string) func() {
	old := ksmRootPath
	ksmRootPath = root
	return func() {
		ksmRootPath = old
	}
}

func newSilentLinuxOps() *LinuxOps {
	return NewLinuxOps(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func writeTestKSMFile(t *testing.T, root string, name string, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestKSMFile(t *testing.T, root string, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}
