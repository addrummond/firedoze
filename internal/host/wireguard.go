package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"firedoze/internal/config"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func (o *LinuxOps) EnsureWireGuard(ctx context.Context, cfg config.WireGuardConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	privateKey, err := ensureWireGuardPrivateKey(cfg.PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("private key: %w", err)
	}

	link, err := ensureWireGuardLink(cfg.Interface)
	if err != nil {
		return fmt.Errorf("link: %w", err)
	}

	addr, err := netlink.ParseAddr(cfg.Address)
	if err != nil {
		return fmt.Errorf("parse address: %w", err)
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("assign address: %w", err)
	}

	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("open wgctrl: %w", err)
	}
	defer client.Close()

	listenPort := cfg.ListenPort
	deviceConfig := wgtypes.Config{
		PrivateKey:   &privateKey,
		ListenPort:   &listenPort,
		ReplacePeers: true,
		Peers:        make([]wgtypes.PeerConfig, 0, len(cfg.Peers)),
	}

	for _, peer := range cfg.Peers {
		publicKey, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil {
			return fmt.Errorf("peer %q public key: %w", peer.Name, err)
		}
		var allowedIPs []net.IPNet
		for _, allowedCIDR := range peer.AllowedCIDRs() {
			_, allowedIP, err := net.ParseCIDR(allowedCIDR)
			if err != nil {
				return fmt.Errorf("peer %q allowed_ip: %w", peer.Name, err)
			}
			allowedIPs = append(allowedIPs, *allowedIP)
		}
		deviceConfig.Peers = append(deviceConfig.Peers, wgtypes.PeerConfig{
			PublicKey:         publicKey,
			ReplaceAllowedIPs: true,
			AllowedIPs:        allowedIPs,
		})
	}

	if err := client.ConfigureDevice(cfg.Interface, deviceConfig); err != nil {
		return fmt.Errorf("configure device: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set link up: %w", err)
	}

	o.logger.InfoContext(ctx, "reconciled wireguard interface", "interface", cfg.Interface, "peers", len(cfg.Peers))
	return nil
}

func ensureWireGuardLink(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err == nil {
		if link.Type() != "wireguard" {
			return nil, fmt.Errorf("existing link %q has type %q, want wireguard", name, link.Type())
		}
		return link, nil
	}
	var notFound netlink.LinkNotFoundError
	if !errors.As(err, &notFound) {
		return nil, err
	}

	link = &netlink.Wireguard{
		LinkAttrs: netlink.LinkAttrs{Name: name},
	}
	if err := netlink.LinkAdd(link); err != nil {
		return nil, err
	}
	return netlink.LinkByName(name)
}

func ensureWireGuardPrivateKey(path string) (wgtypes.Key, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return wgtypes.ParseKey(string(data))
	}
	if !os.IsNotExist(err) {
		return wgtypes.Key{}, err
	}

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return wgtypes.Key{}, err
	}
	if err := os.WriteFile(path, []byte(key.String()+"\n"), 0o600); err != nil {
		return wgtypes.Key{}, err
	}
	return key, nil
}
