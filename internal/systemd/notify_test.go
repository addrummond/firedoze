package systemd

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNotifyWithoutSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if Notify("READY=1") {
		t.Fatal("Notify succeeded without NOTIFY_SOCKET")
	}
}

func TestNotifySendsUnixgram(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "notify.sock")
	conn := listenUnixgram(t, socket)
	t.Setenv("NOTIFY_SOCKET", socket)

	if !Notify("READY=1") {
		t.Fatal("Notify returned false")
	}
	if got := readUnixgram(t, conn); got != "READY=1" {
		t.Fatalf("notify datagram = %q, want READY=1", got)
	}
}

func TestNotifySendsAbstractUnixgramOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abstract unixgram sockets are Linux-specific")
	}
	name := "firedoze-test-notify-" + strings.ReplaceAll(t.Name(), "/", "-")
	conn := listenUnixgram(t, "\x00"+name)
	t.Setenv("NOTIFY_SOCKET", "@"+name)

	if !Notify("STOPPING=1") {
		t.Fatal("Notify returned false")
	}
	if got := readUnixgram(t, conn); got != "STOPPING=1" {
		t.Fatalf("notify datagram = %q, want STOPPING=1", got)
	}
}

func TestReadyAndStopping(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "notify.sock")
	conn := listenUnixgram(t, socket)
	t.Setenv("NOTIFY_SOCKET", socket)

	if !Ready() {
		t.Fatal("Ready returned false")
	}
	if got := readUnixgram(t, conn); got != "READY=1" {
		t.Fatalf("ready datagram = %q, want READY=1", got)
	}
	if !Stopping() {
		t.Fatal("Stopping returned false")
	}
	if got := readUnixgram(t, conn); got != "STOPPING=1" {
		t.Fatalf("stopping datagram = %q, want STOPPING=1", got)
	}
}

func TestStartWatchdog(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "watchdog.sock")
	conn := listenUnixgram(t, socket)
	t.Setenv("NOTIFY_SOCKET", socket)
	t.Setenv("WATCHDOG_USEC", "20000")

	stop := StartWatchdog(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stop()

	if got := readUnixgram(t, conn); got != "WATCHDOG=1" {
		t.Fatalf("watchdog datagram = %q, want WATCHDOG=1", got)
	}
}

func TestStartWatchdogInvalidEnvIsNoop(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "bad")
	stop := StartWatchdog(nil)
	stop()

	t.Setenv("WATCHDOG_USEC", "0")
	stop = StartWatchdog(nil)
	stop()
}

func listenUnixgram(t *testing.T, name string) *net.UnixConn {
	t.Helper()
	addr := &net.UnixAddr{Name: name, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("unixgram sockets are not permitted in this environment: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		if name != "" && name[0] != 0 {
			_ = os.Remove(name)
		}
	})
	return conn
}

func readUnixgram(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}
