package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"firedoze/internal/api"
	"firedoze/internal/config"
	fdDNS "firedoze/internal/dns"
	"firedoze/internal/firecracker"
	"firedoze/internal/host"
	"firedoze/internal/proxy"
	"firedoze/internal/store"
	"firedoze/internal/systemd"
	wgconfig "firedoze/internal/wireguard"
)

func main() {
	os.Exit(run())
}

func run() int {
	var configPath string
	var initConfig bool
	var initHost string
	var initSSLIPHost string
	var initBaseDomain string
	var initForce bool
	var printConfig bool
	var printAPIEnv bool
	var setupWireGuard bool
	var serve bool
	var wgServerPublicKey bool
	var wgPeerConfig string
	var wgAddPeer string

	flag.StringVar(&configPath, "config", config.DefaultPath, "path to firedoze TOML config")
	flag.BoolVar(&initConfig, "init-config", false, "create an initial firedoze config and exit")
	flag.StringVar(&initHost, "init-host", "", "public DNS name or IP for the WireGuard endpoint when using -init-config")
	flag.StringVar(&initSSLIPHost, "init-sslip-host", "", "public IP or hostname for the WireGuard endpoint and sslip.io VM hostnames when using -init-config")
	flag.StringVar(&initBaseDomain, "init-base-domain", "", "base domain for VM hostnames when using -init-config")
	flag.BoolVar(&initForce, "init-force", false, "replace an existing config when using -init-config")
	flag.BoolVar(&printConfig, "print-config", false, "print resolved config and exit")
	flag.BoolVar(&printAPIEnv, "print-api-env", false, "print shell export for FIREDOZE_API and exit")
	flag.BoolVar(&setupWireGuard, "setup-wireguard", false, "reconcile the configured WireGuard interface")
	flag.BoolVar(&serve, "serve", false, "start the WireGuard-bound management API")
	flag.BoolVar(&wgServerPublicKey, "wg-server-public-key", false, "print the configured server WireGuard public key")
	flag.StringVar(&wgPeerConfig, "wg-peer-config", "", "print a wg-quick config for the configured peer name")
	flag.StringVar(&wgAddPeer, "wg-add-peer", "", "add a WireGuard peer public key to the config and print its client config; optional allowed IP positional argument")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if initConfig {
		if err := config.InitFile(configPath, config.InitOptions{
			Host:       initHost,
			SSLIPHost:  initSSLIPHost,
			BaseDomain: initBaseDomain,
			Force:      initForce,
		}); err != nil {
			logger.Error("initialize config", "path", configPath, "error", err)
			return 1
		}
		fmt.Println(configPath)
		return 0
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		return 1
	}

	if printConfig {
		fmt.Print(cfg.TOML())
		return 0
	}
	if printAPIEnv {
		apiURL, err := wgconfig.APIURL(cfg.WireGuard.Address)
		if err != nil {
			logger.Error("derive API URL", "error", err)
			return 1
		}
		fmt.Printf("export FIREDOZE_API=%q\n", apiURL)
		return 0
	}
	if wgServerPublicKey {
		publicKey, err := wgconfig.ServerPublicKey(cfg)
		if err != nil {
			logger.Error("read wireguard server public key", "error", err)
			return 1
		}
		fmt.Println(publicKey)
		return 0
	}
	if wgPeerConfig != "" {
		for _, peer := range cfg.WireGuard.Peers {
			if peer.Name != wgPeerConfig {
				continue
			}
			config, err := wgconfig.PeerConfig(cfg, peer)
			if err != nil {
				logger.Error("render wireguard peer config", "peer", peer.Name, "error", err)
				return 1
			}
			fmt.Print(config)
			return 0
		}
		logger.Error("wireguard peer not found", "peer", wgPeerConfig)
		return 1
	}
	if wgAddPeer != "" {
		if flag.NArg() < 1 || flag.NArg() > 2 {
			logger.Error("usage: firedozed -wg-add-peer <peer-name> <client-public-key> [client-wireguard-address-cidr]")
			return 1
		}
		publicKey := flag.Arg(0)
		allowedIP := ""
		if flag.NArg() == 2 {
			allowedIP = flag.Arg(1)
		}
		peer, clientConfig, err := wgconfig.NewPeerSetup(cfg, wgAddPeer, publicKey, allowedIP)
		if err != nil {
			logger.Error("render new wireguard peer setup", "peer", wgAddPeer, "error", err)
			return 1
		}
		if err := wgconfig.AppendPeer(configPath, peer); err != nil {
			logger.Error("update config with new wireguard peer", "path", configPath, "peer", wgAddPeer, "error", err)
			return 1
		}
		fmt.Print(clientConfig)
		return 0
	}

	ctx := context.Background()

	if setupWireGuard {
		ops := host.NewLinuxOps(logger)
		if err := ops.EnsureWireGuard(ctx, cfg.WireGuard); err != nil {
			logger.Error("setup wireguard", "interface", cfg.WireGuard.Interface, "error", err)
			return 1
		}
		logger.Info("wireguard interface ready", "interface", cfg.WireGuard.Interface, "address", cfg.WireGuard.Address)
	}

	db, err := store.Open(ctx, cfg.Metadata.Path)
	if err != nil {
		logger.Error("open metadata store", "path", cfg.Metadata.Path, "error", err)
		return 1
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		logger.Error("migrate metadata store", "error", err)
		return 1
	}

	logger.Info("firedoze metadata initialized", "database", cfg.Metadata.Path)

	if serve {
		if !setupWireGuard {
			logger.Error("refusing to serve API without -setup-wireguard")
			return 1
		}
		if cfg.DNS.Enabled {
			if err := host.NewLinuxOps(logger).EnsureLoopbackAddress(ctx, cfg.DNS.ListenIP); err != nil {
				logger.Error("setup dns address", "address", cfg.DNS.ListenIP, "error", err)
				return 1
			}
		}
		manager := firecracker.NewManager(cfg, db, logger)
		if err := manager.ReconcileStartup(ctx); err != nil {
			logger.Error("reconcile firecracker state", "error", err)
			return 1
		}
		if err := wakeRestartVMs(ctx, cfg, manager, logger); err != nil {
			logger.Warn("wake restart vms", "error", err)
		}
		proxyManager := proxy.NewManager(cfg, db, logger)
		wakeProxy := proxy.NewWakeProxy(cfg, db, manager, logger)
		tcpWakeProxy := proxy.NewTCPWakeProxy(cfg, db, manager, logger)
		wakeCtx, cancelWake := context.WithCancel(ctx)
		defer cancelWake()
		go func() {
			if err := wakeProxy.Run(wakeCtx); err != nil {
				logger.Error("serve wake proxy", "error", err)
			}
		}()
		go func() {
			if err := tcpWakeProxy.RunSSH(wakeCtx); err != nil {
				logger.Error("serve ssh wake proxy", "error", err)
			}
		}()
		if cfg.DNS.Enabled {
			go func() {
				if err := fdDNS.NewServer(cfg.DNS, db, logger).Run(wakeCtx); err != nil {
					logger.Error("serve dns", "error", err)
				}
			}()
		}
		if err := proxyManager.Reconcile(ctx); err != nil {
			logger.Error("start caddy", "error", err)
			return 1
		}
		defer proxyManager.Stop()
		idleCtx, cancelIdle := context.WithCancel(ctx)
		defer cancelIdle()
		go firecracker.NewIdleMonitor(manager, proxyManager, logger).Run(idleCtx)
		if err := serveAPI(ctx, logger, cfg, manager, db, proxyManager); err != nil {
			logger.Error("serve api", "error", err)
			return 1
		}
	}

	return 0
}

func serveAPI(ctx context.Context, logger *slog.Logger, cfg config.Config, manager *firecracker.Manager, db *store.Store, proxyManager api.Proxy) error {
	bindIP, err := wireGuardBindIP(cfg.WireGuard.Address)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:    net.JoinHostPort(bindIP.String(), strconv.Itoa(cfg.API.Port)),
		Handler: api.NewServer(cfg, manager, db, proxyManager),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("management API listening", "addr", server.Addr)
		errCh <- server.ListenAndServe()
	}()
	if systemd.Ready() {
		logger.Info("notified systemd ready")
	}
	stopWatchdog := systemd.StartWatchdog(logger)
	defer stopWatchdog()

	select {
	case <-ctx.Done():
		systemd.Stopping()
		sleepCtx, cancelSleep := context.WithTimeout(context.Background(), firecracker.ShutdownSleepTimeout)
		defer cancelSleep()
		start := time.Now()
		runningVMs := manager.RunningVMNames()
		if err := writeRestartWakeFile(cfg, runningVMs); err != nil {
			logger.Warn("record running vms for restart wake", "error", err)
		}
		if err := manager.SleepRunningVMs(sleepCtx); err != nil {
			logger.Warn("sleep running vms during shutdown", "error", err)
		} else {
			logger.Info("slept running vms during shutdown", "vms", len(runningVMs), "duration", time.Since(start))
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), api.ShutdownTimeout)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func restartWakePath(cfg config.Config) string {
	return filepath.Join(cfg.StateDir, "restart-wake.json")
}

func writeRestartWakeFile(cfg config.Config, names []string) error {
	path := restartWakePath(cfg)
	if len(names) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		VMs []string `json:"vms"`
	}{VMs: names}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func wakeRestartVMs(ctx context.Context, cfg config.Config, manager *firecracker.Manager, logger *slog.Logger) error {
	path := restartWakePath(cfg)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logger.Warn("remove restart wake file", "path", path, "error", err)
	}
	var payload struct {
		VMs []string `json:"vms"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(payload.VMs) == 0 {
		return nil
	}
	start := time.Now()
	logger.Info("waking vms that were running before daemon shutdown", "vms", len(payload.VMs))
	if err := manager.StartVMs(ctx, payload.VMs); err != nil {
		return err
	}
	logger.Info("woke restart vms", "vms", len(payload.VMs), "duration", time.Since(start))
	return nil
}

func wireGuardBindIP(address string) (net.IP, error) {
	ip, _, err := net.ParseCIDR(address)
	if err != nil {
		return nil, err
	}
	return ip, nil
}
