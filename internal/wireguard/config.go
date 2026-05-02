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

func NewPeerSetup(cfg config.Config, name string, allowedIP string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("peer name is required")
	}
	if _, ipNet, err := net.ParseCIDR(allowedIP); err != nil {
		return "", fmt.Errorf("allowed IP must be CIDR: %w", err)
	} else if ones, bits := ipNet.Mask.Size(); ones != bits {
		return "", fmt.Errorf("allowed IP must be a single client address, such as 10.77.0.2/32")
	}

	serverPrivateKey, err := ensureServerPrivateKey(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		return "", err
	}
	clientKeyPair, err := GenerateClientKeyPair()
	if err != nil {
		return "", err
	}
	peer := config.WGPeer{
		Name:       name,
		PublicKey:  clientKeyPair.PublicKey,
		AllowedIPs: []string{allowedIP},
	}
	clientConfig, err := peerConfig(cfg, peer, serverPrivateKey.PublicKey().String(), clientKeyPair.PrivateKey)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Add this to /etc/firedoze/firedoze.toml on the firedoze host.\n")
	fmt.Fprintf(&b, "[[wireguard.peers]]\n")
	fmt.Fprintf(&b, "name = %q\n", peer.Name)
	fmt.Fprintf(&b, "public_key = %q\n", peer.PublicKey)
	fmt.Fprintf(&b, "allowed_ips = [%q]\n\n", allowedIP)
	fmt.Fprintf(&b, "# Save this as the WireGuard client config on %s.\n", peer.Name)
	fmt.Fprint(&b, clientConfig)
	return b.String(), nil
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
