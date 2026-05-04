package main

import (
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHandleFavicon(t *testing.T) {
	req := httptest.NewRequest("GET", "/favicon.ico", nil)
	rec := httptest.NewRecorder()

	handleFavicon(rec, req)

	res := rec.Result()
	if got, want := res.Header.Get("Content-Type"), "image/svg+xml; charset=utf-8"; got != want {
		t.Fatalf("content type = %q, want %q", got, want)
	}
	if got, want := res.Header.Get("Cache-Control"), "public, max-age=86400"; got != want {
		t.Fatalf("cache control = %q, want %q", got, want)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "😴") {
		t.Fatalf("favicon body = %q, want sleepy SVG", body)
	}
}

func TestHelloTextPlain(t *testing.T) {
	resetHelloDeps(t)
	verbose = false
	now = func() time.Time {
		return time.Date(2026, 5, 4, 12, 34, 56, 0, time.UTC)
	}
	hostname = func() (string, error) {
		return "demo-vm", nil
	}
	readFile = func(path string) ([]byte, error) {
		if path == "/proc/uptime" {
			return []byte("3661.0 0.0\n"), nil
		}
		return nil, os.ErrNotExist
	}

	got := helloText()
	for _, want := range []string{
		"firedoze hello\n==============\n",
		"Host\n",
		"  time:     2026-05-04T12:34:56Z\n",
		"  hostname: demo-vm\n",
		"  uptime:   1h 1m\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plain hello missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{"  user:", "Network\n", "Routes\n"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("plain hello unexpectedly included %q:\n%s", notWant, got)
		}
	}
}

func TestHelloTextVerbose(t *testing.T) {
	resetHelloDeps(t)
	verbose = true
	now = func() time.Time {
		return time.Date(2026, 5, 4, 12, 34, 56, 0, time.UTC)
	}
	hostname = func() (string, error) {
		return "verbose-vm", nil
	}
	readFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/uptime":
			return []byte("65.0 0.0\n"), nil
		case "/proc/loadavg":
			return []byte("0.10 0.20 0.30 1/99 123\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}
	commandOutput = func(name string, args ...string) string {
		cmd := name + " " + strings.Join(args, " ")
		switch cmd {
		case "id -un":
			return "ubuntu\n"
		case "id ":
			return "uid=1000(ubuntu) gid=1000(ubuntu) groups=1000(ubuntu),27(sudo)\n"
		case "uname -s -r":
			return "Linux 6.8.0-106-generic\n"
		case "ip -6 route":
			return "default via fd31:99d:610d:aa00::a dev eth0\nfd31:99d:610d:aa00::a/127 dev eth0\n\n"
		default:
			return ""
		}
	}
	netInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Name: "eth0"}, {Name: "lo"}}, nil
	}
	interfaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
		switch iface.Name {
		case "eth0":
			return []net.Addr{
				mustCIDR(t, "fd31:99d:610d:aa00::b/127"),
				mustCIDR(t, "fe80::1/64"),
				mustCIDR(t, "10.0.0.2/24"),
			}, nil
		case "lo":
			return []net.Addr{mustCIDR(t, "::1/128")}, nil
		default:
			return nil, errors.New("unexpected interface")
		}
	}

	got := helloText()
	for _, want := range []string{
		"  hostname: verbose-vm\n",
		"  uptime:   1m\n",
		"  user:     uid=1000(ubuntu) gid=1000(ubuntu) groups=1000(ubuntu),27(sudo)\n",
		"  kernel:   Linux 6.8.0-106-generic\n",
		"  load:     0.10 0.20 0.30\n",
		"Network\n",
		"  eth0     fd31:99d:610d:aa00::b/127\n",
		"Routes\n",
		"  default via fd31:99d:610d:aa00::a dev eth0\n",
		"  fd31:99d:610d:aa00::a/127 dev eth0\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose hello missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{"fe80::1", "10.0.0.2", "::1/128"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("verbose hello included filtered address %q:\n%s", notWant, got)
		}
	}
}

func TestHostDataFallbacks(t *testing.T) {
	resetHelloDeps(t)
	commandOutput = func(string, ...string) string {
		return ""
	}
	readFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/sys/kernel/ostype":
			return []byte("Linux\n"), nil
		case "/proc/sys/kernel/osrelease":
			return []byte("6.8.0\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	if got, want := kernelText(), "Linux 6.8.0"; got != want {
		t.Fatalf("kernelText fallback = %q, want %q", got, want)
	}
	if got, want := userText(), fmt.Sprintf("uid (uid %d)", os.Getuid()); got != want {
		t.Fatalf("userText fallback = %q, want %q", got, want)
	}
	if got, want := uptimeText(), "unknown"; got != want {
		t.Fatalf("uptimeText fallback = %q, want %q", got, want)
	}
	if got, want := firstFields("/missing", 3, "fallback"), "fallback"; got != want {
		t.Fatalf("firstFields fallback = %q, want %q", got, want)
	}
}

func TestHandleHello(t *testing.T) {
	resetHelloDeps(t)
	verbose = false
	now = func() time.Time {
		return time.Date(2026, 5, 4, 12, 34, 56, 0, time.UTC)
	}
	hostname = func() (string, error) {
		return "handler-vm", nil
	}
	readFile = func(path string) ([]byte, error) {
		if path == "/proc/uptime" {
			return []byte("0.0 0.0\n"), nil
		}
		return nil, os.ErrNotExist
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	handleHello(rec, req)

	if got, want := rec.Result().Header.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Fatalf("content type = %q, want %q", got, want)
	}
	if !strings.Contains(rec.Body.String(), "hostname: handler-vm") {
		t.Fatalf("hello response missing hostname:\n%s", rec.Body.String())
	}
}

func resetHelloDeps(t *testing.T) {
	t.Helper()
	oldVerbose := verbose
	oldNow := now
	oldHostname := hostname
	oldReadFile := readFile
	oldCommandOutput := commandOutput
	oldNetInterfaces := netInterfaces
	oldInterfaceAddrs := interfaceAddrs
	t.Cleanup(func() {
		verbose = oldVerbose
		now = oldNow
		hostname = oldHostname
		readFile = oldReadFile
		commandOutput = oldCommandOutput
		netInterfaces = oldNetInterfaces
		interfaceAddrs = oldInterfaceAddrs
	})
}

func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	ipNet.IP = ip
	return ipNet
}
