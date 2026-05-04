package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"

	"firedoze/internal/config"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type wgClient interface {
	ConfigureDevice(name string, cfg wgtypes.Config) error
	Close() error
}

var (
	netlinkLinkByName  = netlink.LinkByName
	netlinkLinkAdd     = netlink.LinkAdd
	netlinkAddrReplace = netlink.AddrReplace
	netlinkLinkSetUp   = netlink.LinkSetUp
	wgctrlNew          = func() (wgClient, error) {
		return wgctrl.New()
	}
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
		return fmt.Errorf("parse address %q: %w", cfg.Address, err)
	}
	if err := netlinkAddrReplace(link, addr); err != nil {
		return fmt.Errorf("assign address %q: %w", cfg.Address, err)
	}

	client, err := wgctrlNew()
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
		for _, allowedCIDR := range peer.AllowedIPs {
			_, allowedIP, err := net.ParseCIDR(allowedCIDR)
			if err != nil {
				return fmt.Errorf("peer %q allowed_ips: %w", peer.Name, err)
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
	if err := netlinkLinkSetUp(link); err != nil {
		return fmt.Errorf("set link up: %w", err)
	}

	o.logger.InfoContext(ctx, "reconciled wireguard interface", "interface", cfg.Interface, "peers", len(cfg.Peers))
	return nil
}

func (o *LinuxOps) ReconcileWireGuardPeers(ctx context.Context, oldCfg, newCfg config.WireGuardConfig) error {
	peerConfigs, summary, err := wireGuardPeerDiff(oldCfg.Peers, newCfg.Peers)
	if err != nil {
		return err
	}
	if len(peerConfigs) == 0 {
		return nil
	}

	client, err := wgctrlNew()
	if err != nil {
		return fmt.Errorf("open wgctrl: %w", err)
	}
	defer client.Close()

	deviceConfig := wgtypes.Config{
		ReplacePeers: false,
		Peers:        peerConfigs,
	}
	if err := client.ConfigureDevice(oldCfg.Interface, deviceConfig); err != nil {
		return fmt.Errorf("configure device: %w", err)
	}
	o.logger.InfoContext(ctx, "reconciled wireguard peers", "interface", oldCfg.Interface, "added", summary.added, "updated", summary.updated, "removed", summary.removed)
	return nil
}

type wireGuardPeerDiffSummary struct {
	added   int
	updated int
	removed int
}

type runtimeWGPeer struct {
	name              string
	publicKey         wgtypes.Key
	allowedIPs        []net.IPNet
	allowedIPKeyParts []string
}

func wireGuardPeerDiff(oldPeers, newPeers []config.WGPeer) ([]wgtypes.PeerConfig, wireGuardPeerDiffSummary, error) {
	oldByKey, err := runtimeWGPeersByKey(oldPeers)
	if err != nil {
		return nil, wireGuardPeerDiffSummary{}, err
	}
	newByKey, err := runtimeWGPeersByKey(newPeers)
	if err != nil {
		return nil, wireGuardPeerDiffSummary{}, err
	}

	var peerConfigs []wgtypes.PeerConfig
	var summary wireGuardPeerDiffSummary
	for key, oldPeer := range oldByKey {
		if _, ok := newByKey[key]; ok {
			continue
		}
		peerConfigs = append(peerConfigs, wgtypes.PeerConfig{
			PublicKey: oldPeer.publicKey,
			Remove:    true,
		})
		summary.removed++
	}
	for key, newPeer := range newByKey {
		oldPeer, ok := oldByKey[key]
		if !ok {
			peerConfigs = append(peerConfigs, wgtypes.PeerConfig{
				PublicKey:         newPeer.publicKey,
				ReplaceAllowedIPs: true,
				AllowedIPs:        newPeer.allowedIPs,
			})
			summary.added++
			continue
		}
		if !sameStringSet(oldPeer.allowedIPKeyParts, newPeer.allowedIPKeyParts) {
			peerConfigs = append(peerConfigs, wgtypes.PeerConfig{
				PublicKey:         newPeer.publicKey,
				ReplaceAllowedIPs: true,
				AllowedIPs:        newPeer.allowedIPs,
			})
			summary.updated++
		}
	}
	return peerConfigs, summary, nil
}

func runtimeWGPeersByKey(peers []config.WGPeer) (map[string]runtimeWGPeer, error) {
	out := make(map[string]runtimeWGPeer, len(peers))
	for _, peer := range peers {
		publicKey, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("peer %q public key: %w", peer.Name, err)
		}
		key := publicKey.String()
		if existing, ok := out[key]; ok {
			return nil, fmt.Errorf("wireguard peer %q duplicates public key for peer %q", peer.Name, existing.name)
		}
		allowedIPs := make([]net.IPNet, 0, len(peer.AllowedIPs))
		allowedIPKeys := make([]string, 0, len(peer.AllowedIPs))
		for _, allowedCIDR := range peer.AllowedIPs {
			_, allowedIP, err := net.ParseCIDR(allowedCIDR)
			if err != nil {
				return nil, fmt.Errorf("peer %q allowed_ips: %w", peer.Name, err)
			}
			allowedIPs = append(allowedIPs, *allowedIP)
			allowedIPKeys = append(allowedIPKeys, allowedIP.String())
		}
		sort.Strings(allowedIPKeys)
		out[key] = runtimeWGPeer{
			name:              peer.Name,
			publicKey:         publicKey,
			allowedIPs:        allowedIPs,
			allowedIPKeyParts: allowedIPKeys,
		}
	}
	return out, nil
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ensureWireGuardLink(name string) (netlink.Link, error) {
	link, err := netlinkLinkByName(name)
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
	if err := netlinkLinkAdd(link); err != nil {
		return nil, err
	}
	return netlinkLinkByName(name)
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
	if err := os.WriteFile(path, []byte(key.String()+"\n"), 0o640); err != nil {
		return wgtypes.Key{}, err
	}
	return key, nil
}
