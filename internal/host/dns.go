package host

import (
	"context"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

func (o *LinuxOps) EnsureLoopbackAddress(ctx context.Context, address string) error {
	link, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback: %w", err)
	}
	ip := net.ParseIP(address)
	if ip == nil {
		return fmt.Errorf("parse loopback address: %s", address)
	}
	prefix := "/32"
	if ip.To4() == nil {
		prefix = "/128"
	}
	addr, err := netlink.ParseAddr(address + prefix)
	if err != nil {
		return fmt.Errorf("parse loopback address: %w", err)
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("assign loopback address: %w", err)
	}
	o.logger.InfoContext(ctx, "reconciled dns loopback address", "address", address)
	return nil
}
