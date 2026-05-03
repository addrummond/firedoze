package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#111827"/><text x="32" y="43" text-anchor="middle" font-size="38">😴</text></svg>`

func main() {
	port := "8080"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		fmt.Fprintln(os.Stderr, "usage: firedoze-hello [port]")
		os.Exit(2)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleHello)
	mux.HandleFunc("/favicon.ico", handleFavicon)
	mux.HandleFunc("/favicon.svg", handleFavicon)

	addr := "[::]:" + port
	log.Printf("firedoze-hello listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = fmt.Fprintln(w, faviconSVG)
}

func handleHello(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, helloText())
}

func helloText() string {
	var b strings.Builder
	fmt.Fprintln(&b, "firedoze hello")
	fmt.Fprintln(&b, "==============")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Host")
	fmt.Fprintf(&b, "  time:     %s\n", time.Now().Format(time.RFC3339))
	if hostname, err := os.Hostname(); err == nil {
		fmt.Fprintf(&b, "  hostname: %s\n", hostname)
	}
	fmt.Fprintf(&b, "  user:     %s\n", userText())
	fmt.Fprintf(&b, "  kernel:   %s\n", kernelText())
	fmt.Fprintf(&b, "  uptime:   %s\n", uptimeText())
	fmt.Fprintf(&b, "  load:     %s\n", firstFields("/proc/loadavg", 3, "unknown"))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Network")
	for _, line := range globalIPv6Addrs() {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Routes")
	for _, line := range ipv6Routes() {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
}

func userText() string {
	name := strings.TrimSpace(commandOutput("id", "-un"))
	if name == "" {
		name = "uid"
	}
	id := strings.TrimSpace(commandOutput("id"))
	if id != "" {
		return id
	}
	return fmt.Sprintf("%s (uid %d)", name, os.Getuid())
}

func kernelText() string {
	kernel := strings.TrimSpace(commandOutput("uname", "-s", "-r"))
	if kernel != "" {
		return kernel
	}
	name := strings.TrimSpace(readText("/proc/sys/kernel/ostype"))
	release := strings.TrimSpace(readText("/proc/sys/kernel/osrelease"))
	if name != "" || release != "" {
		return strings.TrimSpace(name + " " + release)
	}
	return "unknown"
}

func uptimeText() string {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "unknown"
	}
	secondsText, _, _ := strings.Cut(string(data), " ")
	secondsFloat, err := strconv.ParseFloat(secondsText, 64)
	if err != nil {
		return "unknown"
	}
	seconds := int64(secondsFloat)
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func firstFields(path string, count int, fallback string) string {
	data := readText(path)
	if data == "" {
		return fallback
	}
	fields := strings.Fields(data)
	if len(fields) < count {
		count = len(fields)
	}
	if count == 0 {
		return fallback
	}
	return strings.Join(fields[:count], " ")
}

func globalIPv6Addrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var lines []string
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, ipnet, ok := parseAddr(addr)
			if !ok || ip.To4() != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			ones, _ := ipnet.Mask.Size()
			lines = append(lines, fmt.Sprintf("%-8s %s/%d", iface.Name, ip, ones))
		}
	}
	return lines
}

func parseAddr(addr net.Addr) (net.IP, *net.IPNet, bool) {
	ipnet, ok := addr.(*net.IPNet)
	if !ok {
		return nil, nil, false
	}
	return ipnet.IP, ipnet, true
}

func ipv6Routes() []string {
	output := commandOutput("ip", "-6", "route")
	if output == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func commandOutput(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	data, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(data)
}

func readText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
