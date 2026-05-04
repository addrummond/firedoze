package host

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"firedoze/internal/config"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNewLinuxOpsLoggerDefaults(t *testing.T) {
	if NewLinuxOps(nil).logger == nil {
		t.Fatal("NewLinuxOps(nil) returned nil logger")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := NewLinuxOps(logger).logger; got != logger {
		t.Fatal("NewLinuxOps did not keep custom logger")
	}
}

func TestEnsureWireGuardPrivateKeyCreatesAndReusesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "firedoze", "wg.key")

	key, err := ensureWireGuardPrivateKey(path)
	if err != nil {
		t.Fatalf("ensureWireGuardPrivateKey create: %v", err)
	}
	if key == (wgtypes.Key{}) {
		t.Fatal("created key is zero")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := wgtypes.ParseKey(string(data))
	if err != nil {
		t.Fatalf("created key file is not parseable: %v", err)
	}
	if parsed != key {
		t.Fatalf("key file = %s, want %s", parsed, key)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("key mode = %v, want 0640", got)
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("key directory mode = %v, want 0700", got)
	}

	reused, err := ensureWireGuardPrivateKey(path)
	if err != nil {
		t.Fatalf("ensureWireGuardPrivateKey reuse: %v", err)
	}
	if reused != key {
		t.Fatalf("reused key = %s, want %s", reused, key)
	}
}

func TestEnsureWireGuardPrivateKeyRejectsMalformedExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg.key")
	if err := os.WriteFile(path, []byte("not-a-wireguard-key\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureWireGuardPrivateKey(path); err == nil {
		t.Fatal("ensureWireGuardPrivateKey accepted malformed existing key")
	}
}

func TestEnsureLoopbackAddress(t *testing.T) {
	restoreNetlink := stubNetlink(t)
	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}
	var assigned *netlink.Addr
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		if name != "lo" {
			t.Fatalf("LinkByName(%q), want lo", name)
		}
		return fakeLink, nil
	}
	netlinkAddrReplace = func(link netlink.Link, addr *netlink.Addr) error {
		if link != fakeLink {
			t.Fatalf("AddrReplace link = %#v, want fake loopback", link)
		}
		assigned = addr
		return nil
	}
	defer restoreNetlink()

	err := NewLinuxOps(slog.New(slog.NewTextHandler(io.Discard, nil))).EnsureLoopbackAddress(context.Background(), "fd7a:115c:a1e0::1")
	if err != nil {
		t.Fatalf("EnsureLoopbackAddress: %v", err)
	}
	if assigned == nil || assigned.IP.String() != "fd7a:115c:a1e0::1" {
		t.Fatalf("assigned address = %#v", assigned)
	}
	ones, bits := assigned.Mask.Size()
	if ones != 128 || bits != 128 {
		t.Fatalf("assigned mask = %d/%d, want 128/128", ones, bits)
	}
}

func TestEnsureFirewallConfiguresIP6Tables(t *testing.T) {
	restore := stubRunCommand(t)
	defer restore()

	var commands []string
	runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if len(args) >= 1 && args[0] == "-C" {
			return []byte("missing rule"), errors.New("missing rule")
		}
		return nil, nil
	}

	cfg := config.Default()
	cfg.WireGuard.Interface = "fdwg-test"
	cfg.VMNetwork.Subnet = "fd7a:115c:a1e0::/64"

	if err := NewLinuxOps(nil).EnsureFirewall(context.Background(), cfg); err != nil {
		t.Fatalf("EnsureFirewall: %v", err)
	}

	want := []string{
		"/usr/sbin/ip6tables -N FIREDOZE-VM",
		"/usr/sbin/ip6tables -F FIREDOZE-VM",
		"/usr/sbin/ip6tables -A FIREDOZE-VM -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"/usr/sbin/ip6tables -A FIREDOZE-VM -i fdwg-test -d fd7a:115c:a1e0::/64 -j ACCEPT",
		"/usr/sbin/ip6tables -A FIREDOZE-VM -i fdtap+ -d fd7a:115c:a1e0::/64 -j ACCEPT",
		"/usr/sbin/ip6tables -A FIREDOZE-VM -i lo -d fd7a:115c:a1e0::/64 -j ACCEPT",
		"/usr/sbin/ip6tables -A FIREDOZE-VM -d fd7a:115c:a1e0::/64 -j DROP",
		"/usr/sbin/ip6tables -A FIREDOZE-VM -j RETURN",
		"/usr/sbin/ip6tables -C INPUT -j FIREDOZE-VM",
		"/usr/sbin/ip6tables -I INPUT 1 -j FIREDOZE-VM",
		"/usr/sbin/ip6tables -C FORWARD -j FIREDOZE-VM",
		"/usr/sbin/ip6tables -I FORWARD 1 -j FIREDOZE-VM",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\n\nwant:\n%s", strings.Join(commands, "\n"), strings.Join(want, "\n"))
	}
}

func TestEnsureFirewallKeepsExistingChainAndHooks(t *testing.T) {
	restore := stubRunCommand(t)
	defer restore()

	var inserts []string
	runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		if len(args) >= 1 && args[0] == "-N" {
			return []byte("Chain already exists"), errors.New("exists")
		}
		if len(args) >= 1 && args[0] == "-I" {
			inserts = append(inserts, command)
		}
		return nil, nil
	}

	cfg := config.Default()
	cfg.WireGuard.Interface = "fdwg-test"
	cfg.VMNetwork.Subnet = "fd7a:115c:a1e0::/64"

	if err := NewLinuxOps(nil).EnsureFirewall(context.Background(), cfg); err != nil {
		t.Fatalf("EnsureFirewall: %v", err)
	}
	if len(inserts) != 0 {
		t.Fatalf("inserted hooks despite successful -C checks: %v", inserts)
	}
}

func TestEnsureFirewallErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
		setup  func()
		want   string
	}{
		{
			name: "bad subnet",
			mutate: func(cfg *config.Config) {
				cfg.VMNetwork.Subnet = "bad"
			},
			want: "vm_network.subnet",
		},
		{
			name: "ipv4 subnet",
			mutate: func(cfg *config.Config) {
				cfg.VMNetwork.Subnet = "10.0.0.0/24"
			},
			want: "vm_network.subnet must be IPv6",
		},
		{
			name: "missing wireguard interface",
			mutate: func(cfg *config.Config) {
				cfg.WireGuard.Interface = ""
			},
			want: "wireguard.interface",
		},
		{
			name: "ip6tables failure",
			setup: func() {
				runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
					return []byte("permission denied"), errors.New("exit 1")
				}
			},
			want: "create firewall chain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := stubRunCommand(t)
			defer restore()
			runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, nil
			}
			if tt.setup != nil {
				tt.setup()
			}
			cfg := config.Default()
			cfg.WireGuard.Interface = "fdwg-test"
			cfg.VMNetwork.Subnet = "fd7a:115c:a1e0::/64"
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			err := NewLinuxOps(nil).EnsureFirewall(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("EnsureFirewall error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestEnsureLoopbackAddressErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T)
		want  string
	}{
		{
			name: "missing loopback",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return nil, errors.New("no loopback")
				}
			},
			want: "find loopback",
		},
		{
			name: "bad address",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}, nil
				}
			},
			want: "parse loopback address",
		},
		{
			name: "assign failure",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}, nil
				}
				netlinkAddrReplace = func(netlink.Link, *netlink.Addr) error {
					return errors.New("permission denied")
				}
			},
			want: "assign loopback address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restoreNetlink := stubNetlink(t)
			defer restoreNetlink()
			tt.setup(t)

			address := "fd7a:115c:a1e0::1"
			if tt.name == "bad address" {
				address = "not-an-ip"
			}
			err := NewLinuxOps(nil).EnsureLoopbackAddress(context.Background(), address)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("EnsureLoopbackAddress error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoopbackCIDR(t *testing.T) {
	tests := []struct {
		address string
		want    string
		ok      bool
	}{
		{address: "127.0.0.1", want: "127.0.0.1/32", ok: true},
		{address: "fd7a:115c:a1e0::1", want: "fd7a:115c:a1e0::1/128", ok: true},
		{address: "not-an-ip", ok: false},
	}
	for _, tt := range tests {
		got, err := loopbackCIDR(tt.address)
		if tt.ok && err != nil {
			t.Fatalf("loopbackCIDR(%q): %v", tt.address, err)
		}
		if !tt.ok && err == nil {
			t.Fatalf("loopbackCIDR(%q) succeeded, want error", tt.address)
		}
		if tt.ok && got != tt.want {
			t.Fatalf("loopbackCIDR(%q) = %q, want %q", tt.address, got, tt.want)
		}
	}
}

func TestEnsureWireGuardConfiguresDevice(t *testing.T) {
	restoreNetlink := stubNetlink(t)
	defer restoreNetlink()
	restoreWG := stubWG(t)
	defer restoreWG()

	cfg := validWireGuardConfig(t)
	fakeLink := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: cfg.Interface}}
	var assigned *netlink.Addr
	var linkSetUp bool
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		if name != cfg.Interface {
			t.Fatalf("LinkByName(%q), want %q", name, cfg.Interface)
		}
		return fakeLink, nil
	}
	netlinkAddrReplace = func(link netlink.Link, addr *netlink.Addr) error {
		if link != fakeLink {
			t.Fatalf("AddrReplace link = %#v, want fake WireGuard link", link)
		}
		assigned = addr
		return nil
	}
	netlinkLinkSetUp = func(link netlink.Link) error {
		if link != fakeLink {
			t.Fatalf("LinkSetUp link = %#v, want fake WireGuard link", link)
		}
		linkSetUp = true
		return nil
	}

	client := &fakeWGClient{}
	wgctrlNew = func() (wgClient, error) {
		return client, nil
	}

	if err := NewLinuxOps(nil).EnsureWireGuard(context.Background(), cfg); err != nil {
		t.Fatalf("EnsureWireGuard: %v", err)
	}
	if assigned == nil || assigned.String() != "fd7a:115c:a1e1::1/64" {
		t.Fatalf("assigned WireGuard address = %#v", assigned)
	}
	if !linkSetUp {
		t.Fatal("WireGuard link was not set up")
	}
	if !client.closed {
		t.Fatal("wgctrl client was not closed")
	}
	if client.device != cfg.Interface {
		t.Fatalf("configured device = %q, want %q", client.device, cfg.Interface)
	}
	if client.config.PrivateKey == nil {
		t.Fatal("device config missing private key")
	}
	if client.config.ListenPort == nil || *client.config.ListenPort != cfg.ListenPort {
		t.Fatalf("listen port = %#v, want %d", client.config.ListenPort, cfg.ListenPort)
	}
	if !client.config.ReplacePeers {
		t.Fatal("device config does not replace peers")
	}
	if got := len(client.config.Peers); got != 1 {
		t.Fatalf("peer count = %d, want 1", got)
	}
	peer := client.config.Peers[0]
	if !peer.ReplaceAllowedIPs {
		t.Fatal("peer config does not replace allowed IPs")
	}
	if got := len(peer.AllowedIPs); got != 2 {
		t.Fatalf("allowed IP count = %d, want 2", got)
	}
	if peer.AllowedIPs[0].String() != "fd7a:115c:a1e1::2/128" || peer.AllowedIPs[1].String() != "fd7a:115c:a1e0::/64" {
		t.Fatalf("allowed IPs = %#v", peer.AllowedIPs)
	}
}

func TestEnsureWireGuardErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.WireGuardConfig)
		setup  func(*testing.T)
		want   string
	}{
		{
			name: "validation stops before host work",
			mutate: func(cfg *config.WireGuardConfig) {
				cfg.Interface = ""
			},
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					t.Fatal("LinkByName called after validation failure")
					return nil, nil
				}
			},
			want: "wireguard.interface",
		},
		{
			name: "private key",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					t.Fatal("LinkByName called after private key failure")
					return nil, nil
				}
			},
			mutate: func(cfg *config.WireGuardConfig) {
				cfg.PrivateKeyFile = filepath.Join(t.TempDir(), "wg.key")
				if err := os.WriteFile(cfg.PrivateKeyFile, []byte("bad key\n"), 0o640); err != nil {
					t.Fatal(err)
				}
			},
			want: "private key",
		},
		{
			name: "link",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return nil, errors.New("netlink failed")
				}
			},
			want: "link",
		},
		{
			name: "assign address",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return &netlink.Wireguard{}, nil
				}
				netlinkAddrReplace = func(netlink.Link, *netlink.Addr) error {
					return errors.New("assign failed")
				}
			},
			want: "assign address",
		},
		{
			name: "open wgctrl",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return &netlink.Wireguard{}, nil
				}
				wgctrlNew = func() (wgClient, error) {
					return nil, errors.New("wgctrl failed")
				}
			},
			want: "open wgctrl",
		},
		{
			name: "peer public key",
			mutate: func(cfg *config.WireGuardConfig) {
				cfg.Peers[0].PublicKey = "bad"
			},
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return &netlink.Wireguard{}, nil
				}
				wgctrlNew = func() (wgClient, error) {
					return &fakeWGClient{}, nil
				}
			},
			want: "public key",
		},
		{
			name: "allowed ips",
			mutate: func(cfg *config.WireGuardConfig) {
				cfg.Peers[0].AllowedIPs = []string{"fd7a:115c:a1e1::2/128", "bad"}
			},
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return &netlink.Wireguard{}, nil
				}
				wgctrlNew = func() (wgClient, error) {
					return &fakeWGClient{}, nil
				}
			},
			want: "allowed_ips",
		},
		{
			name: "configure device",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return &netlink.Wireguard{}, nil
				}
				wgctrlNew = func() (wgClient, error) {
					return &fakeWGClient{configureErr: errors.New("configure failed")}, nil
				}
			},
			want: "configure device",
		},
		{
			name: "set link up",
			setup: func(t *testing.T) {
				netlinkLinkByName = func(string) (netlink.Link, error) {
					return &netlink.Wireguard{}, nil
				}
				wgctrlNew = func() (wgClient, error) {
					return &fakeWGClient{}, nil
				}
				netlinkLinkSetUp = func(netlink.Link) error {
					return errors.New("up failed")
				}
			},
			want: "set link up",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restoreNetlink := stubNetlink(t)
			defer restoreNetlink()
			restoreWG := stubWG(t)
			defer restoreWG()

			cfg := validWireGuardConfig(t)
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			if tt.setup != nil {
				tt.setup(t)
			}
			err := NewLinuxOps(nil).EnsureWireGuard(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("EnsureWireGuard error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestEnsureWireGuardLink(t *testing.T) {
	t.Run("existing wireguard", func(t *testing.T) {
		restoreNetlink := stubNetlink(t)
		defer restoreNetlink()
		link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "fdwg-test"}}
		netlinkLinkByName = func(string) (netlink.Link, error) {
			return link, nil
		}
		got, err := ensureWireGuardLink("fdwg-test")
		if err != nil {
			t.Fatalf("ensureWireGuardLink: %v", err)
		}
		if got != link {
			t.Fatal("ensureWireGuardLink did not reuse existing WireGuard link")
		}
	})

	t.Run("existing wrong type", func(t *testing.T) {
		restoreNetlink := stubNetlink(t)
		defer restoreNetlink()
		netlinkLinkByName = func(string) (netlink.Link, error) {
			return &netlink.Dummy{}, nil
		}
		errLink, err := ensureWireGuardLink("fdwg-test")
		if err == nil || errLink != nil || !strings.Contains(err.Error(), "want wireguard") {
			t.Fatalf("ensureWireGuardLink = %#v, %v; want wrong-type error", errLink, err)
		}
	})

	t.Run("create missing", func(t *testing.T) {
		restoreNetlink := stubNetlink(t)
		defer restoreNetlink()
		var created netlink.Link
		calls := 0
		netlinkLinkByName = func(name string) (netlink.Link, error) {
			calls++
			if calls == 1 {
				return nil, netlink.LinkNotFoundError{}
			}
			if name != "fdwg-test" {
				t.Fatalf("LinkByName(%q), want fdwg-test", name)
			}
			return created, nil
		}
		netlinkLinkAdd = func(link netlink.Link) error {
			created = link
			return nil
		}
		got, err := ensureWireGuardLink("fdwg-test")
		if err != nil {
			t.Fatalf("ensureWireGuardLink: %v", err)
		}
		if got == nil || got.Type() != "wireguard" {
			t.Fatalf("created link = %#v", got)
		}
		if got.Attrs().Name != "fdwg-test" {
			t.Fatalf("created link name = %q, want fdwg-test", got.Attrs().Name)
		}
	})
}

func stubNetlink(t *testing.T) func() {
	t.Helper()
	oldLinkByName := netlinkLinkByName
	oldLinkAdd := netlinkLinkAdd
	oldAddrReplace := netlinkAddrReplace
	oldLinkSetUp := netlinkLinkSetUp
	netlinkLinkByName = func(string) (netlink.Link, error) {
		return nil, errors.New("unexpected LinkByName call")
	}
	netlinkLinkAdd = func(netlink.Link) error {
		return errors.New("unexpected LinkAdd call")
	}
	netlinkAddrReplace = func(netlink.Link, *netlink.Addr) error {
		return nil
	}
	netlinkLinkSetUp = func(netlink.Link) error {
		return nil
	}
	return func() {
		netlinkLinkByName = oldLinkByName
		netlinkLinkAdd = oldLinkAdd
		netlinkAddrReplace = oldAddrReplace
		netlinkLinkSetUp = oldLinkSetUp
	}
}

func stubWG(t *testing.T) func() {
	t.Helper()
	oldWGCtrlNew := wgctrlNew
	wgctrlNew = func() (wgClient, error) {
		return &fakeWGClient{}, nil
	}
	return func() {
		wgctrlNew = oldWGCtrlNew
	}
}

func stubRunCommand(t *testing.T) func() {
	t.Helper()
	oldRunCommand := runCommand
	runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("unexpected command")
	}
	return func() {
		runCommand = oldRunCommand
	}
}

func validWireGuardConfig(t *testing.T) config.WireGuardConfig {
	t.Helper()
	peerPrivateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return config.WireGuardConfig{
		Interface:      "fdwg-test",
		ListenPort:     51820,
		Address:        "fd7a:115c:a1e1::1/64",
		PrivateKeyFile: filepath.Join(t.TempDir(), "wg.key"),
		Peers: []config.WGPeer{
			{
				Name:       "alice",
				PublicKey:  peerPrivateKey.PublicKey().String(),
				AllowedIPs: []string{"fd7a:115c:a1e1::2/128", "fd7a:115c:a1e0::/64"},
			},
		},
	}
}

type fakeWGClient struct {
	device       string
	config       wgtypes.Config
	configureErr error
	closed       bool
}

func (c *fakeWGClient) ConfigureDevice(name string, cfg wgtypes.Config) error {
	c.device = name
	c.config = cfg
	return c.configureErr
}

func (c *fakeWGClient) Close() error {
	c.closed = true
	return nil
}

var _ wgClient = (*fakeWGClient)(nil)
