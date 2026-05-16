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
	iptablesPath      = "/usr/sbin/iptables"
	firewallChainName = "FIREDOZE-VM"
	tapInterfaceMatch = "fdtap+"
)

var runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (o *LinuxOps) EnsureFirewall(ctx context.Context, cfg config.Config) error {
	if !cfg.HostFirewall.Enabled {
		o.logger.WarnContext(ctx, "firedoze host firewall disabled; VM private subnet access is the operator's responsibility")
		return nil
	}
	if cfg.HostFirewall.Backend != "ip6tables" {
		return fmt.Errorf("unsupported host_firewall.backend %q", cfg.HostFirewall.Backend)
	}
	vmSubnet, err := firewallIPv6Subnet(cfg.VMNetwork.Subnet)
	if err != nil {
		return err
	}
	vmIPv4Subnet, err := firewallIPv4Subnet(cfg.VMNetwork.IPv4Subnet)
	if err != nil {
		return err
	}
	if cfg.WireGuard.Interface == "" {
		return errors.New("wireguard.interface is required")
	}
	if err := ensureIPv6FirewallChain(ctx, cfg.WireGuard.Interface, vmSubnet); err != nil {
		return err
	}
	if err := ensureIPv4FirewallChain(ctx, vmIPv4Subnet); err != nil {
		return err
	}
	if err := ensureIPv6OutboundNAT(ctx, vmSubnet); err != nil {
		return err
	}
	if err := ensureIPv4OutboundNAT(ctx, vmIPv4Subnet); err != nil {
		return err
	}
	o.logger.InfoContext(ctx, "reconciled firedoze host firewall", "chain", firewallChainName, "wireguard_interface", cfg.WireGuard.Interface, "vm_subnet", vmSubnet, "vm_ipv4_subnet", vmIPv4Subnet)
	return nil
}

func firewallIPv6Subnet(raw string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(raw)
	if err != nil {
		return "", fmt.Errorf("vm_network.subnet must be CIDR: %w", err)
	}
	if ip.To4() != nil {
		return "", errors.New("vm_network.subnet must be IPv6")
	}
	return ipNet.String(), nil
}

func firewallIPv4Subnet(raw string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(raw)
	if err != nil {
		return "", fmt.Errorf("vm_network.ipv4_subnet must be CIDR: %w", err)
	}
	if ip.To4() == nil {
		return "", errors.New("vm_network.ipv4_subnet must be IPv4")
	}
	return ipNet.String(), nil
}

func ensureIPv6FirewallChain(ctx context.Context, wireGuardInterface string, vmSubnet string) error {
	if err := runIP6Tables(ctx, "-N", firewallChainName); err != nil && !isAlreadyExistsError(err) {
		return fmt.Errorf("create IPv6 firewall chain: %w", err)
	}
	if err := runIP6Tables(ctx, "-F", firewallChainName); err != nil {
		return fmt.Errorf("flush IPv6 firewall chain: %w", err)
	}
	rules := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-i", wireGuardInterface, "-d", vmSubnet, "-j", "ACCEPT"},
		{"-i", tapInterfaceMatch, "-d", vmSubnet, "-j", "ACCEPT"},
		{"-i", tapInterfaceMatch, "-s", vmSubnet, "-j", "ACCEPT"},
		{"-i", "lo", "-d", vmSubnet, "-j", "ACCEPT"},
		{"-d", vmSubnet, "-j", "DROP"},
		{"-j", "RETURN"},
	}
	for _, rule := range rules {
		args := append([]string{"-A", firewallChainName}, rule...)
		if err := runIP6Tables(ctx, args...); err != nil {
			return fmt.Errorf("append IPv6 firewall rule %q: %w", strings.Join(rule, " "), err)
		}
	}
	for _, hook := range []string{"INPUT", "FORWARD"} {
		if err := ensureFirewallHook(ctx, runIP6Tables, hook); err != nil {
			return err
		}
	}
	return nil
}

func ensureIPv4FirewallChain(ctx context.Context, vmSubnet string) error {
	if err := runIPTables(ctx, "-N", firewallChainName); err != nil && !isAlreadyExistsError(err) {
		return fmt.Errorf("create IPv4 firewall chain: %w", err)
	}
	if err := runIPTables(ctx, "-F", firewallChainName); err != nil {
		return fmt.Errorf("flush IPv4 firewall chain: %w", err)
	}
	rules := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-i", tapInterfaceMatch, "-d", vmSubnet, "-j", "ACCEPT"},
		{"-i", tapInterfaceMatch, "-s", vmSubnet, "-j", "ACCEPT"},
		{"-i", "lo", "-d", vmSubnet, "-j", "ACCEPT"},
		{"-d", vmSubnet, "-j", "DROP"},
		{"-j", "RETURN"},
	}
	for _, rule := range rules {
		args := append([]string{"-A", firewallChainName}, rule...)
		if err := runIPTables(ctx, args...); err != nil {
			return fmt.Errorf("append IPv4 firewall rule %q: %w", strings.Join(rule, " "), err)
		}
	}
	for _, hook := range []string{"INPUT", "FORWARD"} {
		if err := ensureFirewallHook(ctx, runIPTables, hook); err != nil {
			return err
		}
	}
	return nil
}

func ensureFirewallHook(ctx context.Context, runTables func(context.Context, ...string) error, chain string) error {
	if err := runTables(ctx, "-C", chain, "-j", firewallChainName); err == nil {
		return nil
	}
	if err := runTables(ctx, "-I", chain, "1", "-j", firewallChainName); err != nil {
		return fmt.Errorf("install firewall hook in %s: %w", chain, err)
	}
	return nil
}

func ensureIPv6OutboundNAT(ctx context.Context, vmSubnet string) error {
	rule := []string{"-t", "nat", "-C", "POSTROUTING", "-s", vmSubnet, "!", "-d", vmSubnet, "-j", "MASQUERADE"}
	if err := runIP6Tables(ctx, rule...); err == nil {
		return nil
	}
	rule = []string{"-t", "nat", "-A", "POSTROUTING", "-s", vmSubnet, "!", "-d", vmSubnet, "-j", "MASQUERADE"}
	if err := runIP6Tables(ctx, rule...); err != nil {
		return fmt.Errorf("install IPv6 outbound nat rule: %w", err)
	}
	return nil
}

func ensureIPv4OutboundNAT(ctx context.Context, vmSubnet string) error {
	rule := []string{"-t", "nat", "-C", "POSTROUTING", "-s", vmSubnet, "!", "-d", vmSubnet, "-j", "MASQUERADE"}
	if err := runIPTables(ctx, rule...); err == nil {
		return nil
	}
	rule = []string{"-t", "nat", "-A", "POSTROUTING", "-s", vmSubnet, "!", "-d", vmSubnet, "-j", "MASQUERADE"}
	if err := runIPTables(ctx, rule...); err != nil {
		return fmt.Errorf("install IPv4 outbound nat rule: %w", err)
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

func runIPTables(ctx context.Context, args ...string) error {
	output, err := runCommand(ctx, iptablesPath, args...)
	if err == nil {
		return nil
	}
	return commandError{name: iptablesPath, args: args, output: output, err: err}
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
