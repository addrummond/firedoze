//go:build !linux

package proxy

import (
	"fmt"
	"net"
	"runtime"
)

func originalDestination(conn net.Conn) (*net.TCPAddr, error) {
	return nil, fmt.Errorf("original destination lookup is not supported on %s", runtime.GOOS)
}
