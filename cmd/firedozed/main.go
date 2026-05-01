package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"firedoze/internal/api"
	"firedoze/internal/config"
	"firedoze/internal/firecracker"
	"firedoze/internal/host"
	"firedoze/internal/proxy"
	"firedoze/internal/resolver"
	"firedoze/internal/store"
	"firedoze/internal/systemd"
	wgconfig "firedoze/internal/wireguard"
)

func main() {
	os.Exit(run())
}

func run() int {
	var configPath string
	var printConfig bool
	var setupWireGuard bool
	var serve bool
	var wgGenClientKey bool
	var wgServerPublicKey bool
	var wgPeerConfig string

	flag.StringVar(&configPath, "config", config.DefaultPath, "path to firedoze TOML config")
	flag.BoolVar(&printConfig, "print-config", false, "print resolved config and exit")
	flag.BoolVar(&setupWireGuard, "setup-wireguard", false, "reconcile the configured WireGuard interface")
	flag.BoolVar(&serve, "serve", false, "start the WireGuard-bound management API")
	flag.BoolVar(&wgGenClientKey, "wg-gen-client-key", false, "generate a WireGuard client key pair")
	flag.BoolVar(&wgServerPublicKey, "wg-server-public-key", false, "print the configured server WireGuard public key")
	flag.StringVar(&wgPeerConfig, "wg-peer-config", "", "print a wg-quick config for the configured peer name")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		return 1
	}

	if printConfig {
		fmt.Print(cfg.TOML())
		return 0
	}
	if wgGenClientKey {
		keyPair, err := wgconfig.GenerateClientKeyPair()
		if err != nil {
			logger.Error("generate wireguard client key", "error", err)
			return 1
		}
		fmt.Printf("private_key = %s\n", keyPair.PrivateKey)
		fmt.Printf("public_key = %s\n", keyPair.PublicKey)
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
		manager := firecracker.NewManager(cfg, db, logger)
		proxyManager := proxy.NewManager(cfg, db, logger)
		wakeProxy := proxy.NewWakeProxy(cfg, db, manager, logger)
		wakeCtx, cancelWake := context.WithCancel(ctx)
		defer cancelWake()
		go func() {
			if err := wakeProxy.Run(wakeCtx); err != nil {
				logger.Error("serve wake proxy", "error", err)
			}
		}()
		if err := proxyManager.Reconcile(ctx); err != nil {
			logger.Error("start caddy", "error", err)
			return 1
		}
		defer proxyManager.Stop()
		idleCtx, cancelIdle := context.WithCancel(ctx)
		defer cancelIdle()
		go firecracker.NewIdleMonitor(manager, proxyManager, logger).Run(idleCtx)
		dnsCtx, cancelDNS := context.WithCancel(ctx)
		defer cancelDNS()
		go func() {
			if err := resolver.NewServer(cfg, db, logger).Run(dnsCtx); err != nil {
				logger.Error("serve dns", "error", err)
			}
		}()
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
		if err := manager.SleepRunningVMs(sleepCtx); err != nil {
			logger.Warn("sleep running vms during shutdown", "error", err)
		} else {
			logger.Info("slept running vms during shutdown", "duration", time.Since(start))
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

func wireGuardBindIP(address string) (net.IP, error) {
	ip, _, err := net.ParseCIDR(address)
	if err != nil {
		return nil, err
	}
	return ip, nil
}
