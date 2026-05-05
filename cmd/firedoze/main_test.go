package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"firedoze/internal/clientwg"
	"firedoze/internal/model"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{name: "seconds", duration: 12 * time.Second, want: "12s"},
		{name: "minutes", duration: 3*time.Minute + 4*time.Second, want: "3m4s"},
		{name: "hours", duration: 5*time.Hour + 6*time.Minute + 7*time.Second, want: "5h6m"},
		{name: "days", duration: 2*24*time.Hour + 3*time.Hour + 4*time.Minute, want: "2d3h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatDuration(tt.duration); got != tt.want {
				t.Fatalf("formatDuration(%s) = %q, want %q", tt.duration, got, tt.want)
			}
		})
	}
}

func TestParseNamesAndFlags(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	memoryMinMiB := flags.Int("memory-min-mib", 0, "")
	diskBytes := flags.Int64("disk-bytes", 0, "")

	names, err := parseNamesAndFlags(flags, []string{"alice", "bob", "-memory-min-mib", "512", "-disk-bytes=1024"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(names), 2; got != want {
		t.Fatalf("len(names) = %d, want %d", got, want)
	}
	if names[0] != "alice" || names[1] != "bob" {
		t.Fatalf("names = %#v", names)
	}
	if *memoryMinMiB != 512 {
		t.Fatalf("memoryMinMiB = %d, want 512", *memoryMinMiB)
	}
	if *diskBytes != 1024 {
		t.Fatalf("diskBytes = %d, want 1024", *diskBytes)
	}
}

func TestSplitUpArgs(t *testing.T) {
	createArgs, sshArgs := splitUpArgs([]string{"demo", "-memory-max-mib", "1024", "--", "-L", "8080:localhost:8080"})
	if got, want := strings.Join(createArgs, "\x00"), strings.Join([]string{"demo", "-memory-max-mib", "1024"}, "\x00"); got != want {
		t.Fatalf("createArgs = %#v", createArgs)
	}
	if got, want := strings.Join(sshArgs, "\x00"), strings.Join([]string{"-L", "8080:localhost:8080"}, "\x00"); got != want {
		t.Fatalf("sshArgs = %#v", sshArgs)
	}
}

func TestNewClientAddsDefaultPort(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare http host", raw: "http://10.77.0.1", want: "http://10.77.0.1:8081"},
		{name: "explicit port", raw: "http://10.77.0.1:18081", want: "http://10.77.0.1:18081"},
		{name: "ipv6 host", raw: "http://[fd00::1]", want: "http://[fd00::1]:8081"},
		{name: "path", raw: "http://firedoze.example.com/api", want: "http://firedoze.example.com:8081/api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := newClient(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if c.baseURL != tt.want {
				t.Fatalf("baseURL = %q, want %q", c.baseURL, tt.want)
			}
		})
	}
}

func TestCommandNeedsAPI(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"help"}, want: false},
		{args: []string{"wg", "keygen"}, want: false},
		{args: []string{"server", "list"}, want: false},
		{args: []string{"health"}, want: true},
		{args: []string{"vm", "list"}, want: true},
		{args: []string{"ssh-proxy", "demo"}, want: true},
		{args: []string{"definitely-not-a-command"}, want: false},
	}
	for _, tt := range tests {
		if got := commandNeedsAPI(tt.args); got != tt.want {
			t.Fatalf("commandNeedsAPI(%#v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestDispatchRejectsTopLevelLifecycleCommands(t *testing.T) {
	for _, command := range []string{"start", "reboot", "publish", "hide", "up"} {
		err := (app{}).dispatch([]string{command, "demo"})
		if err == nil {
			t.Fatalf("dispatch accepted top-level %q", command)
		}
		if !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("dispatch(%q) error = %q, want unknown command", command, err)
		}
	}
}

func TestRunRequiresAPIForDaemonCommands(t *testing.T) {
	t.Setenv("FIREDOZE_API", "")
	t.Setenv("FIREDOZE_SERVER", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := run([]string{"health"}); got != 2 {
		t.Fatalf("run without API = %d, want 2", got)
	}
}

func TestRunAllowsLocalCommandsWithoutAPI(t *testing.T) {
	t.Setenv("FIREDOZE_API", "")
	t.Setenv("FIREDOZE_SERVER", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := run([]string{"wg", "keygen"}); got != 0 {
		t.Fatalf("wg keygen without API = %d, want 0", got)
	}
}

func TestClientServerConfigResolvesDefaultAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := saveClientConfig(clientConfig{
		DefaultServer: "nuc",
		Servers: []clientServerConfig{{
			Name:   "nuc",
			APIURL: "http://[fd7a:115c:a1e1::1]:8081",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveClientAPIURL("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://[fd7a:115c:a1e1::1]:8081" {
		t.Fatalf("resolved API URL = %q", got)
	}
	if !strings.HasSuffix(path, filepath.Join("firedoze", "config.toml")) {
		t.Fatalf("config path = %q", path)
	}
}

func TestClientServerAddNormalizesAndDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := (app{}).server([]string{"add", "nuc", "http://[fd7a:115c:a1e1::1]", "-default"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultServer != "nuc" {
		t.Fatalf("default server = %q, want nuc", cfg.DefaultServer)
	}
	server, ok := cfg.findServer("nuc")
	if !ok {
		t.Fatal("server nuc not found")
	}
	if server.APIURL != "http://[fd7a:115c:a1e1::1]:8081" {
		t.Fatalf("server API URL = %q", server.APIURL)
	}
}

func TestClientServerRequestAndImportKeepPrivateKeyLocal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := (app{}).server([]string{"request", "nuc"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PendingPeers) != 1 {
		t.Fatalf("pending peers = %#v, want one", cfg.PendingPeers)
	}
	pending := cfg.PendingPeers[0]
	if pending.Name != "nuc" || pending.PrivateKey == "" || pending.PublicKey == "" {
		t.Fatalf("pending peer = %#v", pending)
	}
	if _, err := wgtypes.ParseKey(pending.PrivateKey); err != nil {
		t.Fatalf("pending private key is invalid: %v", err)
	}
	if _, err := wgtypes.ParseKey(pending.PublicKey); err != nil {
		t.Fatalf("pending public key is invalid: %v", err)
	}
	serverKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	importPath := filepath.Join(t.TempDir(), "nuc.firedoze.toml")
	importData := fmt.Sprintf(`name = "nuc"
api_url = "http://[fd7a:115c:a1e1::1]"
client_public_key = %q

[wireguard]
address = "fd7a:115c:a1e1::2/128"
server_public_key = %q
endpoint = "203.0.113.10:51820"
allowed_ips = ["fd7a:115c:a1e1::1/128", "fd7a:115c:a1e0::/64"]
`, pending.PublicKey, serverKey.PublicKey().String())
	if err := os.WriteFile(importPath, []byte(importData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (app{}).server([]string{"import", importPath, "-default"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = loadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PendingPeers) != 0 {
		t.Fatalf("pending peers after import = %#v, want none", cfg.PendingPeers)
	}
	server, ok := cfg.findServer("nuc")
	if !ok {
		t.Fatal("server nuc not found")
	}
	if cfg.DefaultServer != "nuc" {
		t.Fatalf("default server = %q, want nuc", cfg.DefaultServer)
	}
	if server.WireGuard == nil {
		t.Fatal("server missing wireguard config")
	}
	if server.WireGuard.PrivateKey != pending.PrivateKey {
		t.Fatal("import did not attach the locally generated private key")
	}
	if strings.Contains(importData, pending.PrivateKey) {
		t.Fatal("test import data unexpectedly contained the client private key")
	}
}

func TestSSHCommandUsesPrivateIPAndPasswordlessGuestAuth(t *testing.T) {
	got := sshCommand(vmInfo{
		VM: model.VM{
			PrivateIP: "fd7a:115c:a1e0::3",
		},
		SSH: "ssh ubuntu@demo.example.com",
	})
	want := []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "PubkeyAuthentication=no",
		"-o", "PreferredAuthentications=none,password",
		"-o", "NumberOfPasswordPrompts=1",
		"ubuntu@fd7a:115c:a1e0::3",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("sshCommand = %#v, want %#v", got, want)
	}
}

func TestSSHCommandUsesProxyCommandWithEmbeddedWireGuard(t *testing.T) {
	oldPath := firedozeCommandPath
	firedozeCommandPath = func() string { return "/usr/local/bin/firedoze" }
	t.Cleanup(func() {
		firedozeCommandPath = oldPath
	})
	got := (app{
		client:     &client{wg: &clientwg.Client{}},
		serverName: "nuc",
	}).sshCommand(vmInfo{
		VM: model.VM{
			Name:      "demo",
			PrivateIP: "fd7a:115c:a1e0::3",
		},
		SSH: "ssh ubuntu@demo.example.com",
	})
	joined := strings.Join(got, "\x00")
	if !strings.Contains(joined, "ProxyCommand=/usr/local/bin/firedoze -server nuc ssh-proxy demo") {
		t.Fatalf("ssh command missing proxy command: %#v", got)
	}
	if got[len(got)-1] != "ubuntu@demo" {
		t.Fatalf("ssh target = %q, want ubuntu@demo", got[len(got)-1])
	}
	if strings.Contains(joined, "ubuntu@fd7a:115c:a1e0::3") {
		t.Fatalf("ssh command should not require routed private IP when proxied: %#v", got)
	}
}

func TestRemoteExecCommandAddsSeparatorBeforeRemoteCommand(t *testing.T) {
	got := remoteExecCommand(vmInfo{
		VM: model.VM{
			PrivateIP: "fd7a:115c:a1e0::3",
		},
		SSH: "ssh ubuntu@demo.example.com",
	}, []string{"echo", "hello"})
	wantSuffix := []string{"ubuntu@fd7a:115c:a1e0::3", "--", "echo", "hello"}
	if got := strings.Join(got[len(got)-len(wantSuffix):], "\x00"); got != strings.Join(wantSuffix, "\x00") {
		t.Fatalf("remoteExecCommand suffix = %#v, want %#v", got, strings.Join(wantSuffix, "\x00"))
	}
}

func TestRsyncCopyCommandUsesPrivateIPAndPasswordlessGuestAuth(t *testing.T) {
	src, dst, err := parseCopyEndpoints("./app/", "demo:/home/ubuntu/app/")
	if err != nil {
		t.Fatal(err)
	}
	got, err := rsyncCopyCommand(vmInfo{
		VM: model.VM{
			PrivateIP: "fd7a:115c:a1e0::3",
		},
		SSH: "ssh ubuntu@demo.example.com",
	}, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "rsync" || got[1] != "-a" || got[2] != "-e" {
		t.Fatalf("rsync prefix = %#v", got[:3])
	}
	if !strings.Contains(got[3], "StrictHostKeyChecking=no") {
		t.Fatalf("rsync ssh transport missing firedoze SSH options: %#v", got[3])
	}
	wantSuffix := []string{"./app/", "ubuntu@[fd7a:115c:a1e0::3]:/home/ubuntu/app/"}
	if got := strings.Join(got[len(got)-len(wantSuffix):], "\x00"); got != strings.Join(wantSuffix, "\x00") {
		t.Fatalf("rsync suffix = %#v, want %#v", got, strings.Join(wantSuffix, "\x00"))
	}
}

func TestParseCopyEndpointsRequiresExactlyOneRemote(t *testing.T) {
	if _, _, err := parseCopyEndpoints("./a", "./b"); err == nil {
		t.Fatal("parseCopyEndpoints accepted two local endpoints")
	}
	if _, _, err := parseCopyEndpoints("a:/x", "b:/y"); err == nil {
		t.Fatal("parseCopyEndpoints accepted two remote endpoints")
	}
}

func TestWithVMIPSetsFiredozeVMIP(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vms" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[{"name":"demo","private_ip":"fd7a:115c:a1e0::3"}]}`)
	})

	c := testClient(t, handler)
	outputPath := filepath.Join(t.TempDir(), "env")
	if err := (app{client: c}).withVMIP([]string{"demo", "sh", "-c", "printf %s \"$FIREDOZE_VM_IP\" > \"$1\"", "sh", outputPath}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fd7a:115c:a1e0::3" {
		t.Fatalf("FIREDOZE_VM_IP = %q, want fd7a:115c:a1e0::3", string(data))
	}
}

func TestVMListUsesPublicURLColumn(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vms" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[
			{"name":"public","state":"running","private_ip":"fd7a:115c:a1e0::3","public_http":true,"urls":{"default":"https://public.example.test"}},
			{"name":"hidden","state":"stopped","private_ip":"fd7a:115c:a1e0::5","public_http":false,"urls":{"default":"https://hidden.example.test"}}
		]}`)
	})

	c := testClient(t, handler)
	var out bytes.Buffer
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = (app{client: c}).vm([]string{"list"})
	_ = w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "NAME    STATE    RUNTIME  PRIVATE IP         PUBLIC URL\n") {
		t.Fatalf("list output missing header:\n%s", got)
	}
	if strings.Contains(got, "VISIBILITY") {
		t.Fatalf("list output still has VISIBILITY column:\n%s", got)
	}
	if !strings.Contains(got, "public  running  -        fd7a:115c:a1e0::3  https://public.example.test\n") {
		t.Fatalf("list output missing public URL row:\n%s", got)
	}
	if !strings.Contains(got, "hidden  stopped  -        fd7a:115c:a1e0::5  -\n") {
		t.Fatalf("list output missing hidden row with dash URL:\n%s", got)
	}
}

func TestVMListNamesOnlyPrintsOneNamePerLine(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vms" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := strings.Join(r.URL.Query()["name"], ","); got != "demo*,test*" {
			t.Fatalf("name query = %q, want demo*,test*", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[
			{"name":"demo-one","state":"running","private_ip":"fd7a:115c:a1e0::3"},
			{"name":"test-two","state":"stopped","private_ip":"fd7a:115c:a1e0::5"}
		]}`)
	})

	c := testClient(t, handler)
	var out bytes.Buffer
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = (app{client: c}).vm([]string{"list", "-names", "demo*", "test*"})
	_ = w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "demo-one\ntest-two\n"; got != want {
		t.Fatalf("names-only output = %q, want %q", got, want)
	}
}

func TestVMListNamesOnlyRejectsJSON(t *testing.T) {
	err := (app{json: true}).vm([]string{"list", "-names"})
	if err == nil {
		t.Fatal("vm list -names accepted -json mode")
	}
	if !strings.Contains(err.Error(), "cannot be combined with -json") {
		t.Fatalf("error = %q", err)
	}
}

func TestVMUsagePrintsResourceTable(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/usage" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := strings.Join(r.URL.Query()["name"], ","); got != "demo*" {
			t.Fatalf("name query = %q, want demo*", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[{
			"name":"demo",
			"state":"running",
			"vcpus":2,
			"memory_min_mib":128,
			"memory_max_mib":512,
			"disk_bytes":4294967296,
			"disk_allocated_bytes":1073741824,
			"guest_memory":{"total_mib":480,"available_mib":320,"swap_total_mib":256,"swap_free_mib":200,"root_disk_total_bytes":4294967296,"root_disk_free_bytes":3221225472,"load1":0.25},
			"process":{"pid":123,"rss_bytes":67108864,"cpu_seconds":65}
		}]}`)
	})

	c := testClient(t, handler)
	var out bytes.Buffer
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = (app{client: c}).vm([]string{"usage", "demo*"})
	_ = w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"NAME",
		"demo",
		"GUEST MEM",
		"320MiB/480MiB",
		"200MiB/256MiB",
		"3.0GiB/4.0GiB",
		"0.25",
		"64MiB",
		"1m5s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "BALLOON") {
		t.Fatalf("usage output includes removed balloon column:\n%s", got)
	}
	if strings.Contains(got, "DISK USED/SIZE") {
		t.Fatalf("usage output includes host disk allocation column:\n%s", got)
	}
}

func TestVMSleepAcceptsMultipleNames(t *testing.T) {
	var requests []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"slept"}`)
	})

	c := testClient(t, handler)
	if err := (app{client: c, json: true}).vm([]string{"sleep", "alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /vms/alpha/sleep",
		"POST /vms/beta/sleep",
	}
	if strings.Join(requests, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func TestVMRebootAcceptsMultipleNames(t *testing.T) {
	var requests []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/vms/"), "/reboot")
		_, _ = io.WriteString(w, `{"vm":{"name":"`+name+`","state":"running"}}`)
	})

	c := testClient(t, handler)
	if err := (app{client: c, json: true}).vm([]string{"reboot", "alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /vms/alpha/reboot",
		"POST /vms/beta/reboot",
	}
	if strings.Join(requests, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func TestVMPublishAndHidePatchSettings(t *testing.T) {
	var requests []string
	var bodies []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(data))
		if r.Method != http.MethodPatch || r.URL.Path != "/vms/demo/settings" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vm":{"name":"demo","state":"running"}}`)
	})

	c := testClient(t, handler)
	app := app{client: c, json: true}
	if err := app.vm([]string{"publish", "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := app.vm([]string{"hide", "demo"}); err != nil {
		t.Fatal(err)
	}
	wantRequests := []string{
		"PATCH /vms/demo/settings",
		"PATCH /vms/demo/settings",
	}
	if strings.Join(requests, "\x00") != strings.Join(wantRequests, "\x00") {
		t.Fatalf("requests = %#v, want %#v", requests, wantRequests)
	}
	wantBodies := []string{
		`{"public_http":true}`,
		`{"public_http":false}`,
	}
	if strings.Join(bodies, "\x00") != strings.Join(wantBodies, "\x00") {
		t.Fatalf("bodies = %#v, want %#v", bodies, wantBodies)
	}
}

func TestVMCreateAutoWakeFlagDoesNotConsumeNames(t *testing.T) {
	params, names, err := parseVMCreateArgs("test", []string{"alpha", "beta", "-auto-wake"})
	if err != nil {
		t.Fatal(err)
	}
	if !params.AutoWake {
		t.Fatal("AutoWake = false, want true")
	}
	if strings.Join(names, ",") != "alpha,beta" {
		t.Fatalf("names = %#v, want alpha,beta", names)
	}
}

func TestVMCreateNoAutoWakeFlagDoesNotConsumeNames(t *testing.T) {
	params, names, err := parseVMCreateArgs("test", []string{"alpha", "beta", "-no-auto-wake"})
	if err != nil {
		t.Fatal(err)
	}
	if !params.NoAutoWake {
		t.Fatal("NoAutoWake = false, want true")
	}
	if strings.Join(names, ",") != "alpha,beta" {
		t.Fatalf("names = %#v, want alpha,beta", names)
	}
}

func TestVMCreatePublishFlag(t *testing.T) {
	params, names, err := parseVMCreateArgs("test", []string{"alpha", "-publish"})
	if err != nil {
		t.Fatal(err)
	}
	if !params.PublicHTTP {
		t.Fatal("PublicHTTP = false, want true")
	}
	if strings.Join(names, ",") != "alpha" {
		t.Fatalf("names = %#v, want alpha", names)
	}
}

func TestVMCreateWarmsBaseImageMetadataOnce(t *testing.T) {
	requests := []string{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/base-image/warmup":
			_, _ = io.WriteString(w, `{"base_image":{"rootfs":{"path":"rootfs.ext4","basename":"rootfs.ext4"}}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms":
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode create VM request: %v", err)
			}
			_, _ = io.WriteString(w, `{"vm":{"name":"`+req.Name+`","state":"stopped"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	c := testClient(t, handler)
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = (app{client: c, json: true}).createVMs(vmCreateParams{}, []string{"alpha", "beta"})
	_ = w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(r)

	want := []string{
		"POST /base-image/warmup",
		"POST /vms",
		"POST /vms",
	}
	if strings.Join(requests, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func TestSnapshotRestoreParsesCreateOptions(t *testing.T) {
	params, snapshotName, vmName, err := parseSnapshotRestoreArgs([]string{
		"base",
		"demo",
		"-vcpus", "2",
		"-memory-min-mib", "256",
		"-memory-max-mib", "1024",
		"-disk-bytes", "8589934592",
		"-http-port", "3000",
		"-idle-sleep-after", "900",
		"-no-auto-wake",
		"-publish",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshotName != "base" || vmName != "demo" {
		t.Fatalf("snapshot/vm = %q/%q, want base/demo", snapshotName, vmName)
	}
	body := restoreSnapshotBody(params, vmName)
	if body["vm"] != "demo" ||
		body["vcpus"] != 2 ||
		body["memory_min_mib"] != 256 ||
		body["memory_max_mib"] != 1024 ||
		body["disk_bytes"] != int64(8589934592) ||
		body["default_http_port"] != 3000 ||
		body["idle_sleep_after_seconds"] != 900 ||
		body["auto_wake"] != false ||
		body["public_http"] != true {
		t.Fatalf("restore body = %#v", body)
	}
}

func TestFoundFlag(t *testing.T) {
	if !foundFlag([]string{"demo", "-publish=false"}, "publish") {
		t.Fatal("foundFlag did not find -publish=false")
	}
	if !foundFlag([]string{"demo", "--publish=false"}, "publish") {
		t.Fatal("foundFlag did not find accepted --publish=false alias")
	}
	if foundFlag([]string{"demo", "--not-publish"}, "publish") {
		t.Fatal("foundFlag matched unrelated flag")
	}
}

func TestVMListPassesNameGlobs(t *testing.T) {
	var gotPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[]}`)
	})

	c := testClient(t, handler)
	if err := (app{client: c, json: true}).vm([]string{"list", "dev-*", "test?"}); err != nil {
		t.Fatal(err)
	}
	if got, want := gotPath, "/vms?name=dev-%2A&name=test%3F"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestWGKeygen(t *testing.T) {
	oldStdout := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	err = (app{}).wg([]string{"keygen"})
	_ = write.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	if !strings.Contains(output, "private_key = ") || !strings.Contains(output, "public_key = ") {
		t.Fatalf("keygen output missing keys:\n%s", output)
	}
}
