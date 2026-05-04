package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"firedoze/internal/config"
)

const (
	ip6tablesPath     = "/usr/sbin/ip6tables"
	firewallChainName = "FIREDOZE-VM"
	tapInterfaceMatch = "fdtap+"
)

var runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (o *LinuxOps) EnsureFirewall(ctx context.Context, cfg config.Config) error {
	vmSubnet, err := firewallVMSubnet(cfg.VMNetwork.Subnet)
	if err != nil {
		return err
	}
	if cfg.WireGuard.Interface == "" {
		return errors.New("wireguard.interface is required")
	}
	if err := ensureFirewallChain(ctx, cfg.WireGuard.Interface, vmSubnet); err != nil {
		return err
	}
	o.logger.InfoContext(ctx, "reconciled firedoze host firewall", "chain", firewallChainName, "wireguard_interface", cfg.WireGuard.Interface, "vm_subnet", vmSubnet)
	return nil
}

func firewallVMSubnet(raw string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(raw)
	if err != nil {
		return "", fmt.Errorf("vm_network.subnet must be CIDR: %w", err)
	}
	if ip.To4() != nil {
		return "", errors.New("vm_network.subnet must be IPv6")
	}
	return ipNet.String(), nil
}

func ensureFirewallChain(ctx context.Context, wireGuardInterface string, vmSubnet string) error {
	if err := runIP6Tables(ctx, "-N", firewallChainName); err != nil && !isAlreadyExistsError(err) {
		return fmt.Errorf("create firewall chain: %w", err)
	}
	if err := runIP6Tables(ctx, "-F", firewallChainName); err != nil {
		return fmt.Errorf("flush firewall chain: %w", err)
	}
	rules := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-i", wireGuardInterface, "-d", vmSubnet, "-j", "ACCEPT"},
		{"-i", tapInterfaceMatch, "-d", vmSubnet, "-j", "ACCEPT"},
		{"-i", "lo", "-d", vmSubnet, "-j", "ACCEPT"},
		{"-d", vmSubnet, "-j", "DROP"},
		{"-j", "RETURN"},
	}
	for _, rule := range rules {
		args := append([]string{"-A", firewallChainName}, rule...)
		if err := runIP6Tables(ctx, args...); err != nil {
			return fmt.Errorf("append firewall rule %q: %w", strings.Join(rule, " "), err)
		}
	}
	for _, hook := range []string{"INPUT", "FORWARD"} {
		if err := ensureFirewallHook(ctx, hook); err != nil {
			return err
		}
	}
	return nil
}

func ensureFirewallHook(ctx context.Context, chain string) error {
	if err := runIP6Tables(ctx, "-C", chain, "-j", firewallChainName); err == nil {
		return nil
	}
	if err := runIP6Tables(ctx, "-I", chain, "1", "-j", firewallChainName); err != nil {
		return fmt.Errorf("install firewall hook in %s: %w", chain, err)
	}
	return nil
}

func runIP6Tables(ctx context.Context, args ...string) error {
	output, err := runCommand(ctx, ip6tablesPath, args...)
	if err == nil {
		return nil
	}
	return commandError{name: ip6tablesPath, args: args, output: output, err: err}
}

func isAlreadyExistsError(err error) bool {
	return strings.Contains(err.Error(), "Chain already exists") || strings.Contains(err.Error(), "File exists")
}

type commandError struct {
	name   string
	args   []string
	output []byte
	err    error
}

func (e commandError) Error() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s: %v", e.name, strings.Join(e.args, " "), e.err)
	if trimmed := strings.TrimSpace(string(e.output)); trimmed != "" {
		fmt.Fprintf(&b, ": %s", trimmed)
	}
	return b.String()
}

func (e commandError) Unwrap() error {
	return e.err
}
