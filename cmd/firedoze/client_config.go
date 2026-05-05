package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"firedoze/internal/clientwg"
	wgconfig "firedoze/internal/wireguard"

	"github.com/pelletier/go-toml/v2"
)

type clientConfig struct {
	DefaultServer string                 `toml:"default_server,omitempty"`
	Servers       []clientServerConfig   `toml:"servers,omitempty"`
	PendingPeers  []clientPendingRequest `toml:"pending_wireguard_requests,omitempty"`
}

type clientServerConfig struct {
	Name      string                 `toml:"name"`
	APIURL    string                 `toml:"api_url"`
	WireGuard *clientWireGuardConfig `toml:"wireguard,omitempty"`
}

type clientWireGuardConfig struct {
	PrivateKey      string   `toml:"private_key,omitempty"`
	Address         string   `toml:"address"`
	ServerPublicKey string   `toml:"server_public_key"`
	Endpoint        string   `toml:"endpoint"`
	AllowedIPs      []string `toml:"allowed_ips"`
}

type clientPendingRequest struct {
	Name       string `toml:"name"`
	PrivateKey string `toml:"private_key"`
}

type serverImportConfig struct {
	APIURL          string                      `toml:"api_url"`
	ClientPublicKey string                      `toml:"client_public_key"`
	WireGuard       clientWireGuardImportConfig `toml:"wireguard"`
}

type clientWireGuardImportConfig struct {
	Address         string   `toml:"address"`
	ServerPublicKey string   `toml:"server_public_key"`
	Endpoint        string   `toml:"endpoint"`
	AllowedIPs      []string `toml:"allowed_ips"`
}

type clientServerSummary struct {
	Name      string `json:"name"`
	APIURL    string `json:"api_url"`
	Default   bool   `json:"default"`
	WireGuard bool   `json:"wireguard"`
}

func clientConfigPath() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "firedoze", "config.toml"), nil
	}
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home != "" {
		return filepath.Join(home, ".config", "firedoze", "config.toml"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "firedoze", "config.toml"), nil
}

func loadClientConfig() (clientConfig, string, error) {
	path, err := clientConfigPath()
	if err != nil {
		return clientConfig{}, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return clientConfig{}, path, nil
		}
		return clientConfig{}, path, err
	}
	var cfg clientConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return clientConfig{}, path, fmt.Errorf("read %s: %w", path, err)
	}
	return cfg, path, nil
}

func saveClientConfig(cfg clientConfig) (string, error) {
	path, err := clientConfigPath()
	if err != nil {
		return "", err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmpPath, path)
}

func resolveClientAPIURL(apiURL string, serverName string) (string, error) {
	server, err := resolveClientServer(apiURL, serverName)
	if err != nil {
		return "", err
	}
	return server.APIURL, nil
}

func resolveClientServer(apiURL string, serverName string) (clientServerConfig, error) {
	if strings.TrimSpace(apiURL) != "" {
		return clientServerConfig{APIURL: apiURL}, nil
	}
	cfg, path, err := loadClientConfig()
	if err != nil {
		return clientServerConfig{}, err
	}
	name := strings.TrimSpace(serverName)
	if name == "" {
		name = strings.TrimSpace(cfg.DefaultServer)
	}
	if name == "" && len(cfg.Servers) == 1 {
		name = cfg.Servers[0].Name
	}
	if name == "" {
		return clientServerConfig{}, fmt.Errorf("missing API URL: run \"firedoze server import <file> -default\", \"firedoze server add <name> <api-url> -default\", set FIREDOZE_API, or pass -api")
	}
	server, ok := cfg.findServer(name)
	if !ok {
		if path == "" {
			return clientServerConfig{}, fmt.Errorf("unknown firedoze server %q", name)
		}
		return clientServerConfig{}, fmt.Errorf("unknown firedoze server %q in %s", name, path)
	}
	return server, nil
}

func (cfg clientConfig) findServer(name string) (clientServerConfig, bool) {
	for _, server := range cfg.Servers {
		if server.Name == name {
			return server, true
		}
	}
	return clientServerConfig{}, false
}

func (cfg clientConfig) findServerIndex(name string) int {
	for i, server := range cfg.Servers {
		if server.Name == name {
			return i
		}
	}
	return -1
}

func (cfg clientConfig) findPendingByPublicKey(publicKey string) (clientPendingRequest, bool, error) {
	for _, pending := range cfg.PendingPeers {
		pendingPublicKey, err := wgconfig.PublicKeyFromPrivateKey(pending.PrivateKey)
		if err != nil {
			return clientPendingRequest{}, false, fmt.Errorf("pending WireGuard request %q: %w", pending.Name, err)
		}
		if pendingPublicKey == publicKey {
			return pending, true, nil
		}
	}
	return clientPendingRequest{}, false, nil
}

func (cfg clientConfig) findPendingIndexByName(name string) int {
	for i, pending := range cfg.PendingPeers {
		if pending.Name == name {
			return i
		}
	}
	return -1
}

func (cfg clientConfig) findPendingByName(name string) (clientPendingRequest, bool) {
	for _, pending := range cfg.PendingPeers {
		if pending.Name == name {
			return pending, true
		}
	}
	return clientPendingRequest{}, false
}

func (cfg *clientConfig) removePendingByPublicKey(publicKey string) error {
	out := cfg.PendingPeers[:0]
	for _, pending := range cfg.PendingPeers {
		pendingPublicKey, err := wgconfig.PublicKeyFromPrivateKey(pending.PrivateKey)
		if err != nil {
			return fmt.Errorf("pending WireGuard request %q: %w", pending.Name, err)
		}
		if pendingPublicKey != publicKey {
			out = append(out, pending)
		}
	}
	cfg.PendingPeers = out
	return nil
}

func (cfg clientConfig) serverSummaries() []clientServerSummary {
	out := make([]clientServerSummary, 0, len(cfg.Servers))
	for _, server := range cfg.Servers {
		out = append(out, clientServerSummary{
			Name:      server.Name,
			APIURL:    server.APIURL,
			Default:   server.Name == cfg.DefaultServer,
			WireGuard: server.WireGuard != nil,
		})
	}
	return out
}

func validateServerName(name string) error {
	if name == "" {
		return errors.New("server name is required")
	}
	for _, r := range name {
		if unicode.IsSpace(r) || r == '/' || r == '\\' || r == 0 {
			return fmt.Errorf("server name %q contains an invalid character", name)
		}
	}
	return nil
}

func (a app) server(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze server <request|import|add|list|use|remove|current|path>")
	}
	switch args[0] {
	case "request":
		if len(args) != 2 {
			return errors.New("usage: firedoze server request <name>")
		}
		name := args[1]
		if err := validateServerName(name); err != nil {
			return err
		}
		keyPair, err := wgconfig.GenerateClientKeyPair()
		if err != nil {
			return err
		}
		cfg, _, err := loadClientConfig()
		if err != nil {
			return err
		}
		pending := clientPendingRequest{
			Name:       name,
			PrivateKey: keyPair.PrivateKey,
		}
		if i := cfg.findPendingIndexByName(name); i >= 0 {
			cfg.PendingPeers[i] = pending
		} else {
			cfg.PendingPeers = append(cfg.PendingPeers, pending)
		}
		path, err := saveClientConfig(cfg)
		if err != nil {
			return err
		}
		adminCommand := fmt.Sprintf("sudo firedozed -wg-add-peer %s %s", name, keyPair.PublicKey)
		if a.json {
			return printJSON(map[string]any{
				"name":          name,
				"public_key":    keyPair.PublicKey,
				"admin_command": adminCommand,
				"path":          path,
			})
		}
		fmt.Printf("WireGuard request saved for %s at %s\n\n", name, path)
		fmt.Println("Send this public key to the Firedoze admin:")
		fmt.Println(keyPair.PublicKey)
		fmt.Println()
		fmt.Println("Admin command:")
		fmt.Println(adminCommand)
		return nil
	case "import":
		inputPath, nameOverride, makeDefault, err := parseServerImportArgs(args[1:])
		if err != nil {
			return fmt.Errorf("%w\nusage: firedoze server import <file|-> [-name NAME] [-default]", err)
		}
		data, err := readServerImportInput(inputPath)
		if err != nil {
			return err
		}
		var importConfig serverImportConfig
		if err := toml.Unmarshal(data, &importConfig); err != nil {
			return fmt.Errorf("read server import: %w", err)
		}
		cfg, _, err := loadClientConfig()
		if err != nil {
			return err
		}
		pending, ok, err := cfg.findPendingByPublicKey(importConfig.ClientPublicKey)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("no matching pending WireGuard request: run \"firedoze server request <name>\" on this client and ask the admin to add that public key")
		}
		name := strings.TrimSpace(nameOverride)
		if name == "" {
			name = serverNameFromImportPath(inputPath)
		}
		if name == "" {
			name = pending.Name
		}
		if err := validateServerName(name); err != nil {
			return err
		}
		normalizedAPIURL, err := normalizeAPIURL(importConfig.APIURL)
		if err != nil {
			return err
		}
		wireGuardConfig := clientWireGuardConfig{
			PrivateKey:      pending.PrivateKey,
			Address:         importConfig.WireGuard.Address,
			ServerPublicKey: importConfig.WireGuard.ServerPublicKey,
			Endpoint:        importConfig.WireGuard.Endpoint,
			AllowedIPs:      importConfig.WireGuard.AllowedIPs,
		}
		if err := wireGuardConfig.clientWGConfig().Validate(); err != nil {
			return err
		}
		server := clientServerConfig{
			Name:      name,
			APIURL:    normalizedAPIURL,
			WireGuard: &wireGuardConfig,
		}
		if i := cfg.findServerIndex(name); i >= 0 {
			cfg.Servers[i] = server
		} else {
			cfg.Servers = append(cfg.Servers, server)
		}
		if err := cfg.removePendingByPublicKey(importConfig.ClientPublicKey); err != nil {
			return err
		}
		if makeDefault || cfg.DefaultServer == "" {
			cfg.DefaultServer = name
		}
		path, err := saveClientConfig(cfg)
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"server": clientServerSummary{Name: name, APIURL: normalizedAPIURL, Default: cfg.DefaultServer == name, WireGuard: true}, "path": path})
		}
		suffix := ""
		if cfg.DefaultServer == name {
			suffix = " (default)"
		}
		fmt.Printf("%s imported%s\n", name, suffix)
		return nil
	case "add":
		name, apiURL, makeDefault, err := parseServerAddArgs(args[1:])
		if err != nil {
			return fmt.Errorf("%w\nusage: firedoze server add <name> <api-url> [-default]", err)
		}
		if err := validateServerName(name); err != nil {
			return err
		}
		normalized, err := normalizeAPIURL(apiURL)
		if err != nil {
			return err
		}
		cfg, _, err := loadClientConfig()
		if err != nil {
			return err
		}
		server := clientServerConfig{Name: name, APIURL: normalized}
		if i := cfg.findServerIndex(name); i >= 0 {
			cfg.Servers[i] = server
		} else {
			cfg.Servers = append(cfg.Servers, server)
		}
		if makeDefault || cfg.DefaultServer == "" {
			cfg.DefaultServer = name
		}
		path, err := saveClientConfig(cfg)
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"server": clientServerSummary{Name: server.Name, APIURL: server.APIURL, Default: cfg.DefaultServer == name, WireGuard: false}, "path": path})
		}
		suffix := ""
		if cfg.DefaultServer == name {
			suffix = " (default)"
		}
		fmt.Printf("%s added%s\n", name, suffix)
		return nil
	case "list", "ls":
		cfg, path, err := loadClientConfig()
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"default_server": cfg.DefaultServer, "servers": cfg.serverSummaries(), "path": path})
		}
		if len(cfg.Servers) == 0 {
			fmt.Printf("no firedoze servers configured at %s\n", path)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tDEFAULT\tWIREGUARD\tAPI URL")
		for _, server := range cfg.Servers {
			defaultMark := "-"
			if server.Name == cfg.DefaultServer {
				defaultMark = "yes"
			}
			wireGuardMark := "-"
			if server.WireGuard != nil {
				wireGuardMark = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", server.Name, defaultMark, wireGuardMark, server.APIURL)
		}
		return w.Flush()
	case "use":
		if len(args) != 2 {
			return errors.New("usage: firedoze server use <name>")
		}
		name := args[1]
		cfg, _, err := loadClientConfig()
		if err != nil {
			return err
		}
		if _, ok := cfg.findServer(name); !ok {
			return fmt.Errorf("unknown firedoze server %q", name)
		}
		cfg.DefaultServer = name
		path, err := saveClientConfig(cfg)
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"default_server": name, "path": path})
		}
		fmt.Printf("%s selected\n", name)
		return nil
	case "remove", "rm":
		if len(args) < 2 {
			return errors.New("usage: firedoze server remove <name> [name...]")
		}
		cfg, _, err := loadClientConfig()
		if err != nil {
			return err
		}
		removed := []string{}
		for _, name := range args[1:] {
			i := cfg.findServerIndex(name)
			if i < 0 {
				return fmt.Errorf("unknown firedoze server %q", name)
			}
			cfg.Servers = append(cfg.Servers[:i], cfg.Servers[i+1:]...)
			removed = append(removed, name)
			if cfg.DefaultServer == name {
				cfg.DefaultServer = ""
			}
		}
		path, err := saveClientConfig(cfg)
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"removed": removed, "default_server": cfg.DefaultServer, "path": path})
		}
		for _, name := range removed {
			fmt.Printf("%s removed\n", name)
		}
		return nil
	case "current":
		cfg, path, err := loadClientConfig()
		if err != nil {
			return err
		}
		name := cfg.DefaultServer
		if name == "" && len(cfg.Servers) == 1 {
			name = cfg.Servers[0].Name
		}
		if name == "" {
			return fmt.Errorf("no default firedoze server configured at %s", path)
		}
		server, ok := cfg.findServer(name)
		if !ok {
			return fmt.Errorf("default firedoze server %q is not configured in %s", name, path)
		}
		if a.json {
			return printJSON(map[string]any{"server": clientServerSummary{Name: server.Name, APIURL: server.APIURL, Default: true, WireGuard: server.WireGuard != nil}, "path": path})
		}
		fmt.Printf("%s %s\n", server.Name, server.APIURL)
		return nil
	case "path":
		path, err := clientConfigPath()
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"path": path})
		}
		fmt.Println(path)
		return nil
	default:
		return fmt.Errorf("unknown server command %q", args[0])
	}
}

func parseServerAddArgs(args []string) (name string, apiURL string, makeDefault bool, err error) {
	flags := flagSet("firedoze server add")
	defaultFlag := flags.Bool("default", false, "make this the default server")
	values, err := parseNamesAndFlags(flags, args)
	if err != nil {
		return "", "", false, err
	}
	if len(values) != 2 {
		return "", "", false, errors.New("expected name and api-url")
	}
	return values[0], values[1], *defaultFlag, nil
}

func parseServerImportArgs(args []string) (inputPath string, nameOverride string, makeDefault bool, err error) {
	if len(args) == 0 {
		return "", "", false, errors.New("missing import file")
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-default":
			makeDefault = true
		case strings.HasPrefix(arg, "-default="):
			value := strings.TrimPrefix(arg, "-default=")
			parsed, parseErr := strconv.ParseBool(value)
			if parseErr != nil {
				return "", "", false, parseErr
			}
			makeDefault = parsed
		case arg == "-name":
			if i+1 >= len(args) {
				return "", "", false, errors.New("-name requires a value")
			}
			i++
			nameOverride = args[i]
		case strings.HasPrefix(arg, "-name="):
			nameOverride = strings.TrimPrefix(arg, "-name=")
		case strings.HasPrefix(arg, "-") && arg != "-":
			return "", "", false, fmt.Errorf("unknown flag %s", arg)
		default:
			if inputPath != "" {
				return "", "", false, fmt.Errorf("unexpected argument %q", arg)
			}
			inputPath = arg
		}
	}
	if inputPath == "" {
		return "", "", false, errors.New("missing import file")
	}
	return inputPath, nameOverride, makeDefault, nil
}

func readServerImportInput(inputPath string) ([]byte, error) {
	if inputPath == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(inputPath)
}

func serverNameFromImportPath(inputPath string) string {
	if inputPath == "-" {
		return ""
	}
	name := strings.TrimSpace(filepath.Base(inputPath))
	if ext := filepath.Ext(name); ext != "" {
		name = strings.TrimSuffix(name, ext)
	}
	return strings.TrimSpace(name)
}

func (cfg clientWireGuardConfig) clientWGConfig() clientwg.Config {
	return clientwg.Config{
		PrivateKey:      cfg.PrivateKey,
		Address:         cfg.Address,
		ServerPublicKey: cfg.ServerPublicKey,
		Endpoint:        cfg.Endpoint,
		AllowedIPs:      cfg.AllowedIPs,
	}
}

func flagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}
