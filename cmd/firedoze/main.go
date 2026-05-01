package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"firedoze/internal/store"
)

const defaultAPI = "http://10.77.0.1:8081"

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
	case "ssh":
		return a.ssh(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a app) vm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: firedoze vm <list|create|start|sleep|stop|delete|settings>")
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
		fmt.Fprintln(w, "NAME\tSTATE\tIP\tSSH\tURL")
		for _, vm := range out.VMs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", vm.Name, vm.State, vm.PrivateIP, vm.SSH, vm.URLs["default"])
		}
		return w.Flush()
	case "create":
		return a.vmCreate(args[1:])
	case "start", "sleep", "stop":
		if len(args) != 2 {
			return fmt.Errorf("usage: firedoze vm %s <name>", args[0])
		}
		methodPath := "/vms/" + url.PathEscape(args[1]) + "/" + args[0]
		var out map[string]any
		if err := a.client.do(context.Background(), http.MethodPost, methodPath, nil, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s %s", args[1], pastTense(args[0])))
	case "delete", "rm":
		if len(args) != 2 {
			return errors.New("usage: firedoze vm delete <name>")
		}
		var out map[string]any
		if err := a.client.do(context.Background(), http.MethodDelete, "/vms/"+url.PathEscape(args[1]), nil, &out); err != nil {
			return err
		}
		return a.printJSONOrLine(out, fmt.Sprintf("%s deleted", args[1]))
	case "settings":
		return a.vmSettings(args[1:])
	default:
		return fmt.Errorf("unknown vm command %q", args[0])
	}
}

func (a app) vmCreate(args []string) error {
	flags := flag.NewFlagSet("firedoze vm create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	vcpus := flags.Int("vcpus", 0, "vCPUs")
	memoryMiB := flags.Int("memory-mib", 0, "memory in MiB")
	diskBytes := flags.Int64("disk-bytes", 0, "disk size in bytes")
	httpPort := flags.Int("http-port", 0, "default guest HTTP port")
	idle := flags.Int("idle-sleep-after", 0, "idle sleep timeout in seconds")
	name, err := parseNameAndFlags(flags, args)
	if err != nil {
		return fmt.Errorf("%w\nusage: firedoze vm create <name> [--vcpus N] [--memory-mib N] [--disk-bytes N] [--http-port N] [--idle-sleep-after N]", err)
	}
	body := map[string]any{"name": name}
	addInt(body, "vcpus", *vcpus)
	addInt(body, "memory_mib", *memoryMiB)
	addInt64(body, "disk_bytes", *diskBytes)
	addInt(body, "default_http_port", *httpPort)
	addInt(body, "idle_sleep_after_seconds", *idle)
	var out map[string]any
	if err := a.client.do(context.Background(), http.MethodPost, "/vms", body, &out); err != nil {
		return err
	}
	return a.printJSONOrLine(out, fmt.Sprintf("%s created", name))
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
		return errors.New("usage: firedoze snapshot <list|save|restore|delete>")
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

func (a app) ssh(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: firedoze ssh <vm> [ssh args...]")
	}
	var out struct {
		VMs []vmInfo `json:"vms"`
	}
	if err := a.client.do(context.Background(), http.MethodGet, "/vms", nil, &out); err != nil {
		return err
	}
	for _, vm := range out.VMs {
		if vm.Name != args[0] {
			continue
		}
		sshArgs := sshCommand(vm)
		sshArgs = append(sshArgs, args[1:]...)
		cmd := exec.Command(sshArgs[0], sshArgs[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("VM not found: %s", args[0])
}

func sshCommand(vm vmInfo) []string {
	fields := strings.Fields(vm.SSH)
	if len(fields) >= 2 && vm.PrivateIP != "" {
		userHost := fields[1]
		if at := strings.LastIndex(userHost, "@"); at >= 0 {
			fields[1] = userHost[:at+1] + vm.PrivateIP
			return fields
		}
	}
	return append([]string{}, fields...)
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
  vm create <name> [--vcpus N] [--memory-mib N] [--disk-bytes N] [--http-port N] [--idle-sleep-after N]
  vm start <name>
  vm sleep <name>
  vm stop <name>
  vm delete <name>
  vm settings <name> [--http-port N] [--idle-sleep-after N]
  snapshot list
  snapshot save <snapshot> <vm>
  snapshot restore <snapshot> <vm>
  snapshot delete <snapshot>
  route list
  route create <route> <vm> <port>
  route delete <route>
  ssh <vm> [ssh args...]

Environment:
  FIREDOZE_API  API URL (default http://10.77.0.1:8081)
`)
}
