# Client WireGuard Broker

This note explains why the `firedoze` client has a local WireGuard broker, how
that broker works, and how SSH is layered on top of it.

## The Problem

Firedoze lets users run client commands without manually bringing up an
operating-system WireGuard tunnel. The imported client config contains:

- the client's WireGuard private key,
- the client's assigned WireGuard address,
- the server public key,
- the server UDP endpoint,
- the routes needed to reach the Firedoze API and VM private network.

A naive implementation would let every `firedoze` process create its own
userspace WireGuard client. That works for one short command, but it breaks down
when multiple long-lived commands run at the same time.

The conflict is at the WireGuard peer identity layer. The Firedoze server has
one peer entry for the laptop's public key. If two local processes both create a
WireGuard client with the same private key, the server does not see two
independent sessions. It sees one peer whose UDP endpoint keeps changing.

For example:

```text
terminal 1: firedoze ssh vm-a
  -> userspace WireGuard client A
  -> laptop peer key, UDP socket A

terminal 2: firedoze ssh vm-b
  -> userspace WireGuard client B
  -> same laptop peer key, UDP socket B
```

WireGuard supports endpoint roaming. When a valid encrypted packet arrives from
a new source address/port for a peer, the peer's endpoint is updated. This is
correct behavior for laptops moving between networks, but bad when two local
processes pretend to be the same peer. The server can alternate between socket A
and socket B, so one SSH session can steal the endpoint from another.

The visible symptom is an SSH session that hangs, especially after it has been
idle and another `firedoze` command or keepalive packet has moved the peer
endpoint.

## The Broker

The broker makes the laptop's WireGuard peer single-owned again.

Instead of every `firedoze` process creating its own WireGuard tunnel, the first
command that needs a tunnel starts a local broker process:

```sh
firedoze -server <name> tunnel-daemon
```

This is not a separate executable. It is a hidden mode of the same `firedoze`
binary, launched via `os.Executable()`.

The broker:

- owns the single userspace WireGuard client for the selected server,
- listens on a local Unix socket,
- accepts simple local `CONNECT <network> <address>` requests,
- opens those connections through the one WireGuard tunnel,
- pipes bytes between the local client process and the remote private address,
- exits after it has no active connections and has been idle for the broker idle
  timeout.

The socket path is derived from the selected server profile and lives under:

```text
$XDG_RUNTIME_DIR/firedoze/
```

or the OS temp directory if `XDG_RUNTIME_DIR` is unset.

With the broker, concurrent commands look like this:

```text
terminal 1: firedoze ssh vm-a
  -> local broker socket
     -> one userspace WireGuard tunnel

terminal 2: firedoze ssh vm-b
  -> same local broker socket
     -> same userspace WireGuard tunnel
```

The Firedoze server now sees one stable endpoint for the laptop peer, even while
multiple SSH sessions are active.

## API Calls

When a stored server profile has WireGuard details, `newClientForServer` starts
or reuses the broker before constructing the HTTP client. The HTTP transport's
`DialContext` is set to a `clientwg.BrokerDialer`, so API requests are dialed
through the broker instead of the OS network stack.

The API URL still points at the server's WireGuard address, for example:

```text
http://[fdxx:...::1]:8081
```

but no OS route to that address is required. The broker supplies the route by
dialing over userspace WireGuard.

If the user supplies `FIREDOZE_API` or `-api`, that bypasses stored server
profiles. In that mode the client uses normal HTTP dialing and assumes the user
has provided equivalent network reachability.

## SSH Flow

Firedoze still uses ordinary OpenSSH for interactive sessions. The client does
not implement SSH itself.

For an imported WireGuard server profile, `firedoze ssh <vm>` builds an OpenSSH
command that uses `firedoze ssh-proxy <vm>` as a `ProxyCommand`:

```text
ssh ... -o ProxyCommand="/path/to/firedoze -server <name> ssh-proxy <vm>" ubuntu@<vm>
```

The flow is:

```text
firedoze ssh vm-a
  -> looks up and starts/wakes vm-a through the API
  -> launches OpenSSH
     -> OpenSSH launches firedoze ssh-proxy vm-a
        -> ssh-proxy connects to the local broker
           -> broker connects to [vm-a-private-ip]:22 over WireGuard
              -> OpenSSH speaks normal SSH to guest sshd
```

`ssh-proxy` does not terminate SSH and does not authenticate the user. It is only
a byte pipe from OpenSSH to the VM's private port 22. SSH encryption and protocol
behavior remain OpenSSH-to-guest-`sshd`.

The same transport path is used by:

- `firedoze ssh <vm>`
- `firedoze exec <vm> -- ...`
- `firedoze cp ...`
- manual OpenSSH config using `firedoze ssh-proxy <vm>` as `ProxyCommand`

## Lifetime Rules

The broker idle timeout only applies when there are no active broker
connections. An open SSH session counts as active even if the user is not typing,
so the broker should not exit underneath an idle SSH session.

If the broker is gone, the next `firedoze` command starts a new one. Stale socket
files are tolerated by probing the socket first; if no broker responds, the
client starts a fresh broker.

## Why Not A Goroutine?

The broker cannot just be a goroutine because separate terminal commands are
separate OS processes. A goroutine would only be shared within one `firedoze`
process and would disappear when that command exits. The Unix socket gives all
client processes on the laptop a single local rendezvous point for the selected
server profile.

## Tradeoff

This broker exists because Firedoze hides WireGuard tunnel management from the
user. If users brought up an OS-level WireGuard tunnel with `wg-quick`, the OS
would already provide one stable tunnel shared by all processes, and the broker
would not be necessary.

The embedded-client model improves the user workflow, but it means Firedoze owns
local tunnel lifecycle and concurrency. The broker is the small local component
that makes that model behave like ordinary SSH over a stable network.
