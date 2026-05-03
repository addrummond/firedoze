package main

import (
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"firedoze/internal/model"
	wgconfig "firedoze/internal/wireguard"
)

const (
	defaultAPIPort       = "8081"
	baseImageWarmupLabel = "preparing base image metadata (hashes once after image changes)"
)

type client struct {
	baseURL string
	http    *http.Client
}

type app struct {
	client *client
	json   bool
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
	if commandNeedsAPI(flags.Args()) {
		resolvedAPIURL, err := resolveClientAPIURL(apiURL, serverName)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		c, err = newClient(resolvedAPIURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	a := app{client: c, json: *jsonOutput}
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
	default:
		return true
	}
}

func newClient(rawURL string) (*client, error) {
	normalizedURL, err := normalizeAPIURL(rawURL)
	if err != nil {
		return nil, err
	}
	return &client{
		baseURL: normalizedURL,
		http: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}, nil
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
	case "start":
		if len(args) != 2 {
			return errors.New("usage: firedoze start <vm>")
		}
		vm, err := a.startVM(args[1])
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"vm": vm})
		}
		fmt.Printf("%s started\n", vm.Name)
		return nil
	case "reboot":
		if len(args) != 2 {
			return errors.New("usage: firedoze reboot <vm>")
		}
		vm, err := a.rebootVM(args[1])
		if err != nil {
			return err
		}
		if a.json {
			return printJSON(map[string]any{"vm": vm})
		}
		fmt.Printf("%s rebooted\n", vm.Name)
		return nil
	case "publish", "hide":
		if len(args) != 2 {
			return fmt.Errorf("usage: firedoze %s <vm>", args[0])
		}
		public := args[0] == "publish"
		return a.setPublicHTTP(args[1], public)
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
	case "exec":
		return a.exec(args[1:])
	case "cp":
		return a.cp(args[1:])
	case "up":
		return a.up(args[1:])
	case "with-vm-ip":
		return a.withVMIP(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a app) wg(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze wg keygen")
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
	default:
		return fmt.Errorf("unknown wg command: %s", args[0])
	}
}

func (a app) vm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze vm <list|inspect|create|start|reboot|sleep|stop|delete|settings>")
	}
	switch args[0] {
	case "list", "ls":
		patterns := args[1:]
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
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSTATE\tRUNTIME\tPRIVATE IP\tPUBLIC URL")
		for _, vm := range out.VMs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", vm.Name, vm.State, runtimeSinceStart(vm), vm.PrivateIP, displayURL(vm))
		}
		return w.Flush()
	case "inspect", "show":
		if len(args) != 2 {
			return errors.New("usage: firedoze vm inspect <name>")
		}
		var out struct {
			VM vmInfo `json:"vm"`
		}
		if err := a.client.do(context.Background(), http.MethodGet, "/vms/"+url.PathEscape(args[1]), nil, &out); err != nil {
			return err
		}
		return printJSON(out)
	case "create":
		params, names, err := parseVMCreateArgs("firedoze vm create", args[1:])
		if err != nil {
			return fmt.Errorf("%w\nusage: firedoze vm create <name> [name...] [-vcpus N] [-memory-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish]", err)
		}
		return a.createVMs(params, names)
	case "start", "stop":
		if len(args) != 2 {
			return fmt.Errorf("usage: firedoze vm %s <name>", args[0])
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
			methodPath := "/vms/" + url.PathEscape(args[1]) + "/" + args[0]
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
			return errors.New("usage: firedoze vm sleep <name> [name...]")
		}
		slept := []map[string]string{}
		for _, name := range args[1:] {
			var out map[string]any
			if err := a.client.do(context.Background(), http.MethodPost, "/vms/"+url.PathEscape(name)+"/sleep", nil, &out); err != nil {
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
			return errors.New("usage: firedoze vm reboot <name> [name...]")
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
	case "delete", "rm":
		if len(args) < 2 {
			return errors.New("usage: firedoze vm delete <name> [name...]")
		}
		deleted := []map[string]string{}
		for _, name := range args[1:] {
			var out map[string]any
			if err := a.client.do(context.Background(), http.MethodDelete, "/vms/"+url.PathEscape(name), nil, &out); err != nil {
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

type vmCreateParams struct {
	VCPUs                 int
	MemoryMiB             int
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
	memoryMiB := flags.Int("memory-mib", 0, "memory in MiB")
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
	return vmCreateParams{
		VCPUs:                 *vcpus,
		MemoryMiB:             *memoryMiB,
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
	addInt(body, "memory_mib", params.MemoryMiB)
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
	var out struct {
		VM vmInfo `json:"vm"`
	}
	if err := a.client.do(context.Background(), http.MethodPost, "/vms/"+url.PathEscape(name)+"/start", nil, &out); err != nil {
		return vmInfo{}, err
	}
	return out.VM, nil
}

func (a app) rebootVM(name string) (vmInfo, error) {
	var out struct {
		VM vmInfo `json:"vm"`
	}
	if err := a.client.do(context.Background(), http.MethodPost, "/vms/"+url.PathEscape(name)+"/reboot", nil, &out); err != nil {
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
		return fmt.Errorf("%w\nusage: firedoze vm settings <name> [-http-port N] [-idle-sleep-after N] [-auto-wake true|false] [-publish true|false]", err)
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
	var out map[string]any
	if err := a.client.do(context.Background(), http.MethodPatch, "/vms/"+url.PathEscape(name)+"/settings", body, &out); err != nil {
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
	body := map[string]any{"public_http": public}
	var out struct {
		VM vmInfo `json:"vm"`
	}
	if err := a.client.do(context.Background(), http.MethodPatch, "/vms/"+url.PathEscape(name)+"/settings", body, &out); err != nil {
		return vmInfo{}, err
	}
	return out.VM, nil
}

func (a app) snapshot(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze snapshot <list|inspect|save|restore|delete>")
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
		var out map[string]any
		body := map[string]any{"name": args[1], "vm": args[2]}
		if err := a.client.do(context.Background(), http.MethodPost, "/snapshots", body, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s saved from %s", args[1], args[2]))
	case "restore":
		params, snapshotName, vmName, err := parseSnapshotRestoreArgs(args[1:])
		if err != nil {
			return fmt.Errorf("%w\nusage: firedoze snapshot restore <snapshot> <vm> [-vcpus N] [-memory-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish]", err)
		}
		var out map[string]any
		body := restoreSnapshotBody(params, vmName)
		if err := a.client.do(context.Background(), http.MethodPost, "/snapshots/"+url.PathEscape(snapshotName)+"/restore", body, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s restored as %s", snapshotName, vmName))
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

func (a app) route(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze route <list|create|delete>")
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
		var out map[string]any
		body := map[string]any{"name": args[1], "vm": args[2], "port": port}
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
	default:
		return fmt.Errorf("unknown route command %q", args[0])
	}
}

func (a app) up(args []string) error {
	if a.json {
		return errors.New("firedoze up does not support -json")
	}
	createArgs, sshArgs := splitUpArgs(args)
	params, names, err := parseVMCreateArgs("firedoze up", createArgs)
	if err != nil {
		return fmt.Errorf("%w\nusage: firedoze up <name> [-vcpus N] [-memory-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish=false] [-- ssh args...]", err)
	}
	if len(names) != 1 {
		return errors.New("usage: firedoze up <name> [-vcpus N] [-memory-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish=false] [-- ssh args...]")
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
			vm, err = a.startVM(name)
			return err
		}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "VM %s is already running\n", name)
	}
	if err := runWithSpinner(os.Stderr, "waiting for SSH on "+vm.PrivateIP, func() error {
		return waitForSSH(vm.PrivateIP, 2*time.Minute)
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
	if ip == "" {
		return errors.New("VM has no private IP")
	}
	addr := net.JoinHostPort(ip, "22")
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for SSH at %s", addr)
		}
		time.Sleep(1 * time.Second)
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
	sshArgs := sshCommand(vm)
	sshArgs = append(sshArgs, args[1:]...)
	cmd := exec.Command(sshArgs[0], sshArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
	sshArgs := remoteExecCommand(vm, commandArgs)
	cmd := exec.Command(sshArgs[0], sshArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
	cmdArgs, err := rsyncCopyCommand(vm, src, dst)
	if err != nil {
		return err
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (a app) ensureVMReadyForSSH(name string) (vmInfo, error) {
	vm, err := a.lookupVM(name)
	if err != nil {
		return vmInfo{}, err
	}
	if vm.State == "running" {
		return vm, nil
	}
	vm, err = a.startVM(vm.Name)
	if err != nil {
		return vmInfo{}, err
	}
	if err := waitForSSH(vm.PrivateIP, 2*time.Minute); err != nil {
		return vmInfo{}, err
	}
	return vm, nil
}

func remoteExecCommand(vm vmInfo, commandArgs []string) []string {
	sshArgs := append(sshCommand(vm), "--")
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
	if vm.PrivateIP == "" {
		return nil, errors.New("VM has no private IP")
	}
	remoteUser, err := sshUser(vm)
	if err != nil {
		return nil, err
	}
	args := []string{"rsync", "-a", "-e", strings.Join(sshTransportCommand(), " ")}
	if src.remote() {
		args = append(args, rsyncRemote(remoteUser, vm.PrivateIP, src.path), dst.path)
	} else {
		args = append(args, src.path, rsyncRemote(remoteUser, vm.PrivateIP, dst.path))
	}
	return args, nil
}

func rsyncRemote(user string, ip string, path string) string {
	return fmt.Sprintf("%s@[%s]:%s", user, ip, path)
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
	return cmd.Run()
}

func (a app) lookupVM(name string) (vmInfo, error) {
	vm, found, err := a.findVM(name)
	if err != nil {
		return vmInfo{}, err
	}
	if !found {
		return vmInfo{}, fmt.Errorf("VM not found: %s", name)
	}
	return vm, nil
}

func (a app) findVM(name string) (vmInfo, bool, error) {
	var out struct {
		VMs []vmInfo `json:"vms"`
	}
	if err := a.client.do(context.Background(), http.MethodGet, "/vms", nil, &out); err != nil {
		return vmInfo{}, false, err
	}
	for _, vm := range out.VMs {
		if vm.Name == name {
			return vm, true, nil
		}
	}
	return vmInfo{}, false, nil
}

func sshCommand(vm vmInfo) []string {
	fields := strings.Fields(vm.SSH)
	if len(fields) >= 2 && vm.PrivateIP != "" {
		userHost := fields[1]
		if at := strings.LastIndex(userHost, "@"); at >= 0 {
			fields[1] = userHost[:at+1] + vm.PrivateIP
			return append(sshTransportCommand(), fields[1:]...)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return append(sshTransportCommand(), fields[1:]...)
}

func sshTransportCommand() []string {
	return []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "PubkeyAuthentication=no",
		"-o", "PreferredAuthentications=none,password",
		"-o", "NumberOfPasswordPrompts=1",
	}
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

func (c *client) do(ctx context.Context, method string, path string, body any, out any) error {
	var requestBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		requestBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, requestBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &errBody) == nil && errBody.Error != "" {
			return apiError{StatusCode: resp.StatusCode, Message: errBody.Error}
		}
		return apiError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
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
	body := map[string]any{"vm": vmName}
	addInt(body, "vcpus", params.VCPUs)
	addInt(body, "memory_mib", params.MemoryMiB)
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
  server add <name> <api-url> [-default]
  server list
  server use <name>
  server current
  server remove <name> [name...]
  server path
  start <vm>
  reboot <vm>
  publish <vm>
  hide <vm>
  vm list [name-glob...]
  vm inspect <name>
  vm create <name> [name...] [-vcpus N] [-memory-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish]
  vm start <name>
  vm reboot <name> [name...]
  vm sleep <name> [name...]
  vm stop <name>
  vm delete <name> [name...]
  vm settings <name> [-http-port N] [-idle-sleep-after N] [-auto-wake true|false] [-publish true|false]
  snapshot list
  snapshot inspect <snapshot>
  snapshot save <snapshot> <vm>
  snapshot restore <snapshot> <vm> [-vcpus N] [-memory-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish]
  snapshot delete <snapshot>
  route list
  route create <route> <vm> <port>
  route delete <route>
  wg keygen
  ssh <vm> [ssh args...]
  exec <vm> -- <command> [args...]
  cp <src> <dst>
  up <vm> [-vcpus N] [-memory-mib N] [-disk-bytes N] [-http-port N] [-idle-sleep-after N] [-no-auto-wake] [-publish=false] [-- ssh args...]
  with-vm-ip <vm> <command> [args...]

Environment:
  FIREDOZE_API     API URL override.
  FIREDOZE_SERVER  configured server name override.

Client config:
  Use "firedoze server add <name> <api-url> -default" once, then daemon
  commands use that server automatically. Port 8081 is added if omitted.
`)
}
