package host

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"
)

func (o *LinuxOps) EnsureLoopbackAddress(ctx context.Context, address string) error {
	link, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback: %w", err)
	}
	addr, err := netlink.ParseAddr(address + "/32")
	if err != nil {
		return fmt.Errorf("parse loopback address: %w", err)
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("assign loopback address: %w", err)
	}
	o.logger.InfoContext(ctx, "reconciled dns loopback address", "address", address)
	return nil
}
