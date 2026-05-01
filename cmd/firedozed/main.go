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

	"firedoze/internal/api"
	"firedoze/internal/config"
	"firedoze/internal/host"
	"firedoze/internal/store"
	"firedoze/internal/vmm"
)

func main() {
	os.Exit(run())
}

func run() int {
	var configPath string
	var printConfig bool
	var setupWireGuard bool
	var serve bool

	flag.StringVar(&configPath, "config", config.DefaultPath, "path to firedoze TOML config")
	flag.BoolVar(&printConfig, "print-config", false, "print resolved config and exit")
	flag.BoolVar(&setupWireGuard, "setup-wireguard", false, "reconcile the configured WireGuard interface")
	flag.BoolVar(&serve, "serve", false, "start the WireGuard-bound management API")
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
		manager := vmm.NewManager(cfg, db, logger)
		if err := serveAPI(ctx, logger, cfg, manager); err != nil {
			logger.Error("serve api", "error", err)
			return 1
		}
	}

	return 0
}

func serveAPI(ctx context.Context, logger *slog.Logger, cfg config.Config, manager *vmm.Manager) error {
	bindIP, err := wireGuardBindIP(cfg.WireGuard.Address)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:    net.JoinHostPort(bindIP.String(), strconv.Itoa(cfg.API.Port)),
		Handler: api.NewServer(cfg, manager),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("management API listening", "addr", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
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
