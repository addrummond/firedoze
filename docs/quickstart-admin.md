# Quickstart

firedoze runs shared, persistent Firecracker dev VMs on one Linux host. The management API and VM SSH access are only reachable through WireGuard.

This is early dev software. Use it for shared development environments, not production.

## 1. Host Requirements

Use an x86_64 Linux box with:

- KVM available at `/dev/kvm`.
- Kernel WireGuard support.
- `debugfs`, `ssh-keygen`, and `systemd`.
- Firecracker installed at `/usr/local/bin/firecracker`; the setup steps below install it from the upstream release tarball.
- Enough disk space to build and store base images, VM disks, and snapshots.
- Recommended: **put `state_dir` on a filesystem with reflink support for fast VM disk clones**. XFS with reflinks enabled is a good default choice (see [Fast VM Disk Clones](#fast-vm-disk-clones)).
- IPv6 egress if guests need outbound internet access. The private VM network is IPv6-only.

The systemd service runs as a dedicated `firedoze` system user, not as root. It uses Linux capabilities for the privileged network operations it still needs at runtime.

On Ubuntu, the host packages are roughly:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates git wireguard-tools e2fsprogs openssh-client xfsprogs
```

## 2. Setup

Install `mise` on the Linux host. The project uses `.tool-versions` to pin the Go toolchain and Task version.

```sh
curl https://mise.run/bash | sh
source ~/.bashrc
```

Clone the private repo on the Linux host and install the pinned tools:

```sh
git clone REPO_URL firedoze
cd firedoze
mise install
```

The fastest possible setup is:

```sh
task setup:host
# use -init-host <DOMAIN_NAME> if you have a real domain
sudo firedozed -init-config -init-sslip-host $(curl -4 https://ifconfig.me)

# Alice runs this on her laptop and sends you only the public_key:
firedoze wg keygen

# You add Alice's laptop as a WireGuard peer on the server, which prints her
# client config template.
sudo firedozed -wg-add-peer alice-laptop <ALICE_PUBLIC_KEY>
# Alice replaces <client-private-key> in the printed config with the private
# key she generated, then connects WireGuard and runs the printed
# "firedoze server add ..." command on her laptop.

sudo systemctl enable --now firedozed
```

For fast VM creation, set up `/var/lib/firedoze` on XFS or another reflink-capable filesystem before building and installing the base image. The normal setup works on ext4, but VM disks are then copied with the slower sparse-copy fallback. See [Fast VM Disk Clones](#fast-vm-disk-clones) for the details.

Alice can now connect:

```sh
sudo wg-quick up /path/to/alice-client.conf
firedoze server add firedoze http://[fdxx:xxxx:xxxx:xxxx::1] -default
firedoze health # check API connectivity
```

The rest of this section explains the above steps in order.

### 2.1 Install firedoze

The installer:

- Builds `firedoze`, `firedozed`, and `firedoze-image-builder` from the checked-out source.
- Installs them to `/usr/local/bin`.
- Creates the `firedoze` system user and adds it to the `kvm` group when that group exists.
- Creates `/etc/firedoze`, `/var/lib/firedoze`, `/var/lib/firedoze/images`, and `/var/log/firedoze`.
- Installs an example config at `/etc/firedoze/firedoze.example.toml`.
- Installs a systemd unit that runs `firedozed` as the `firedoze` user with `CAP_NET_ADMIN` and `CAP_NET_BIND_SERVICE`.

Existing config and VM state are left alone when you reinstall. The real config is created later with `firedozed -init-config`.

### 2.2 Install Firecracker

Install the pinned upstream Firecracker VMM binary:

```sh
task firecracker:install
```

This downloads the release tarball declared in `Taskfile.yml`, validates its SHA-256 checksum, and installs it as `/usr/local/bin/firecracker`.

### 2.3 Build and install base images

Build the firedoze Ubuntu base image on the Linux host. The builder is native Go; it does not require Docker, Podman, root, mounting, or host ext4 support. Run it from the firedoze source checkout so it can compile the small Linux guest helper binaries.

From the repo checkout, run:

```sh
task image:build
```

The `image:build` task builds `./firedoze-image-builder`, downloads pinned Ubuntu cloud image artifacts, verifies their SHA-256 checksums, turns the root tarball into a raw ext4 root filesystem, and adds the small firedoze guest configuration needed for SSH and Firecracker networking.

Install the generated image artifacts:

```sh
task image:install
```

The install task copies the generated files here:

```text
/var/lib/firedoze/images/vmlinux.bin
/var/lib/firedoze/images/initrd.img
/var/lib/firedoze/images/rootfs.ext4
```

The generated image uses the normal Ubuntu `ubuntu` user for passwordless SSH. firedoze relies on WireGuard for access control; do not expose VM SSH publicly.

### 2.4 Configure firedoze

firedoze uses WireGuard as the access-control layer. The generated base image configures the `ubuntu` guest account for passwordless SSH, and VM SSH is reachable only through the WireGuard-routed private VM network.

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

Those randomized ranges make it less likely that one laptop will see route conflicts when connecting to multiple firedoze servers.

Reviewing the generated config is optional. If you do edit it, look for any `# EDIT PLACEHOLDER` comments:

```sh
sudoedit /etc/firedoze/firedoze.toml
```

The main fields to check are:

- `base_domain`: the wildcard DNS domain for VM URLs.
- `wireguard.endpoint`: the public host and UDP port laptops will connect to.
- `wireguard.peers`: one peer per laptop.
- `firecracker.default_memory_mib`: the default VM memory size.

Each client generates their own WireGuard key pair locally:

```sh
firedoze wg keygen
```

The client keeps `private_key` secret and sends only `public_key` to the admin.

To add Alice's laptop, the admin runs this on the firedoze host:

```sh
sudo firedozed -wg-add-peer alice-laptop <ALICE_PUBLIC_KEY>
```

The command picks the next free client address, updates `/etc/firedoze/firedoze.toml` automatically, and prints a WireGuard client config for Alice. The printed config contains `<client-private-key>` as a placeholder; Alice replaces that placeholder with the private key generated on her laptop.

To choose the client address yourself, pass a unique `/128` address inside the generated WireGuard subnet:

```sh
sudo firedozed -wg-add-peer alice-laptop <ALICE_PUBLIC_KEY> fd7a:115c:a1e1::2/128
```

### 2.5 Configure firewall and public DNS

Open these inbound ports to the host:

- UDP `51820` for WireGuard.
- TCP `80` and `443` for public web routes.

Set public wildcard DNS for web routes:

```text
*.dev.example.com -> your firedoze host public IP
```

Caddy obtains certificates automatically for each VM or route hostname. The host must be publicly reachable on ports `80` and `443`, and the wildcard DNS must point at the host.

### 2.6 Start firedozed

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

Config and key material under `/etc/firedoze` are not world-readable. Use `sudo firedozed ...` for admin helper commands such as `-wg-add-peer`, `-wg-peer-config`, and `-print-api-env`.

### 2.7 Connect WireGuard

Save the WireGuard client config printed by `-wg-add-peer` on the client laptop, replace `<client-private-key>` with the locally generated private key, then bring the tunnel up with `wg-quick` or your WireGuard client.

The generated config includes a commented `firedoze server add ...` command. Run it once on the client after connecting WireGuard. It stores the server API URL in `~/.config/firedoze/config.toml`, so normal client commands do not need an environment variable or command-line API URL.

For scripts that deliberately use `FIREDOZE_API` instead of the client config file, you can print the API shell export on the firedoze host:

```sh
sudo firedozed -print-api-env
```

The generated config includes the laptop's WireGuard `Address`. That address comes from the peer's `allowed_ips` entry in `/etc/firedoze/firedoze.toml`. With the default automatic peer address selection, Alice's config will contain the next free `/128` address from the generated WireGuard subnet:

```ini
Address = fdxx:xxxx:xxxx:xxxx::2/128
```

The server config is the source of truth for the laptop's WireGuard address. Do not edit `Address` to a different value only on the laptop; it must match that peer's `allowed_ips` entry on the server. If you change the peer address in `/etc/firedoze/firedoze.toml`, regenerate the client config:

```sh
sudo firedozed -wg-peer-config alice-laptop
```

## 3. Use firedoze

The `firedoze` client runs on your laptop and talks to the WireGuard-only API. If you ran the `firedoze server add ... -default` command from the generated WireGuard config, the client will find the server automatically.

The client adds the default API port, `8081`, when the configured URL has no port. If your server uses a different API port, include it explicitly in the `firedoze server add` URL.

Check that the API is reachable:

```sh
firedoze health
```

Create and start a VM:

```sh
firedoze vm create demo
firedoze start demo
```

VMs created with `firedoze vm create` are ‘hidden‘ by default (i.e. they do not get a public HTTPS URL).

Create a VM if needed, publish it, start it, and SSH in when it is ready:

```sh
firedoze up demo
```

Create several VMs with the same settings:

```sh
firedoze vm create alice bob charlie -memory-mib 512 -disk-bytes 8589934592
```

Toggle public HTTPS access:

```sh
firedoze publish demo
firedoze hide demo
```

By default, a sleeping public VM can wake from public HTTPS after the browser completes a small "Are you human?" challenge. The browser gets a signed host-specific cookie, so future requests can wake that VM without repeating the challenge until the cookie expires. The signing key is generated automatically in the firedoze state directory; if it is lost, visitors just complete the challenge again.

Prefer `firedoze vm start` when you mean to explicitly wake an existing VM; `firedoze up` creates the VM if it does not already exist.

To disable passive wake for a VM:

```sh
firedoze vm create demo-public -publish -no-auto-wake
```

Update a VM's firedoze settings, such as default HTTP port, idle timeout, public HTTPS visibility, or passive network wake:

```sh
firedoze vm settings demo -http-port 3000 -idle-sleep-after 900 -publish true -auto-wake false
```

This changes firedoze metadata. It does not edit the guest disk, rename the VM, or change an exact sleep snapshot.

List VMs and SSH to one:

```sh
firedoze vm list
firedoze vm inspect demo
firedoze ssh demo
```

For shell scripts, print just matching VM names:

```sh
firedoze vm list -names 'demo*'
```

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
firedoze reboot demo
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

Snapshots can only be saved from stopped VMs. firedoze rejects snapshots of
running or sleeping VMs because restored snapshots boot as new VM identities.
Use `sleep` for exact suspend/resume of the same VM, and `stop` before creating
a cloneable snapshot.

A sleeping VM includes live memory and device state that belongs to that exact
VM identity. A snapshot restore creates a new VM identity, so it uses a clean
disk snapshot and boots normally.

Restore a snapshot as a new VM:

```sh
firedoze snapshot restore demo-base demo-copy
```

Override clone settings while restoring:

```sh
firedoze snapshot restore demo-base bigger-demo -memory-mib 2048 -vcpus 2 -disk-bytes 17179869184
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

If `demo` is sleeping when a request reaches `app.dev.example.com`, firedoze wakes it before proxying the request. If wake takes longer than the client allows, retry the request.

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

firedoze stores VM disks as plain raw image files. When the state directory is on a filesystem that supports reflinks, firedoze can clone the base image with copy-on-write instead of physically copying all allocated blocks.

This makes VM start after first create much faster. On filesystems without reflinks, firedoze still works and falls back to sparse-aware copying.

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
```

Then add an `/etc/fstab` entry appropriate for that disk or partition, for example:

```text
/dev/disk/by-id/<DISK_OR_PARTITION_ID> /var/lib/firedoze xfs defaults,nofail 0 0
```

If `/var/lib/firedoze` already contains data, copy it aside before mounting the XFS filesystem, then copy it back into the mounted filesystem.

### Option B: XFS Loopback File

If you do not want to repartition or attach another disk, create a file-backed XFS filesystem and mount that at `/var/lib/firedoze`.

This is useful for testing and small servers. It is still kernel XFS, not a userspace filesystem. The backing file can be sparse, so it only consumes blocks as data is written, but it cannot actually grow beyond the free space available on the outer filesystem.

For a new install:

```sh
sudo mkdir -p /var/lib
sudo truncate -s 64G /var/lib/firedoze.xfs.img
sudo mkfs.xfs -f -m reflink=1 /var/lib/firedoze.xfs.img
sudo mkdir -p /var/lib/firedoze
sudo mount -o loop /var/lib/firedoze.xfs.img /var/lib/firedoze
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
sudo systemctl start firedozed
```

Check that XFS reflinks are enabled:

```sh
findmnt /var/lib/firedoze
sudo xfs_info /var/lib/firedoze | grep reflink
```

You should see `reflink=1`.

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

[firecracker]
binary_path = "/usr/local/bin/firecracker"
base_kernel_path = "/var/lib/firedoze/images/vmlinux.bin"
base_initrd_path = "/var/lib/firedoze/images/initrd.img"
base_rootfs_path = "/var/lib/firedoze/images/rootfs.ext4"
default_vcpus = 1
default_memory_mib = 512
default_disk_bytes = 4294967296
```

`caddy.tls_mode = "auto"` is the normal direct-internet mode: firedoze serves
HTTPS locally on `https_port` and redirects HTTP to HTTPS. If firedoze is behind
a local TLS-terminating tunnel or reverse proxy such as Cloudflare Tunnel, use:

```toml
[caddy]
http_port = 80
https_port = 443
internal_proxy_port = 18082
tls_mode = "behind_proxy"
```

Then point the tunnel origin at `http://localhost:80`. Public users still see
HTTPS from the tunnel/proxy, while firedoze serves plain HTTP only on the local
origin connection.

## Upgrade or Uninstall

To upgrade from a newer checkout, run the installer again:

```sh
git pull
mise install
./scripts/install.sh
sudo systemctl restart firedozed
```

The installer leaves existing config and VM state untouched.

Restarting the daemon temporarily interrupts the management API and public
proxy. Running VMs are slept during shutdown and automatically started again
after the new daemon comes up.

To remove installed binaries and the systemd unit while keeping config, images, VMs, snapshots, and logs:

```sh
sudo ./scripts/uninstall.sh
```

To remove everything, including config and all VM state:

```sh
sudo ./scripts/uninstall.sh -purge
```
