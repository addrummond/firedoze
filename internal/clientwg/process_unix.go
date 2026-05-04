//go:build !windows

package clientwg

import (
	"errors"
	"syscall"
)

func brokerProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
