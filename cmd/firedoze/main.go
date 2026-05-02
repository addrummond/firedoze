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

	"firedoze/internal/store"
	wgconfig "firedoze/internal/wireguard"
)

const (
	defaultAPIPort = "8081"
	defaultAPI     = "http://10.77.0.1:" + defaultAPIPort
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
	store.VM
	Hostname string            `json:"hostname"`
	SSH      string            `json:"ssh"`
	URLs     map[string]string `json:"urls"`
}

type routeInfo struct {
	store.Route
	Hostname string `json:"hostname"`
	URL      string `json:"url"`
}

type snapshotInfo = store.Snapshot

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
	if apiURL == "" {
		apiURL = defaultAPI
	}

	flags := flag.NewFlagSet("firedoze", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&apiURL, "api", apiURL, "firedoze API URL")
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

	c, err := newClient(apiURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	a := app{client: c, json: *jsonOutput}
	if err := a.dispatch(flags.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func newClient(rawURL string) (*client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("API URL must include scheme and host: %s", rawURL)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), defaultAPIPort)
	}
	return &client{
		baseURL: strings.TrimRight(u.String(), "/"),
		http: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}, nil
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
	case "ssh":
		return a.ssh(args[1:])
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
		return errors.New("usage: firedoze vm <list|inspect|create|start|sleep|stop|delete|settings>")
	}
	switch args[0] {
	case "list", "ls":
		var out struct {
			VMs []vmInfo `json:"vms"`
		}
		if err := a.client.do(context.Background(), http.MethodGet, "/vms", nil, &out); err != nil {
			return err
		}
		if a.json {
			return printJSON(out)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSTATE\tRUNTIME\tIP\tURL")
		for _, vm := range out.VMs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", vm.Name, vm.State, runtimeSinceStart(vm), vm.PrivateIP, vm.URLs["default"])
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
			return fmt.Errorf("%w\nusage: firedoze vm create <name> [name...] [--vcpus N] [--memory-mib N] [--disk-bytes N] [--http-port N] [--idle-sleep-after N]", err)
		}
		return a.createVMs(params, names)
	case "start", "sleep", "stop":
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
}

func parseVMCreateArgs(command string, args []string) (vmCreateParams, []string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	vcpus := flags.Int("vcpus", 0, "vCPUs")
	memoryMiB := flags.Int("memory-mib", 0, "memory in MiB")
	diskBytes := flags.Int64("disk-bytes", 0, "disk size in bytes")
	httpPort := flags.Int("http-port", 0, "default guest HTTP port")
	idle := flags.Int("idle-sleep-after", 0, "idle sleep timeout in seconds")
	names, err := parseNamesAndFlags(flags, args)
	if err != nil {
		return vmCreateParams{}, nil, err
	}
	return vmCreateParams{
		VCPUs:                 *vcpus,
		MemoryMiB:             *memoryMiB,
		DiskBytes:             *diskBytes,
		DefaultHTTPPort:       *httpPort,
		IdleSleepAfterSeconds: *idle,
	}, names, nil
}

func (a app) createVMs(params vmCreateParams, names []string) error {
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

func (a app) createVM(params vmCreateParams, name string) (vmInfo, error) {
	body := map[string]any{"name": name}
	addInt(body, "vcpus", params.VCPUs)
	addInt(body, "memory_mib", params.MemoryMiB)
	addInt64(body, "disk_bytes", params.DiskBytes)
	addInt(body, "default_http_port", params.DefaultHTTPPort)
	addInt(body, "idle_sleep_after_seconds", params.IdleSleepAfterSeconds)
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

func (a app) vmSettings(args []string) error {
	flags := flag.NewFlagSet("firedoze vm settings", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	httpPort := flags.Int("http-port", -1, "default guest HTTP port")
	idle := flags.Int("idle-sleep-after", -1, "idle sleep timeout in seconds")
	name, err := parseNameAndFlags(flags, args)
	if err != nil {
		return fmt.Errorf("%w\nusage: firedoze vm settings <name> [--http-port N] [--idle-sleep-after N]", err)
	}
	body := map[string]any{}
	if *httpPort >= 0 {
		body["default_http_port"] = *httpPort
	}
	if *idle >= 0 {
		body["idle_sleep_after_seconds"] = *idle
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
		if len(args) != 3 {
			return errors.New("usage: firedoze snapshot restore <snapshot> <vm>")
		}
		var out map[string]any
		body := map[string]any{"vm": args[2]}
		if err := a.client.do(context.Background(), http.MethodPost, "/snapshots/"+url.PathEscape(args[1])+"/restore", body, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s restored as %s", args[1], args[2]))
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
		return errors.New("firedoze up does not support --json")
	}
	createArgs, sshArgs := splitUpArgs(args)
	params, names, err := parseVMCreateArgs("firedoze up", createArgs)
	if err != nil {
		return fmt.Errorf("%w\nusage: firedoze up <name> [--vcpus N] [--memory-mib N] [--disk-bytes N] [--http-port N] [--idle-sleep-after N] [-- ssh args...]", err)
	}
	if len(names) != 1 {
		return errors.New("usage: firedoze up <name> [--vcpus N] [--memory-mib N] [--disk-bytes N] [--http-port N] [--idle-sleep-after N] [-- ssh args...]")
	}
	name := names[0]
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
	vm, err := a.lookupVM(args[0])
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
			return withFiredozeSSHOptions(fields)
		}
	}
	return withFiredozeSSHOptions(fields)
}

func withFiredozeSSHOptions(fields []string) []string {
	if len(fields) == 0 {
		return nil
	}
	args := []string{
		fields[0],
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "PubkeyAuthentication=no",
		"-o", "PreferredAuthentications=none,password",
		"-o", "NumberOfPasswordPrompts=1",
	}
	return append(args, fields[1:]...)
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
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
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
	fmt.Fprint(os.Stderr, `usage: firedoze [--api URL] [--json] <command>

Commands:
  health
  config
  vm list
  vm inspect <name>
  vm create <name> [name...] [--vcpus N] [--memory-mib N] [--disk-bytes N] [--http-port N] [--idle-sleep-after N]
  vm start <name>
  vm sleep <name>
  vm stop <name>
  vm delete <name> [name...]
  vm settings <name> [--http-port N] [--idle-sleep-after N]
  snapshot list
  snapshot inspect <snapshot>
  snapshot save <snapshot> <vm>
  snapshot restore <snapshot> <vm>
  snapshot delete <snapshot>
  route list
  route create <route> <vm> <port>
  route delete <route>
  wg keygen
  ssh <vm> [ssh args...]
  up <vm> [--vcpus N] [--memory-mib N] [--disk-bytes N] [--http-port N] [--idle-sleep-after N] [-- ssh args...]
  with-vm-ip <vm> <command> [args...]

Environment:
  FIREDOZE_API  API URL (default http://10.77.0.1:8081; port 8081 is added if omitted)
`)
}
