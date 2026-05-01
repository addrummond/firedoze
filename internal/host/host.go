package host

import (
	"context"
	"log/slog"

	"firedoze/internal/config"
)

type Ops interface {
	EnsureWireGuard(ctx context.Context, cfg config.WireGuardConfig) error
}

type LinuxOps struct {
	logger *slog.Logger
}

func NewLinuxOps(logger *slog.Logger) *LinuxOps {
	if logger == nil {
		logger = slog.Default()
	}
	return &LinuxOps{logger: logger}
}
