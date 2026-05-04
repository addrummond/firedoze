//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func wireGuardBrokerRuntimeDir() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); dir != "" {
		return filepath.Join(dir, "firedoze")
	}
	return filepath.Join(os.TempDir(), "firedoze-"+strconv.Itoa(os.Getuid()))
}
