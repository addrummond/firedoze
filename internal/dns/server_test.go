package dns

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"firedoze/internal/config"
	"firedoze/internal/store"

	miekgdns "github.com/miekg/dns"
)

func TestAnswersVMName(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateVM(ctx, store.CreateVMParams{Name: "demo", PrivateIP: "10.88.0.2", VCPUs: 1, MemoryMiB: 128, DiskBytes: 1024, DefaultHTTPPort: 8080}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(config.DNSConfig{Domain: "firedoze", TTLSeconds: 30}, st, nil)
	req := new(miekgdns.Msg)
	req.SetQuestion("demo.firedoze.", miekgdns.TypeA)
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
	answer, ok := rec.msg.Answer[0].(*miekgdns.A)
	if !ok {
		t.Fatalf("answer = %T, want A", rec.msg.Answer[0])
	}
	if got, want := answer.A.String(), "10.88.0.2"; got != want {
		t.Fatalf("answer IP = %s, want %s", got, want)
	}
}

func TestUnknownVMNameIsNXDOMAIN(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "firedoze.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	server := NewServer(config.DNSConfig{Domain: "firedoze", TTLSeconds: 30}, st, nil)
	req := new(miekgdns.Msg)
	req.SetQuestion("missing.firedoze.", miekgdns.TypeA)
	rec := &responseRecorder{}
	server.handle(rec, req)

	if rec.msg == nil {
		t.Fatal("no DNS response")
	}
	if rec.msg.Rcode != miekgdns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", rec.msg.Rcode)
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
