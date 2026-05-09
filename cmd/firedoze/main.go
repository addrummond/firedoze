package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"firedoze/internal/clientwg"
	"firedoze/internal/model"
	wgconfig "firedoze/internal/wireguard"

	"github.com/google/uuid"
)

const (
	defaultAPIPort       = "8081"
	baseImageWarmupLabel = "preparing base image metadata (hashes once after image changes)"
)

type client struct {
	baseURL      string
	http         *http.Client
	wg           *clientwg.Client
	brokerSocket string
}

var newHTTPClient = func() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
	}
}

type app struct {
	client       *client
	serverName   string
	serverConfig clientServerConfig
	json         bool
	runCommand   func(*exec.Cmd) error
	waitForSSHFn func(string, time.Duration) error
	proxyDial    func(context.Context, string, string) (net.Conn, error)
	proxyInput   io.Reader
	proxyOutput  io.Writer
}

type vmInfo struct {
	model.VM
	Hostname string            `json:"hostname"`
	SSH      string            `json:"ssh"`
	URLs     map[string]string `json:"urls"`
}

type routeInfo struct {
	model.Route
	Hostname string `json:"hostname"`
	URL      string `json:"url"`
}

type snapshotInfo = model.Snapshot

type apiError struct {
	StatusCode int
	Message    string
}

func (e apiError) Error() string {
	return fmt.Sprintf("API returned %d: %s", e.StatusCode, e.Message)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	apiURL := os.Getenv("FIREDOZE_API")
	serverName := os.Getenv("FIREDOZE_SERVER")

	flags := flag.NewFlagSet("firedoze", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&apiURL, "api", apiURL, "firedoze API URL")
	flags.StringVar(&serverName, "server", serverName, "configured firedoze server name")
	jsonOutput := flags.Bool("json", false, "print raw JSON responses")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		usage()
		return 2
	}
	if flags.NArg() == 0 {
		usage()
		return 2
	}

	var c *client
	var resolvedServer clientServerConfig
	if commandNeedsAPI(flags.Args()) || commandNeedsServerConfig(flags.Args()) {
		var err error
		resolvedServer, err = resolveClientServer(apiURL, serverName)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if commandNeedsAPI(flags.Args()) {
		var err error
		c, err = newClientForServer(context.Background(), resolvedServer)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		defer c.Close()
	}
	resolvedServerName := resolvedServer.Name
	if resolvedServerName == "" {
		resolvedServerName = serverName
	}
	a := app{client: c, serverName: resolvedServerName, serverConfig: resolvedServer, json: *jsonOutput}
	if err := a.dispatch(flags.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func commandNeedsAPI(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "help", "-h", "--help":
		return false
	case "wg":
		return false
	case "server":
		return false
	case "health", "config", "vm", "snapshot", "route", "ssh", "ssh-proxy", "exec", "cp", "with-vm-ip":
		return true
	default:
		return false
	}
}

func commandNeedsServerConfig(args []string) bool {
	return len(args) > 0 && args[0] == "tunnel-daemon"
}

func newClient(rawURL string) (*client, error) {
	normalizedURL, err := normalizeAPIURL(rawURL)
	if err != nil {
		return nil, err
	}
	return &client{
		baseURL: normalizedURL,
		http:    newHTTPClient(),
	}, nil
}

func newClientForServer(ctx context.Context, server clientServerConfig) (*client, error) {
	normalizedURL, err := normalizeAPIURL(server.APIURL)
	if err != nil {
		return nil, err
	}
	httpClient := newHTTPClient()
	var wgClient *clientwg.Client
	brokerSocket := ""
	if server.WireGuard != nil {
		brokerSocket, err = ensureWireGuardBroker(ctx, server)
		if err != nil {
			return nil, err
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = clientwg.BrokerDialer{SocketPath: brokerSocket}.DialContext
		httpClient.Transport = transport
	}
	return &client{
		baseURL:      normalizedURL,
		http:         httpClient,
		wg:           wgClient,
		brokerSocket: brokerSocket,
	}, nil
}

func (c *client) Close() error {
	if c == nil {
		return nil
	}
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
	return c.CloseWireGuard()
}

func (c *client) CloseWireGuard() error {
	if c == nil {
		return nil
	}
	if c.wg != nil {
		err := c.wg.Close()
		c.wg = nil
		return err
	}
	return nil
}

func (c *client) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	if c != nil && c.brokerSocket != "" {
		return (clientwg.BrokerDialer{SocketPath: c.brokerSocket}).DialContext(ctx, network, address)
	}
	if c != nil && c.wg != nil {
		return c.wg.DialContext(ctx, network, address)
	}
	dialer := &net.Dialer{}
	return dialer.DialContext(ctx, network, address)
}

func normalizeAPIURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("API URL must include scheme and host: %s", rawURL)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), defaultAPIPort)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (a app) dispatch(args []string) error {
	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return nil
	case "health":
		var out map[string]any
		if err := a.client.do(context.Background(), http.MethodGet, "/health", nil, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, "ok")
	case "config":
		var out map[string]any
		if err := a.client.do(context.Background(), http.MethodGet, "/config", nil, &out); err != nil {
			return err
		}
		return printJSON(out)
	case "vm":
		return a.vm(args[1:])
	case "snapshot":
		return a.snapshot(args[1:])
	case "route":
		return a.route(args[1:])
	case "wg":
		return a.wg(args[1:])
	case "server":
		return a.server(args[1:])
	case "ssh":
		return a.ssh(args[1:])
	case "ssh-proxy":
		return a.sshProxy(args[1:])
	case "exec":
		return a.exec(args[1:])
	case "cp":
		return a.cp(args[1:])
	case "with-vm-ip":
		return a.withVMIP(args[1:])
	case "tunnel-daemon":
		return a.tunnelDaemon(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a app) wg(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze wg <keygen|pubkey>")
	}
	switch args[0] {
	case "keygen":
		if len(args) != 1 {
			return errors.New("usage: firedoze wg keygen")
		}
		keyPair, err := wgconfig.GenerateClientKeyPair()
		if err != nil {
			return err
		}
		fmt.Printf("private_key = %s\n", keyPair.PrivateKey)
		fmt.Printf("public_key = %s\n", keyPair.PublicKey)
		return nil
	case "pubkey":
		return a.wgPubkey(args[1:])
	default:
		return fmt.Errorf("unknown wg command: %s", args[0])
	}
}

func (a app) wgPubkey(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: firedoze wg pubkey [name]")
	}
	name := strings.TrimSpace(a.serverName)
	if len(args) == 1 {
		name = strings.TrimSpace(args[0])
	}
	cfg, path, err := loadClientConfig()
	if err != nil {
		return err
	}
	privateKey, err := findClientWireGuardPrivateKey(cfg, name)
	if err != nil {
		if path != "" {
			return fmt.Errorf("%w in %s", err, path)
		}
		return err
	}
	publicKey, err := wgconfig.PublicKeyFromPrivateKey(privateKey)
	if err != nil {
		return err
	}
	fmt.Println(publicKey)
	return nil
}

func findClientWireGuardPrivateKey(cfg clientConfig, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(cfg.DefaultServer)
	}
	if name == "" && len(cfg.Servers) == 1 {
		name = cfg.Servers[0].Name
	}
	if name == "" && len(cfg.PendingPeers) == 1 {
		name = cfg.PendingPeers[0].Name
	}
	if name == "" {
		return "", errors.New("missing Firedoze server/request name")
	}
	if server, ok := cfg.findServer(name); ok {
		if server.WireGuard == nil || strings.TrimSpace(server.WireGuard.PrivateKey) == "" {
			return "", fmt.Errorf("configured Firedoze server %q has no WireGuard private key", name)
		}
		return server.WireGuard.PrivateKey, nil
	}
	if pending, ok := cfg.findPendingByName(name); ok {
		return pending.PrivateKey, nil
	}
	return "", fmt.Errorf("unknown Firedoze server/request %q", name)
}

func (a app) vm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze vm <list|usage|inspect|id|create|up|start|reboot|sleep|stop|delete|publish|hide|settings>")
	}
	switch args[0] {
	case "list", "ls":
		listFlags := flag.NewFlagSet("firedoze vm list", flag.ContinueOnError)
		listFlags.SetOutput(io.Discard)
		namesOnly := listFlags.Bool("names", false, "print only VM names")
		idsOnly := listFlags.Bool("ids", false, "print only VM UUIDs")
		if err := listFlags.Parse(args[1:]); err != nil {
			return fmt.Errorf("%w\nusage: firedoze vm list [-names|-ids] [name-glob...]", err)
		}
		if *namesOnly && *idsOnly {
			return errors.New("firedoze vm list -names and -ids cannot be combined")
		}
		if a.json && (*namesOnly || *idsOnly) {
			return errors.New("firedoze vm list -names/-ids cannot be combined with -json")
		}
		patterns := listFlags.Args()
		var out struct {
			VMs []vmInfo `json:"vms"`
		}
		listURL := "/vms"
		if len(patterns) > 0 {
			values := url.Values{}
			for _, pattern := range patterns {
				values.Add("name", pattern)
			}
			listURL += "?" + values.Encode()
		}
		if err := a.client.do(context.Background(), http.MethodGet, listURL, nil, &out); err != nil {
			return err
		}
		if a.json {
			return printJSON(out)
		}
		if *namesOnly {
			for _, vm := range out.VMs {
				fmt.Println(vm.Name)
			}
			return nil
		}
		if *idsOnly {
			for _, vm := range out.VMs {
				fmt.Println(vm.UUID)
			}
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSTATE\tRUNTIME\tPRIVATE IP\tPUBLIC URL")
		for _, vm := range out.VMs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", vm.Name, vm.State, runtimeSinceStart(vm), vm.PrivateIP, displayURL(vm))
		}
		return w.Flush()
	case "usage":
		return a.vmUsage(args[1:])
	case "inspect", "show":
		if len(args) != 2 {
			return errors.New("usage: firedoze vm inspect <vm>")
		}
		vm, err := a.lookupVM(args[1])
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"vm": vm})
	case "id", "uuid":
		if len(args) != 2 {
			return errors.New("usage: firedoze vm id <vm>")
		}
		vm, err := a.lookupVM(args[1])
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"uuid": vm.UUID})
		}
		fmt.Println(vm.UUID)
		return nil
	case "create":
		params, names, err := parseVMCreateArgs("firedoze vm create", args[1:])
		if err != nil {
			return fmt.Errorf("%w\nusage: firedoze vm create <name> [name...] [-vcpus N] [-memory-min-mib N] [-memory-max-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish]", err)
		}
		return a.createVMs(params, names)
	case "start", "stop":
		if len(args) != 2 {
			return fmt.Errorf("usage: firedoze vm %s <vm>", args[0])
		}
		if args[0] == "start" {
			vm, err := a.startVM(args[1])
			if err != nil {
				return err
			}
			if a.json {
				return printJSON(map[string]any{"vm": vm})
			}
		} else {
			vm, err := a.lookupVM(args[1])
			if err != nil {
				return err
			}
			methodPath := "/vms/" + url.PathEscape(vm.UUID) + "/" + args[0]
			var out map[string]any
			if err := a.client.do(context.Background(), http.MethodPost, methodPath, nil, &out); err != nil {
				return err
			}
			if a.json {
				return printJSON(out)
			}
		}
		return a.printJSONOrLine(map[string]any{"status": pastTense(args[0])}, fmt.Sprintf("%s %s", args[1], pastTense(args[0])))
	case "sleep":
		if len(args) < 2 {
			return errors.New("usage: firedoze vm sleep <vm> [vm...]")
		}
		slept := []map[string]string{}
		for _, name := range args[1:] {
			vm, err := a.lookupVM(name)
			if err != nil {
				return err
			}
			var out map[string]any
			if err := a.client.do(context.Background(), http.MethodPost, "/vms/"+url.PathEscape(vm.UUID)+"/sleep", nil, &out); err != nil {
				return err
			}
			slept = append(slept, map[string]string{"name": name, "status": "slept"})
		}
		if a.json {
			return printJSON(map[string]any{"vms": slept})
		}
		for _, vm := range slept {
			fmt.Printf("%s slept\n", vm["name"])
		}
		return nil
	case "reboot":
		if len(args) < 2 {
			return errors.New("usage: firedoze vm reboot <vm> [vm...]")
		}
		rebooted := []vmInfo{}
		for _, name := range args[1:] {
			vm, err := a.rebootVM(name)
			if err != nil {
				return err
			}
			rebooted = append(rebooted, vm)
		}
		if a.json {
			return printJSON(map[string]any{"vms": rebooted})
		}
		for _, vm := range rebooted {
			fmt.Printf("%s rebooted\n", vm.Name)
		}
		return nil
	case "publish", "hide":
		if len(args) != 2 {
			return fmt.Errorf("usage: firedoze vm %s <vm>", args[0])
		}
		public := args[0] == "publish"
		return a.setPublicHTTP(args[1], public)
	case "up":
		return a.up(args[1:])
	case "delete", "rm":
		if len(args) < 2 {
			return errors.New("usage: firedoze vm delete <vm> [vm...]")
		}
		deleted := []map[string]string{}
		for _, name := range args[1:] {
			vm, err := a.lookupVM(name)
			if err != nil {
				return err
			}
			var out map[string]any
			if err := a.client.do(context.Background(), http.MethodDelete, "/vms/"+url.PathEscape(vm.UUID), nil, &out); err != nil {
				return err
			}
			deleted = append(deleted, map[string]string{"name": name, "status": "deleted"})
		}
		if a.json {
			return printJSON(map[string]any{"vms": deleted})
		}
		for _, vm := range deleted {
			fmt.Printf("%s deleted\n", vm["name"])
		}
		return nil
	case "settings":
		return a.vmSettings(args[1:])
	default:
		return fmt.Errorf("unknown vm command %q", args[0])
	}
}

func (a app) vmUsage(args []string) error {
	var out struct {
		VMs []model.VMResourceUsage `json:"vms"`
	}
	usageURL := "/usage"
	if len(args) > 0 {
		values := url.Values{}
		for _, pattern := range args {
			values.Add("name", pattern)
		}
		usageURL += "?" + values.Encode()
	}
	if err := a.client.do(context.Background(), http.MethodGet, usageURL, nil, &out); err != nil {
		return err
	}
	if a.json {
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tVCPU\tMEMORY\tGUEST MEM AVAIL/TOTAL\tGUEST SWAP FREE/TOTAL\tGUEST DISK FREE/TOTAL\tLOAD\tHOTPLUG\tHOST MEM\tHOST CPU\tHOST IO")
	for _, vm := range out.VMs {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			vm.Name,
			vm.State,
			vm.VCPUs,
			displayMemoryRange(vm),
			displayGuestMemory(vm),
			displayGuestSwap(vm),
			displayGuestDisk(vm),
			displayGuestLoad(vm),
			displayMemoryHotplug(vm),
			displayHostMemory(vm),
			displayHostCPU(vm),
			displayHostIO(vm),
		)
	}
	return w.Flush()
}

type vmCreateParams struct {
	VCPUs                 int
	MemoryMinMiB          int
	MemoryMaxMiB          int
	DiskBytes             int64
	DefaultHTTPPort       int
	IdleSleepAfterSeconds int
	AutoWake              bool
	NoAutoWake            bool
	PublicHTTP            bool
}

func parseVMCreateArgs(command string, args []string) (vmCreateParams, []string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	vcpus := flags.Int("vcpus", 0, "vCPUs")
	memoryMinMiB := flags.Int("memory-min-mib", 0, "minimum memory in MiB")
	memoryMaxMiB := flags.Int("memory-max-mib", 0, "maximum memory in MiB")
	diskBytes := flags.Int64("disk-bytes", 0, "disk size in bytes")
	httpPort := flags.Int("http-port", 0, "default guest HTTP port")
	idle := flags.Int("idle-sleep-after", 0, "idle sleep timeout in seconds")
	autoWake := flags.Bool("auto-wake", false, "allow passive network traffic to wake this VM (default)")
	noAutoWake := flags.Bool("no-auto-wake", false, "disable passive network traffic wake for this VM")
	publicHTTP := flags.Bool("publish", false, "publish the VM over public HTTPS")
	names, err := parseNamesAndFlags(flags, args)
	if err != nil {
		return vmCreateParams{}, nil, err
	}
	if *autoWake && *noAutoWake {
		return vmCreateParams{}, nil, errors.New("-auto-wake and -no-auto-wake cannot both be set")
	}
	if *memoryMinMiB != 0 && *memoryMaxMiB != 0 && *memoryMinMiB > *memoryMaxMiB {
		return vmCreateParams{}, nil, errors.New("-memory-min-mib must be less than or equal to -memory-max-mib")
	}
	return vmCreateParams{
		VCPUs:                 *vcpus,
		MemoryMinMiB:          *memoryMinMiB,
		MemoryMaxMiB:          *memoryMaxMiB,
		DiskBytes:             *diskBytes,
		DefaultHTTPPort:       *httpPort,
		IdleSleepAfterSeconds: *idle,
		AutoWake:              *autoWake,
		NoAutoWake:            *noAutoWake,
		PublicHTTP:            *publicHTTP,
	}, names, nil
}

func (a app) createVMs(params vmCreateParams, names []string) error {
	if err := a.prepareBaseImageMetadata(); err != nil {
		return err
	}
	created := []vmInfo{}
	for _, name := range names {
		vm, err := a.createVM(params, name)
		if err != nil {
			return err
		}
		created = append(created, vm)
	}
	if a.json {
		return printJSON(map[string]any{"vms": created})
	}
	for _, vm := range created {
		fmt.Printf("%s created\n", vm.Name)
	}
	return nil
}

func (a app) prepareBaseImageMetadata() error {
	if a.json {
		return a.warmBaseImageMetadata()
	}
	return runWithSpinner(os.Stderr, baseImageWarmupLabel, a.warmBaseImageMetadata)
}

func (a app) warmBaseImageMetadata() error {
	var out map[string]any
	return a.client.do(context.Background(), http.MethodPost, "/base-image/warmup", nil, &out)
}

func (a app) createVM(params vmCreateParams, name string) (vmInfo, error) {
	body := map[string]any{"name": name}
	addInt(body, "vcpus", params.VCPUs)
	addInt(body, "memory_min_mib", params.MemoryMinMiB)
	addInt(body, "memory_max_mib", params.MemoryMaxMiB)
	addInt64(body, "disk_bytes", params.DiskBytes)
	addInt(body, "default_http_port", params.DefaultHTTPPort)
	addInt(body, "idle_sleep_after_seconds", params.IdleSleepAfterSeconds)
	if params.AutoWake {
		body["auto_wake"] = true
	}
	if params.NoAutoWake {
		body["auto_wake"] = false
	}
	if params.PublicHTTP {
		body["public_http"] = true
	}
	var out struct {
		VM vmInfo `json:"vm"`
	}
	if err := a.client.do(context.Background(), http.MethodPost, "/vms", body, &out); err != nil {
		return vmInfo{}, err
	}
	return out.VM, nil
}

func (a app) startVM(name string) (vmInfo, error) {
	vm, err := a.lookupVM(name)
	if err != nil {
		return vmInfo{}, err
	}
	return a.startVMByUUID(vm.UUID)
}

func (a app) startVMByUUID(vmUUID string) (vmInfo, error) {
	var out struct {
		VM vmInfo `json:"vm"`
	}
	if err := a.client.do(context.Background(), http.MethodPost, "/vms/"+url.PathEscape(vmUUID)+"/start", nil, &out); err != nil {
		return vmInfo{}, err
	}
	return out.VM, nil
}

func (a app) rebootVM(name string) (vmInfo, error) {
	vm, err := a.lookupVM(name)
	if err != nil {
		return vmInfo{}, err
	}
	var out struct {
		VM vmInfo `json:"vm"`
	}
	if err := a.client.do(context.Background(), http.MethodPost, "/vms/"+url.PathEscape(vm.UUID)+"/reboot", nil, &out); err != nil {
		return vmInfo{}, err
	}
	return out.VM, nil
}

func (a app) vmSettings(args []string) error {
	flags := flag.NewFlagSet("firedoze vm settings", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	httpPort := flags.Int("http-port", -1, "default guest HTTP port")
	idle := flags.Int("idle-sleep-after", -1, "idle sleep timeout in seconds")
	autoWake := optionalBoolFlag(flags, "auto-wake")
	publicHTTP := optionalBoolFlag(flags, "publish")
	name, err := parseNameAndFlags(flags, args)
	if err != nil {
		return fmt.Errorf("%w\nusage: firedoze vm settings <vm> [-http-port N] [-idle-sleep-after N] [-auto-wake true|false] [-publish true|false]", err)
	}
	body := map[string]any{}
	if *httpPort >= 0 {
		body["default_http_port"] = *httpPort
	}
	if *idle >= 0 {
		body["idle_sleep_after_seconds"] = *idle
	}
	if autoWake.set {
		body["auto_wake"] = autoWake.value
	}
	if publicHTTP.set {
		body["public_http"] = publicHTTP.value
	}
	if len(body) == 0 {
		return errors.New("no settings provided")
	}
	vm, err := a.lookupVM(name)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := a.client.do(context.Background(), http.MethodPatch, "/vms/"+url.PathEscape(vm.UUID)+"/settings", body, &out); err != nil {
		return err
	}
	return a.printJSONOrLine(out, fmt.Sprintf("%s settings updated", name))
}

func (a app) setPublicHTTP(name string, public bool) error {
	vm, err := a.updatePublicHTTP(name, public)
	if err != nil {
		return err
	}
	status := "hidden"
	if public {
		status = "public"
	}
	return a.printJSONOrLine(map[string]any{"status": status, "vm": vm}, fmt.Sprintf("%s is now %s", name, status))
}

func (a app) updatePublicHTTP(name string, public bool) (vmInfo, error) {
	vm, err := a.lookupVM(name)
	if err != nil {
		return vmInfo{}, err
	}
	body := map[string]any{"public_http": public}
	var out struct {
		VM vmInfo `json:"vm"`
	}
	if err := a.client.do(context.Background(), http.MethodPatch, "/vms/"+url.PathEscape(vm.UUID)+"/settings", body, &out); err != nil {
		return vmInfo{}, err
	}
	return out.VM, nil
}

func (a app) snapshot(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze snapshot <list|inspect|save|restore|export|import|delete>")
	}
	switch args[0] {
	case "list", "ls":
		var out struct {
			Snapshots []snapshotInfo `json:"snapshots"`
		}
		if err := a.client.do(context.Background(), http.MethodGet, "/snapshots", nil, &out); err != nil {
			return err
		}
		if a.json {
			return printJSON(out)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSOURCE_VM\tCREATED_AT")
		for _, snap := range out.Snapshots {
			fmt.Fprintf(w, "%s\t%s\t%s\n", snap.Name, snap.SourceVM, snap.CreatedAt)
		}
		return w.Flush()
	case "inspect", "show":
		if len(args) != 2 {
			return errors.New("usage: firedoze snapshot inspect <snapshot>")
		}
		var out struct {
			Snapshot snapshotInfo `json:"snapshot"`
		}
		if err := a.client.do(context.Background(), http.MethodGet, "/snapshots/"+url.PathEscape(args[1]), nil, &out); err != nil {
			return err
		}
		return printJSON(out)
	case "save":
		if len(args) != 3 {
			return errors.New("usage: firedoze snapshot save <snapshot> <vm>")
		}
		vm, err := a.lookupVM(args[2])
		if err != nil {
			return err
		}
		var out map[string]any
		body := map[string]any{"name": args[1], "vm_uuid": vm.UUID}
		if err := a.client.do(context.Background(), http.MethodPost, "/snapshots", body, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s saved from %s", args[1], args[2]))
	case "restore":
		params, snapshotName, vmName, err := parseSnapshotRestoreArgs(args[1:])
		if err != nil {
			return fmt.Errorf("%w\nusage: firedoze snapshot restore <snapshot> <name> [-vcpus N] [-memory-min-mib N] [-memory-max-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish]", err)
		}
		var out map[string]any
		body := restoreSnapshotBody(params, vmName)
		if err := a.client.do(context.Background(), http.MethodPost, "/snapshots/"+url.PathEscape(snapshotName)+"/restore", body, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s restored as %s", snapshotName, vmName))
	case "export":
		if len(args) != 3 {
			return errors.New("usage: firedoze snapshot export <snapshot> <file>")
		}
		return a.exportSnapshot(args[1], args[2])
	case "import":
		if len(args) != 3 {
			return errors.New("usage: firedoze snapshot import <snapshot> <file>")
		}
		return a.importSnapshot(args[1], args[2])
	case "delete", "rm":
		if len(args) != 2 {
			return errors.New("usage: firedoze snapshot delete <snapshot>")
		}
		var out map[string]any
		if err := a.client.do(context.Background(), http.MethodDelete, "/snapshots/"+url.PathEscape(args[1]), nil, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s deleted", args[1]))
	default:
		return fmt.Errorf("unknown snapshot command %q", args[0])
	}
}

func (a app) exportSnapshot(name string, outputPath string) error {
	if outputPath == "" {
		return errors.New("output file is required")
	}
	dir := filepath.Dir(outputPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(outputPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	resp, err := a.client.doRaw(context.Background(), http.MethodGet, "/snapshots/"+url.PathEscape(name)+"/export", nil, "")
	if err != nil {
		_ = tmp.Close()
		return err
	}
	defer resp.Body.Close()
	_, copyErr := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		return err
	}
	cleanup = false
	return a.printJSONOrLine(map[string]string{"snapshot": name, "file": outputPath}, fmt.Sprintf("%s exported to %s", name, outputPath))
}

func (a app) importSnapshot(name string, inputPath string) error {
	in, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer in.Close()
	var out map[string]any
	if err := a.client.doStreamJSON(context.Background(), http.MethodPost, "/snapshots/"+url.PathEscape(name)+"/import", "application/gzip", in, &out); err != nil {
		return err
	}
	return a.printJSONOrLine(out, fmt.Sprintf("%s imported from %s", name, inputPath))
}

func (a app) route(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze route <list|create|delete|protect|unprotect|get-signed-url>")
	}
	switch args[0] {
	case "list", "ls":
		var out struct {
			Routes []routeInfo `json:"routes"`
		}
		if err := a.client.do(context.Background(), http.MethodGet, "/routes", nil, &out); err != nil {
			return err
		}
		if a.json {
			return printJSON(out)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tVM\tPORT\tURL")
		for _, route := range out.Routes {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", route.Name, route.VMName, route.Port, route.URL)
		}
		return w.Flush()
	case "create":
		if len(args) != 4 {
			return errors.New("usage: firedoze route create <route> <vm> <port>")
		}
		port, err := strconv.Atoi(args[3])
		if err != nil {
			return fmt.Errorf("invalid port %q", args[3])
		}
		vm, err := a.lookupVM(args[2])
		if err != nil {
			return err
		}
		var out map[string]any
		body := map[string]any{"name": args[1], "vm_uuid": vm.UUID, "port": port}
		if err := a.client.do(context.Background(), http.MethodPost, "/routes", body, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s routes to %s:%d", args[1], args[2], port))
	case "delete", "rm":
		if len(args) != 2 {
			return errors.New("usage: firedoze route delete <route>")
		}
		var out map[string]any
		if err := a.client.do(context.Background(), http.MethodDelete, "/routes/"+url.PathEscape(args[1]), nil, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s deleted", args[1]))
	case "protect":
		if len(args) != 2 {
			return errors.New("usage: firedoze route protect <hostname>")
		}
		var out map[string]any
		if err := a.client.do(context.Background(), http.MethodPost, "/route-protections", map[string]string{"hostname": args[1]}, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s protected", args[1]))
	case "unprotect":
		if len(args) != 2 {
			return errors.New("usage: firedoze route unprotect <hostname>")
		}
		var out map[string]any
		if err := a.client.do(context.Background(), http.MethodDelete, "/route-protections/"+url.PathEscape(args[1]), nil, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s unprotected", args[1]))
	case "get-signed-url":
		target, ttl, err := parseRouteSignedURLArgs(args[1:])
		if err != nil {
			return errors.New("usage: firedoze route get-signed-url <hostname[/path]> [-ttl seconds]")
		}
		hostname, next, err := parseRouteSignedURLTarget(target)
		if err != nil {
			return err
		}
		var out struct {
			Hostname   string `json:"hostname"`
			URL        string `json:"url"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		body := map[string]any{"hostname": hostname, "ttl_seconds": ttl}
		if err := a.client.do(context.Background(), http.MethodPost, "/route-auth/signed-url", body, &out); err != nil {
			return err
		}
		if next != "" {
			out.URL, err = addSignedURLNext(out.URL, next)
			if err != nil {
				return err
			}
		}
		if a.json {
			return printJSON(out)
		}
		fmt.Println(out.URL)
		return nil
	default:
		return fmt.Errorf("unknown route command %q", args[0])
	}
}

func parseRouteSignedURLArgs(args []string) (string, int, error) {
	ttl := 24 * 60 * 60
	hostname := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-ttl", "--ttl":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing ttl")
			}
			value, err := strconv.Atoi(args[i+1])
			if err != nil {
				return "", 0, err
			}
			ttl = value
			i++
		default:
			if strings.HasPrefix(args[i], "-") || hostname != "" {
				return "", 0, errors.New("invalid args")
			}
			hostname = args[i]
		}
	}
	if hostname == "" {
		return "", 0, errors.New("missing hostname")
	}
	return hostname, ttl, nil
}

func parseRouteSignedURLTarget(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("missing hostname")
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", "", err
		}
		if parsed.Host == "" {
			return "", "", errors.New("missing hostname")
		}
		if parsed.Port() != "" {
			return "", "", errors.New("hostname must not include a port")
		}
		next := parsed.EscapedPath()
		if next == "" {
			next = parsed.Path
		}
		if next == "" {
			next = "/"
		}
		if parsed.RawQuery != "" {
			next += "?" + parsed.RawQuery
		}
		if parsed.Fragment != "" {
			next += "#" + parsed.EscapedFragment()
		}
		next, err = normalizeSignedURLNext(next)
		if err != nil {
			return "", "", err
		}
		return parsed.Hostname(), next, nil
	}
	index := strings.IndexAny(raw, "/?#")
	if index < 0 {
		return raw, "", nil
	}
	hostname := raw[:index]
	if hostname == "" {
		return "", "", errors.New("missing hostname")
	}
	next := raw[index:]
	if strings.HasPrefix(next, "?") || strings.HasPrefix(next, "#") {
		next = "/" + next
	}
	next, err := normalizeSignedURLNext(next)
	if err != nil {
		return "", "", err
	}
	return hostname, next, nil
}

func normalizeSignedURLNext(next string) (string, error) {
	if next == "" || next == "/" {
		return "", nil
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "", errors.New("signed URL path must be a relative absolute path")
	}
	return next, nil
}

func addSignedURLNext(rawURL string, next string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := parsed.Query()
	q.Set("next", next)
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

func (a app) up(args []string) error {
	if a.json {
		return errors.New("firedoze vm up does not support -json")
	}
	createArgs, sshArgs := splitUpArgs(args)
	params, names, err := parseVMCreateArgs("firedoze vm up", createArgs)
	if err != nil {
		return fmt.Errorf("%w\nusage: firedoze vm up <name> [-vcpus N] [-memory-min-mib N] [-memory-max-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish=false] [-- ssh args...]", err)
	}
	if len(names) != 1 {
		return errors.New("usage: firedoze vm up <name> [-vcpus N] [-memory-min-mib N] [-memory-max-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish=false] [-- ssh args...]")
	}
	name := names[0]
	publishOnUp := true
	if foundFlag(createArgs, "publish") {
		publishOnUp = params.PublicHTTP
	}
	params.PublicHTTP = publishOnUp
	var vm vmInfo
	var found bool
	if err := runWithSpinner(os.Stderr, "checking VM "+name, func() error {
		var err error
		vm, found, err = a.findVM(name)
		return err
	}); err != nil {
		return err
	}
	if !found {
		if _, ok := parseVMUUIDRef(name); ok {
			return fmt.Errorf("VM not found: %s", name)
		}
		if err := runWithSpinner(os.Stderr, baseImageWarmupLabel, a.warmBaseImageMetadata); err != nil {
			return err
		}
		if err := runWithSpinner(os.Stderr, "creating VM "+name, func() error {
			var err error
			vm, err = a.createVM(params, name)
			return err
		}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "using existing VM %s (%s)\n", name, vm.State)
	}
	if publishOnUp && !vm.PublicHTTP {
		if err := runWithSpinner(os.Stderr, "publishing VM "+name, func() error {
			vm, err = a.updatePublicHTTP(name, true)
			return err
		}); err != nil {
			return err
		}
	}
	if vm.State != "running" {
		if err := runWithSpinner(os.Stderr, "starting VM "+name, func() error {
			var err error
			vm, err = a.startVMByUUID(vm.UUID)
			return err
		}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "VM %s is already running\n", name)
	}
	if err := runWithSpinner(os.Stderr, "waiting for SSH on "+vm.PrivateIP, func() error {
		return a.waitForSSH(vm.PrivateIP, 2*time.Minute)
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "connecting to %s (%s)\n", name, vm.PrivateIP)
	return a.ssh(append([]string{name}, sshArgs...))
}

func runWithSpinner(w io.Writer, label string, fn func() error) error {
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	if !isTerminal(w) {
		fmt.Fprintf(w, "start: %s\n", label)
		err := <-done
		if err != nil {
			fmt.Fprintf(w, "failed: %s (%s)\n", label, formatDuration(time.Since(started)))
			return err
		}
		fmt.Fprintf(w, "done: %s (%s)\n", label, formatDuration(time.Since(started)))
		return nil
	}

	frames := []byte{'|', '/', '-', '\\'}
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()

	frame := 0
	for {
		select {
		case err := <-done:
			fmt.Fprint(w, "\r\033[K")
			if err != nil {
				fmt.Fprintf(w, "failed: %s (%s)\n", label, formatDuration(time.Since(started)))
				return err
			}
			fmt.Fprintf(w, "done: %s (%s)\n", label, formatDuration(time.Since(started)))
			return nil
		case <-ticker.C:
			fmt.Fprintf(w, "\r%c %s", frames[frame%len(frames)], label)
			frame++
		}
	}
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func splitUpArgs(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func waitForSSH(ip string, timeout time.Duration) error {
	dialer := &net.Dialer{}
	return waitForSSHWithDial(ip, timeout, dialer.DialContext)
}

func waitForSSHWithDial(ip string, timeout time.Duration, dial func(context.Context, string, string) (net.Conn, error)) error {
	if ip == "" {
		return errors.New("VM has no private IP")
	}
	addr := net.JoinHostPort(ip, "22")
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := dial(ctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for SSH at %s", addr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (a app) ssh(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: firedoze ssh <vm> [ssh args...]")
	}
	vm, err := a.ensureVMReadyForSSH(args[0])
	if err != nil {
		return err
	}
	sshArgs := a.sshCommand(vm)
	sshArgs = append(sshArgs, args[1:]...)
	if err := a.closeEmbeddedWireGuardBeforeProxyCommand(); err != nil {
		return err
	}
	cmd := exec.Command(sshArgs[0], sshArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return a.runWithActivity(vm, cmd)
}

func (a app) sshProxy(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: firedoze ssh-proxy <vm>")
	}
	vm, err := a.ensureVMReadyForSSH(args[0])
	if err != nil {
		return err
	}
	if vm.PrivateIP == "" {
		return fmt.Errorf("VM %s has no private IP", args[0])
	}
	conn, err := a.dialContext()(context.Background(), "tcp", net.JoinHostPort(vm.PrivateIP, "22"))
	if err != nil {
		return err
	}
	defer conn.Close()
	stopActivity := a.activityHeartbeat(vm)
	defer stopActivity()

	input := a.proxyInput
	if input == nil {
		input = os.Stdin
	}
	output := a.proxyOutput
	if output == nil {
		output = os.Stdout
	}
	return proxyConnection(conn, input, output)
}

func (a app) exec(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: firedoze exec <vm> -- <command> [args...]")
	}
	vmName := args[0]
	commandArgs := args[1:]
	if commandArgs[0] == "--" {
		commandArgs = commandArgs[1:]
	}
	if len(commandArgs) == 0 {
		return errors.New("usage: firedoze exec <vm> -- <command> [args...]")
	}
	vm, err := a.ensureVMReadyForSSH(vmName)
	if err != nil {
		return err
	}
	sshArgs := a.remoteExecCommand(vm, commandArgs)
	if err := a.closeEmbeddedWireGuardBeforeProxyCommand(); err != nil {
		return err
	}
	cmd := exec.Command(sshArgs[0], sshArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return a.runWithActivity(vm, cmd)
}

func (a app) cp(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: firedoze cp <src> <dst>")
	}
	src, dst, err := parseCopyEndpoints(args[0], args[1])
	if err != nil {
		return err
	}
	vm, err := a.ensureVMReadyForSSH(src.vmNameOr(dst.vmName))
	if err != nil {
		return err
	}
	cmdArgs, err := a.rsyncCopyCommand(vm, src, dst)
	if err != nil {
		return err
	}
	if err := a.closeEmbeddedWireGuardBeforeProxyCommand(); err != nil {
		return err
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return a.runWithActivity(vm, cmd)
}

func (a app) ensureVMReadyForSSH(name string) (vmInfo, error) {
	vm, err := a.lookupVM(name)
	if err != nil {
		return vmInfo{}, err
	}
	if vm.State == "running" {
		return vm, nil
	}
	vm, err = a.startVMByUUID(vm.UUID)
	if err != nil {
		return vmInfo{}, err
	}
	if err := a.waitForSSH(vm.PrivateIP, 2*time.Minute); err != nil {
		return vmInfo{}, err
	}
	return vm, nil
}

func (a app) touchVMActivity(name string) error {
	vm, err := a.lookupVM(name)
	if err != nil {
		return err
	}
	return a.touchVMActivityByUUID(vm.UUID)
}

func (a app) touchVMActivityByUUID(vmUUID string) error {
	if a.client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out struct {
		VM vmInfo `json:"vm"`
	}
	return a.client.do(ctx, http.MethodPost, "/vms/"+url.PathEscape(vmUUID)+"/activity", nil, &out)
}

func (a app) activityHeartbeat(vm vmInfo) func() {
	_ = a.touchVMActivityByUUID(vm.UUID)
	interval := activityHeartbeatInterval(vm)
	if interval <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = a.touchVMActivityByUUID(vm.UUID)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func activityHeartbeatInterval(vm vmInfo) time.Duration {
	seconds := vm.IdleSleepAfterSeconds
	if seconds <= 0 {
		return 0
	}
	interval := time.Duration(seconds) * time.Second / 4
	if interval < 10*time.Second {
		return 10 * time.Second
	}
	return interval
}

func remoteExecCommand(vm vmInfo, commandArgs []string) []string {
	return remoteExecCommandWithProxy(vm, commandArgs, "")
}

func (a app) remoteExecCommand(vm vmInfo, commandArgs []string) []string {
	return remoteExecCommandWithProxy(vm, commandArgs, a.sshProxyCommand(vm.Name))
}

func remoteExecCommandWithProxy(vm vmInfo, commandArgs []string, proxyCommand string) []string {
	sshArgs := append(sshCommandWithProxy(vm, proxyCommand), "--")
	return append(sshArgs, commandArgs...)
}

type copyEndpoint struct {
	raw    string
	vmName string
	path   string
}

func (e copyEndpoint) remote() bool {
	return e.vmName != ""
}

func (e copyEndpoint) vmNameOr(other string) string {
	if e.vmName != "" {
		return e.vmName
	}
	return other
}

func parseCopyEndpoints(src string, dst string) (copyEndpoint, copyEndpoint, error) {
	srcEndpoint := parseCopyEndpoint(src)
	dstEndpoint := parseCopyEndpoint(dst)
	if srcEndpoint.remote() == dstEndpoint.remote() {
		return copyEndpoint{}, copyEndpoint{}, errors.New("usage: firedoze cp requires exactly one endpoint in the form <vm>:<path>")
	}
	if srcEndpoint.remote() && dstEndpoint.remote() && srcEndpoint.vmName != dstEndpoint.vmName {
		return copyEndpoint{}, copyEndpoint{}, errors.New("copying directly between VMs is not supported")
	}
	return srcEndpoint, dstEndpoint, nil
}

func parseCopyEndpoint(raw string) copyEndpoint {
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") {
		return copyEndpoint{raw: raw, path: raw}
	}
	vmName, path, ok := strings.Cut(raw, ":")
	if !ok || vmName == "" || path == "" || strings.Contains(vmName, "/") {
		return copyEndpoint{raw: raw, path: raw}
	}
	return copyEndpoint{raw: raw, vmName: vmName, path: path}
}

func rsyncCopyCommand(vm vmInfo, src copyEndpoint, dst copyEndpoint) ([]string, error) {
	return rsyncCopyCommandWithProxy(vm, src, dst, "")
}

func (a app) rsyncCopyCommand(vm vmInfo, src copyEndpoint, dst copyEndpoint) ([]string, error) {
	return rsyncCopyCommandWithProxy(vm, src, dst, a.sshProxyCommand(vm.Name))
}

func rsyncCopyCommandWithProxy(vm vmInfo, src copyEndpoint, dst copyEndpoint, proxyCommand string) ([]string, error) {
	if vm.PrivateIP == "" {
		return nil, errors.New("VM has no private IP")
	}
	remoteUser, err := sshUser(vm)
	if err != nil {
		return nil, err
	}
	remoteHost := vm.PrivateIP
	if proxyCommand != "" {
		remoteHost = vm.Name
	}
	args := []string{"rsync", "-a", "-e", strings.Join(sshTransportCommand(proxyCommand), " ")}
	if src.remote() {
		args = append(args, rsyncRemote(remoteUser, remoteHost, src.path), dst.path)
	} else {
		args = append(args, src.path, rsyncRemote(remoteUser, remoteHost, dst.path))
	}
	return args, nil
}

func rsyncRemote(user string, host string, path string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s@%s:%s", user, host, path)
}

func proxyConnection(conn net.Conn, input io.Reader, output io.Writer) error {
	errs := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, input)
		if closeWriter, ok := conn.(interface{ CloseWrite() error }); ok {
			if closeErr := closeWriter.CloseWrite(); err == nil {
				err = closeErr
			}
		} else {
			_ = conn.Close()
		}
		errs <- err
	}()
	go func() {
		_, err := io.Copy(output, conn)
		_ = conn.Close()
		errs <- err
	}()

	var joined error
	for i := 0; i < 2; i++ {
		err := <-errs
		if ignoreProxyCopyError(err) {
			continue
		}
		joined = errors.Join(joined, err)
	}
	return joined
}

func ignoreProxyCopyError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}

func (a app) withVMIP(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: firedoze with-vm-ip <vm> <command> [args...]")
	}
	vm, err := a.lookupVM(args[0])
	if err != nil {
		return err
	}
	if vm.PrivateIP == "" {
		return fmt.Errorf("VM %s has no private IP", args[0])
	}
	cmd := exec.Command(args[1], args[2:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "FIREDOZE_VM_IP="+vm.PrivateIP)
	return a.run(cmd)
}

func (a app) tunnelDaemon(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: firedoze tunnel-daemon")
	}
	if a.serverConfig.WireGuard == nil {
		return errors.New("selected server has no imported WireGuard config")
	}
	socketPath, err := wireGuardBrokerSocketPath(a.serverConfig)
	if err != nil {
		return err
	}
	if err := clientwg.RunBroker(context.Background(), a.serverConfig.WireGuard.clientWGConfig(), socketPath, 10*time.Minute); err != nil {
		if errors.Is(err, clientwg.ErrBrokerAlreadyRunning) {
			return nil
		}
		return err
	}
	return nil
}

func (a app) run(cmd *exec.Cmd) error {
	if a.runCommand != nil {
		return a.runCommand(cmd)
	}
	return cmd.Run()
}

func (a app) runWithActivity(vm vmInfo, cmd *exec.Cmd) error {
	stopActivity := a.activityHeartbeat(vm)
	defer stopActivity()
	return a.run(cmd)
}

func (a app) waitForSSH(ip string, timeout time.Duration) error {
	if a.waitForSSHFn != nil {
		return a.waitForSSHFn(ip, timeout)
	}
	return waitForSSHWithDial(ip, timeout, a.dialContext())
}

func (a app) dialContext() func(context.Context, string, string) (net.Conn, error) {
	if a.proxyDial != nil {
		return a.proxyDial
	}
	if a.client != nil {
		return a.client.DialContext
	}
	dialer := &net.Dialer{}
	return dialer.DialContext
}

func (a app) lookupVM(ref string) (vmInfo, error) {
	vm, found, err := a.findVM(ref)
	if err != nil {
		return vmInfo{}, err
	}
	if !found {
		return vmInfo{}, fmt.Errorf("VM not found: %s", ref)
	}
	return vm, nil
}

func (a app) findVM(ref string) (vmInfo, bool, error) {
	var out struct {
		VM vmInfo `json:"vm"`
	}
	path := "/vms-by-name/" + url.PathEscape(ref)
	if parsed, ok := parseVMUUIDRef(ref); ok {
		path = "/vms/" + url.PathEscape(parsed)
	}
	if err := a.client.do(context.Background(), http.MethodGet, path, nil, &out); err != nil {
		var apiErr apiError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return vmInfo{}, false, nil
		}
		return vmInfo{}, false, err
	}
	return out.VM, true, nil
}

func parseVMUUIDRef(ref string) (string, bool) {
	parsed, err := uuid.Parse(ref)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

func sshCommand(vm vmInfo) []string {
	return sshCommandWithProxy(vm, "")
}

func (a app) sshCommand(vm vmInfo) []string {
	return sshCommandWithProxy(vm, a.sshProxyCommand(vm.Name))
}

func sshCommandWithProxy(vm vmInfo, proxyCommand string) []string {
	fields := strings.Fields(vm.SSH)
	if proxyCommand != "" && len(fields) >= 2 {
		target := vm.Name
		if at := strings.LastIndex(fields[1], "@"); at >= 0 {
			target = fields[1][:at+1] + vm.Name
		}
		args := append(sshTransportCommand(proxyCommand), target)
		return append(args, fields[2:]...)
	}
	if len(fields) >= 2 && vm.PrivateIP != "" {
		userHost := fields[1]
		if at := strings.LastIndex(userHost, "@"); at >= 0 {
			fields[1] = userHost[:at+1] + vm.PrivateIP
			return append(sshTransportCommand(""), fields[1:]...)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return append(sshTransportCommand(""), fields[1:]...)
}

func sshTransportCommand(proxyCommand string) []string {
	args := []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "PubkeyAuthentication=no",
		"-o", "PreferredAuthentications=none,password",
		"-o", "NumberOfPasswordPrompts=1",
	}
	if proxyCommand != "" {
		args = append(args, "-o", "ProxyCommand="+proxyCommand)
	}
	return args
}

func (a app) sshProxyCommand(vmName string) string {
	if !a.usesEmbeddedWireGuard() {
		return ""
	}
	args := []string{firedozeCommandPath()}
	if a.serverName != "" {
		args = append(args, "-server", a.serverName)
	}
	args = append(args, "ssh-proxy", vmName)
	return shellJoin(args)
}

func (a app) usesEmbeddedWireGuard() bool {
	return a.client != nil && (a.client.wg != nil || a.client.brokerSocket != "")
}

func (a app) closeEmbeddedWireGuardBeforeProxyCommand() error {
	if !a.usesEmbeddedWireGuard() {
		return nil
	}
	return a.client.CloseWireGuard()
}

var firedozeCommandPath = func() string {
	path, err := os.Executable()
	if err != nil || path == "" {
		return "firedoze"
	}
	return path
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '_' || r == '-' || r == ':' || r == '=' || r == '@' ||
			r >= '0' && r <= '9' ||
			r >= 'A' && r <= 'Z' ||
			r >= 'a' && r <= 'z')
	}) < 0 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func ensureWireGuardBroker(ctx context.Context, server clientServerConfig) (string, error) {
	socketPath, err := wireGuardBrokerSocketPath(server)
	if err != nil {
		return "", err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	err = clientwg.PingBroker(pingCtx, socketPath)
	cancel()
	if err == nil {
		return socketPath, nil
	}
	if server.Name == "" {
		return "", errors.New("imported WireGuard server has no name")
	}
	cmd := exec.Command(firedozeCommandPath(), "-server", server.Name, "tunnel-daemon")
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	detachCommand(cmd)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		lastErr = clientwg.PingBroker(pingCtx, socketPath)
		cancel()
		if lastErr == nil {
			return socketPath, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("start wireguard broker: %w", lastErr)
}

func wireGuardBrokerSocketPath(server clientServerConfig) (string, error) {
	if server.WireGuard == nil {
		return "", errors.New("server has no WireGuard config")
	}
	dir := wireGuardBrokerRuntimeDir()
	key := strings.Join([]string{
		server.Name,
		server.APIURL,
		server.WireGuard.Address,
		server.WireGuard.ServerPublicKey,
		server.WireGuard.Endpoint,
	}, "\x00")
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, "wg-"+hex.EncodeToString(sum[:8])+".sock"), nil
}

func sshUser(vm vmInfo) (string, error) {
	fields := strings.Fields(vm.SSH)
	if len(fields) < 2 {
		return "", errors.New("VM has no SSH command")
	}
	if at := strings.LastIndex(fields[1], "@"); at >= 0 {
		return fields[1][:at], nil
	}
	return "", fmt.Errorf("VM SSH command has no user: %s", vm.SSH)
}

func runtimeSinceStart(vm vmInfo) string {
	if vm.State != "running" || vm.LastStartedAt == "" {
		return "-"
	}
	startedAt, err := time.Parse(time.RFC3339Nano, vm.LastStartedAt)
	if err != nil {
		return "-"
	}
	elapsed := time.Since(startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return formatDuration(elapsed)
}

func displayURL(vm vmInfo) string {
	if !vm.PublicHTTP {
		return "-"
	}
	return vm.URLs["default"]
}

func displayProcessRSS(vm model.VMResourceUsage) string {
	if vm.Process == nil || vm.Process.RSSBytes == 0 {
		return "-"
	}
	return formatBytes(vm.Process.RSSBytes)
}

func displayProcessCPU(vm model.VMResourceUsage) string {
	if vm.Process == nil || vm.Process.CPUSeconds == 0 {
		return "-"
	}
	return formatDuration(time.Duration(vm.Process.CPUSeconds * float64(time.Second)))
}

func formatMiB(value int64) string {
	return fmt.Sprintf("%dMiB", value)
}

func formatDuration(duration time.Duration) string {
	duration = duration.Truncate(time.Second)
	days := duration / (24 * time.Hour)
	duration -= days * 24 * time.Hour
	hours := duration / time.Hour
	duration -= hours * time.Hour
	minutes := duration / time.Minute
	duration -= minutes * time.Minute
	seconds := duration / time.Second

	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func formatBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%dB", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	f := float64(value)
	for _, suffix := range units {
		f /= unit
		if f < unit {
			if f >= 10 {
				return fmt.Sprintf("%.0f%s", f, suffix)
			}
			return fmt.Sprintf("%.1f%s", f, suffix)
		}
	}
	return fmt.Sprintf("%.1fEiB", f/unit)
}

func (c *client) do(ctx context.Context, method string, path string, body any, out any) error {
	var requestBody io.Reader
	contentType := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		requestBody = bytes.NewReader(data)
		contentType = "application/json"
	}
	resp, err := c.doRaw(ctx, method, path, requestBody, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *client) doStreamJSON(ctx context.Context, method string, path string, contentType string, body io.Reader, out any) error {
	resp, err := c.doRaw(ctx, method, path, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *client) doRaw(ctx context.Context, method string, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &errBody) == nil && errBody.Error != "" {
			return nil, apiError{StatusCode: resp.StatusCode, Message: errBody.Error}
		}
		return nil, apiError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	return resp, nil
}

func (a app) printJSONOrLine(value any, line string) error {
	if a.json {
		return printJSON(value)
	}
	fmt.Println(line)
	return nil
}

func printJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func addInt(body map[string]any, key string, value int) {
	if value != 0 {
		body[key] = value
	}
}

func addInt64(body map[string]any, key string, value int64) {
	if value != 0 {
		body[key] = value
	}
}

func displayMemoryRange(vm model.VMResourceUsage) string {
	if vm.MemoryMinMiB == vm.MemoryMaxMiB {
		return formatMiB(int64(vm.MemoryMaxMiB))
	}
	return fmt.Sprintf("%s-%s", formatMiB(int64(vm.MemoryMinMiB)), formatMiB(int64(vm.MemoryMaxMiB)))
}

func displayMemoryHotplug(vm model.VMResourceUsage) string {
	if vm.MemoryHotplug == nil {
		return "-"
	}
	return fmt.Sprintf("%s/%s",
		formatMiB(int64(vm.MemoryHotplug.PluggedMiB)),
		formatMiB(int64(vm.MemoryHotplug.RequestedMiB)),
	)
}

func displayGuestMemory(vm model.VMResourceUsage) string {
	if vm.GuestMemory == nil || vm.GuestMemory.TotalMiB == 0 {
		return "-"
	}
	if vm.GuestMemory.AvailableMiB == 0 {
		return formatMiB(int64(vm.GuestMemory.TotalMiB))
	}
	return fmt.Sprintf("%s/%s", formatMiB(int64(vm.GuestMemory.AvailableMiB)), formatMiB(int64(vm.GuestMemory.TotalMiB)))
}

func displayGuestSwap(vm model.VMResourceUsage) string {
	if vm.GuestMemory == nil || vm.GuestMemory.SwapTotalMiB == 0 {
		return "-"
	}
	return fmt.Sprintf("%s/%s", formatMiB(int64(vm.GuestMemory.SwapFreeMiB)), formatMiB(int64(vm.GuestMemory.SwapTotalMiB)))
}

func displayGuestDisk(vm model.VMResourceUsage) string {
	if vm.GuestMemory == nil || vm.GuestMemory.RootDiskTotalBytes == 0 {
		return "-"
	}
	return fmt.Sprintf("%s/%s", formatBytes(vm.GuestMemory.RootDiskFreeBytes), formatBytes(vm.GuestMemory.RootDiskTotalBytes))
}

func displayGuestLoad(vm model.VMResourceUsage) string {
	if vm.GuestMemory == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", vm.GuestMemory.Load1)
}

func displayHostMemory(vm model.VMResourceUsage) string {
	var value uint64
	if vm.Cgroup != nil {
		value = vm.Cgroup.MemoryCurrentBytes
	}
	if vm.Process != nil && vm.Process.RSSBytes > value {
		value = vm.Process.RSSBytes
	}
	if value == 0 {
		return "-"
	}
	return formatBytes(value)
}

func displayHostCPU(vm model.VMResourceUsage) string {
	if vm.Cgroup != nil && vm.Cgroup.CPUUsageSeconds != 0 {
		return formatDuration(time.Duration(vm.Cgroup.CPUUsageSeconds * float64(time.Second)))
	}
	return displayProcessCPU(vm)
}

func displayHostIO(vm model.VMResourceUsage) string {
	if vm.Cgroup == nil || (vm.Cgroup.IOReadBytes == 0 && vm.Cgroup.IOWriteBytes == 0) {
		return "-"
	}
	return fmt.Sprintf("%s/%s", formatBytes(vm.Cgroup.IOReadBytes), formatBytes(vm.Cgroup.IOWriteBytes))
}

func parseSnapshotRestoreArgs(args []string) (vmCreateParams, string, string, error) {
	params, names, err := parseVMCreateArgs("firedoze snapshot restore", args)
	if err != nil {
		return vmCreateParams{}, "", "", err
	}
	if len(names) != 2 {
		return vmCreateParams{}, "", "", errors.New("restore requires snapshot and VM names")
	}
	return params, names[0], names[1], nil
}

func restoreSnapshotBody(params vmCreateParams, vmName string) map[string]any {
	body := map[string]any{"vm_name": vmName}
	addInt(body, "vcpus", params.VCPUs)
	addInt(body, "memory_min_mib", params.MemoryMinMiB)
	addInt(body, "memory_max_mib", params.MemoryMaxMiB)
	addInt64(body, "disk_bytes", params.DiskBytes)
	addInt(body, "default_http_port", params.DefaultHTTPPort)
	addInt(body, "idle_sleep_after_seconds", params.IdleSleepAfterSeconds)
	if params.AutoWake {
		body["auto_wake"] = true
	}
	if params.NoAutoWake {
		body["auto_wake"] = false
	}
	if params.PublicHTTP {
		body["public_http"] = true
	}
	return body
}

type optionalBool struct {
	value bool
	set   bool
}

func optionalBoolFlag(flags *flag.FlagSet, name string) *optionalBool {
	value := &optionalBool{}
	flags.Func(name, "true or false", func(raw string) error {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		value.value = parsed
		value.set = true
		return nil
	})
	return value
}

func parseNameAndFlags(flags *flag.FlagSet, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("missing name")
	}
	if !strings.HasPrefix(args[0], "-") {
		if err := flags.Parse(args[1:]); err != nil {
			return "", err
		}
		if flags.NArg() != 0 {
			return "", fmt.Errorf("unexpected argument %q", flags.Arg(0))
		}
		return args[0], nil
	}
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 1 {
		return "", errors.New("missing name")
	}
	return flags.Arg(0), nil
}

func parseNamesAndFlags(flags *flag.FlagSet, args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, errors.New("missing name")
	}
	var names []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if strings.Contains(arg, "=") {
				continue
			}
			if flagConsumesValue(flags, arg) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
			continue
		}
		names = append(names, arg)
	}
	if err := flags.Parse(flagArgs); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if len(names) == 0 {
		return nil, errors.New("missing name")
	}
	return names, nil
}

func flagConsumesValue(flags *flag.FlagSet, arg string) bool {
	name := strings.TrimLeft(arg, "-")
	if name == "" {
		return false
	}
	flagValue := flags.Lookup(name)
	if flagValue == nil {
		return true
	}
	if boolFlag, ok := flagValue.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
		return false
	}
	return true
}

func foundFlag(args []string, name string) bool {
	for _, arg := range args {
		trimmed := strings.TrimLeft(arg, "-")
		if trimmed == name || strings.HasPrefix(trimmed, name+"=") {
			return true
		}
	}
	return false
}

func pastTense(verb string) string {
	switch verb {
	case "start":
		return "started"
	case "sleep":
		return "slept"
	case "stop":
		return "stopped"
	default:
		return verb
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: firedoze [-api URL] [-server NAME] [-json] <command>

Commands:
  health
  config
  server request <name>
  server import <file> [-name NAME] [-default]
  server add <name> <api-url> [-default]
  server list
  server use <name>
  server current
  server remove <name> [name...]
  server path
  vm list [-names|-ids] [name-glob...]
  vm usage [name-glob...]
  vm inspect <vm>
  vm id <vm>
  vm create <name> [name...] [-vcpus N] [-memory-min-mib N] [-memory-max-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish]
  vm up <name> [-vcpus N] [-memory-min-mib N] [-memory-max-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish=false] [-- ssh args...]
  vm start <vm>
  vm reboot <vm> [vm...]
  vm sleep <vm> [vm...]
  vm stop <vm>
  vm delete <vm> [vm...]
  vm publish <vm>
  vm hide <vm>
  vm settings <vm> [-http-port N] [-idle-sleep-after N] [-auto-wake true|false] [-publish true|false]
  snapshot list
  snapshot inspect <snapshot>
  snapshot save <snapshot> <vm>
  snapshot restore <snapshot> <name> [-vcpus N] [-memory-min-mib N] [-memory-max-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish]
  snapshot export <snapshot> <file>
  snapshot import <snapshot> <file>
  snapshot delete <snapshot>
  route list
  route create <route> <vm> <port>
  route delete <route>
  route protect <hostname>
  route unprotect <hostname>
  route get-signed-url <hostname[/path]> [-ttl seconds]
  wg keygen
  wg pubkey [name]
  ssh <vm> [ssh args...]
  ssh-proxy <vm>
  exec <vm> -- <command> [args...]
  cp <src> <dst>
  with-vm-ip <vm> <command> [args...]

Environment:
  FIREDOZE_API     API URL override.
  FIREDOZE_SERVER  configured server name override.

Client config:
  Use "firedoze server request <name>", send the printed public key to the
  Firedoze admin, then import the returned config with "firedoze server import".
  Port 8081 is added to API URLs if omitted.
`)
}
