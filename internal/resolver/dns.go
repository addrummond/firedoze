package resolver

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/store"

	mdns "github.com/miekg/dns"
)

type Server struct {
	cfg    config.Config
	store  *store.Store
	logger *slog.Logger
}

func NewServer(cfg config.Config, st *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, store: st, logger: logger}
}

func (s *Server) Run(ctx context.Context) error {
	bindIP, err := wireGuardIP(s.cfg.WireGuard.Address)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(bindIP.String(), strconv.Itoa(s.cfg.DNS.Port))
	handler := mdns.HandlerFunc(s.handleDNS)
	servers := []*mdns.Server{
		{Addr: addr, Net: "udp", Handler: handler},
		{Addr: addr, Net: "tcp", Handler: handler},
	}

	errCh := make(chan error, len(servers))
	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Add(1)
		go func(server *mdns.Server) {
			defer wg.Done()
			if err := server.ListenAndServe(); err != nil {
				select {
				case <-ctx.Done():
				case errCh <- err:
				}
			}
		}(server)
	}
	s.logger.Info("dns resolver listening", "addr", addr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, server := range servers {
			_ = server.ShutdownContext(shutdownCtx)
		}
		wg.Wait()
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleDNS(w mdns.ResponseWriter, r *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	msg.RecursionAvailable = false

	for _, q := range r.Question {
		if q.Qclass != mdns.ClassINET || q.Qtype != mdns.TypeA {
			continue
		}
		name := strings.TrimSuffix(strings.ToLower(q.Name), ".")
		vmName, ok := s.vmNameForHost(name)
		if !ok {
			continue
		}
		vm, err := s.store.GetVM(context.Background(), vmName)
		if err != nil || vm.PrivateIP == "" {
			continue
		}
		ip := net.ParseIP(vm.PrivateIP).To4()
		if ip == nil {
			continue
		}
		msg.Answer = append(msg.Answer, &mdns.A{
			Hdr: mdns.RR_Header{
				Name:   q.Name,
				Rrtype: mdns.TypeA,
				Class:  mdns.ClassINET,
				Ttl:    5,
			},
			A: ip,
		})
	}

	if len(msg.Answer) == 0 {
		msg.Rcode = mdns.RcodeNameError
	}
	_ = w.WriteMsg(msg)
}

func (s *Server) vmNameForHost(host string) (string, bool) {
	base := strings.TrimSuffix(strings.ToLower(s.cfg.BaseDomain), ".")
	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	vmName := strings.TrimSuffix(host, suffix)
	if vmName == "" || strings.Contains(vmName, ".") {
		return "", false
	}
	return vmName, true
}

func wireGuardIP(address string) (net.IP, error) {
	ip, _, err := net.ParseCIDR(address)
	return ip, err
}
