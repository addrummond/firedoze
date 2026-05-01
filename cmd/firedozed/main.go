package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"firedoze/internal/config"
	"firedoze/internal/store"
)

func main() {
	os.Exit(run())
}

func run() int {
	var configPath string
	var printConfig bool

	flag.StringVar(&configPath, "config", config.DefaultPath, "path to firedoze TOML config")
	flag.BoolVar(&printConfig, "print-config", false, "print resolved config and exit")
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
	return 0
}
