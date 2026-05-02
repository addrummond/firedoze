//go:build linux

package proxy

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

const soOriginalDst = 80

func originalDestination(conn net.Conn) (*net.TCPAddr, error) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return nil, fmt.Errorf("connection is %T, not *net.TCPConn", conn)
	}
	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		return nil, err
	}

	var dst *net.TCPAddr
	var sockErr error
	controlErr := rawConn.Control(func(fd uintptr) {
		var addr [16]byte
		size := uint32(len(addr))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(syscall.SOL_IP),
			uintptr(soOriginalDst),
			uintptr(unsafe.Pointer(&addr[0])),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			sockErr = errno
			return
		}
		port := int(binary.BigEndian.Uint16(addr[2:4]))
		ip := net.IPv4(addr[4], addr[5], addr[6], addr[7])
		dst = &net.TCPAddr{IP: ip, Port: port}
	})
	if controlErr != nil {
		return nil, controlErr
	}
	if sockErr != nil {
		return nil, sockErr
	}
	if dst == nil {
		return nil, fmt.Errorf("original destination unavailable")
	}
	return dst, nil
}
