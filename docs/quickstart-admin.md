# Quickstart

This guide explains how to install and use Firedoze on a Linux host. It is written for the administrator who sets up the Firedoze server and manages access for client laptops. If you are a developer who just wants to connect to an existing Firedoze server, see [Quickstart for Developers](quickstart-dev.md) instead.

## 1. Host Requirements

Use an x86_64 Linux box with:

- KVM available at `/dev/kvm`.
- Kernel WireGuard support.
- `debugfs`, `ssh-keygen`, and `systemd`.
- Firecracker installed at `/usr/lib/firedoze/firecracker`; the Firedoze release package installs the pinned supported version.
- Enough disk space to build and store base images, VM disks, and snapshots.
- Recommended on small hosts: add a modest swap file as a memory-spike guardrail. Swap is not a substitute for real RAM, but it makes tiny test hosts less brittle.
- Recommended: **put `state_dir` on a filesystem with reflink support for fast VM disk clones**. XFS with reflinks enabled is a good default choice (see [Fast VM Disk Clones](#fast-vm-disk-clones)).
- Optional: configure cold storage if you want disks from long-stopped VMs moved to cheaper/slower storage automatically.
- IPv6 egress if guests need outbound internet access. The private VM network is IPv6-only.

The systemd service runs as a dedicated `firedoze` system user, not as root. It uses Linux capabilities for the privileged network operations it still needs at runtime.

Firedoze also enables Linux Kernel Samepage Merging on startup when the host
supports it. KSM lets the kernel deduplicate identical memory pages across
running VMs, which is useful when many Firedoze VMs boot from the same Ubuntu
image. Sleeping VMs still use no running memory; KSM only helps VMs that are
currently running.

The current tested host is **Ubuntu 24.04.4 LTS (Noble Numbat)** with kernel
`6.8.0-111-generic`, `ip6tables v1.8.10 (nf_tables)`, and `nftables v1.0.9`.
Other modern Linux distributions should work, but this is the baseline we have
actually exercised.

Firedoze's optional host firewall support currently uses `ip6tables`. On modern
systems this is usually the nftables-backed iptables compatibility layer, not
the old legacy firewall stack. Make sure the `iptables`/`ip6tables` tools are
installed and that the kernel has the required netfilter modules available. On
minimal hosts, `sudo modprobe nf_tables nft_compat` is a useful quick check; if
those modules are missing, install the distribution's normal kernel/modules
package before enabling Firedoze firewall management.

On Ubuntu, the host packages are roughly:

```sh
sudo apt-get install -y ca-certificates curl wireguard-tools e2fsprogs xfsprogs iptables
```

On small hosts, add swap before building the base image or running VMs:

```sh
sudo fallocate -l 8G /swapfile
sudo chmod 600 /swapfile
sudo mkswap /swapfile
sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
echo 'vm.swappiness=10' | sudo tee /etc/sysctl.d/99-firedoze-swap.conf
sudo sysctl --system
```

For a 4 GiB RAM test host, 4-8 GiB of swap is reasonable.
For larger hosts, 8-16 GiB is usually enough as a safety buffer. If active VM
memory is regularly spilling into swap, reduce the number or size of running VMs
or use a larger host. Swap can contain fragments of VM memory, so treat the
host swap device or swap file as sensitive data. If that matters for your
threat model, use encrypted host storage or skip swap and size the host with
enough physical RAM instead.

## 2. Setup

The fastest possible setup is to install a release package, build the base
image, create the host config, add each laptop as a WireGuard peer, then start
`firedozed`. Replace `0.1.0` with the current Firedoze release:

```sh
version=0.1.0
deb="/tmp/firedoze_${version}_linux_amd64.deb"
curl -fsSL -o "$deb" "https://github.com/addrummond/firedoze/releases/download/v${version}/firedoze_${version}_linux_amd64.deb"
chmod 0644 "$deb"
sudo apt-get install -y -o DPkg::Post-Invoke::= "$deb"
# OR
sudo dnf install "./firedoze_${version}_linux_amd64.rpm"

# Optional, but recommended before installing the base image:
# set up XFS at /var/lib/firedoze (see ‘Fast VM Disk Clones’ below).

sudo firedoze-image-builder setup

# use -init-host <DOMAIN_NAME> if you have a real domain
sudo firedozed -init-config -init-sslip-host $(curl -4 https://ifconfig.me)

# Alice runs this on her laptop and sends you only the printed public key:
#     firedoze server request alice-laptop

# You add Alice's laptop as a WireGuard peer on the server. This prints a
# Firedoze client import config with no client private key in it.
sudo firedozed -wg-add-peer alice-laptop <ALICE_PUBLIC_KEY> > team-dev.conf
# Send team-dev.conf back to Alice:
#     firedoze server import team-dev.conf -default

sudo systemctl enable --now firedozed
```

For fast VM creation, set up `/var/lib/firedoze` on XFS or another reflink-capable filesystem before building and installing the base image. The normal setup works on ext4, but VM disks are then copied with the slower sparse-copy fallback. See [Fast VM Disk Clones](#fast-vm-disk-clones) for the details.

Alice can now connect:

```sh
firedoze server import team-dev.conf -default
firedoze health # check API connectivity
```

The rest of this section explains the above steps in order.

### 2.1 Install Firedoze

Download the release package for your host. For the full Sigstore verification
flow, see [Release Packages](release-packages.md).

On Debian or Ubuntu:

```sh
version=0.1.0
deb="/tmp/firedoze_${version}_linux_amd64.deb"
curl -fsSL -o "$deb" "https://github.com/addrummond/firedoze/releases/download/v${version}/firedoze_${version}_linux_amd64.deb"
chmod 0644 "$deb"
sudo apt-get install -y -o DPkg::Post-Invoke::= "$deb"
```

The `DPkg::Post-Invoke` override skips Ubuntu's `needrestart` scan for this
one local package install. It does not change Firedoze's package contents or
systemd service.

On RPM-based distributions:

```sh
version=0.1.0
curl -LO "https://github.com/addrummond/firedoze/releases/download/v${version}/firedoze_${version}_linux_amd64.rpm"
sudo dnf install "./firedoze_${version}_linux_amd64.rpm"
```

The package:

- Installs `firedoze`, `firedozed`, and `firedoze-image-builder` to `/usr/bin`.
- Installs the pinned upstream Firecracker binary to `/usr/lib/firedoze/firecracker`.
- Installs the packaged Linux guest helper used by the base image builder.
- Creates the `firedoze` system user and adds it to the `kvm` group when that group exists.
- Creates `/etc/firedoze`, `/var/lib/firedoze`, `/var/lib/firedoze/images`, and `/var/log/firedoze`.
- Installs an example config at `/etc/firedoze/firedoze.example.toml`.
- Installs a systemd unit that runs `firedozed` as the `firedoze` user with `CAP_NET_ADMIN` and `CAP_NET_BIND_SERVICE`.

Existing config and VM state are left alone when you reinstall. The real config is created later with `firedozed -init-config`.

### 2.2 Build and install base images

Build and install the Firedoze Ubuntu 26.04 LTS base image on the Linux host.
The builder is native Go; it does not require Docker, Podman, mounting, a
source checkout, or host ext4 support.

Run:

```sh
sudo firedoze-image-builder setup
```

The image builder downloads pinned Ubuntu 26.04 cloud image artifacts, verifies
their SHA-256 checksums, turns the root tarball into a raw ext4 root
filesystem, and adds the small Firedoze guest configuration needed for SSH and
Firecracker networking. It does not boot the image or run `apt-get update`
inside it; the generated image is built from the pinned artifacts and packages
recorded in `manifest.txt`.

These files are installed here:

```text
/var/lib/firedoze/images/vmlinux.bin
/var/lib/firedoze/images/initrd.img
/var/lib/firedoze/images/rootfs.ext4
/var/lib/firedoze/images/manifest.txt
```

Keep `manifest.txt` with any copied or archived base image. It records the
Ubuntu release, cloud image serial, artifact URLs, SHA-256 checksums, extra
package versions, and the Firedoze guest helper hash used for the build.

The builder's pinned URLs and SHA-256 checksums are the current base-image
lock. That gives repeatable rebuilds while Ubuntu continues serving the same
cloud-image serial and package artifacts. If you need stronger long-term
rebuild guarantees, archive the artifacts listed in `manifest.txt` in your own
object store or snapshot-capable mirror before relying on that image for future
rebuilds.

Rebuild and reinstall the base image periodically to pick up Ubuntu security
updates for newly created VMs:

```sh
sudo firedoze-image-builder setup
```

This only changes VMs created after the new image is installed. Existing VMs are
persistent computers; update them from inside the VM or recreate them from a
fresh image/snapshot workflow if you need their packages refreshed.

For debugging or offline copying, you can still split this into
`firedoze-image-builder build -out DIR` and
`sudo firedoze-image-builder install -src DIR`.

The generated image uses the normal Ubuntu `ubuntu` user for passwordless SSH. Firedoze relies on WireGuard for access control; do not expose VM SSH publicly.

### 2.3 Configure Firedoze

Firedoze uses WireGuard as the access-control layer. The generated base image configures the `ubuntu` guest account for passwordless SSH, and VM SSH is reachable only through the WireGuard-routed private VM network.

Create the host config:

```sh
sudo firedozed -init-config -init-sslip-host $(curl -4 https://ifconfig.me)
```

`-init-sslip-host` also sets `base_domain` to `PUBLIC_IP.sslip.io`, which is useful when the host has no real domain yet.

If you already have DNS, use `-init-host`:

```sh
sudo firedozed -init-config -init-host dev.example.com
```

`-init-host` sets both the WireGuard endpoint host and `base_domain`. If the WireGuard endpoint host and VM wildcard domain are different, pass `-init-base-domain` explicitly.

`-init-config` writes `/etc/firedoze/firedoze.toml`, refuses to overwrite an existing config unless you pass `-init-force`, and chooses random private ranges for:

- `wireguard.address`
- `vm_network.subnet`

Those randomized ranges make it less likely that one laptop will see route conflicts when connecting to multiple Firedoze servers.

Reviewing the generated config is optional. If you do edit it, look for any `# EDIT PLACEHOLDER` comments:

```sh
sudoedit /etc/firedoze/firedoze.toml
```

The main fields to check are:

- `base_domain`: the wildcard DNS domain for VM URLs.
- `wireguard.endpoint`: the public host and UDP port laptops will connect to.
- `wireguard.peers`: one peer per laptop.
- `firecracker.default_memory_min_mib` and `firecracker.default_memory_max_mib`:
  the default elastic memory range.

Each client requests access locally:

```sh
firedoze server request alice-laptop
```

This generates a WireGuard key pair on the client laptop and stores the private
key in the client config. The client sends only the printed public key, or the
printed admin command, to the admin.

If Alice already created the request and needs to print the public key again,
she can run:

```sh
firedoze wg pubkey alice-laptop
```

To add Alice's laptop, the admin runs this on the Firedoze host:

```sh
sudo firedozed -wg-add-peer alice-laptop <ALICE_PUBLIC_KEY>
```

The command picks the next free client address, updates
`/etc/firedoze/firedoze.toml` automatically, and prints a Firedoze client import
config for Alice. The import config contains server routing details and Alice's
public key, but it does not contain Alice's private key. Send it back to Alice so
she can import it:

```sh
firedoze server import /path/to/team-dev.conf -default
```

Name the file after the Firedoze server profile the client should use. For
example, `team-dev.conf` imports as `team-dev`.

If `firedozed` is already running, it watches the config file and applies WireGuard peer additions, removals, and peer address changes automatically. Other config changes still need a service restart.

To choose the client address yourself, pass a unique `/128` address inside the generated WireGuard subnet:

```sh
sudo firedozed -wg-add-peer alice-laptop <ALICE_PUBLIC_KEY> fd7a:115c:a1e1::2/128
```

### 2.4 Configure firewall and public DNS

Open these inbound ports to the host:

- UDP `51820` for WireGuard.
- TCP `80` and `443` for public web routes.

Firedoze can install host firewall rules for its private IPv6 VM subnet when
`firedozed` starts with `-setup-wireguard`. The generated config enables this
with:

```toml
[host_firewall]
enabled = true
backend = "ip6tables"
```

When enabled, `backend` is required. Only `ip6tables` is implemented for now.
The rules allow WireGuard clients, VM-to-VM traffic, VM outbound internet
traffic, local host proxying, and established replies, while blocking new
traffic from ordinary LAN/public interfaces into the VM private subnet. Because
Firedoze VMs use private IPv6 addresses, this also installs IPv6 masquerading
for outbound VM traffic. Set `enabled = false` only if you are managing
equivalent firewall and outbound NAT policy yourself.

Set public wildcard DNS for web routes:

```text
*.dev.example.com -> your Firedoze host public IP
```

Caddy obtains certificates automatically for each VM or route hostname. The host must be publicly reachable on ports `80` and `443`, and the wildcard DNS must point at the host.

### 2.5 Start firedozed

```sh
sudo systemctl enable --now firedozed
sudo systemctl status firedozed
```

Logs:

```sh
journalctl -u firedozed -f
```

When systemd stops firedozed, the daemon records the VMs that are currently
running, sleeps them cleanly, and wakes them again on the next daemon start.
This keeps daemon upgrades simple while preserving VM state. Existing SSH
sessions and open TCP connections will still disconnect during the restart.

The provided unit uses systemd readiness notification and a watchdog. If the daemon stops sending watchdog pings, systemd will restart it.

The daemon runs as the `firedoze` system user. It is not UID 0, but systemd grants it the narrow runtime privileges it needs:

- `CAP_NET_ADMIN` for WireGuard, TAP devices, routes, and related network setup.
- `CAP_NET_BIND_SERVICE` for ports `80` and `443`.
- `kvm` group membership for `/dev/kvm`.

The unit also runs a root-only startup step that enables KSM before `firedozed`
drops to the `firedoze` user. Hosts without KSM support continue normally.
The unit delegates a cgroup v2 subtree to Firedoze so each running Firecracker
process can live in its own VM cgroup. Firedoze uses those cgroups for more
accurate host CPU, memory, and IO accounting, and assigns every VM the same CPU
and IO weight. It does not set CPU quotas by default.

Config and key material under `/etc/firedoze` are not world-readable. Use `sudo firedozed ...` for admin helper commands such as `-wg-add-peer`, `-wg-peer-config`, and `-print-api-env`.

### 2.6 Import Client Config

Send the Firedoze client import config printed by `-wg-add-peer` back to the
client. The client imports it on their laptop:

```sh
firedoze server import /path/to/team-dev.conf -default
```

Normal `firedoze` commands do not require the user to run `wg-quick`. The client
stores the server API URL, the server WireGuard details, and the locally
generated client private key in `~/.config/firedoze/config.toml`. It starts or
reuses a local per-server userspace WireGuard broker for API calls, SSH, `exec`,
and `cp`. The broker exits automatically after it has been idle for several
minutes.

For scripts that deliberately use `FIREDOZE_API` instead of the client config
file, you can print the API shell export on the Firedoze host. This bypasses the
client's imported WireGuard transport, so it is only useful from the server
itself or from a machine that already has equivalent WireGuard routing:

```sh
sudo firedozed -print-api-env
```

The generated import config includes the laptop's WireGuard `address`. That
address comes from the peer's `allowed_ips` entry in
`/etc/firedoze/firedoze.toml`. With the default automatic peer address
selection, Alice's config will contain the next free `/128` address from the
generated WireGuard subnet.

The server config is the source of truth for the laptop's WireGuard address. Do
not edit `address` to a different value only on the laptop; it must match that
peer's `allowed_ips` entry on the server. If you change the peer address in
`/etc/firedoze/firedoze.toml`, regenerate the client import config:

```sh
sudo firedozed -wg-peer-config alice-laptop
```

## 3. Use Firedoze

The `firedoze` client runs on your laptop and talks to the WireGuard-only API. If you imported the generated client config with `firedoze server import ... -default`, the client will find the server automatically and use its built-in WireGuard transport.

The client adds the default API port, `8081`, when the configured URL has no port. If your server uses a different API port, include it explicitly in the generated import config or in a manual `firedoze server add` URL.

Check that the API is reachable:

```sh
firedoze health
```

Create and start a VM:

```sh
firedoze vm create demo
firedoze vm start demo
```

VMs created with `firedoze vm create` are ‘hidden‘ by default (i.e. they do not get a public HTTPS URL).

Create a VM if needed, publish it, start it, and SSH in when it is ready:

```sh
firedoze vm up demo
```

Create several VMs with the same settings:

```sh
firedoze vm create alice bob charlie -memory-min-mib 512 -memory-max-mib 1024 -disk-bytes 8589934592
```

Toggle public HTTPS access:

```sh
firedoze vm publish demo
firedoze vm hide demo
```

By default, a sleeping public VM can wake from public HTTPS after the browser completes a small "Are you human?" challenge. The browser gets a signed host-specific cookie, so future requests can wake that VM without repeating the challenge until the cookie expires.

Protect or unprotect a public hostname independently of the VM or route:

```sh
firedoze route protect demo.example.com
firedoze route unprotect demo.example.com
```

Create a signed access URL for a protected hostname:

```sh
firedoze route get-signed-url demo.example.com
firedoze route get-signed-url demo.example.com/foo/bar
firedoze route get-signed-url demo.example.com -ttl 3600
```

The default signed URL lifetime is 24 hours. `-ttl` is in seconds. If you include a path after the hostname, the signed URL sets the cookie and then redirects the visitor to that path.

The route-auth signing key is generated automatically. Firedoze keeps it in memory while running, saves it to the systemd runtime directory on SIGHUP and graceful shutdown, and reads then removes the saved copy on startup. The packaged service preserves that runtime directory across `systemctl restart firedozed`, but removes it on `systemctl stop firedozed` and host reboot. If the key is lost, visitors just need a new signed URL or a fresh human check.

Prefer `firedoze vm start` when you mean to explicitly wake an existing VM; `firedoze vm up` creates the VM if it does not already exist.

To disable passive wake for a VM:

```sh
firedoze vm create demo-public -publish -no-auto-wake
```

Update a VM's Firedoze settings, such as default HTTP port, idle timeout, public HTTPS visibility, or passive network wake:

```sh
firedoze vm settings demo -http-port 3000 -idle-sleep-after 900 -publish true -auto-wake false
```

This changes Firedoze metadata. It does not edit the guest disk, rename the VM, or change an exact sleep snapshot.

List VMs and SSH to one:

```sh
firedoze vm list
firedoze vm usage
firedoze vm inspect demo
firedoze ssh demo
```

Inside a VM, `firedoze-stop` stops the VM from its own shell:

```sh
firedoze-stop
```

On x86_64 Firecracker this is implemented with the guest `reboot` command,
because that exits the microVM cleanly and lets Firedoze mark it as `stopped`.
Do not rely on `shutdown -h now`, `poweroff`, or `halt` inside the guest to stop
a VM; Firecracker can leave the VMM process running after those commands.

For shell scripts, print just matching VM names:

```sh
firedoze vm list -names 'demo*'
```

Or print just matching VM UUIDs:

```sh
firedoze vm list -ids 'demo*'
```

Commands that take `<vm>` accept either the VM name or its UUID. UUIDs are
useful in scripts when a stable identity matters.

Run a command inside a VM and snapshot it after the command succeeds:

```sh
firedoze exec demo -- sh -lc 'set -eu; echo ready > /home/ubuntu/ready.txt'
firedoze vm sleep demo
firedoze snapshot save demo-ready demo
```

Copy files between your laptop and a VM:

```sh
firedoze cp ./app/ demo:/home/ubuntu/app/
firedoze cp demo:/home/ubuntu/app/results.log ./results.log
```

For tools that need the VM private IP directly, run another local command with the VM private IP in `FIREDOZE_VM_IP`:

```sh
firedoze with-vm-ip demo sh -c 'printf "%s\n" "$FIREDOZE_VM_IP"'
```

Firedoze VMs can run containers when a project needs them. Install a daemonless
runtime such as Podman inside the VM:

```sh
firedoze ssh demo
sudo apt-get update
sudo apt-get install -y podman buildah crun
podman run --rm hello-world
```

This is optional. Firedoze itself does not use Docker, Podman, or a container
runtime as part of its VM model.

Run the built-in hello web server inside the VM:

```sh
firedoze ssh demo
firedoze-hello
```

In another terminal on your laptop, open or curl the VM URL shown by `firedoze vm list`. The default route proxies to port `8080`, which is also the default `firedoze-hello` port.

Custom web services should listen on IPv6, for example `[::]:8080`, because the private VM network is IPv6-only.

To keep the hello server running as a systemd service inside the VM:

```sh
sudo firedoze-hello-service install 8080
```

The default hello page exposes only basic liveness information. To include extra diagnostics such as user, kernel, addresses, and routes:

```sh
sudo firedoze-hello-service install 8080 -verbose
```

After it has been installed, manage it with normal systemd commands:

```sh
sudo systemctl start firedoze-hello.service
sudo systemctl status firedoze-hello.service
```

Reboot, sleep, or stop a VM:

```sh
firedoze vm reboot demo
firedoze vm sleep demo
firedoze vm stop demo
```

Reboot starts the VM from disk. If the VM is sleeping, reboot discards the exact
suspended runtime state instead of resuming it.

Delete a VM and its state directory:

```sh
firedoze vm delete demo
# or delete several at once:
firedoze vm delete demo old-test scratch
```

Save a named snapshot:

```sh
firedoze vm stop demo
firedoze snapshot save demo-base demo
```

Snapshots can only be saved from stopped VMs. Firedoze rejects snapshots of
running or sleeping VMs because restored snapshots boot as new VM identities.
Use `sleep` for exact suspend/resume of the same VM, and `stop` before creating
a cloneable snapshot.

A sleeping VM includes live memory and device state that belongs to that exact
VM identity. A snapshot restore creates a new VM identity, so it uses a clean
disk snapshot and boots normally.

Export a snapshot to a portable file:

```sh
firedoze snapshot export demo-base demo-base.firedoze-snapshot.tgz
```

Import that file as a snapshot on another Firedoze server:

```sh
firedoze snapshot import demo-base demo-base.firedoze-snapshot.tgz
```

The imported snapshot name does not have to match the original name. Snapshot
bundles include the cloneable disk image and lineage metadata; they do not
include base images or exact sleep memory state.

Restore a snapshot as a new VM:

```sh
firedoze snapshot restore demo-base demo-copy
```

Override clone settings while restoring:

```sh
firedoze snapshot restore demo-base bigger-demo -memory-min-mib 512 -memory-max-mib 2048 -vcpus 2 -disk-bytes 17179869184
```

Delete a snapshot and its files:

```sh
firedoze snapshot delete demo-base
```

Create a public web route alias:

```sh
firedoze route create app demo 8080
```

That route maps:

```text
https://app.dev.example.com -> demo VM port 8080
```

If `demo` is sleeping when a request reaches `app.dev.example.com`, Firedoze wakes it before proxying the request. If wake takes longer than the client allows, retry the request.

Protect the route before or after creating it:

```sh
firedoze route protect app.dev.example.com
firedoze route get-signed-url app.dev.example.com/dashboard
firedoze route get-signed-url app.dev.example.com -ttl 86400
```

Delete the route alias:

```sh
firedoze route delete app
```

For scripts that need exact API response bodies from table-oriented commands, add `-json`. Inspect commands already print the single resource as JSON:

```sh
firedoze -json vm list
firedoze vm inspect demo
firedoze snapshot inspect demo-snap
```

## Fast VM Disk Clones

Firedoze stores VM disks as plain raw image files. When the state directory is on a filesystem that supports reflinks, Firedoze can clone the base image with copy-on-write instead of physically copying all allocated blocks.

This makes VM start after first create much faster. On filesystems without reflinks, Firedoze still works and falls back to sparse-aware copying.

XFS is a good default choice of filesystem that supports reflinks without bringing in a larger storage-management model. Btrfs and other reflink-capable filesystems can also work.

The important rule is that these paths should live on the same reflink-capable filesystem:

```text
images/rootfs.ext4
vms/<name>/rootfs.ext4
snapshots/<name>/rootfs.ext4
```

The default config already uses `/var/lib/firedoze` as the base path for all of these, so the simplest approach is to mount the filesystem at `/var/lib/firedoze`.

### Option A: XFS Partition Or Disk

If you have a spare disk, cloud volume, or existing partition, format it as XFS with reflinks enabled and mount it at `/var/lib/firedoze`.

Example after the partition already exists:

```sh
sudo systemctl stop firedozed 2>/dev/null || true
sudo mkfs.xfs -m reflink=1 /dev/disk/by-id/<DISK_OR_PARTITION_ID>
sudo mkdir -p /var/lib/firedoze
sudo mount /dev/disk/by-id/<DISK_OR_PARTITION_ID> /var/lib/firedoze
sudo install -d -o firedoze -g firedoze -m 0750 /var/lib/firedoze
sudo install -d -o firedoze -g firedoze -m 0755 /var/lib/firedoze/images
```

Then add an `/etc/fstab` entry appropriate for that disk or partition, for example:

```text
/dev/disk/by-id/<DISK_OR_PARTITION_ID> /var/lib/firedoze xfs defaults,nofail 0 0
```

If `/var/lib/firedoze` already contains data, copy it aside before mounting the XFS filesystem, then copy it back into the mounted filesystem.

### Option B: XFS Loopback File

If you do not want to repartition or attach another disk, create a file-backed XFS filesystem and mount that at `/var/lib/firedoze`.

This is useful for testing and small servers. It is still kernel XFS, not a userspace filesystem. The backing file can be sparse, so it only consumes blocks as data is written.

For a new install:

```sh
sudo mkdir -p /var/lib
sudo truncate -s 64G /var/lib/firedoze.xfs.img
sudo mkfs.xfs -f -m reflink=1 /var/lib/firedoze.xfs.img
sudo mkdir -p /var/lib/firedoze
sudo mount -o loop /var/lib/firedoze.xfs.img /var/lib/firedoze
sudo install -d -o firedoze -g firedoze -m 0750 /var/lib/firedoze
sudo install -d -o firedoze -g firedoze -m 0755 /var/lib/firedoze/images
```

Make it persistent across reboots:

```sh
echo '/var/lib/firedoze.xfs.img /var/lib/firedoze xfs loop,defaults,nofail 0 0' | sudo tee -a /etc/fstab
```

For an existing install with data:

```sh
sudo systemctl stop firedozed
stamp=$(date +%Y%m%d%H%M%S)
sudo truncate -s 64G /var/lib/firedoze.xfs.img
sudo mkfs.xfs -f -m reflink=1 /var/lib/firedoze.xfs.img
sudo mv /var/lib/firedoze /var/lib/firedoze.before-xfs.$stamp
sudo mkdir -p /var/lib/firedoze
sudo mount -o loop /var/lib/firedoze.xfs.img /var/lib/firedoze
sudo rsync -aHAX --numeric-ids /var/lib/firedoze.before-xfs.$stamp/ /var/lib/firedoze/
sudo chown firedoze:firedoze /var/lib/firedoze
sudo chmod 0750 /var/lib/firedoze
sudo install -d -o firedoze -g firedoze -m 0755 /var/lib/firedoze/images
sudo systemctl start firedozed
```

Check that XFS reflinks are enabled:

```sh
findmnt /var/lib/firedoze
sudo xfs_info /var/lib/firedoze | grep reflink
```

You should see `reflink=1`.

## Resource Usage

Use the client to inspect what Firedoze can currently see:

```sh
firedoze vm usage
```

The `MEMORY` column shows the configured min-max range. The `HOTPLUG` column
shows currently plugged/requested virtio-mem memory for running VMs. `HOST MEM`
uses the best host-side value Firedoze has, usually process RSS when that is
larger than cgroup memory accounting. `HOST CPU` and `HOST IO` come from the
VM's host cgroup when available. `HOST IO` is read/write bytes.
`GUEST DISK FREE/TOTAL` is reported from inside the VM and reflects usable
guest filesystem space, not host-side image allocation.

## Cold Storage For Stopped VMs

Cold storage is optional. If configured, Firedoze periodically moves disks from VMs that have been stopped for long enough to a cheaper/slower directory. The VM stays in the normal metadata store and can still be listed, started, snapshotted, or deleted.

Only stopped VM disks are moved. Running VMs and sleeping VMs are not moved.

Example:

```toml
[cold_storage]
dir = "/mnt/slow/firedoze"
archive_stopped_after_seconds = 2592000 # 30 days
```

Prepare the directory so the `firedoze` service user can write to it:

```sh
sudo mkdir -p /mnt/slow/firedoze
sudo chown firedoze:firedoze /mnt/slow/firedoze
```

When a disk is archived, Firedoze copies:

```text
/var/lib/firedoze/vms/<name>/rootfs.ext4
```

to:

```text
<cold_storage.dir>/vms/<name>/rootfs.ext4
```

and records that path in SQLite before removing the hot copy. Starting the VM copies the disk back before booting. Saving a snapshot of an archived stopped VM copies directly from the archived disk. Deleting the VM removes the archived disk too.

If a start, snapshot, or delete command arrives while an archive copy is still in progress, Firedoze cancels the archive, removes the partial temporary file, and lets the explicit command continue.

Cold storage is not a backup system. It is just a way to reclaim faster local disk from stopped VMs you are not actively using.

## Reference Config

The generated config starts from this shape:

```toml
base_domain = "dev.example.com"
default_http_port = 8080
state_dir = "/var/lib/firedoze"

[api]
port = 8081

[wireguard]
interface = "fdwg0"
listen_port = 51820
address = "fd7a:115c:a1e1::1/64"
endpoint = "YOUR_SERVER_PUBLIC_IP_OR_DNS:51820"
private_key_file = "/etc/firedoze/wg.key"

[[wireguard.peers]]
name = "alice-laptop"
public_key = "PASTE_CLIENT_PUBLIC_KEY_HERE"
allowed_ips = ["fd7a:115c:a1e1::2/128"]

[vm_network]
subnet = "fd7a:115c:a1e0::/64"

[caddy]
http_port = 80
https_port = 443
internal_proxy_port = 18082
tls_mode = "auto"

[metadata]
path = "/var/lib/firedoze/firedoze.db"

[ssh]
user = "ubuntu"
wake_proxy_port = 18022

[idle]
check_interval_seconds = 30
default_sleep_after_seconds = 21600

[cold_storage]
dir = ""
archive_stopped_after_seconds = 2592000

[firecracker]
binary_path = "/usr/lib/firedoze/firecracker"
base_kernel_path = "/var/lib/firedoze/images/vmlinux.bin"
base_initrd_path = "/var/lib/firedoze/images/initrd.img"
base_rootfs_path = "/var/lib/firedoze/images/rootfs.ext4"
default_vcpus = 1
default_memory_min_mib = 512
default_memory_max_mib = 1024
default_disk_bytes = 4294967296
```

`caddy.tls_mode = "auto"` is the normal direct-internet mode: Firedoze serves
HTTPS locally on `https_port` and redirects HTTP to HTTPS. If Firedoze is behind
a local TLS-terminating tunnel or reverse proxy such as Cloudflare Tunnel, use:

```toml
[caddy]
http_port = 80
https_port = 443
internal_proxy_port = 18082
tls_mode = "behind_proxy"
```

Then point the tunnel origin at `http://localhost:80`. Public users still see
HTTPS from the tunnel/proxy, while Firedoze serves plain HTTP only on the local
origin connection.

## Upgrade or Uninstall

To upgrade, install the newer release package, then restart the daemon:

```sh
version=0.1.1
deb="/tmp/firedoze_${version}_linux_amd64.deb"
curl -fsSL -o "$deb" "https://github.com/addrummond/firedoze/releases/download/v${version}/firedoze_${version}_linux_amd64.deb"
chmod 0644 "$deb"
sudo apt-get install -y -o DPkg::Post-Invoke::= "$deb"
sudo systemctl restart firedozed
```

Use the matching `.rpm` with `sudo dnf install` on RPM-based distributions.
Package upgrades leave existing config and VM state untouched.

Restarting the daemon temporarily interrupts the management API and public
proxy. Running VMs are slept during shutdown and automatically started again
after the new daemon comes up.

Systemd socket activation is not used in v1. It would only preserve newly
arriving connections to selected listening sockets during a daemon restart; it
would not preserve existing SSH sessions, public HTTP connections, or the
embedded Caddy process. Less disruptive upgrades would require Firedoze to adopt
already-running Firecracker processes after restart instead of sleeping and
waking them.

To remove installed binaries and the systemd unit while keeping config, images,
VMs, snapshots, and logs:

```sh
sudo apt remove firedoze
```

Use `sudo dnf remove firedoze` on RPM-based distributions.

To remove everything, including config and all VM state, stop the daemon, remove
the package, and delete the state directories:

```sh
sudo systemctl stop firedozed
sudo apt remove firedoze
sudo rm -rf /etc/firedoze /var/lib/firedoze /var/log/firedoze
```
