package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/firecracker"
	"firedoze/internal/store"
)

const sshReadyTimeout = 2 * time.Minute

type TCPWakeProxy struct {
	cfg     config.Config
	store   *store.Store
	manager VMStarter
	logger  *slog.Logger
}

func NewTCPWakeProxy(cfg config.Config, st *store.Store, manager VMStarter, logger *slog.Logger) *TCPWakeProxy {
	if logger == nil {
		logger = slog.Default()
	}
	return &TCPWakeProxy{
		cfg:     cfg,
		store:   st,
		manager: manager,
		logger:  logger,
	}
}

func (p *TCPWakeProxy) RunSSH(ctx context.Context) error {
	if err := p.ensureSSHRedirect(ctx); err != nil {
		return err
	}
	defer func() {
		if err := p.deleteSSHRedirect(context.Background()); err != nil {
			p.logger.Warn("remove ssh wake redirect", "error", err)
		}
	}()

	bindIP, _, err := net.ParseCIDR(p.cfg.WireGuard.Address)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(bindIP.String(), strconv.Itoa(p.cfg.SSH.WakeProxyPort))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	errCh := make(chan error, 1)
	go func() {
		<-ctx.Done()
		errCh <- listener.Close()
	}()

	p.logger.Info("ssh wake proxy listening", "addr", addr)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			select {
			case closeErr := <-errCh:
				if errors.Is(closeErr, net.ErrClosed) {
					return nil
				}
				return closeErr
			default:
				return err
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.handleSSHConn(ctx, conn)
		}()
	}
}

func (p *TCPWakeProxy) handleSSHConn(ctx context.Context, client net.Conn) {
	defer client.Close()

	dst, err := originalDestination(client)
	if err != nil {
		p.logger.Warn("get original ssh destination", "remote", client.RemoteAddr(), "error", err)
		return
	}
	vm, err := p.vmByPrivateIP(ctx, dst.IP.String())
	if err != nil {
		p.logger.Warn("find vm for ssh wake", "ip", dst.IP, "error", err)
		return
	}
	if vm.State != "running" {
		started, err := p.manager.StartVM(ctx, vm.Name)
		if err != nil && !errors.Is(err, firecracker.ErrAlreadyRunning) {
			p.logger.Warn("wake vm for ssh", "vm", vm.Name, "ip", dst.IP, "error", err)
			return
		}
		if err == nil {
			vm = started
		} else if refreshed, refreshErr := p.store.GetVM(ctx, vm.Name); refreshErr == nil {
			vm = refreshed
		}
		p.logger.Info("woke vm for ssh", "vm", vm.Name, "ip", dst.IP)
	}
	if err := waitForTCP(ctx, net.JoinHostPort(vm.PrivateIP, "22"), sshReadyTimeout); err != nil {
		p.logger.Warn("wait for vm ssh", "vm", vm.Name, "ip", vm.PrivateIP, "error", err)
		return
	}

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(vm.PrivateIP, "22"), 10*time.Second)
	if err != nil {
		p.logger.Warn("connect vm ssh", "vm", vm.Name, "ip", vm.PrivateIP, "error", err)
		return
	}
	defer upstream.Close()

	errCh := make(chan error, 2)
	go proxyCopy(errCh, upstream, client)
	go proxyCopy(errCh, client, upstream)
	<-errCh
}

func (p *TCPWakeProxy) vmByPrivateIP(ctx context.Context, ip string) (store.VM, error) {
	vms, err := p.store.ListVMs(ctx)
	if err != nil {
		return store.VM{}, err
	}
	for _, vm := range vms {
		if vm.PrivateIP == ip {
			return vm, nil
		}
	}
	return store.VM{}, fmt.Errorf("%w: vm private ip %s", store.ErrNotFound, ip)
}

func proxyCopy(errCh chan<- error, dst net.Conn, src net.Conn) {
	_, err := io.Copy(dst, src)
	errCh <- err
	_ = dst.Close()
	_ = src.Close()
}

func waitForTCP(ctx context.Context, address string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *TCPWakeProxy) ensureSSHRedirect(ctx context.Context) error {
	args := p.sshRedirectRule("-C")
	if err := run(ctx, "/usr/sbin/iptables", args...); err == nil {
		return nil
	}
	return run(ctx, "/usr/sbin/iptables", p.sshRedirectRule("-A")...)
}

func (p *TCPWakeProxy) deleteSSHRedirect(ctx context.Context) error {
	return run(ctx, "/usr/sbin/iptables", p.sshRedirectRule("-D")...)
}

func (p *TCPWakeProxy) sshRedirectRule(op string) []string {
	return []string{
		"-t", "nat", op, "PREROUTING",
		"-i", p.cfg.WireGuard.Interface,
		"-p", "tcp",
		"-d", p.cfg.VMNetwork.Subnet,
		"--dport", "22",
		"-j", "REDIRECT",
		"--to-ports", strconv.Itoa(p.cfg.SSH.WakeProxyPort),
	}
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, string(output))
	}
	return nil
}
