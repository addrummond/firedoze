package host

import (
	"context"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

func (o *LinuxOps) EnsureLoopbackAddress(ctx context.Context, address string) error {
	link, err := netlinkLinkByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback: %w", err)
	}
	cidr, err := loopbackCIDR(address)
	if err != nil {
		return err
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return fmt.Errorf("parse loopback address: %w", err)
	}
	if err := netlinkAddrReplace(link, addr); err != nil {
		return fmt.Errorf("assign loopback address: %w", err)
	}
	o.logger.InfoContext(ctx, "reconciled dns loopback address", "address", address)
	return nil
}

func loopbackCIDR(address string) (string, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return "", fmt.Errorf("parse loopback address: %s", address)
	}
	prefix := "/32"
	if ip.To4() == nil {
		prefix = "/128"
	}
	return address + prefix, nil
}
