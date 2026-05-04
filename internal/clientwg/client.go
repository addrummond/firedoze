package clientwg

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const defaultMTU = 1420

type Config struct {
	PrivateKey      string
	Address         string
	ServerPublicKey string
	Endpoint        string
	AllowedIPs      []string
}

type Client struct {
	device *device.Device
	tun    tun.Device
	net    *netstack.Net
}

func New(ctx context.Context, cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	localAddress, err := clientAddress(cfg.Address)
	if err != nil {
		return nil, err
	}
	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{localAddress}, nil, defaultMTU)
	if err != nil {
		return nil, err
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	client := &Client{device: dev, tun: tdev, net: tnet}
	ipcConfig, err := wireGuardIPCConfig(ctx, cfg)
	if err != nil {
		client.Close()
		return nil, err
	}
	if err := dev.IpcSet(ipcConfig); err != nil {
		client.Close()
		return nil, err
	}
	if err := dev.Up(); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	if c == nil || c.net == nil {
		return nil, errors.New("wireguard client is closed")
	}
	return c.net.DialContext(ctx, network, address)
}

func (c *Client) Close() error {
	if c.device != nil {
		c.device.Close()
		c.device = nil
		c.tun = nil
		return nil
	}
	if c.tun != nil {
		return c.tun.Close()
	}
	return nil
}

func (cfg Config) Validate() error {
	if _, err := wgtypes.ParseKey(cfg.PrivateKey); err != nil {
		return fmt.Errorf("wireguard private key: %w", err)
	}
	if _, err := wgtypes.ParseKey(cfg.ServerPublicKey); err != nil {
		return fmt.Errorf("wireguard server public key: %w", err)
	}
	if _, err := clientAddress(cfg.Address); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return errors.New("wireguard endpoint is required")
	}
	if len(cfg.AllowedIPs) == 0 {
		return errors.New("wireguard allowed_ips is required")
	}
	for i, allowedIP := range cfg.AllowedIPs {
		if _, err := netip.ParsePrefix(allowedIP); err != nil {
			return fmt.Errorf("wireguard allowed_ips[%d]: %w", i, err)
		}
	}
	return nil
}

func clientAddress(raw string) (netip.Addr, error) {
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("wireguard address must be CIDR: %w", err)
	}
	return prefix.Addr(), nil
}

func wireGuardIPCConfig(ctx context.Context, cfg Config) (string, error) {
	endpoint, err := resolveEndpoint(ctx, cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("wireguard endpoint: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", mustKeyHex(cfg.PrivateKey))
	fmt.Fprintf(&b, "listen_port=0\n")
	fmt.Fprintf(&b, "replace_peers=true\n")
	fmt.Fprintf(&b, "public_key=%s\n", mustKeyHex(cfg.ServerPublicKey))
	fmt.Fprintf(&b, "endpoint=%s\n", endpoint)
	fmt.Fprintf(&b, "persistent_keepalive_interval=25\n")
	fmt.Fprintf(&b, "replace_allowed_ips=true\n")
	for _, allowedIP := range cfg.AllowedIPs {
		fmt.Fprintf(&b, "allowed_ip=%s\n", allowedIP)
	}
	return b.String(), nil
}

func resolveEndpoint(ctx context.Context, endpoint string) (string, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", err
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", err
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return netip.AddrPortFrom(addr, uint16(portNumber)).String(), nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		return netip.AddrPortFrom(addr, uint16(portNumber)).String(), nil
	}
	return "", fmt.Errorf("no IP addresses for %s", host)
}

func mustKeyHex(raw string) string {
	key, err := wgtypes.ParseKey(raw)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(key[:])
}
