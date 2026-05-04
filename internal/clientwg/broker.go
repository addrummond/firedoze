package clientwg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const maxBrokerLineLength = 4096

type BrokerDialer struct {
	SocketPath string
}

func (d BrokerDialer) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", d.SocketPath)
	if err != nil {
		return nil, err
	}
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
	return conn, nil
}

func PingBroker(ctx context.Context, socketPath string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
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
	wgClient, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	defer wgClient.Close()

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath)
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
