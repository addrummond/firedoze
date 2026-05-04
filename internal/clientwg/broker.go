package clientwg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const maxBrokerLineLength = 4096

var ErrBrokerAlreadyRunning = errors.New("wireguard broker already running")

type BrokerDialer struct {
	SocketPath string
}

func (d BrokerDialer) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", d.SocketPath)
	if err != nil {
		return nil, err
	}
	clearDeadline := setDeadlineFromContext(conn, ctx)
	if _, err := fmt.Fprintf(conn, "CONNECT %s %s\n", network, address); err != nil {
		_ = conn.Close()
		return nil, err
	}
	line, err := readBrokerLine(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if line != "OK" {
		_ = conn.Close()
		return nil, fmt.Errorf("wireguard broker: %s", strings.TrimPrefix(line, "ERR "))
	}
	clearDeadline()
	return conn, nil
}

func PingBroker(ctx context.Context, socketPath string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	clearDeadline := setDeadlineFromContext(conn, ctx)
	defer clearDeadline()
	if _, err := io.WriteString(conn, "PING\n"); err != nil {
		return err
	}
	line, err := readBrokerLine(conn)
	if err != nil {
		return err
	}
	if line != "OK" {
		return fmt.Errorf("unexpected broker response: %s", line)
	}
	return nil
}

func RunBroker(ctx context.Context, cfg Config, socketPath string, idleTimeout time.Duration) error {
	if idleTimeout <= 0 {
		idleTimeout = 10 * time.Minute
	}
	if err := ensureBrokerSocketDir(filepath.Dir(socketPath)); err != nil {
		return err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	err := PingBroker(pingCtx, socketPath)
	cancel()
	if err == nil {
		return ErrBrokerAlreadyRunning
	}

	releaseLock, err := acquireBrokerLock(ctx, socketPath)
	if err != nil {
		return err
	}
	defer releaseLock()

	wgClient, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	defer wgClient.Close()

	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return err
	}

	unixListener, _ := listener.(*net.UnixListener)
	var active atomic.Int64
	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())

	for {
		if unixListener != nil {
			_ = unixListener.SetDeadline(time.Now().Add(30 * time.Second))
		}
		conn, err := listener.Accept()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				if active.Load() == 0 && time.Since(time.Unix(0, lastActive.Load())) > idleTimeout {
					return nil
				}
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		active.Add(1)
		lastActive.Store(time.Now().UnixNano())
		go func() {
			defer active.Add(-1)
			defer lastActive.Store(time.Now().UnixNano())
			handleBrokerConn(ctx, wgClient, conn)
		}()
	}
}

func handleBrokerConn(ctx context.Context, wgClient *Client, conn net.Conn) {
	defer conn.Close()
	line, err := readBrokerLine(conn)
	if err != nil {
		return
	}
	if line == "PING" {
		_, _ = io.WriteString(conn, "OK\n")
		return
	}
	network, address, ok := strings.Cut(strings.TrimPrefix(line, "CONNECT "), " ")
	if !strings.HasPrefix(line, "CONNECT ") || !ok || address == "" {
		_, _ = io.WriteString(conn, "ERR expected CONNECT <network> <address>\n")
		return
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		_, _ = fmt.Fprintf(conn, "ERR unsupported network %q\n", network)
		return
	}
	upstream, err := wgClient.DialContext(ctx, network, address)
	if err != nil {
		_, _ = fmt.Fprintf(conn, "ERR %s\n", sanitizeBrokerError(err))
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(conn, "OK\n"); err != nil {
		return
	}
	proxyConn(conn, upstream)
}

func proxyConn(a net.Conn, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		_ = a.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		_ = b.Close()
		done <- struct{}{}
	}()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

func readBrokerLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < maxBrokerLineLength {
		n, err := conn.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimSuffix(b.String(), "\r"), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("broker line too long")
}

func sanitizeBrokerError(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	return msg
}

func setDeadlineFromContext(conn net.Conn, ctx context.Context) func() {
	deadline, ok := ctx.Deadline()
	if !ok {
		return func() {}
	}
	_ = conn.SetDeadline(deadline)
	return func() {
		_ = conn.SetDeadline(time.Time{})
	}
}

func acquireBrokerLock(ctx context.Context, socketPath string) (func(), error) {
	lockPath := socketPath + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := os.Mkdir(lockPath, 0o700); err == nil {
			pidPath := filepath.Join(lockPath, "pid")
			if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
				_ = os.RemoveAll(lockPath)
				return nil, err
			}
			return func() {
				_ = os.Remove(pidPath)
				_ = os.Remove(lockPath)
			}, nil
		} else if !os.IsExist(err) {
			return nil, err
		}

		pingCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		err := PingBroker(pingCtx, socketPath)
		cancel()
		if err == nil {
			return nil, ErrBrokerAlreadyRunning
		}

		if staleBrokerLock(lockPath) {
			_ = os.RemoveAll(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("wireguard broker lock is held: %s", lockPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func ensureBrokerSocketDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func staleBrokerLock(lockPath string) bool {
	pidBytes, err := os.ReadFile(filepath.Join(lockPath, "pid"))
	if err == nil {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if parseErr == nil {
			if !brokerProcessAlive(pid) {
				return true
			}
			return brokerLockOlderThan(lockPath, 30*time.Second)
		}
	}
	return brokerLockOlderThan(lockPath, 2*time.Second)
}

func brokerLockOlderThan(lockPath string, age time.Duration) bool {
	info, statErr := os.Stat(lockPath)
	if statErr != nil {
		return true
	}
	return time.Since(info.ModTime()) > age
}
