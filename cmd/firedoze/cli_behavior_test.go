package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})

	err := (app{client: testClient(t, handler), json: true}).vm([]string{
		"create", "alpha", "beta",
		"-vcpus", "2",
		"-memory-min-mib", "256",
		"-memory-max-mib", "1024",
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
			body["memory_min_mib"] != float64(256) ||
			body["memory_max_mib"] != float64(1024) ||
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/vms-by-name/"):
			name := strings.TrimPrefix(r.URL.Path, "/vms-by-name/")
			_, _ = io.WriteString(w, `{"vm":{"uuid":"`+name+`-uuid","name":"`+name+`","state":"stopped"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			_, _ = io.WriteString(w, `{"vm":{"name":"demo","state":"running"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			_, _ = io.WriteString(w, `{"status":"stopped"}`)
		case r.Method == http.MethodDelete:
			_, _ = io.WriteString(w, `{"status":"deleted"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	a := app{client: testClient(t, handler), json: true}
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
		"GET /vms-by-name/demo",
		"POST /vms/demo-uuid/start",
		"GET /vms-by-name/demo",
		"POST /vms/demo-uuid/stop",
		"GET /vms-by-name/alpha",
		"DELETE /vms/alpha-uuid",
		"GET /vms-by-name/beta",
		"DELETE /vms/beta-uuid",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
}

func TestVMCommandsAcceptUUIDReferences(t *testing.T) {
	const vmUUID = "550e8400-e29b-41d4-a716-446655440000"
	var requests []cliRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vms/"+vmUUID:
			_, _ = io.WriteString(w, `{"vm":{"uuid":"`+vmUUID+`","name":"demo","state":"stopped"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/"+vmUUID+"/start":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"`+vmUUID+`","name":"demo","state":"running"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	if err := (app{client: testClient(t, handler), json: true}).vm([]string{"start", vmUUID}); err != nil {
		t.Fatal(err)
	}
	got := requestKeys(requests)
	want := []string{"GET /vms/" + vmUUID, "POST /vms/" + vmUUID + "/start"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
}

func TestVMIDPrintsUUID(t *testing.T) {
	const vmUUID = "550e8400-e29b-41d4-a716-446655440000"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vms-by-name/demo" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vm":{"uuid":"`+vmUUID+`","name":"demo","state":"running"}}`)
	})

	var out bytes.Buffer
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = (app{client: testClient(t, handler)}).vm([]string{"id", "demo"})
	_ = w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), vmUUID+"\n"; got != want {
		t.Fatalf("id output = %q, want %q", got, want)
	}
}

func TestVMSettingsPatchBody(t *testing.T) {
	var request cliRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = readCLIRequest(t, r)
		if request.method == http.MethodGet && request.path == "/vms-by-name/demo" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running"}}`)
			return
		}
		if request.method != http.MethodPatch || request.path != "/vms/demo-uuid/settings" {
			t.Fatalf("unexpected request: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vm":{"name":"demo","state":"running"}}`)
	})

	err := (app{client: testClient(t, handler), json: true}).vm([]string{
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
	inputPath := filepath.Join(t.TempDir(), "snap.firedoze-snapshot.tgz")
	if err := os.WriteFile(inputPath, []byte("bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "exported.firedoze-snapshot.tgz")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots":
			_, _ = io.WriteString(w, `{"snapshots":[{"name":"snap","source_vm":"demo"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots/snap":
			_, _ = io.WriteString(w, `{"snapshot":{"name":"snap","source_vm":"demo"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/vms-by-name/demo":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"stopped"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/snapshots":
			_, _ = io.WriteString(w, `{"snapshot":{"name":"snap","source_vm":"demo"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/snapshots/snap/restore":
			_, _ = io.WriteString(w, `{"vm":{"name":"copy","state":"stopped"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots/snap/export":
			_, _ = io.WriteString(w, `bundle`)
		case r.Method == http.MethodPost && r.URL.Path == "/snapshots/imported/import":
			_, _ = io.WriteString(w, `{"snapshot":{"name":"imported","source_vm":"demo"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/snapshots/snap":
			_, _ = io.WriteString(w, `{"status":"deleted"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	a := app{client: testClient(t, handler), json: true}
	commands := [][]string{
		{"list"},
		{"inspect", "snap"},
		{"save", "snap", "demo"},
		{"restore", "snap", "copy", "-vcpus", "2", "-memory-min-mib", "128", "-memory-max-mib", "512", "-http-port", "3000", "-publish"},
		{"export", "snap", outputPath},
		{"import", "imported", inputPath},
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
		"GET /vms-by-name/demo",
		"POST /snapshots",
		"POST /snapshots/snap/restore",
		"GET /snapshots/snap/export",
		"POST /snapshots/imported/import",
		"DELETE /snapshots/snap",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
	var saveBody map[string]any
	if err := json.Unmarshal([]byte(requests[3].body), &saveBody); err != nil {
		t.Fatal(err)
	}
	if saveBody["name"] != "snap" || saveBody["vm_uuid"] != "demo-uuid" {
		t.Fatalf("save body = %#v", saveBody)
	}
	var restoreBody map[string]any
	if err := json.Unmarshal([]byte(requests[4].body), &restoreBody); err != nil {
		t.Fatal(err)
	}
	if restoreBody["vm_name"] != "copy" ||
		restoreBody["vcpus"] != float64(2) ||
		restoreBody["memory_min_mib"] != float64(128) ||
		restoreBody["memory_max_mib"] != float64(512) ||
		restoreBody["default_http_port"] != float64(3000) ||
		restoreBody["public_http"] != true {
		t.Fatalf("restore body = %#v", restoreBody)
	}
	exported, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(exported) != "bundle" {
		t.Fatalf("exported file = %q, want bundle", exported)
	}
	if requests[6].body != "bundle" {
		t.Fatalf("import body = %q, want bundle", requests[6].body)
	}
}

func TestRouteCommandsUseExpectedEndpointsAndBodies(t *testing.T) {
	var requests []cliRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/routes":
			_, _ = io.WriteString(w, `{"routes":[{"name":"web","vm_name":"demo","port":8080}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/vms-by-name/demo":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/routes":
			_, _ = io.WriteString(w, `{"route":{"name":"web","vm_name":"demo","port":8080}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/routes/web":
			_, _ = io.WriteString(w, `{"status":"deleted"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/route-protections":
			_, _ = io.WriteString(w, `{"hostname":"secret.dev.test","status":"protected"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/route-protections/secret.dev.test":
			_, _ = io.WriteString(w, `{"hostname":"secret.dev.test","status":"unprotected"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/route-auth/signed-url":
			_, _ = io.WriteString(w, `{"hostname":"secret.dev.test","url":"https://secret.dev.test/_firedoze/auth?token=test","ttl_seconds":60}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	a := app{client: testClient(t, handler), json: true}
	for _, args := range [][]string{
		{"list"},
		{"create", "web", "demo", "8080"},
		{"delete", "web"},
		{"protect", "secret.dev.test"},
		{"unprotect", "secret.dev.test"},
		{"get-signed-url", "secret.dev.test", "-ttl", "60"},
	} {
		if err := a.route(args); err != nil {
			t.Fatalf("route %#v: %v", args, err)
		}
	}
	got := requestKeys(requests)
	want := []string{"GET /routes", "GET /vms-by-name/demo", "POST /routes", "DELETE /routes/web", "POST /route-protections", "DELETE /route-protections/secret.dev.test", "POST /route-auth/signed-url"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(requests[2].body), &body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "web" || body["vm_uuid"] != "demo-uuid" || body["port"] != float64(8080) {
		t.Fatalf("route create body = %#v", body)
	}
}

func TestSSHStartsSleepingVMWaitsAndRunsSSH(t *testing.T) {
	var requests []cliRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vms-by-name/demo":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"sleeping","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/demo-uuid/start":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/demo-uuid/activity":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	var waitedIP string
	var cmdArgs []string
	a := app{
		client: testClient(t, handler),
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
	if !reflect.DeepEqual(requestKeys(requests), []string{"GET /vms-by-name/demo", "POST /vms/demo-uuid/start", "POST /vms/demo-uuid/activity"}) {
		t.Fatalf("requests = %#v", requestKeys(requests))
	}
	wantSuffix := []string{"ubuntu@fd00::2", "-L", "8080:localhost:8080"}
	if got := cmdArgs[len(cmdArgs)-len(wantSuffix):]; !reflect.DeepEqual(got, wantSuffix) {
		t.Fatalf("ssh command suffix = %#v, want %#v", got, wantSuffix)
	}
}

func TestSSHProxyStartsSleepingVMWaitsDialsAndPipes(t *testing.T) {
	var requests []cliRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readCLIRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vms-by-name/demo":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"sleeping","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/demo-uuid/start":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/demo-uuid/activity":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	inputReader, inputWriter := io.Pipe()
	serverErr := make(chan error, 2)
	go func() {
		_, err := io.WriteString(inputWriter, "client hello")
		if err != nil {
			serverErr <- err
		}
	}()

	var waitedIP, dialNetwork, dialAddress string
	var output bytes.Buffer
	a := app{
		client: testClient(t, handler),
		waitForSSHFn: func(ip string, _ time.Duration) error {
			waitedIP = ip
			return nil
		},
		proxyDial: func(_ context.Context, network string, address string) (net.Conn, error) {
			dialNetwork = network
			dialAddress = address
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				data := make([]byte, len("client hello"))
				if _, err := io.ReadFull(server, data); err != nil {
					serverErr <- err
					return
				}
				if string(data) != "client hello" {
					serverErr <- errors.New("proxy did not send client input")
					return
				}
				if _, err := io.WriteString(server, "server hello"); err != nil {
					serverErr <- err
					return
				}
				if err := inputWriter.Close(); err != nil {
					serverErr <- err
					return
				}
				serverErr <- nil
			}()
			return client, nil
		},
		proxyInput:  inputReader,
		proxyOutput: &output,
	}
	if err := a.sshProxy([]string{"demo"}); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if waitedIP != "fd00::2" {
		t.Fatalf("waited IP = %q, want fd00::2", waitedIP)
	}
	if dialNetwork != "tcp" || dialAddress != "[fd00::2]:22" {
		t.Fatalf("dial = %s %s, want tcp [fd00::2]:22", dialNetwork, dialAddress)
	}
	if output.String() != "server hello" {
		t.Fatalf("proxy output = %q, want server hello", output.String())
	}
	if !reflect.DeepEqual(requestKeys(requests), []string{"GET /vms-by-name/demo", "POST /vms/demo-uuid/start", "POST /vms/demo-uuid/activity"}) {
		t.Fatalf("requests = %#v", requestKeys(requests))
	}
}

func TestExecCpAndWithVMIPUseCommandRunner(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/vms/demo-uuid/activity" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/vms-by-name/demo" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","ssh":"ssh ubuntu@demo.example.test"}}`)
	})

	var commands [][]string
	var withVMIPEnv []string
	a := app{
		client: testClient(t, handler),
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readCLIRequest(t, r)
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vms-by-name/demo" && len(requests) == 1:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/base-image/warmup":
			_, _ = io.WriteString(w, `{"base_image":{}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"stopped","private_ip":"fd00::2","public_http":true,"ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/demo-uuid/start":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","public_http":true,"ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/vms/demo-uuid/activity":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","public_http":true,"ssh":"ssh ubuntu@demo.example.test"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/vms-by-name/demo":
			_, _ = io.WriteString(w, `{"vm":{"uuid":"demo-uuid","name":"demo","state":"running","private_ip":"fd00::2","public_http":true,"ssh":"ssh ubuntu@demo.example.test"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	var waitedIP string
	var command []string
	a := app{
		client: testClient(t, handler),
		runCommand: func(cmd *exec.Cmd) error {
			command = append([]string(nil), cmd.Args...)
			return nil
		},
		waitForSSHFn: func(ip string, _ time.Duration) error {
			waitedIP = ip
			return nil
		},
	}
	if err := a.up([]string{"demo", "-memory-min-mib", "128", "-memory-max-mib", "256", "--", "-A"}); err != nil {
		t.Fatal(err)
	}
	got := requestKeys(requests)
	want := []string{"GET /vms-by-name/demo", "POST /base-image/warmup", "POST /vms", "POST /vms/demo-uuid/start", "GET /vms-by-name/demo", "POST /vms/demo-uuid/activity"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
	var createBody map[string]any
	if err := json.Unmarshal([]byte(requests[2].body), &createBody); err != nil {
		t.Fatal(err)
	}
	if createBody["name"] != "demo" || createBody["memory_min_mib"] != float64(128) || createBody["memory_max_mib"] != float64(256) || createBody["public_http"] != true {
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"already exists"}`)
	})

	err := testClient(t, handler).do(t.Context(), http.MethodPost, "/vms", map[string]any{"name": "demo"}, nil)
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/health" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	t.Setenv("FIREDOZE_API", installTestHTTPClient(t, handler))

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

const testClientBaseURL = "http://firedoze.test"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testClient(t *testing.T, handler http.Handler) *client {
	t.Helper()
	return &client{
		baseURL: testClientBaseURL,
		http:    testHTTPClient(handler),
	}
}

func installTestHTTPClient(t *testing.T, handler http.Handler) string {
	t.Helper()
	old := newHTTPClient
	newHTTPClient = func() *http.Client {
		return testHTTPClient(handler)
	}
	t.Cleanup(func() {
		newHTTPClient = old
	})
	return testClientBaseURL
}

func testHTTPClient(handler http.Handler) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			serverReq := req.Clone(req.Context())
			u := *req.URL
			u.Scheme = ""
			u.Host = ""
			serverReq.URL = &u
			serverReq.RequestURI = u.RequestURI()
			handler.ServeHTTP(rec, serverReq)
			return rec.Result(), nil
		}),
	}
}

func readCLIRequest(t *testing.T, r *http.Request) cliRequest {
	t.Helper()
	var data []byte
	if r.Body != nil {
		var err error
		data, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
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

func TestActivityHeartbeatIntervalUsesConfiguredIdleTimeout(t *testing.T) {
	if got := activityHeartbeatInterval(vmInfo{VM: model.VM{IdleSleepAfterSeconds: 21600}}); got != 90*time.Minute {
		t.Fatalf("default heartbeat interval = %s, want 90m", got)
	}
	if got := activityHeartbeatInterval(vmInfo{VM: modelVM("running", "")}); got != 0 {
		t.Fatalf("disabled heartbeat interval = %s, want 0", got)
	}
	if got := activityHeartbeatInterval(vmInfo{VM: model.VM{IdleSleepAfterSeconds: 60}}); got != 15*time.Second {
		t.Fatalf("override heartbeat interval = %s, want 15s", got)
	}
	if got := activityHeartbeatInterval(vmInfo{VM: model.VM{IdleSleepAfterSeconds: 20}}); got != 10*time.Second {
		t.Fatalf("short timeout heartbeat interval = %s, want 10s floor", got)
	}
}

func modelVM(state string, lastStartedAt string) model.VM {
	return model.VM{
		Name:          "demo",
		State:         state,
		LastStartedAt: lastStartedAt,
	}
}
