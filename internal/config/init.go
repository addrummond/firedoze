package config

import (
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type InitOptions struct {
	Host       string
	SSLIPHost  string
	BaseDomain string
	Force      bool
}

func InitFile(path string, opts InitOptions) error {
	if path == "" {
		path = DefaultPath
	}
	if !opts.Force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; use -init-force to replace it", path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	text, err := InitTOML(opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := Load(tmpPath); err != nil {
		return fmt.Errorf("validate generated config: %w", err)
	}
	return os.Rename(tmpPath, path)
}

func InitTOML(opts InitOptions) (string, error) {
	wgOctet, vmOctet, err := randomNetworkOctets()
	if err != nil {
		return "", err
	}
	host := strings.TrimSpace(opts.Host)
	sslipHost := strings.TrimSpace(opts.SSLIPHost)
	if host != "" && sslipHost != "" {
		return "", fmt.Errorf("init host and init sslip host are mutually exclusive")
	}
	baseDomain := strings.TrimSpace(opts.BaseDomain)
	endpointHost := host
	if sslipHost != "" {
		endpointHost = sslipHost
		if baseDomain == "" {
			baseDomain = hostOnly(sslipHost) + ".sslip.io"
		}
	}
	if baseDomain == "" {
		baseDomain = "dev.example.com"
	}
	endpoint := "YOUR_SERVER_PUBLIC_IP_OR_DNS:51820"
	if endpointHost != "" {
		endpoint = endpointForHost(endpointHost, 51820)
	}
	return RenderExample(ConfigTemplate{
		BaseDomain: baseDomain,
		Endpoint:   endpoint,
		WGAddress:  fmt.Sprintf("10.%d.0.1/24", wgOctet),
		VMSubnet:   fmt.Sprintf("10.%d.0.0/16", vmOctet),
	}), nil
}

func randomNetworkOctets() (int, int, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, 0, err
	}
	return 64 + int(b[0]%64), 128 + int(b[1]%96), nil
}

func endpointForHost(host string, defaultPort int) string {
	if host == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		return fmt.Sprintf("[%s]:%d", host, defaultPort)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]" + fmt.Sprintf(":%d", defaultPort)
	}
	return fmt.Sprintf("%s:%d", host, defaultPort)
}

func hostOnly(host string) string {
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(host, "[]")
}
