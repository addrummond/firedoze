package wireguard

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"firedoze/internal/config"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type ClientKeyPair struct {
	PrivateKey string
	PublicKey  string
}

func GenerateClientKeyPair() (ClientKeyPair, error) {
	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return ClientKeyPair{}, err
	}
	return ClientKeyPair{
		PrivateKey: privateKey.String(),
		PublicKey:  privateKey.PublicKey().String(),
	}, nil
}

func ServerPublicKey(cfg config.Config) (string, error) {
	privateKey, err := readServerPrivateKey(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		return "", err
	}
	return privateKey.PublicKey().String(), nil
}

func NewPeerSetup(cfg config.Config, name string, allowedIP string) (config.WGPeer, string, error) {
	if name == "" {
		return config.WGPeer{}, "", fmt.Errorf("peer name is required")
	}
	if _, ipNet, err := net.ParseCIDR(allowedIP); err != nil {
		return config.WGPeer{}, "", fmt.Errorf("allowed IP must be CIDR: %w", err)
	} else if ones, bits := ipNet.Mask.Size(); ones != bits {
		return config.WGPeer{}, "", fmt.Errorf("allowed IP must be a single client address, such as 10.77.0.2/32")
	}
	for _, peer := range cfg.WireGuard.Peers {
		if peer.Name == name {
			return config.WGPeer{}, "", fmt.Errorf("wireguard peer %q already exists", name)
		}
		if slices.Contains(peer.AllowedIPs, allowedIP) {
			return config.WGPeer{}, "", fmt.Errorf("wireguard peer %q already uses %s", peer.Name, allowedIP)
		}
	}

	serverPrivateKey, err := ensureServerPrivateKey(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		return config.WGPeer{}, "", err
	}
	clientKeyPair, err := GenerateClientKeyPair()
	if err != nil {
		return config.WGPeer{}, "", err
	}
	peer := config.WGPeer{
		Name:       name,
		PublicKey:  clientKeyPair.PublicKey,
		AllowedIPs: []string{allowedIP},
	}
	clientConfig, err := peerConfig(cfg, peer, serverPrivateKey.PublicKey().String(), clientKeyPair.PrivateKey)
	if err != nil {
		return config.WGPeer{}, "", err
	}
	return peer, clientConfig, nil
}

func AppendPeer(path string, peer config.WGPeer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	appended := append([]byte(nil), data...)
	if len(appended) > 0 && appended[len(appended)-1] != '\n' {
		appended = append(appended, '\n')
	}
	appended = append(appended, []byte(renderPeerTOML(peer))...)

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(appended); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := config.Load(tmpPath); err != nil {
		return fmt.Errorf("validate updated config: %w", err)
	}
	return os.Rename(tmpPath, path)
}

func renderPeerTOML(peer config.WGPeer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "[[wireguard.peers]]\n")
	fmt.Fprintf(&b, "name = %q\n", peer.Name)
	fmt.Fprintf(&b, "public_key = %q\n", peer.PublicKey)
	fmt.Fprintf(&b, "allowed_ips = [")
	for i, allowedIP := range peer.AllowedIPs {
		if i > 0 {
			fmt.Fprintf(&b, ", ")
		}
		fmt.Fprintf(&b, "%q", allowedIP)
	}
	fmt.Fprintf(&b, "]\n")
	return b.String()
}

func readServerPrivateKey(path string) (wgtypes.Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return wgtypes.Key{}, err
	}
	privateKey, err := wgtypes.ParseKey(strings.TrimSpace(string(data)))
	if err != nil {
		return wgtypes.Key{}, err
	}
	return privateKey, nil
}

func ensureServerPrivateKey(path string) (wgtypes.Key, error) {
	privateKey, err := readServerPrivateKey(path)
	if err == nil {
		return privateKey, nil
	}
	if !os.IsNotExist(err) {
		return wgtypes.Key{}, err
	}
	privateKey, err = wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return wgtypes.Key{}, err
	}
	if err := os.WriteFile(path, []byte(privateKey.String()+"\n"), 0o600); err != nil {
		return wgtypes.Key{}, err
	}
	return privateKey, nil
}

func PeerConfig(cfg config.Config, peer config.WGPeer) (string, error) {
	serverPublicKey, err := ServerPublicKey(cfg)
	if err != nil {
		return "", err
	}
	return peerConfig(cfg, peer, serverPublicKey, "<client-private-key>")
}

func peerConfig(cfg config.Config, peer config.WGPeer, serverPublicKey string, clientPrivateKey string) (string, error) {
	clientAddresses := peerClientAddresses(peer.AllowedIPs)
	if len(clientAddresses) == 0 {
		clientAddresses = []string{"<client-wireguard-address>"}
	}
	allowedIPs := []string{wireGuardHostCIDR(cfg.WireGuard.Address), cfg.VMNetwork.Subnet}
	allowedIPs = compactStrings(allowedIPs)
	dnsIP, _, err := net.ParseCIDR(cfg.WireGuard.Address)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if clientPrivateKey != "<client-private-key>" {
		fmt.Fprintf(&b, "# WARNING: THIS FILE CONTAINS A PRIVATE WIREGUARD KEY.\n")
		fmt.Fprintf(&b, "# SHARE IT WITH %s SECURELY. DO NOT PASTE IT INTO CHAT.\n\n", peer.Name)
	}
	fmt.Fprintf(&b, "[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", clientPrivateKey)
	fmt.Fprintf(&b, "Address = %s\n", strings.Join(clientAddresses, ", "))
	fmt.Fprintf(&b, "DNS = %s\n\n", dnsIP.String())
	fmt.Fprintf(&b, "[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", serverPublicKey)
	fmt.Fprintf(&b, "Endpoint = %s\n", endpoint(cfg))
	fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(allowedIPs, ", "))
	fmt.Fprintf(&b, "PersistentKeepalive = 25\n")
	return b.String(), nil
}

func Endpoint(cfg config.Config) string {
	return endpoint(cfg)
}

func endpoint(cfg config.Config) string {
	if cfg.WireGuard.Endpoint != "" {
		return cfg.WireGuard.Endpoint
	}
	return "<firedoze-public-host>:" + fmt.Sprint(cfg.WireGuard.ListenPort)
}

func peerClientAddresses(allowedIPs []string) []string {
	var addresses []string
	for _, cidr := range allowedIPs {
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		ones, bits := ipNet.Mask.Size()
		if ones != bits {
			continue
		}
		addresses = append(addresses, ip.String()+fmt.Sprintf("/%d", bits))
	}
	return addresses
}

func wireGuardHostCIDR(address string) string {
	ip, ipNet, err := net.ParseCIDR(address)
	if err != nil {
		return address
	}
	_, bits := ipNet.Mask.Size()
	return ip.String() + fmt.Sprintf("/%d", bits)
}

func compactStrings(values []string) []string {
	var out []string
	for _, value := range values {
		if value == "" || slices.Contains(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}
