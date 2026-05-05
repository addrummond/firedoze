package wireguard

import (
	"fmt"
	"math/big"
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

func PublicKeyFromPrivateKey(privateKey string) (string, error) {
	key, err := wgtypes.ParseKey(strings.TrimSpace(privateKey))
	if err != nil {
		return "", fmt.Errorf("private key: %w", err)
	}
	return key.PublicKey().String(), nil
}

func ServerPublicKey(cfg config.Config) (string, error) {
	privateKey, err := readServerPrivateKey(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		return "", err
	}
	return privateKey.PublicKey().String(), nil
}

func NewPeerSetup(cfg config.Config, name string, publicKey string, allowedIP string) (config.WGPeer, string, error) {
	if name == "" {
		return config.WGPeer{}, "", fmt.Errorf("peer name is required")
	}
	if publicKey == "" {
		return config.WGPeer{}, "", fmt.Errorf("peer public key is required")
	}
	if _, err := wgtypes.ParseKey(publicKey); err != nil {
		return config.WGPeer{}, "", fmt.Errorf("peer public key: %w", err)
	}
	if allowedIP == "" {
		var err error
		allowedIP, err = nextPeerAllowedIP(cfg)
		if err != nil {
			return config.WGPeer{}, "", err
		}
	}
	var normalizedAllowedIP string
	if ip, ipNet, err := net.ParseCIDR(allowedIP); err != nil {
		return config.WGPeer{}, "", fmt.Errorf("allowed IP must be CIDR: %w", err)
	} else if ones, bits := ipNet.Mask.Size(); ones != bits {
		return config.WGPeer{}, "", fmt.Errorf("allowed IP must be a single client address, such as fd7a:115c:a1e1::2/128")
	} else if ip.To4() != nil {
		return config.WGPeer{}, "", fmt.Errorf("allowed IP must be IPv6")
	} else {
		normalizedAllowedIP = ipNet.String()
	}
	for _, peer := range cfg.WireGuard.Peers {
		if peer.Name == name {
			return config.WGPeer{}, "", fmt.Errorf("wireguard peer %q already exists", name)
		}
		if peer.PublicKey == publicKey {
			return config.WGPeer{}, "", fmt.Errorf("wireguard peer %q already uses that public key", peer.Name)
		}
		for _, peerAllowedIP := range peer.AllowedIPs {
			if _, peerAllowedIPNet, err := net.ParseCIDR(peerAllowedIP); err == nil && peerAllowedIPNet.String() == normalizedAllowedIP {
				return config.WGPeer{}, "", fmt.Errorf("wireguard peer %q already uses %s", peer.Name, normalizedAllowedIP)
			}
		}
	}

	serverPrivateKey, err := ensureServerPrivateKey(cfg.WireGuard.PrivateKeyFile)
	if err != nil {
		return config.WGPeer{}, "", err
	}
	peer := config.WGPeer{
		Name:       name,
		PublicKey:  publicKey,
		AllowedIPs: []string{normalizedAllowedIP},
	}
	clientConfig, err := peerConfig(cfg, peer, serverPrivateKey.PublicKey().String())
	if err != nil {
		return config.WGPeer{}, "", err
	}
	return peer, clientConfig, nil
}

func nextPeerAllowedIP(cfg config.Config) (string, error) {
	ip, ipNet, err := net.ParseCIDR(cfg.WireGuard.Address)
	if err != nil {
		return "", fmt.Errorf("wireguard.address must be CIDR: %w", err)
	}
	base := ip.To16()
	ones, bits := ipNet.Mask.Size()
	if base == nil || ip.To4() != nil || bits != 128 {
		return "", fmt.Errorf("automatic peer addresses require an IPv6 wireguard.address")
	}
	if ones > 126 {
		return "", fmt.Errorf("wireguard.address subnet is too small for automatic peer addresses")
	}
	network := ip.Mask(ipNet.Mask)
	hostIP := ip.String()
	used := map[string]struct{}{
		hostIP: {},
	}
	for _, peer := range cfg.WireGuard.Peers {
		for _, allowedIP := range peer.AllowedIPs {
			ip, ipNet, err := net.ParseCIDR(allowedIP)
			if err != nil {
				continue
			}
			ones, bits := ipNet.Mask.Size()
			if bits != 128 || ones != 128 || ip.To4() != nil {
				continue
			}
			used[ip.String()] = struct{}{}
		}
	}
	size := new(big.Int).Lsh(big.NewInt(1), uint(128-ones))
	for offset := int64(1); ; offset++ {
		if big.NewInt(offset).Cmp(size) >= 0 {
			break
		}
		candidate, err := addToIP(network, offset)
		if err != nil {
			return "", err
		}
		if _, ok := used[candidate.String()]; ok {
			continue
		}
		return candidate.String() + "/128", nil
	}
	return "", fmt.Errorf("no free wireguard peer addresses in %s", cfg.WireGuard.Address)
}

func addToIP(ip net.IP, offset int64) (net.IP, error) {
	ip16 := ip.To16()
	if ip16 == nil {
		return nil, fmt.Errorf("invalid IP address %q", ip)
	}
	value := new(big.Int).SetBytes(ip16)
	value.Add(value, big.NewInt(offset))
	if value.Sign() < 0 || value.BitLen() > 128 {
		return nil, fmt.Errorf("IP offset %d overflows %s", offset, ip)
	}
	out := value.FillBytes(make([]byte, 16))
	return net.IP(out), nil
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
	if err := os.WriteFile(path, []byte(privateKey.String()+"\n"), 0o640); err != nil {
		return wgtypes.Key{}, err
	}
	return privateKey, nil
}

func PeerConfig(cfg config.Config, peer config.WGPeer) (string, error) {
	serverPublicKey, err := ServerPublicKey(cfg)
	if err != nil {
		return "", err
	}
	return peerConfig(cfg, peer, serverPublicKey)
}

func peerConfig(cfg config.Config, peer config.WGPeer, serverPublicKey string) (string, error) {
	clientAddresses := peerClientAddresses(peer.AllowedIPs)
	if len(clientAddresses) == 0 {
		clientAddresses = []string{"<client-wireguard-address>"}
	}
	allowedIPs := []string{wireGuardHostCIDR(cfg.WireGuard.Address), cfg.VMNetwork.Subnet}
	allowedIPs = compactStrings(allowedIPs)
	apiURL, err := APIURL(cfg.WireGuard.Address)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Firedoze client import config.\n")
	fmt.Fprintf(&b, "# This contains no client private key. Send it back to the client so they can run:\n")
	fmt.Fprintf(&b, "#   firedoze server import <this-file> -default\n\n")
	fmt.Fprintf(&b, "api_url = %q\n", apiURL)
	fmt.Fprintf(&b, "client_public_key = %q\n\n", peer.PublicKey)
	fmt.Fprintf(&b, "[wireguard]\n")
	fmt.Fprintf(&b, "address = %q\n", clientAddresses[0])
	fmt.Fprintf(&b, "server_public_key = %q\n", serverPublicKey)
	fmt.Fprintf(&b, "endpoint = %q\n", endpoint(cfg))
	fmt.Fprintf(&b, "allowed_ips = %s\n", renderStringList(allowedIPs))
	return b.String(), nil
}

func renderStringList(values []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[")
	for i, value := range values {
		if i > 0 {
			fmt.Fprintf(&b, ", ")
		}
		fmt.Fprintf(&b, "%q", value)
	}
	fmt.Fprintf(&b, "]")
	return b.String()
}

func APIURL(wireGuardAddress string) (string, error) {
	ip, _, err := net.ParseCIDR(wireGuardAddress)
	if err != nil {
		return "", fmt.Errorf("wireguard.address must be CIDR: %w", err)
	}
	if ip.To4() == nil {
		return "http://[" + ip.String() + "]", nil
	}
	return "http://" + ip.String(), nil
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
