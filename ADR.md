# firedoze Architecture Decision Record

Status: draft

Project name: firedoze

License: MIT

## Purpose

firedoze is a Go daemon for shared, persistent development environments backed by Firecracker VMs.

The model is "create and forget": users can create many disposable computers, let inactive ones sleep, and care only about the ones they are actively using. Sleeping VMs should consume storage only. Production usage is explicitly out of scope.

## Primary Use Case

firedoze is for shared dev environments, not production workloads.

Security matters, but reliability and high availability do not. The environment is intentionally shared. If a team wants stronger isolation, they should run a separate firedoze instance.

## Non-Goals

- Production support.
- Multi-node operation.
- Host clustering, scheduling, migration, or distributed state.
- User accounts, teams, ACLs, or per-user authorization.
- Strong tenant isolation between users.
- Public SSH access.
- Dynamic public DNS management.
- General raw TCP ingress.
- Portable snapshot import/export bundles in v1.

## Target Platforms

The intended real deployment target is a single Linux cloud VM that supports nested virtualization.

AWS is the primary target for the intended deployment model. As of February 2026, EC2 supports nested virtualization on virtual instances in the C8i, M8i, and R8i families. firedoze should assume KVM availability for Firecracker.

DigitalOcean Droplets are acceptable for cheaper initial development work, subject to nested virtualization behavior and performance caveats.

The host OS should be any modern Linux distribution with KVM, kernel WireGuard, and required networking support. Ubuntu-specific assumptions should be kept small.

## Single-Node Scope

firedoze is deliberately single-node only.

The daemon runs on one sufficiently large machine. There is no host pool, scheduler, migration story, or distributed database.

## Process Model

v1 will run as a single root daemon.

This is acceptable for the dev-only threat model and keeps installation/debugging simple. Privileged operations should still be isolated behind an internal Go interface, for example `HostOps`, so a future version can move TAP, route, Firecracker, and filesystem operations into a privileged helper.

## Packaging

firedoze should eventually be packaged as a systemd service.

Likely layout:

- Config: `/etc/firedoze`
- State: `/var/lib/firedoze`
- Logs: journald

## Management Security

WireGuard is the only security layer for the management plane.

There are no firedoze user accounts. Anyone with WireGuard access is trusted inside that firedoze instance.

The management HTTP API must listen only on the WireGuard interface. There must be no localhost, public-interface, or other escape-hatch listener for the management API.

## WireGuard

The daemon should create and manage a simple WireGuard interface itself.

v1 should use kernel WireGuard via Go libraries, not an embedded userspace WireGuard implementation.

Expected libraries:

- `wgctrl` for WireGuard configuration.
- `vishvananda/netlink` or equivalent for interface/address setup.

The host must have Linux kernel WireGuard support.

Peer definitions are static in daemon config for v1. Adding a developer may require editing config and restarting the daemon.

Example config shape:

```toml
[wireguard]
interface = "fdwg0"
listen_port = 51820
address = "10.77.0.1/24"
private_key_file = "/etc/firedoze/wg.key"

[[wireguard.peers]]
name = "alice-laptop"
public_key = "..."
allowed_ip = "10.77.0.2/32"
```

firedoze should make WireGuard easy by generating sample peer configs.

## Host Firewall

The daemon does not manage host firewall rules or cloud security groups in v1.

Required firewall/security-group setup should be documented instead.

## API Style

The management API is command-oriented HTTP, optimized for `curl`.

JSON request bodies are acceptable for commands with more complex input.

The API is experimental in early versions and may change freely.

The API should expose a simple help/landing endpoint with available commands and example `curl` invocations.

API responses should optimize usability and include ready-to-run commands where useful, such as:

```text
ssh ubuntu@myvm.dev.example.com
```

## Metadata

Metadata is stored in local SQLite.

Use the Go SQLite library:

```text
github.com/mattn/go-sqlite3
```

This means firedoze requires cgo and a C compiler for builds. That is acceptable because the practical target is x86_64 Linux.

No external database is used.

## VM Identity

VM names are globally unique and map one-to-one with default virtual hostnames.

VM names must be DNS-safe: lowercase letters, numbers, and hyphens.

Every VM always gets its default hostname:

```text
{vm}.{base_domain}
```

The VM name reserves that hostname. No separate route/alias can use another VM's name.

## DNS

Public DNS is configured by the admin as a wildcard record pointing to the firedoze host/proxy:

```text
*.dev.example.com -> firedoze host public IP
```

firedoze does not dynamically manage public DNS.

firedoze should include a minimal authoritative DNS responder bound only to the WireGuard interface.

The WireGuard DNS responder is only for SSH usability. It answers VM default hostnames with VM private IPs:

```text
myvm.dev.example.com -> VM private IP
```

It should not recurse, forward, or manage public DNS. It should answer only the configured base domain and only for VM default hostnames. Public HTTPS aliases do not need split-horizon answers in v1.

Likely DNS library:

```text
github.com/miekg/dns
```

WireGuard peer configs should set the firedoze WireGuard IP as DNS where practical.

## VM Networking

v1 uses private VM networking only.

Each VM gets a private IP on a host-managed VM subnet. There is no per-VM public IPv6 requirement in v1.

WireGuard clients should be able to route directly to VM private IPs for SSH:

```text
laptop -> WireGuard -> firedoze host -> VM private IP:22
```

There is no SSH jump service and no public SSH.

VM private IPs do not need to be stable across sleep/resume or clone operations, but API responses should expose them for debugging.

## SSH

SSH is private over WireGuard only.

Every new VM receives a shared admin-configured authorized-keys list. There is no per-VM or per-user SSH authorization model in v1.

The default user is expected to be the base image's normal cloud user, likely `ubuntu`.

The preferred user experience is:

```text
ssh ubuntu@myvm.dev.example.com
```

This relies on the WireGuard-only DNS responder resolving VM hostnames to private VM IPs for connected peers.

## Public HTTPS

Public web access is provided through embedded Caddy.

Caddy is embedded as a Go library, not run as an external process.

Auto HTTPS is crucial. v1 should use normal per-host ACME HTTP-01 certificates. Wildcard certificates are not required because wildcard DNS provider automation is out of scope.

Caddy listens publicly on ports 80 and 443. Public HTTPS routes are unauthenticated by default in v1.

Additional auth layers, such as basic auth, forward auth, bearer tokens, or IP allowlists, can be added later without changing the core model.

## HTTPS Routes

The base domain is configured once, for example:

```text
dev.example.com
```

Every VM gets a default HTTPS route:

```text
https://{vm}.dev.example.com -> http://{vm_private_ip}:{default_http_port}
```

The default HTTP port is configurable globally, with `8080` as the default.

Additional public HTTPS aliases are explicit API-managed mappings:

```text
{route}.dev.example.com -> vm:port
```

Route names are globally unique DNS-safe labels.

Routes may target any guest TCP port number, but the target service must speak HTTP. WebSockets should work through Caddy. Raw TCP forwarding and TLS passthrough are not v1 features.

Caddy proxies to the VM private IP over the host-to-VM private network.

## Wake on HTTPS

If a sleeping VM receives an HTTPS request for one of its routes, firedoze should wake the VM before proxying.

The request should wait. If wake takes too long and the client times out, the user can retry.

It is preferable to overwake rather than underwake. ACME challenge behavior does not need special underwake optimization in v1.

## Idle Detection and Sleep

VMs automatically sleep after inactivity.

The idle threshold is configurable globally, with a per-VM override.

Activity includes:

- Public HTTPS traffic.
- SSH traffic over WireGuard.
- Other network traffic to/from the VM.

No heartbeat mechanism is planned.

Sleeping must preserve exact runtime state. On wake, the VM should resume exactly where it left off.

This requires Firecracker memory snapshot state, disk state, and VM metadata to remain consistent.

## Snapshot Model

Snapshots are named frozen computers.

Users can save a running VM as a named snapshot. Snapshot names are global and may be freer than VM names, but duplicate names fail.

Snapshots include all state:

- Memory state.
- Disk state.
- Device/VM metadata required for exact restore.
- Base image/kernel/runtime lineage metadata.

Restoring a snapshot should create a new VM by default rather than overwriting an existing VM.

Example model:

```text
snapshot "base-node-app" -> clone as VM "alice-node-app"
```

A destructive in-place restore may be added later, but is not the v1 default.

When cloning/restoring, firedoze must rewrite guest identity so multiple VMs do not share properties such as hostname, machine-id, SSH host keys, or network identity.

## Base Image and Kernel

The base image is non-configurable in v1.

Default guest OS should be Ubuntu LTS cloud image or equivalent. The VM should feel like a normal human-usable Linux computer, not a minimal appliance.

The base image is used only for fresh VMs. Existing VMs and snapshots do not track base image updates.

Kernel/runtime changes apply only to newly created VMs.

Snapshot metadata should record lineage, for example:

```json
{
  "base_image_id": "ubuntu-lts-firecracker-v1",
  "kernel_id": "linux-fc-v1",
  "created_with_orchestrator_version": "0.1.0"
}
```

Old snapshots should restore if compatible. If incompatible, the API should fail clearly or warn when appropriate. No attempt should be made to rebase exact VM snapshots onto newer base images.

## Storage

v1 uses plain image files on local disk.

No ZFS, btrfs, LVM thin provisioning, or qcow2 overlay requirement in v1.

Space efficiency is not a primary goal. Storage is assumed cheap enough for initial use, and old images/snapshots can be moved manually to slower storage by admins.

firedoze v1 does not manage archival storage or automatic restore from archived files. If files are missing because an admin moved them, commands should fail clearly.

Long-term, archive/restore integration may be added.

## Resource Management

Users are trusted to size VMs sensibly.

Per-VM memory, vCPU, and disk size are configurable.

No hard maximums are required in daemon config for v1.

firedoze should expose simple aggregate resource usage in the API, but does not enforce resource limits.

## Caddy and ACME Assumptions

For public HTTPS to work:

- Public wildcard DNS must point to the host.
- Ports 80 and 443 must reach Caddy.
- Caddy will obtain per-host certificates for concrete names as they are used.

No DNS-01 provider integration is required in v1.

## Open Implementation Notes

Exact Firecracker snapshot behavior needs careful implementation because memory state, disk state, network identity, and guest identity must line up.

Wake-on-request through embedded Caddy needs a mechanism for Caddy route handling to block, wake the target VM, and then proxy to the now-running private IP.

Idle detection should likely start with host-visible network counters per VM TAP interface. More complex eBPF or conntrack approaches can come later if needed.

Host firewall/security group requirements must be documented before real deployment, especially:

- Public 80/443 to host.
- WireGuard UDP listen port to host.
- No public SSH to VMs.
- Management API bound only to WireGuard.

## Initial Build Order

1. Create repository and ADR.
2. Bootstrap Go daemon with config and SQLite metadata.
3. Manage kernel WireGuard interface from config.
4. Expose command-oriented HTTP API only on WireGuard.
5. Start/stop one Firecracker VM from the fixed base image.
6. Inject shared authorized keys and make SSH work over WireGuard/private DNS.
7. Embed Caddy and serve default VM route.
8. Add explicit HTTPS route aliases.
9. Add named exact-state snapshots.
10. Add clone-from-snapshot with identity rewrite.
11. Add idle detection and exact sleep/resume.
12. Add usability polish: help endpoint, generated WG configs, ready-to-run SSH/curl outputs.
