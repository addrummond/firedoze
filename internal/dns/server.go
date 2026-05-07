package dns

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/store"

	miekgdns "github.com/miekg/dns"
)

type Server struct {
	cfg    config.DNSConfig
	store  *store.Store
	logger *slog.Logger
	domain string
}

func NewServer(cfg config.DNSConfig, st *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:    cfg,
		store:  st,
		logger: logger,
		domain: normalizeDomain(cfg.Domain),
	}
}

func (s *Server) Run(ctx context.Context) error {
	mux := miekgdns.NewServeMux()
	mux.HandleFunc(".", s.handle)

	addr := net.JoinHostPort(s.cfg.ListenIP, "53")
	if s.cfg.Port != 0 {
		addr = net.JoinHostPort(s.cfg.ListenIP, strconv.Itoa(s.cfg.Port))
	}
	udpServer := &miekgdns.Server{Addr: addr, Net: "udp", Handler: mux}
	tcpServer := &miekgdns.Server{Addr: addr, Net: "tcp", Handler: mux}
	errCh := make(chan error, 2)

	go func() {
		s.logger.InfoContext(ctx, "dns server listening", "network", "udp", "addr", addr, "domain", s.domain)
		errCh <- udpServer.ListenAndServe()
	}()
	go func() {
		s.logger.InfoContext(ctx, "dns server listening", "network", "tcp", "addr", addr, "domain", s.domain)
		errCh <- tcpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := errors.Join(udpServer.ShutdownContext(shutdownCtx), tcpServer.ShutdownContext(shutdownCtx))
		if err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func (s *Server) handle(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
	if len(r.Question) == 0 {
		writeDNS(w, newReply(r, miekgdns.RcodeFormatError))
		return
	}
	q := r.Question[0]
	name := strings.ToLower(q.Name)
	if vmName, ok := s.vmName(name); ok {
		s.answerVM(w, r, q, vmName)
		return
	}
	s.forward(w, r)
}

func (s *Server) answerVM(w miekgdns.ResponseWriter, r *miekgdns.Msg, q miekgdns.Question, vmName string) {
	msg := newReply(r, miekgdns.RcodeSuccess)
	if q.Qtype != miekgdns.TypeAAAA && q.Qtype != miekgdns.TypeANY {
		writeDNS(w, msg)
		return
	}
	vm, err := s.store.GetVMByName(context.Background(), vmName)
	if err != nil {
		writeDNS(w, newReply(r, miekgdns.RcodeNameError))
		return
	}
	ip := net.ParseIP(vm.PrivateIP)
	if ip == nil || ip.To4() != nil {
		writeDNS(w, newReply(r, miekgdns.RcodeNameError))
		return
	}
	msg.Answer = append(msg.Answer, &miekgdns.AAAA{
		Hdr: miekgdns.RR_Header{
			Name:   q.Name,
			Rrtype: miekgdns.TypeAAAA,
			Class:  miekgdns.ClassINET,
			Ttl:    uint32(s.cfg.TTLSeconds),
		},
		AAAA: ip,
	})
	writeDNS(w, msg)
}

func (s *Server) forward(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
	client := &miekgdns.Client{Net: "udp", Timeout: 5 * time.Second}
	for _, upstream := range s.cfg.UpstreamServers {
		resp, _, err := client.Exchange(r, upstream)
		if err == nil && resp != nil {
			writeDNS(w, resp)
			return
		}
	}
	writeDNS(w, newReply(r, miekgdns.RcodeServerFailure))
}

func (s *Server) vmName(name string) (string, bool) {
	if !strings.HasSuffix(name, "."+s.domain) {
		return "", false
	}
	label := strings.TrimSuffix(name, "."+s.domain)
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

func normalizeDomain(domain string) string {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		domain = "firedoze"
	}
	return domain + "."
}

func newReply(r *miekgdns.Msg, rcode int) *miekgdns.Msg {
	msg := new(miekgdns.Msg)
	msg.SetReply(r)
	msg.Rcode = rcode
	return msg
}

func writeDNS(w miekgdns.ResponseWriter, msg *miekgdns.Msg) {
	_ = w.WriteMsg(msg)
}
