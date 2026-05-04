//go:build windows

package main

import (
	"os"
	"path/filepath"
)

func wireGuardBrokerRuntimeDir() string {
	return filepath.Join(os.TempDir(), "firedoze")
}
