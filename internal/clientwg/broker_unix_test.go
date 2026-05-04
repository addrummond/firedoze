//go:build !windows

package clientwg

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireBrokerLockRemovesStaleLock(t *testing.T) {
	socketPath := testSocketPath(t)
	lockPath := socketPath + ".lock"
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockPath, "pid"), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := acquireBrokerLock(context.Background(), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestAcquireBrokerLockWaitsForLiveOwner(t *testing.T) {
	socketPath := testSocketPath(t)
	release, err := acquireBrokerLock(context.Background(), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = acquireBrokerLock(ctx, socketPath)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquireBrokerLock error = %v, want context deadline exceeded", err)
	}
}

func TestAcquireBrokerLockDetectsRunningBroker(t *testing.T) {
	socketPath := testSocketPath(t)
	lockPath := socketPath + ".lock"
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockPath, "pid"), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.ReadAll(io.LimitReader(conn, 5))
		_, _ = io.WriteString(conn, "OK\n")
	}()

	_, err = acquireBrokerLock(context.Background(), socketPath)
	if !errors.Is(err, ErrBrokerAlreadyRunning) {
		t.Fatalf("acquireBrokerLock error = %v, want ErrBrokerAlreadyRunning", err)
	}
}

func TestPingBrokerHonorsContextDeadlineAfterDial(t *testing.T) {
	socketPath := testSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(time.Second)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := PingBroker(ctx, socketPath); err == nil {
		t.Fatal("PingBroker unexpectedly succeeded")
	}
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "firedoze-broker-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return filepath.Join(dir, "b.sock")
}
