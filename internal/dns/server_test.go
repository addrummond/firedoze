package dns

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/store"

	miekgdns "github.com/miekg/dns"
)

func TestAnswersVMName(t *testing.T) {
	ctx, st := newTestStore(t)
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(config.DNSConfig{Domain: "firedoze", TTLSeconds: 30}, st, nil)
	req := new(miekgdns.Msg)
	req.SetQuestion("demo.firedoze.", miekgdns.TypeAAAA)
	rec := &responseRecorder{}
	server.handle(rec, req)

	if rec.msg == nil {
		t.Fatal("no DNS response")
	}
	if rec.msg.Rcode != miekgdns.RcodeSuccess {
		t.Fatalf("rcode = %d, want success", rec.msg.Rcode)
	}
	if len(rec.msg.Answer) != 1 {
		t.Fatalf("answers = %#v, want one answer", rec.msg.Answer)
	}
	answer, ok := rec.msg.Answer[0].(*miekgdns.AAAA)
	if !ok {
		t.Fatalf("answer = %T, want AAAA", rec.msg.Answer[0])
	}
	if got, want := answer.AAAA.String(), "fd7a:115c:a1e0::3"; got != want {
		t.Fatalf("answer IP = %s, want %s", got, want)
	}
}

func TestAnswersAnyButNotA(t *testing.T) {
	ctx, st := newTestStore(t)
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.DNSConfig{Domain: "firedoze", TTLSeconds: 30}, st, nil)

	anyReq := new(miekgdns.Msg)
	anyReq.SetQuestion("demo.firedoze.", miekgdns.TypeANY)
	anyRec := &responseRecorder{}
	server.handle(anyRec, anyReq)
	if anyRec.msg == nil || anyRec.msg.Rcode != miekgdns.RcodeSuccess || len(anyRec.msg.Answer) != 1 {
		t.Fatalf("ANY response = %#v, want one success answer", anyRec.msg)
	}

	aReq := new(miekgdns.Msg)
	aReq.SetQuestion("demo.firedoze.", miekgdns.TypeA)
	aRec := &responseRecorder{}
	server.handle(aRec, aReq)
	if aRec.msg == nil || aRec.msg.Rcode != miekgdns.RcodeSuccess || len(aRec.msg.Answer) != 0 {
		t.Fatalf("A response = %#v, want success without answers", aRec.msg)
	}
}

func TestUnknownVMNameIsNXDOMAIN(t *testing.T) {
	_, st := newTestStore(t)
	server := NewServer(config.DNSConfig{Domain: "firedoze", TTLSeconds: 30}, st, nil)
	req := new(miekgdns.Msg)
	req.SetQuestion("missing.firedoze.", miekgdns.TypeAAAA)
	rec := &responseRecorder{}
	server.handle(rec, req)

	if rec.msg == nil {
		t.Fatal("no DNS response")
	}
	if rec.msg.Rcode != miekgdns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", rec.msg.Rcode)
	}
}

func TestVMNameWithInvalidPrivateIPIsNXDOMAIN(t *testing.T) {
	ctx, st := newTestStore(t)
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "demo", PrivateIP: "10.0.0.2", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.DNSConfig{Domain: "firedoze", TTLSeconds: 30}, st, nil)
	req := new(miekgdns.Msg)
	req.SetQuestion("demo.firedoze.", miekgdns.TypeAAAA)
	rec := &responseRecorder{}
	server.handle(rec, req)

	if rec.msg == nil || rec.msg.Rcode != miekgdns.RcodeNameError {
		t.Fatalf("rcode = %#v, want NXDOMAIN", rec.msg)
	}
}

func TestNoQuestionIsFormatError(t *testing.T) {
	_, st := newTestStore(t)
	server := NewServer(config.DNSConfig{Domain: "firedoze", TTLSeconds: 30}, st, nil)
	rec := &responseRecorder{}
	server.handle(rec, new(miekgdns.Msg))

	if rec.msg == nil || rec.msg.Rcode != miekgdns.RcodeFormatError {
		t.Fatalf("rcode = %#v, want format error", rec.msg)
	}
}

func TestForwardSuccess(t *testing.T) {
	upstream := startUDPUpstream(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
		msg := newReply(r, miekgdns.RcodeSuccess)
		msg.Answer = append(msg.Answer, &miekgdns.A{
			Hdr: miekgdns.RR_Header{Name: r.Question[0].Name, Rrtype: miekgdns.TypeA, Class: miekgdns.ClassINET, Ttl: 60},
			A:   net.ParseIP("192.0.2.10"),
		})
		writeDNS(w, msg)
	})
	_, st := newTestStore(t)
	server := NewServer(config.DNSConfig{Domain: "firedoze", UpstreamServers: []string{upstream}}, st, nil)
	req := new(miekgdns.Msg)
	req.SetQuestion("example.com.", miekgdns.TypeA)
	rec := &responseRecorder{}

	server.handle(rec, req)

	if rec.msg == nil || rec.msg.Rcode != miekgdns.RcodeSuccess {
		t.Fatalf("forward response = %#v, want success", rec.msg)
	}
	if len(rec.msg.Answer) != 1 {
		t.Fatalf("answers = %#v, want one forwarded answer", rec.msg.Answer)
	}
	answer, ok := rec.msg.Answer[0].(*miekgdns.A)
	if !ok || answer.A.String() != "192.0.2.10" {
		t.Fatalf("answer = %#v, want A 192.0.2.10", rec.msg.Answer[0])
	}
}

func TestForwardFailure(t *testing.T) {
	_, st := newTestStore(t)
	server := NewServer(config.DNSConfig{Domain: "firedoze", UpstreamServers: []string{"bad-upstream-address"}}, st, nil)
	req := new(miekgdns.Msg)
	req.SetQuestion("example.com.", miekgdns.TypeA)
	rec := &responseRecorder{}

	server.handle(rec, req)

	if rec.msg == nil || rec.msg.Rcode != miekgdns.RcodeServerFailure {
		t.Fatalf("forward response = %#v, want server failure", rec.msg)
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	ctx, st := newTestStore(t)
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "demo", PrivateIP: "fd7a:115c:a1e0::3", VCPUs: 1, MemoryMinMiB: 128, MemoryMaxMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}
	port := freeDNSPort(t)
	server := NewServer(config.DNSConfig{
		Domain:     "firedoze",
		ListenIP:   "127.0.0.1",
		Port:       port,
		TTLSeconds: 30,
	}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Run(runCtx)
	}()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var udpResp *miekgdns.Msg
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		resp, _, err := queryDNS(addr, "udp")
		if err == nil && resp != nil {
			udpResp = resp
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if udpResp == nil {
		cancel()
		t.Fatal("DNS server did not answer UDP query")
	}
	if udpResp.Rcode != miekgdns.RcodeSuccess || len(udpResp.Answer) != 1 {
		cancel()
		t.Fatalf("UDP response = %#v, want one success answer", udpResp)
	}
	tcpResp, _, err := queryDNS(addr, "tcp")
	if err != nil {
		cancel()
		t.Fatalf("TCP query: %v", err)
	}
	if tcpResp.Rcode != miekgdns.RcodeSuccess || len(tcpResp.Answer) != 1 {
		cancel()
		t.Fatalf("TCP response = %#v, want one success answer", tcpResp)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestNormalizeDomainAndVMName(t *testing.T) {
	if got, want := normalizeDomain(" Firedoze. "), "firedoze."; got != want {
		t.Fatalf("normalizeDomain = %q, want %q", got, want)
	}
	if got, want := normalizeDomain(""), "firedoze."; got != want {
		t.Fatalf("normalizeDomain empty = %q, want %q", got, want)
	}
	server := NewServer(config.DNSConfig{Domain: "dev.example.com"}, nil, nil)
	tests := []struct {
		name string
		vm   string
		ok   bool
	}{
		{name: "demo.dev.example.com.", vm: "demo", ok: true},
		{name: "DEMO.DEV.EXAMPLE.COM.", vm: "demo", ok: true},
		{name: "nested.demo.dev.example.com.", ok: false},
		{name: "dev.example.com.", ok: false},
		{name: "demo.other.example.com.", ok: false},
	}
	for _, tt := range tests {
		got, ok := server.vmName(strings.ToLower(tt.name))
		if ok != tt.ok || got != tt.vm {
			t.Fatalf("vmName(%q) = %q, %v; want %q, %v", tt.name, got, ok, tt.vm, tt.ok)
		}
	}
}

type responseRecorder struct {
	msg *miekgdns.Msg
}

func (r *responseRecorder) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (r *responseRecorder) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (r *responseRecorder) WriteMsg(msg *miekgdns.Msg) error { r.msg = msg; return nil }
func (r *responseRecorder) Write(data []byte) (int, error)   { return len(data), nil }
func (r *responseRecorder) Close() error                     { return nil }
func (r *responseRecorder) TsigStatus() error                { return nil }
func (r *responseRecorder) TsigTimersOnly(bool)              {}
func (r *responseRecorder) Hijack()                          {}

func newTestStore(t *testing.T) (context.Context, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, st
}

func startUDPUpstream(t *testing.T, handler miekgdns.HandlerFunc) string {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("udp sockets unavailable: %v", err)
		}
		t.Fatal(err)
	}
	mux := miekgdns.NewServeMux()
	mux.HandleFunc(".", handler)
	server := &miekgdns.Server{PacketConn: packetConn, Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ActivateAndServe()
	}()
	t.Cleanup(func() {
		_ = server.Shutdown()
		select {
		case err := <-errCh:
			if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				t.Errorf("upstream shutdown: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("upstream did not shut down")
		}
	})
	return packetConn.LocalAddr().String()
}

func freeDNSPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("tcp sockets unavailable: %v", err)
		}
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	packetConn, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", portText))
	if err != nil {
		_ = ln.Close()
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("udp sockets unavailable: %v", err)
		}
		t.Fatal(err)
	}
	_ = packetConn.Close()
	_ = ln.Close()
	return port
}

func queryDNS(addr string, network string) (*miekgdns.Msg, time.Duration, error) {
	req := new(miekgdns.Msg)
	req.SetQuestion("demo.firedoze.", miekgdns.TypeAAAA)
	client := &miekgdns.Client{Net: network, Timeout: 100 * time.Millisecond}
	return client.Exchange(req, addr)
}
