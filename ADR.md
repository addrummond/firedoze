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

The intended real deployment target is a single Linux server running on dedicated hardware, or in a cloud VM that supports nested virtualization.

AWS compatibility is required for the intended deployment model. As of February 2026, EC2 supports nested virtualization on virtual instances in the C8i, M8i, and R8i families. firedoze should assume KVM availability for Firecracker.

DigitalOcean Droplets may be useful for cheap initial development work, subject to nested virtualization behavior and performance caveats.

The host OS should be any modern Linux distribution with KVM, kernel WireGuard, and required networking support. Initial building and testing will be done on Ubuntu, but Ubuntu-specific assumptions should be kept small.

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
allowed_ips = ["10.77.0.2/32"]
```

firedoze should make WireGuard easy by generating sample peer configs. The v1 HTTP API exposes configured peers and a `wg-quick` config template for each peer. The generated config includes the server public key, peer address, DNS, management and VM subnet routes, and a `<client-private-key>` placeholder. firedoze does not manage developer client private keys.

## Host Firewall

The daemon does not manage host firewall rules or cloud security groups in v1.

Required firewall/security-group setup should be documented instead.

## API And Client Style

The management API is a WireGuard-only HTTP API with JSON request and response bodies.

The API is primarily a machine interface for the `firedoze` client command, not a human-first curl interface. It should be regular, small, and easy for scripts to consume, but it does not need to include ready-to-run shell commands, curl examples, or tutorial-style response bodies.

The API is experimental in early versions and may change freely. Compatibility should favor the `firedoze` client UX over preserving raw HTTP ergonomics.

The root endpoint returns a compact JSON resource index. Errors are JSON objects. Operational endpoints return structured resources such as VMs, routes, snapshots, WireGuard peers, and WireGuard peer config templates. The WireGuard peer config endpoint returns the generated `wg-quick` config as a JSON string field, not as `text/plain`.

The primary human interface is a separate `firedoze` client command that runs on a developer laptop and talks to the WireGuard-only HTTP API. The `firedozed` binary is the privileged host daemon.

The client should provide the friendly operational surface:

- `firedoze vm list`
- `firedoze vm inspect <vm>`
- `firedoze vm create/start/sleep/stop/delete/settings`
- `firedoze ssh <vm>`
- `firedoze snapshot list/inspect/save/restore/delete`
- `firedoze route ...`

For scripts that need exact API responses, the client supports `--json`. Human-readable client output can include convenience commands such as `firedoze ssh <vm>`, public URLs, and runtime/status columns, but those are client presentation choices rather than command strings embedded in the API.

The API may still expose useful derived fields, such as default hostnames, public URLs, private IPs, and SSH targets, when those fields are part of the resource model rather than follow-up command guidance.

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

DNS library:

```text
codeberg.org/miekg/dns
```

WireGuard peer configs should set the firedoze WireGuard IP as DNS where practical.

The v1 daemon starts UDP and TCP DNS listeners on the WireGuard address. It answers A queries for `{vm}.{base_domain}` with the VM private IP and does not recurse or forward.

WireGuard peer configuration must include routes for both the WireGuard management address and the VM private subnet. The config format should support multiple peer allowed IP CIDRs.

## VM Networking

v1 uses private VM networking only.

Each VM gets a private IP on a host-managed VM subnet. There is no per-VM public IPv6 requirement in v1.

WireGuard clients should be able to route directly to VM private IPs for SSH:

```text
laptop -> WireGuard -> firedoze host -> VM private IP:22
```

There is no SSH jump service and no public SSH.

VM private IPs do not need to be stable across sleep/resume or clone operations, but API responses should expose them for debugging.

The initial Firecracker implementation uses one TAP device per VM, created and configured through Linux netlink rather than shelling out to `ip`. The quickstart guest image derives its guest IP from a `06:00:*` MAC address and configures a `/30`; firedoze currently matches that behavior by assigning the host side as `guest_ip - 1` and the guest as `guest_ip`.

For v1, firedoze applies host-side SNAT/MASQUERADE from the WireGuard subnet to each VM TAP network. This avoids requiring the guest to know routes back to the WireGuard management subnet.

## SSH

SSH is private over WireGuard only.

There is no per-VM or per-user SSH authorization model in v1.

The default user is expected to be the base image's normal user. For the Ubuntu base image, this is `ubuntu`.

The generated Ubuntu base image configures `sshd` for passwordless `ubuntu` login over the private VM network. This deliberately treats SSH as a terminal and file-transfer transport, not as a second identity layer. WireGuard is the security boundary.

The preferred user experience is:

```text
ssh ubuntu@myvm.dev.example.com
```

This relies on the WireGuard-only DNS responder resolving VM hostnames to private VM IPs for connected peers.

Sleeping VMs should wake from direct SSH/network activity. The v1 implementation redirects WireGuard TCP/22 traffic for VM private IPs into a daemon-side SSH wake proxy. The proxy identifies the original destination VM, starts or resumes it, waits for guest SSH, then relays the original connection.

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

Caddy routes public hosts to firedoze's internal wake proxy. The wake proxy resumes sleeping VMs when needed, then proxies to the VM private IP over the host-to-VM private network.

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

The implementation exposes a manual exact sleep/resume primitive: `POST /vms/{name}/sleep` saves Firecracker memory and VM state into the VM's state directory, stops the Firecracker process, and marks the VM `sleeping`; `POST /vms/{name}/start` loads that state back into Firecracker.

Automatic idle detection is layered on top of that primitive. The daemon samples host TAP interface RX/TX byte counters for running VMs. If a VM has no byte counter movement for its configured idle threshold, the monitor calls the same exact sleep path. `idle.default_sleep_after_seconds` is the global threshold, `idle.check_interval_seconds` controls sample frequency, and `idle_sleep_after_seconds` on a VM can override the global threshold.

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

The first restore implementation creates a stopped VM from the snapshot disk copy, rewrites guest identity, and boots it normally when started. The Firecracker memory and VM state files are saved and tracked, but are not yet loaded for clone restore because exact memory restore conflicts with changing guest identity. Exact memory resume remains part of the later sleep/resume path.

## Base Image and Kernel

The base image is non-configurable in v1.

Default guest OS should be Ubuntu LTS cloud image or equivalent. The VM should feel like a normal human-usable Linux computer, not a minimal appliance.

The base image should be built from pinned Ubuntu cloud image artifacts rather than from the minimal Firecracker quickstart image. firedoze keeps using a plain ext4 root filesystem as `/dev/vda`, so the image builder turns the root tarball into `rootfs.ext4` and downloads the matching published `vmlinux.bin` and `initrd.img` boot artifacts. Default artifacts are version-pinned in source and SHA-256 verified; overrides must provide checksums or explicitly opt into an insecure build.

The image builder should be host-portable for development. v1 uses a native Go builder so the same script can run on macOS or Linux without Docker, Podman, root, mounting, or host ext4 filesystem support.

The guest image carries a tiny firedoze network service. At boot, it reads the Firecracker MAC address in the `06:00:<guest-ip-octets>` convention and configures `eth0` with the derived `/30` guest IP and default route through `guest_ip - 1`.

The base image is used only for fresh VMs. Existing VMs and snapshots do not change when the configured base image changes.

Kernel/runtime changes apply only to newly created VMs.

New VMs store base image lineage metadata at creation time. Snapshots copy that metadata from the source VM. firedoze stores all available metadata rather than choosing a single identifier: parsed `manifest.txt` fields, artifact path, basename, SHA-256, size, and modification time for the root filesystem, kernel, and initrd where present.

The compact `base_image_id` and `kernel_id` fields are summary identifiers for easy display and compatibility checks; the full `base_image_metadata` object is the source of detail.

Example metadata:

```json
{
  "base_image_id": "sha256-of-rootfs",
  "kernel_id": "sha256-of-kernel",
  "base_image_metadata": {
    "rootfs": {
      "path": "/var/lib/firedoze/base/rootfs.ext4",
      "basename": "rootfs.ext4",
      "sha256": "sha256-of-rootfs",
      "size": 8589934592,
      "mod_time": "2026-05-02T12:00:00Z"
    },
    "kernel": {
      "path": "/var/lib/firedoze/base/vmlinux.bin",
      "basename": "vmlinux.bin",
      "sha256": "sha256-of-kernel"
    },
    "initrd": {
      "path": "/var/lib/firedoze/base/initrd.img",
      "basename": "initrd.img",
      "sha256": "sha256-of-initrd"
    },
    "manifest": {
      "release": "noble",
      "arch": "amd64",
      "builder": "firedoze-image native-go"
    }
  }
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

The embedded Caddy integration always runs with Auto HTTPS enabled. firedoze can configure the HTTP and HTTPS listener ports, but there is no separate insecure public routing mode. The normal deployment path is per-host Auto HTTPS on ports 80/443.

## Open Implementation Notes

Exact Firecracker snapshot behavior needs careful implementation because memory state, disk state, network identity, and guest identity must line up.

Wake-on-request through embedded Caddy needs a mechanism for Caddy route handling to block, wake the target VM, and then proxy to the now-running private IP.

Idle detection should likely start with host-visible network counters per VM TAP interface. More complex eBPF or conntrack approaches can come later if needed.

The initial Firecracker integration uses `firecracker --api-sock ... --config-file ...`, which starts the microVM immediately from the config file. In this launch mode, firedoze must not send a separate `InstanceStart` action. Later snapshot/restore work may require moving to a more explicit API-driven configuration flow.

Host firewall/security group requirements must be documented before real deployment, especially:

- Public 80/443 to host.
- WireGuard UDP listen port to host.
- No public SSH to VMs.
- Management API bound only to WireGuard.

## Initial Build Order

1. Create repository and ADR.
2. Bootstrap Go daemon with config and SQLite metadata.
3. Manage kernel WireGuard interface from config.
4. Expose JSON HTTP API only on WireGuard.
5. Start/stop one Firecracker VM from the fixed base image.
6. Make passwordless guest SSH work over WireGuard/private DNS.
7. Embed Caddy and serve default VM route.
8. Add explicit HTTPS route aliases.
9. Add named exact-state snapshots.
10. Add clone-from-snapshot with identity rewrite.
11. Add idle detection and exact sleep/resume.
12. Add usability polish: generated WG configs, client command, friendly VM list output, and ready-to-run SSH/public URL output in the client.
