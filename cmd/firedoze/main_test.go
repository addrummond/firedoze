package main

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"firedoze/internal/store"
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
	memoryMiB := flags.Int("memory-mib", 0, "")
	diskBytes := flags.Int64("disk-bytes", 0, "")

	names, err := parseNamesAndFlags(flags, []string{"alice", "bob", "-memory-mib", "512", "-disk-bytes=1024"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(names), 2; got != want {
		t.Fatalf("len(names) = %d, want %d", got, want)
	}
	if names[0] != "alice" || names[1] != "bob" {
		t.Fatalf("names = %#v", names)
	}
	if *memoryMiB != 512 {
		t.Fatalf("memoryMiB = %d, want 512", *memoryMiB)
	}
	if *diskBytes != 1024 {
		t.Fatalf("diskBytes = %d, want 1024", *diskBytes)
	}
}

func TestSplitUpArgs(t *testing.T) {
	createArgs, sshArgs := splitUpArgs([]string{"demo", "-memory-mib", "1024", "--", "-L", "8080:localhost:8080"})
	if got, want := strings.Join(createArgs, "\x00"), strings.Join([]string{"demo", "-memory-mib", "1024"}, "\x00"); got != want {
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
		{args: []string{"health"}, want: true},
		{args: []string{"vm", "list"}, want: true},
	}
	for _, tt := range tests {
		if got := commandNeedsAPI(tt.args); got != tt.want {
			t.Fatalf("commandNeedsAPI(%#v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestRunRequiresAPIForDaemonCommands(t *testing.T) {
	t.Setenv("FIREDOZE_API", "")
	if got := run([]string{"health"}); got != 2 {
		t.Fatalf("run without API = %d, want 2", got)
	}
}

func TestRunAllowsLocalCommandsWithoutAPI(t *testing.T) {
	t.Setenv("FIREDOZE_API", "")
	if got := run([]string{"wg", "keygen"}); got != 0 {
		t.Fatalf("wg keygen without API = %d, want 0", got)
	}
}

func TestSSHCommandUsesPrivateIPAndPasswordlessGuestAuth(t *testing.T) {
	got := sshCommand(vmInfo{
		VM: store.VM{
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

func TestRemoteExecCommandAddsSeparatorBeforeRemoteCommand(t *testing.T) {
	got := remoteExecCommand(vmInfo{
		VM: store.VM{
			PrivateIP: "fd7a:115c:a1e0::3",
		},
		SSH: "ssh ubuntu@demo.example.com",
	}, []string{"echo", "hello"})
	wantSuffix := []string{"ubuntu@fd7a:115c:a1e0::3", "--", "echo", "hello"}
	if got := strings.Join(got[len(got)-len(wantSuffix):], "\x00"); got != strings.Join(wantSuffix, "\x00") {
		t.Fatalf("remoteExecCommand suffix = %#v, want %#v", got, strings.Join(wantSuffix, "\x00"))
	}
}

func TestWithVMIPSetsFiredozeVMIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vms" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[{"name":"demo","private_ip":"fd7a:115c:a1e0::3"}]}`)
	}))
	defer server.Close()

	c, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "env")
	err = (app{client: c}).withVMIP([]string{"demo", "sh", "-c", "printf %s \"$FIREDOZE_VM_IP\" > \"$1\"", "sh", outputPath})
	if err != nil {
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vms" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[
			{"name":"public","state":"running","private_ip":"fd7a:115c:a1e0::3","public_http":true,"urls":{"default":"https://public.example.test"}},
			{"name":"hidden","state":"stopped","private_ip":"fd7a:115c:a1e0::5","public_http":false,"urls":{"default":"https://hidden.example.test"}}
		]}`)
	}))
	defer server.Close()

	c, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
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

func TestVMSleepAcceptsMultipleNames(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"slept"}`)
	}))
	defer server.Close()

	c, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
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

func TestVMCreatePublicFlag(t *testing.T) {
	params, names, err := parseVMCreateArgs("test", []string{"alpha", "-public"})
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

func TestFoundFlag(t *testing.T) {
	if !foundFlag([]string{"demo", "-public=false"}, "public") {
		t.Fatal("foundFlag did not find -public=false")
	}
	if !foundFlag([]string{"demo", "--public=false"}, "public") {
		t.Fatal("foundFlag did not find accepted --public=false alias")
	}
	if foundFlag([]string{"demo", "--not-public"}, "public") {
		t.Fatal("foundFlag matched unrelated flag")
	}
}

func TestVMListPassesNameGlobs(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vms":[]}`)
	}))
	defer server.Close()

	c, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
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
