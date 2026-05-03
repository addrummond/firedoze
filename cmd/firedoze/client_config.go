package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/pelletier/go-toml/v2"
)

type clientConfig struct {
	DefaultServer string               `toml:"default_server,omitempty"`
	Servers       []clientServerConfig `toml:"servers,omitempty"`
}

type clientServerConfig struct {
	Name   string `toml:"name"`
	APIURL string `toml:"api_url"`
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
	if strings.TrimSpace(apiURL) != "" {
		return apiURL, nil
	}
	cfg, path, err := loadClientConfig()
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(serverName)
	if name == "" {
		name = strings.TrimSpace(cfg.DefaultServer)
	}
	if name == "" && len(cfg.Servers) == 1 {
		name = cfg.Servers[0].Name
	}
	if name == "" {
		return "", fmt.Errorf("missing API URL: run \"firedoze server add <name> <api-url> -default\", set FIREDOZE_API, or pass -api")
	}
	server, ok := cfg.findServer(name)
	if !ok {
		if path == "" {
			return "", fmt.Errorf("unknown firedoze server %q", name)
		}
		return "", fmt.Errorf("unknown firedoze server %q in %s", name, path)
	}
	return server.APIURL, nil
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
		return errors.New("usage: firedoze server <add|list|use|remove|current|path>")
	}
	switch args[0] {
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
			return printJSON(map[string]any{"server": server, "default": cfg.DefaultServer == name, "path": path})
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
			return printJSON(map[string]any{"default_server": cfg.DefaultServer, "servers": cfg.Servers, "path": path})
		}
		if len(cfg.Servers) == 0 {
			fmt.Printf("no firedoze servers configured at %s\n", path)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tDEFAULT\tAPI URL")
		for _, server := range cfg.Servers {
			defaultMark := "-"
			if server.Name == cfg.DefaultServer {
				defaultMark = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", server.Name, defaultMark, server.APIURL)
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
			return printJSON(map[string]any{"server": server, "path": path})
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

func flagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}
