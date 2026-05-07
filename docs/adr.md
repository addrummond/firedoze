# Firedoze Architecture Decision Record

Status: draft

Project name: Firedoze

License: MIT

## Purpose

Firedoze is a Go daemon for shared, persistent development environments backed by Firecracker VMs.

The model is "create and forget": users can create many disposable computers, let inactive ones sleep, and care only about the ones they are actively using. Sleeping VMs should consume storage only. Production usage is explicitly out of scope.

## Primary Use Case

Firedoze is for shared dev environments, not production workloads.

Security matters, but reliability and high availability do not. The environment is intentionally shared. If a team wants stronger isolation, they should run a separate Firedoze instance.

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

The intended real deployment target is a single Linux server running on dedicated hardware, or in a cloud VM that supports nested virtualization. Firedoze should assume KVM availability for Firecracker.

VPS instances may be useful for cheap initial development work, subject to nested virtualization behavior and performance caveats.

The host OS should be any modern Linux distribution with KVM, kernel WireGuard, and required networking support. Initial building and testing will be done on Ubuntu, but Ubuntu-specific assumptions should be kept small.

## Single-Node Scope

Firedoze is deliberately single-node only.

The daemon runs on one sufficiently large machine. There is no host pool, scheduler, migration story, or distributed database.

## Process Model

v1 will run as a single root daemon.

This is acceptable for the dev-only threat model and keeps installation/debugging simple. Privileged operations should still be isolated behind an internal Go interface, for example `HostOps`, so a future version can move TAP, route, Firecracker, and filesystem operations into a privileged helper.

## Packaging

Firedoze should eventually be packaged as a systemd service.

Likely layout:

- Config: `/etc/firedoze`
- State: `/var/lib/firedoze`
- Logs: journald

The systemd unit uses readiness notification and a watchdog, but not socket
activation. Socket activation would only protect a small part of daemon restart
behavior because Firedoze owns several listeners, embeds Caddy, and currently
sleeps running VMs during shutdown before waking them on the next start. If
less disruptive upgrades become important, the better direction is adopting
already-running Firecracker processes across daemon restarts.

## Management Security

WireGuard is the only security layer for the management plane.

There are no Firedoze user accounts. Anyone with WireGuard access is trusted inside that Firedoze instance.

The management HTTP API must listen only on the WireGuard interface. There must be no localhost, public-interface, or other escape-hatch listener for the management API.

## WireGuard

The daemon should create and manage a simple host WireGuard interface itself.

The host side should use kernel WireGuard via Go libraries, not an embedded userspace WireGuard implementation.

Expected libraries:

- `wgctrl` for WireGuard configuration.
- `vishvananda/netlink` or equivalent for interface/address setup.

The host must have Linux kernel WireGuard support. The laptop client may use a local userspace WireGuard broker so users do not have to manage an operating-system tunnel for normal Firedoze commands.

Peer definitions are static in daemon config for v1. Adding a developer may require editing config and restarting the daemon.

Example config shape:

```toml
[wireguard]
interface = "fdwg0"
listen_port = 51820
address = "fd7a:115c:a1e1::1/64"
private_key_file = "/etc/firedoze/wg.key"

[[wireguard.peers]]
name = "alice-laptop"
public_key = "..."
allowed_ips = ["fd7a:115c:a1e1::2/128"]
```

Firedoze should make WireGuard easy without asking admins to generate or see developer private keys. A developer runs `firedoze server request <peer-name>` locally, which creates a client WireGuard key pair and stores the private key in the local client config. The developer sends only the public key to the admin. The admin runs `firedozed -wg-add-peer <peer-name> <client-public-key>`, which updates the host config and prints a Firedoze client import TOML containing server public key, endpoint, API URL, peer address, and allowed routes. The generated import config intentionally does not include a local client-side server profile name; by default, `firedoze server import <file> -default` uses the import filename basename as the server profile name, and still matches the import to the locally stored pending private key by public key.

## Host Firewall

The daemon can manage a small host firewall boundary for the private IPv6 VM
subnet. It allows WireGuard clients, VM-to-VM traffic, VM outbound internet
traffic, host-local proxy traffic, and established replies. It drops new traffic
from ordinary LAN/public interfaces into the VM subnet. Because VM addresses are
private IPv6 ULA addresses, it also installs IPv6 masquerading for outbound VM
traffic.

Host firewalling is configured explicitly:

```toml
[host_firewall]
enabled = true
backend = "ip6tables"
```

When `enabled = true`, `backend` is required. There are no `auto` or `none`
backend values. Only `ip6tables` is implemented initially; future versions may
add an `nftables` backend for RHEL/CentOS-style hosts.

Cloud firewall/security-group setup remains outside Firedoze. Operators still
need to expose only the intended public ports to the host.

## API And Client Style

The management API is a WireGuard-only HTTP API with JSON request and response bodies.

The API is primarily a machine interface for the `firedoze` client command, not a human-first curl interface. It should be regular, small, and easy for scripts to consume, but it does not need to include ready-to-run shell commands, curl examples, or tutorial-style response bodies.

The API is experimental in early versions and may change freely. Compatibility should favor the `firedoze` client UX over preserving raw HTTP ergonomics.

The root endpoint returns a compact JSON resource index. Errors are JSON objects. Operational endpoints return structured resources such as VMs, routes, snapshots, WireGuard peers, and WireGuard peer import configs. The WireGuard peer config endpoint returns the generated Firedoze client import config as a JSON string field, not as `text/plain`.

The primary human interface is a separate `firedoze` client command that runs on a developer laptop and talks to the WireGuard-only HTTP API. The `firedozed` binary is the privileged host daemon.

The client stores named server profiles in `~/.config/firedoze/config.toml`. Imported profiles can include the client-side WireGuard private key and server routing details. When those details are present, normal `firedoze` commands start or reuse a local per-server userspace WireGuard broker for API calls and SSH proxying, so users do not need to run `wg-quick` for the common workflow. The `FIREDOZE_SERVER` and `-server` overrides select among stored profiles. The `FIREDOZE_API` and `-api` overrides bypass stored profiles and are useful for scripts or server-local debugging when equivalent network routing already exists.

The client should provide the friendly operational surface:

- `firedoze server request/import/add/list/use/current/remove`
- `firedoze vm list [-names] [name-glob...]`
- `firedoze vm inspect <vm>`
- `firedoze vm create/up/start/reboot/sleep/stop/delete/publish/hide/settings`
- `firedoze ssh <vm>`
- `firedoze snapshot list/inspect/save/restore/export/import/delete`
- `firedoze route ...`

For scripts that need exact API responses, the client supports `-json`. Human-readable client output can include convenience commands such as `firedoze ssh <vm>`, public URLs, and runtime/status columns, but those are client presentation choices rather than command strings embedded in the API.

The API may still expose useful derived fields, such as default hostnames, public URLs, private IPs, and SSH targets, when those fields are part of the resource model rather than follow-up command guidance.

## Metadata

Metadata is stored in local SQLite.

Use the Go SQLite library:

```text
modernc.org/sqlite
```

This keeps the metadata database local without requiring cgo or a C compiler for normal builds.

No external database is used.

## VM Identity

VM names are globally unique and map one-to-one with default virtual hostnames.

VM names must be DNS-safe: lowercase letters, numbers, and hyphens.

Every VM always gets its default hostname:

```text
{vm}.{base_domain}
```

The VM name reserves that hostname. No separate route/alias can use another VM's name.
Likewise, no VM can be created or restored with a name already used by a route alias.

## DNS

Public DNS is configured by the admin as a wildcard record pointing to the Firedoze host/proxy:

```text
*.dev.example.com -> Firedoze host public IP
```

Firedoze does not dynamically manage public DNS.

Firedoze does not run a private DNS resolver in v1. Direct VM SSH should use the `firedoze ssh <vm>` client command, which gets the VM private IP from the management API instead of relying on laptop DNS configuration.

WireGuard peer configuration must include routes for both the WireGuard management address and the VM private subnet. The config format should support multiple peer allowed IP CIDRs.

## VM Networking

v1 uses private IPv6 VM networking only.

Each VM gets a private IPv6 ULA address on a host-managed VM subnet. There is no per-VM public IPv6 requirement in v1.

WireGuard clients should be able to route directly to VM private IPs for SSH:

```text
laptop -> WireGuard -> Firedoze host -> VM private IP:22
```

There is no SSH jump service and no public SSH.

VM private IPs do not need to be stable across sleep/resume or clone operations, but API responses should expose them for debugging.

The Firecracker implementation uses one TAP device per VM, created and configured through Linux netlink rather than shelling out to `ip`. Each VM gets a `/127` host/guest point-to-point pair inside `vm_network.subnet`: the even address is assigned to the host TAP and the odd address is passed to the guest as a kernel argument.

The guest default route points at the host-side address for its `/127`. WireGuard clients receive IPv6 routes for both the management address and the VM subnet.

## SSH

SSH is private over WireGuard only.

There is no per-VM or per-user SSH authorization model in v1.

The default user is expected to be the base image's normal user. For the Ubuntu base image, this is `ubuntu`.

The generated Ubuntu base image configures `sshd` for passwordless `ubuntu` login over the private VM network. This deliberately treats SSH as a terminal and file-transfer transport, not as a second identity layer. WireGuard is the security boundary.

The preferred user experience is:

```text
firedoze ssh myvm
```

The client resolves the VM to its private IP through the management API before starting OpenSSH, so no client-side DNS setup is required.

The `firedoze ssh <vm>` client starts or resumes the VM when needed, waits for guest SSH, then execs OpenSSH against the VM private IPv6 address. Passive SSH wake-on-network is disabled for the IPv6-only VM network for now.

Standard OpenSSH tooling can use `firedoze ssh-proxy <vm>` as a `ProxyCommand`.
The proxy resolves the VM through the WireGuard-only management API, starts it
if needed, waits for guest SSH, then bridges stdin/stdout to the VM private
port 22. It does not terminate SSH or replace OpenSSH; it is just a local
connection helper. SSH config should use an absolute path to the trusted
`firedoze` binary.

Public HTTP wake is gated by a self-hosted CAPTCHA when the target VM is sleeping. After a browser completes the challenge, Firedoze sets a signed, host-scoped cookie and then allows that browser to wake the VM on future public HTTPS requests until the cookie expires. The cookie signing key is generated automatically under the Firedoze state directory and is not part of hand-written config.

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

Caddy routes public hosts to Firedoze's internal wake proxy. The wake proxy resumes sleeping VMs when needed, then proxies to the VM private IP over the host-to-VM private network.

## Wake on HTTPS

If a sleeping VM receives an HTTPS request for one of its routes, Firedoze should wake the VM before proxying.

The request should wait. If wake takes too long and the client times out, the user can retry.

It is preferable to overwake rather than underwake. ACME challenge behavior does not need special underwake optimization in v1.

## Idle Detection and Sleep

VMs automatically sleep after inactivity.

The idle threshold is configurable globally, with a per-VM override.

Activity includes:

- Public HTTPS traffic.
- Explicit client sessions such as `firedoze ssh`, `firedoze exec`, and `firedoze cp`.
- SSH proxy sessions opened by standard SSH tooling through the `firedoze ssh-proxy` command.

Firedoze host/guest control traffic, such as guest resource-monitor reports, must not count as VM activity.

Sleeping must preserve exact runtime state. On wake, the VM should resume exactly where it left off.

This requires Firecracker memory snapshot state, disk state, and VM metadata to remain consistent.

The implementation exposes a manual exact sleep/resume primitive: `POST /vms/{name}/sleep` saves Firecracker memory and VM state into the VM's state directory, stops the Firecracker process, and marks the VM `sleeping`; `POST /vms/{name}/start` loads that state back into Firecracker.

Automatic idle detection is layered on top of that primitive. The daemon tracks a stored last-activity timestamp for each running VM and updates it from meaningful user-facing paths, rather than raw TAP byte counters. If a VM has no recorded activity for its configured idle threshold, the monitor calls the same exact sleep path. `idle.default_sleep_after_seconds` is the global threshold, `idle.check_interval_seconds` controls sample frequency, and `idle_sleep_after_seconds` on a VM can override the global threshold.

## Snapshot Model

Snapshots are named cloneable disk images.

Users can save a stopped VM as a named snapshot. Snapshot names are global and may be freer than VM names, but duplicate names fail. Saving a snapshot from a running or sleeping VM is rejected because clone restore boots a new VM identity from the snapshot disk.

Snapshots include the cloneable VM state:

- Disk state.
- Base image/kernel/runtime lineage metadata.

Restoring a snapshot should create a new VM by default rather than overwriting an existing VM.

Example model:

```text
snapshot "base-node-app" -> clone as VM "alice-node-app"
```

A destructive in-place restore may be added later, but is not the v1 default.

When cloning/restoring, Firedoze must rewrite guest identity so multiple VMs do not share properties such as hostname, machine-id, SSH host keys, or network identity.

Restore creates a stopped VM from the snapshot disk copy, rewrites guest identity, and boots it normally when started. Exact Firecracker memory restore remains part of the sleep/resume path for the same VM, not the cloneable named snapshot model, because exact memory restore conflicts with changing guest identity.

Snapshots can be exported and imported as portable gzip tar bundles. A bundle contains `manifest.json` with snapshot lineage metadata and `rootfs.ext4` with the cloneable disk image. This is deliberately snapshot-only: Firedoze does not import or export base images through this path, and bundles do not contain exact sleep memory state.

## Base Image and Kernel

The base image is non-configurable in v1.

Default guest OS is the pinned Ubuntu 26.04 LTS cloud image. The VM should feel like a normal human-usable Linux computer, not a minimal appliance.

The base image should be built from pinned Ubuntu cloud image artifacts rather than from the minimal Firecracker quickstart image. Firedoze keeps using a plain ext4 root filesystem as `/dev/vda`, so the image builder turns the root tarball into `rootfs.ext4` and downloads the matching published `vmlinux.bin` and `initrd.img` boot artifacts. Artifact URLs and SHA-256 checksums are version-pinned in source; the builder has no release-selection option.

The image builder should be host-portable for development. v1 uses a native Go builder so the same script can run on macOS or Linux without Docker, Podman, root, mounting, or host ext4 filesystem support.

Container runtimes are not part of the Firedoze host model or image build
pipeline. Users can still install daemonless tools such as Podman, Buildah, and
crun inside a VM when a particular project benefits from containers.

The guest image carries a tiny Firedoze network service. At boot, it reads `firedoze.guest_ip`, `firedoze.host_ip`, and optional DNS kernel arguments, then configures `eth0` with the guest `/127` IPv6 address and default route through the host-side address.

The guest image includes `firedoze-stop`, which stops a VM from inside its own
shell. On x86_64 Firecracker this wraps `reboot`, because `reboot` exits the
microVM when the kernel is booted with `reboot=k`; Firedoze treats that process
exit as a stopped VM. In-guest `poweroff`, `halt`, and default `shutdown` are
not reliable VM stop signals on x86_64 Firecracker because the guest OS can halt
without terminating the VMM process. Users should prefer the client command
`firedoze vm stop <name>` or `firedoze-stop` inside the VM.

The base image is used only for fresh VMs. Existing VMs and snapshots do not change when the configured base image changes.

Kernel/runtime changes apply only to newly created VMs.

New VMs store base image lineage metadata at creation time. Snapshots copy that metadata from the source VM. Firedoze stores all available metadata rather than choosing a single identifier: parsed `manifest.txt` fields, artifact path, basename, SHA-256, size, and modification time for the root filesystem, kernel, and initrd where present.

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
      "release": "resolute",
      "ubuntu_version": "26.04",
      "arch": "amd64",
      "builder": "firedoze-image-builder native-go"
    }
  }
}
```

Old snapshots should restore if compatible. If incompatible, the API should fail clearly or warn when appropriate. No attempt should be made to rebase exact VM snapshots onto newer base images.

## Storage

v1 uses plain image files on local disk.

No ZFS, btrfs, LVM thin provisioning, or qcow2 overlay requirement in v1.

Fast cloning can use ordinary filesystem reflinks when the configured state directory supports them, but Firedoze must still work with regular file copies.

Cold storage is opt-in. If `cold_storage.dir` is configured, Firedoze can move disks from VMs that have been stopped longer than `cold_storage.archive_stopped_after_seconds` to that directory using a regular file copy, then remove the hot copy. The SQLite VM record stores the archived disk path, so starts can restore the disk before booting, snapshots can copy from the archived disk, and deletes can reclaim it.

Cold-storage archive copies are cancellable. Explicit VM operations such as start, snapshot, and delete should preempt an in-progress archive, wait for temporary-file cleanup, and then continue.

Only stopped VM disks are eligible for cold storage. Sleeping VMs are not moved because their exact runtime state belongs to the hot VM state directory.

## Resource Management

Users are trusted to size VMs sensibly.

Per-VM memory range, vCPU count, and disk size are configurable. Memory is
represented as a minimum and maximum; Firecracker boots at the minimum and uses
virtio-mem for hotplug growth up to the maximum.

No hard maximums are required in daemon config for v1.

Firedoze exposes VM resource usage through the API and `firedoze vm usage`.
Elastic memory is implemented with Firecracker virtio-mem plus a constrained
guest hint path: the guest can report a desired target, but the host clamps that
target to the VM's configured min/max and applies it through Firecracker.

## Caddy and ACME Assumptions

For public HTTPS to work:

- Public wildcard DNS must point to the host.
- Ports 80 and 443 must reach Caddy.
- Caddy will obtain per-host certificates for concrete names as they are used.

No DNS-01 provider integration is required in v1.

The embedded Caddy integration always runs with Auto HTTPS enabled. Firedoze can configure the HTTP and HTTPS listener ports, but there is no separate insecure public routing mode. The normal deployment path is per-host Auto HTTPS on ports 80/443.

## Open Implementation Notes

Exact Firecracker snapshot behavior needs careful implementation because memory state, disk state, network identity, and guest identity must line up.

Wake-on-request through embedded Caddy needs a mechanism for Caddy route handling to block, wake the target VM, and then proxy to the now-running private IP.

Idle detection should likely start with host-visible network counters per VM TAP interface. More complex eBPF or conntrack approaches can come later if needed.

The initial Firecracker integration uses `firecracker --api-sock ... --config-file ...`, which starts the microVM immediately from the config file. In this launch mode, Firedoze must not send a separate `InstanceStart` action. Later snapshot/restore work may require moving to a more explicit API-driven configuration flow.

Host firewall/security group requirements must be documented before real deployment, especially:

- Public 80/443 to host.
- WireGuard UDP listen port to host.
- No public SSH to VMs.
- Management API bound only to WireGuard.
- Host firewall rules block new non-WireGuard/non-VM traffic into the VM private subnet.

## Initial Build Order

1. Create repository and ADR.
2. Bootstrap Go daemon with config and SQLite metadata.
3. Manage kernel WireGuard interface from config.
4. Expose JSON HTTP API only on WireGuard.
5. Start/stop one Firecracker VM from the fixed base image.
6. Make passwordless guest SSH work over WireGuard.
7. Embed Caddy and serve default VM route.
8. Add explicit HTTPS route aliases.
9. Add named exact-state snapshots.
10. Add clone-from-snapshot with identity rewrite.
11. Add idle detection and exact sleep/resume.
12. Add usability polish: generated WG configs, client command, friendly VM list output, and ready-to-run SSH/public URL output in the client.
