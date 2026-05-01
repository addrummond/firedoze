package wireguard

import (
	"fmt"
	"net"
	"os"
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
	data, err := os.ReadFile(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		return "", err
	}
	privateKey, err := wgtypes.ParseKey(strings.TrimSpace(string(data)))
	if err != nil {
		return "", err
	}
	return privateKey.PublicKey().String(), nil
}

func PeerConfig(cfg config.Config, peer config.WGPeer) (string, error) {
	serverPublicKey, err := ServerPublicKey(cfg)
	if err != nil {
		return "", err
	}
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
	fmt.Fprintf(&b, "PrivateKey = <client-private-key>\n")
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
