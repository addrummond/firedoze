package systemd

import (
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func Notify(state string) bool {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return false
	}
	addrName := socket
	if strings.HasPrefix(addrName, "@") {
		addrName = "\x00" + strings.TrimPrefix(addrName, "@")
	}
	addr := &net.UnixAddr{Name: addrName, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return false
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err == nil
}

func Ready() bool {
	return Notify("READY=1")
}

func Stopping() bool {
	return Notify("STOPPING=1")
}

func StartWatchdog(logger *slog.Logger) func() {
	raw := os.Getenv("WATCHDOG_USEC")
	if raw == "" {
		return func() {}
	}
	usec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || usec <= 0 {
		return func() {}
	}
	interval := time.Duration(usec) * time.Microsecond / 2
	if interval <= 0 {
		return func() {}
	}

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if ok := Notify("WATCHDOG=1"); !ok && logger != nil {
					logger.Debug("systemd watchdog notify failed")
				}
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}
