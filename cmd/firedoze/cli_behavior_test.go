package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"firedoze/internal/model"
)

type cliRequest struct {
	method string
	path   string
	body   string
}

func TestVMCreatePostsOptionsForEachName(t *testing.T) {
	var requests []cliRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/base-image/warmup":
			_, _ = io.WriteString(w, `{"base_image":{}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms":
			var req struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(requests[len(requests)-1].body), &req); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(w, `{"vm":{"name":"`+req.Name+`","state":"stopped"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	err := (app{client: testClient(t, server), json: true}).vm([]string{
		"create", "alpha", "beta",
		"-vcpus", "2",
		"-memory-mib", "1024",
		"-disk-bytes", "123456",
		"-http-port", "3000",
		"-idle-sleep-after", "600",
		"-no-auto-wake",
		"-publish",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := requestKeys(requests)
	want := []string{"POST /base-image/warmup", "POST /vms", "POST /vms"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
	for i, wantName := range []string{"alpha", "beta"} {
		var body map[string]any
		if err := json.Unmarshal([]byte(requests[i+1].body), &body); err != nil {
			t.Fatal(err)
		}
		if body["name"] != wantName ||
			body["vcpus"] != float64(2) ||
			body["memory_mib"] != float64(1024) ||
			body["disk_bytes"] != float64(123456) ||
			body["default_http_port"] != float64(3000) ||
			body["idle_sleep_after_seconds"] != float64(600) ||
			body["auto_wake"] != false ||
			body["public_http"] != true {
			t.Fatalf("create body %d = %#v", i, body)
		}
	}
}

func TestVMLifecycleCommandsUseExpectedEndpoints(t *testing.T) {
	var requests []cliRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			_, _ = io.WriteString(w, `{"vm":{"name":"demo","state":"running"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			_, _ = io.WriteString(w, `{"status":"stopped"}`)
		case r.Method == http.MethodDelete:
			_, _ = io.WriteString(w, `{"status":"deleted"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	a := app{client: testClient(t, server), json: true}
	for _, args := range [][]string{
		{"start", "demo"},
		{"stop", "demo"},
		{"delete", "alpha", "beta"},
	} {
		if err := a.vm(args); err != nil {
			t.Fatalf("vm %#v: %v", args, err)
		}
	}

	got := requestKeys(requests)
	want := []string{
		"POST /vms/demo/start",
		"POST /vms/demo/stop",
		"DELETE /vms/alpha",
		"DELETE /vms/beta",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
}

func TestVMSettingsPatchBody(t *testing.T) {
	var request cliRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = readCLIRequest(t, r)
		if request.method != http.MethodPatch || request.path != "/vms/demo/settings" {
			t.Fatalf("unexpected request: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vm":{"name":"demo","state":"running"}}`)
	}))
	defer server.Close()

	err := (app{client: testClient(t, server), json: true}).vm([]string{
		"settings", "demo",
		"-http-port", "3000",
		"-idle-sleep-after", "60",
		"-auto-wake", "false",
		"-publish", "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(request.body), &body); err != nil {
		t.Fatal(err)
	}
	if body["default_http_port"] != float64(3000) ||
		body["idle_sleep_after_seconds"] != float64(60) ||
		body["auto_wake"] != false ||
		body["public_http"] != true {
		t.Fatalf("settings body = %#v", body)
	}
}

func TestSnapshotCommandsUseExpectedEndpointsAndBodies(t *testing.T) {
	var requests []cliRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots":
			_, _ = io.WriteString(w, `{"snapshots":[{"name":"snap","source_vm":"demo"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots/snap":
			_, _ = io.WriteString(w, `{"snapshot":{"name":"snap","source_vm":"demo"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/snapshots":
			_, _ = io.WriteString(w, `{"snapshot":{"name":"snap","source_vm":"demo"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/snapshots/snap/restore":
			_, _ = io.WriteString(w, `{"vm":{"name":"copy","state":"stopped"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/snapshots/snap":
			_, _ = io.WriteString(w, `{"status":"deleted"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	a := app{client: testClient(t, server), json: true}
	commands := [][]string{
		{"list"},
		{"inspect", "snap"},
		{"save", "snap", "demo"},
		{"restore", "snap", "copy", "-vcpus", "2", "-memory-mib", "512", "-http-port", "3000", "-publish"},
		{"delete", "snap"},
	}
	for _, args := range commands {
		if err := a.snapshot(args); err != nil {
			t.Fatalf("snapshot %#v: %v", args, err)
		}
	}

	got := requestKeys(requests)
	want := []string{
		"GET /snapshots",
		"GET /snapshots/snap",
		"POST /snapshots",
		"POST /snapshots/snap/restore",
		"DELETE /snapshots/snap",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
	var saveBody map[string]any
	if err := json.Unmarshal([]byte(requests[2].body), &saveBody); err != nil {
		t.Fatal(err)
	}
	if saveBody["name"] != "snap" || saveBody["vm"] != "demo" {
		t.Fatalf("save body = %#v", saveBody)
	}
	var restoreBody map[string]any
	if err := json.Unmarshal([]byte(requests[3].body), &restoreBody); err != nil {
		t.Fatal(err)
	}
	if restoreBody["vm"] != "copy" ||
		restoreBody["vcpus"] != float64(2) ||
		restoreBody["memory_mib"] != float64(512) ||
		restoreBody["default_http_port"] != float64(3000) ||
		restoreBody["public_http"] != true {
		t.Fatalf("restore body = %#v", restoreBody)
	}
}

func TestRouteCommandsUseExpectedEndpointsAndBodies(t *testing.T) {
	var requests []cliRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/routes":
			_, _ = io.WriteString(w, `{"routes":[{"name":"web","vm_name":"demo","port":8080}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/routes":
			_, _ = io.WriteString(w, `{"route":{"name":"web","vm_name":"demo","port":8080}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/routes/web":
			_, _ = io.WriteString(w, `{"status":"deleted"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	a := app{client: testClient(t, server), json: true}
	for _, args := range [][]string{
		{"list"},
		{"create", "web", "demo", "8080"},
		{"delete", "web"},
	} {
		if err := a.route(args); err != nil {
			t.Fatalf("route %#v: %v", args, err)
		}
	}
	got := requestKeys(requests)
	want := []string{"GET /routes", "POST /routes", "DELETE /routes/web"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(requests[1].body), &body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "web" || body["vm"] != "demo" || body["port"] != float64(8080) {
		t.Fatalf("route create body = %#v", body)
	}
}

func TestSSHStartsSleepingVMWaitsAndRunsSSH(t *testing.T) {
	var requests []cliRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vms":
			_, _ = io.WriteString(w, `{"vms":[{"name":"demo","state":"sleeping","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/demo/start":
			_, _ = io.WriteString(w, `{"vm":{"name":"demo","state":"running","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var waitedIP string
	var cmdArgs []string
	a := app{
		client: testClient(t, server),
		runCommand: func(cmd *exec.Cmd) error {
			cmdArgs = append([]string(nil), cmd.Args...)
			return nil
		},
		waitForSSHFn: func(ip string, _ time.Duration) error {
			waitedIP = ip
			return nil
		},
	}
	if err := a.ssh([]string{"demo", "-L", "8080:localhost:8080"}); err != nil {
		t.Fatal(err)
	}
	if waitedIP != "fd00::2" {
		t.Fatalf("waited IP = %q, want fd00::2", waitedIP)
	}
	if !reflect.DeepEqual(requestKeys(requests), []string{"GET /vms", "POST /vms/demo/start"}) {
		t.Fatalf("requests = %#v", requestKeys(requests))
	}
	wantSuffix := []string{"ubuntu@fd00::2", "-L", "8080:localhost:8080"}
	if got := cmdArgs[len(cmdArgs)-len(wantSuffix):]; !reflect.DeepEqual(got, wantSuffix) {
		t.Fatalf("ssh command suffix = %#v, want %#v", got, wantSuffix)
	}
}

func TestExecCpAndWithVMIPUseCommandRunner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vms" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[{"name":"demo","state":"running","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}]}`)
	}))
	defer server.Close()

	var commands [][]string
	var withVMIPEnv []string
	a := app{
		client: testClient(t, server),
		runCommand: func(cmd *exec.Cmd) error {
			commands = append(commands, append([]string(nil), cmd.Args...))
			if cmd.Args[0] == "printenv" {
				withVMIPEnv = append([]string(nil), cmd.Env...)
			}
			return nil
		},
	}
	if err := a.exec([]string{"demo", "--", "echo", "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := a.cp([]string{"./app/", "demo:/home/ubuntu/app/"}); err != nil {
		t.Fatal(err)
	}
	if err := a.withVMIP([]string{"demo", "printenv", "FIREDOZE_VM_IP"}); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 3 {
		t.Fatalf("commands = %#v", commands)
	}
	if got := commands[0][len(commands[0])-3:]; !reflect.DeepEqual(got, []string{"--", "echo", "hi"}) {
		t.Fatalf("exec command suffix = %#v", got)
	}
	if commands[1][0] != "rsync" || !strings.Contains(strings.Join(commands[1], " "), "ubuntu@[fd00::2]:/home/ubuntu/app/") {
		t.Fatalf("cp command = %#v", commands[1])
	}
	if !containsString(withVMIPEnv, "FIREDOZE_VM_IP=fd00::2") {
		t.Fatalf("with-vm-ip env missing FIREDOZE_VM_IP: %#v", withVMIPEnv)
	}
}

func TestVMUpCreatesStartsPublishesByDefaultAndRunsSSH(t *testing.T) {
	var requests []cliRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readCLIRequest(t, r)
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vms" && len(requests) == 1:
			_, _ = io.WriteString(w, `{"vms":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/base-image/warmup":
			_, _ = io.WriteString(w, `{"base_image":{}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms":
			_, _ = io.WriteString(w, `{"vm":{"name":"demo","state":"stopped","private_ip":"fd00::2","public_http":true,"ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/demo/start":
			_, _ = io.WriteString(w, `{"vm":{"name":"demo","state":"running","private_ip":"fd00::2","public_http":true,"ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/vms":
			_, _ = io.WriteString(w, `{"vms":[{"name":"demo","state":"running","private_ip":"fd00::2","public_http":true,"ssh":"ssh ubuntu@demo.example.test"}]}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var waitedIP string
	var command []string
	a := app{
		client: testClient(t, server),
		runCommand: func(cmd *exec.Cmd) error {
			command = append([]string(nil), cmd.Args...)
			return nil
		},
		waitForSSHFn: func(ip string, _ time.Duration) error {
			waitedIP = ip
			return nil
		},
	}
	if err := a.up([]string{"demo", "-memory-mib", "256", "--", "-A"}); err != nil {
		t.Fatal(err)
	}
	got := requestKeys(requests)
	want := []string{"GET /vms", "POST /base-image/warmup", "POST /vms", "POST /vms/demo/start", "GET /vms"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
	var createBody map[string]any
	if err := json.Unmarshal([]byte(requests[2].body), &createBody); err != nil {
		t.Fatal(err)
	}
	if createBody["name"] != "demo" || createBody["memory_mib"] != float64(256) || createBody["public_http"] != true {
		t.Fatalf("up create body = %#v", createBody)
	}
	if waitedIP != "fd00::2" {
		t.Fatalf("waited IP = %q", waitedIP)
	}
	if got := command[len(command)-2:]; !reflect.DeepEqual(got, []string{"ubuntu@fd00::2", "-A"}) {
		t.Fatalf("up ssh command suffix = %#v", got)
	}
}

func TestClientDoErrorResponseMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"already exists"}`)
	}))
	defer server.Close()

	err := testClient(t, server).do(t.Context(), http.MethodPost, "/vms", map[string]any{"name": "demo"}, nil)
	apiErr, ok := err.(apiError)
	if !ok {
		t.Fatalf("error = %T %v, want apiError", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict || apiErr.Message != "already exists" {
		t.Fatalf("api error = %#v", apiErr)
	}
}

func TestServerCommandsManageLocalConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := saveClientConfig(clientConfig{
		DefaultServer: "nuc",
		Servers: []clientServerConfig{
			{Name: "nuc", APIURL: "http://[fd00::1]:8081"},
			{Name: "lab", APIURL: "http://[fd00::2]:8081"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	a := app{json: true}
	for _, args := range [][]string{
		{"list"},
		{"current"},
		{"path"},
		{"use", "lab"},
		{"remove", "nuc"},
	} {
		if err := a.server(args); err != nil {
			t.Fatalf("server %#v: %v", args, err)
		}
	}
	cfg, path, err := loadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultServer != "lab" {
		t.Fatalf("default server = %q, want lab", cfg.DefaultServer)
	}
	if _, ok := cfg.findServer("nuc"); ok {
		t.Fatal("server nuc still present after remove")
	}
	if _, ok := cfg.findServer("lab"); !ok {
		t.Fatal("server lab missing after remove")
	}
	if !strings.HasSuffix(path, filepath.Join("firedoze", "config.toml")) {
		t.Fatalf("config path = %q", path)
	}
}

func TestServerCommandErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := (app{}).server([]string{"add", "bad/name", "http://127.0.0.1"}); err == nil {
		t.Fatal("server add accepted bad name")
	}
	if err := (app{}).server([]string{"use", "missing"}); err == nil {
		t.Fatal("server use accepted missing server")
	}
	if err := (app{}).server([]string{"remove", "missing"}); err == nil {
		t.Fatal("server remove accepted missing server")
	}
	if err := (app{}).server([]string{"current"}); err == nil {
		t.Fatal("server current accepted empty config")
	}
}

func TestRunHealthUsesAPIEnvironment(t *testing.T) {
	t.Setenv("FIREDOZE_SERVER", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/health" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	t.Setenv("FIREDOZE_API", server.URL)

	if got := run([]string{"health"}); got != 0 {
		t.Fatalf("run health = %d, want 0", got)
	}
}

func TestRuntimeSinceStart(t *testing.T) {
	if got := runtimeSinceStart(vmInfo{VM: modelVM("stopped", "")}); got != "-" {
		t.Fatalf("stopped runtime = %q, want -", got)
	}
	started := time.Now().Add(-2*time.Hour - 3*time.Minute).Format(time.RFC3339Nano)
	got := runtimeSinceStart(vmInfo{VM: modelVM("running", started)})
	if !strings.HasPrefix(got, "2h") {
		t.Fatalf("running runtime = %q, want about 2h", got)
	}
	if got := runtimeSinceStart(vmInfo{VM: modelVM("running", "not-a-time")}); got != "-" {
		t.Fatalf("bad time runtime = %q, want -", got)
	}
}

func TestParseNameAndFlagsAllowsFlagsBeforeName(t *testing.T) {
	flags := flagSet("test")
	httpPort := flags.Int("http-port", 0, "")
	name, err := parseNameAndFlags(flags, []string{"-http-port", "3000", "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo" || *httpPort != 3000 {
		t.Fatalf("name/http-port = %q/%d", name, *httpPort)
	}
}

func testClient(t *testing.T, server *httptest.Server) *client {
	t.Helper()
	c, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readCLIRequest(t *testing.T, r *http.Request) cliRequest {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return cliRequest{
		method: r.Method,
		path:   r.URL.Path,
		body:   string(data),
	}
}

func requestKeys(requests []cliRequest) []string {
	out := make([]string, 0, len(requests))
	for _, request := range requests {
		out = append(out, request.method+" "+request.path)
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func modelVM(state string, lastStartedAt string) model.VM {
	return model.VM{
		Name:          "demo",
		State:         state,
		LastStartedAt: lastStartedAt,
	}
}
