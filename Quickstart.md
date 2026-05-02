# Quickstart

firedoze runs shared, persistent Firecracker dev VMs on one Linux host. The management API and VM SSH access are only reachable through WireGuard.

This is early dev software. Use it for shared development environments, not production.

## 1. Host Requirements

Use an x86_64 Linux box with:

- KVM available at `/dev/kvm`.
- Kernel WireGuard support.
- `iptables`, `debugfs`, `ssh-keygen`, and `systemd`.
- Firecracker installed at `/usr/local/bin/firecracker`.
- Enough disk space to build and store base images, VM disks, and snapshots.

On Ubuntu, the host packages are roughly:

```sh
sudo apt-get update
sudo apt-get install -y build-essential ca-certificates git iptables wireguard-tools e2fsprogs openssh-client
```

## 2. Build and Install

Install `mise` on the Linux host. The project uses `.tool-versions` to pin the Go toolchain and Task version.

```sh
curl https://mise.run/bash | sh
source ~/.bashrc
```

Clone the private repo on the Linux host, install the pinned tools, then run the installer from the repo root:

```sh
git clone REPO_URL firedoze
cd firedoze
mise install
./scripts/install.sh
```

The setup shape is:

```sh
./scripts/install.sh
./firedoze-image build
task image:install
cat ~/.ssh/id_ed25519.pub | sudo tee /etc/firedoze/authorized_keys
sudoedit /etc/firedoze/firedoze.toml
sudo firedozed -wg-new-peer alice-laptop 10.77.0.2/32
# paste the printed [[wireguard.peers]] block into /etc/firedoze/firedoze.toml
sudo systemctl enable --now firedozed
```

The installer:

- Builds `firedoze`, `firedozed`, and `firedoze-image` from the checked-out source.
- Installs them to `/usr/local/bin`.
- Creates `/etc/firedoze`, `/var/lib/firedoze`, `/var/lib/firedoze/images`, and `/var/log/firedoze`.
- Installs an example config at `/etc/firedoze/firedoze.toml` if that file does not already exist.
- Installs the systemd unit and reloads systemd.

Existing config and VM state are left alone when you reinstall.

## 3. Build and Install Base Images

Build the firedoze Ubuntu base image on the Linux host. The builder is native Go; it does not require Docker, Podman, root, mounting, or host ext4 support.

From the repo checkout, run:

```sh
./firedoze-image build
task image:install
```

The builder downloads pinned Ubuntu cloud image artifacts, verifies their SHA-256 checksums, turns the root tarball into a raw ext4 root filesystem, and adds the small firedoze guest configuration needed for SSH and Firecracker networking.

The install task copies the generated files here:

```text
/var/lib/firedoze/images/vmlinux.bin
/var/lib/firedoze/images/initrd.img
/var/lib/firedoze/images/rootfs.ext4
```

The generated image uses the normal Ubuntu `ubuntu` user for SSH.

## 4. Configure firedoze

firedoze injects a shared authorized keys file into new VM disks.

```sh
cat ~/.ssh/id_ed25519.pub | sudo tee /etc/firedoze/authorized_keys
```

Edit the installed config:

```sh
sudoedit /etc/firedoze/firedoze.toml
```

The main fields to set are:

- `base_domain`: the wildcard DNS domain for VM URLs.
- `wireguard.endpoint`: the public host and UDP port laptops will connect to.
- `wireguard.peers`: one peer per laptop.
- `firecracker.default_memory_mib`: the default VM memory size.

To add a laptop, generate a peer setup bundle on the host:

```sh
sudo firedozed -wg-new-peer alice-laptop 10.77.0.2/32
```

The command prints:

- a `[[wireguard.peers]]` TOML block to paste into `/etc/firedoze/firedoze.toml`.
- a WireGuard client config to save on that laptop.

`10.77.0.2/32` is Alice's WireGuard client address. Use a different `/32` address for each laptop.

## 5. Firewall and DNS

Open these inbound ports to the host:

- UDP `51820` for WireGuard.
- TCP `80` and `443` for public web routes.

Set public wildcard DNS for web routes:

```text
*.dev.example.com -> your firedoze host public IP
```

Caddy obtains certificates automatically for each VM or route hostname. The host must be publicly reachable on ports `80` and `443`, and the wildcard DNS must point at the host.

firedoze also runs a private DNS server on the WireGuard IP. It resolves default VM names like:

```text
myvm.dev.example.com -> VM private IP
```

## 6. Start firedozed

```sh
sudo systemctl enable --now firedozed
sudo systemctl status firedozed
```

Logs:

```sh
journalctl -u firedozed -f
```

When systemd stops firedozed, the daemon tries to sleep all running VMs before exit.

The provided unit uses systemd readiness notification and a watchdog. If the daemon stops sending watchdog pings, systemd will restart it.

## 7. Connect WireGuard

Save the WireGuard client config printed by `-wg-new-peer` on your laptop, then bring the tunnel up with `wg-quick` or your WireGuard client.

The generated config includes the laptop's WireGuard `Address`. That address comes from the peer's `allowed_ips` entry in `/etc/firedoze/firedoze.toml`. For the example above, Alice's config will contain:

```ini
Address = 10.77.0.2/32
```

Do not invent a different client address on the laptop. Change the peer's `allowed_ips` entry on the server first, then regenerate the client config if needed:

```sh
sudo firedozed -wg-peer-config alice-laptop
```

## 8. Use firedoze

The `firedoze` client runs on your laptop and talks to the WireGuard-only API. If your server WireGuard address is not `10.77.0.1` or your API port is not `8081`, set `FIREDOZE_API`:

```sh
export FIREDOZE_API=http://10.77.0.1:8081
```

Check that the API is reachable:

```sh
firedoze health
```

Create and start a VM:

```sh
firedoze vm create demo
firedoze vm start demo
```

Create several VMs with the same settings:

```sh
firedoze vm create alice bob charlie --memory-mib 512 --disk-bytes 8589934592
```

Update a VM's firedoze settings, such as default HTTP port or idle timeout:

```sh
firedoze vm settings demo --http-port 3000 --idle-sleep-after 900
```

This changes firedoze metadata. It does not edit the guest disk, rename the VM, or change an exact sleep snapshot.

List VMs and SSH to one:

```sh
firedoze vm list
firedoze vm inspect demo
firedoze ssh demo
```

Run the built-in hello web server inside the VM:

```sh
firedoze ssh demo
firedoze-hello
```

In another terminal on your laptop, open or curl the VM URL shown by `firedoze vm list`. The default route proxies to port `8080`, which is also the default `firedoze-hello` port.

Sleep or stop a VM:

```sh
firedoze vm sleep demo
firedoze vm stop demo
```

Delete a VM and its state directory:

```sh
firedoze vm delete demo
# or delete several at once:
firedoze vm delete demo old-test scratch
```

Save a named snapshot:

```sh
firedoze snapshot save demo-base demo
```

Restore a snapshot as a new VM:

```sh
firedoze snapshot restore demo-base demo-copy
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

For scripts that need exact API response bodies from table-oriented commands, add `--json`. Inspect commands already print the single resource as JSON:

```sh
firedoze --json vm list
firedoze vm inspect demo
firedoze snapshot inspect demo-snap
```

## Reference Config

The installed config starts from this shape:

```toml
base_domain = "dev.example.com"
default_http_port = 8080
state_dir = "/var/lib/firedoze"

[api]
port = 8081

[wireguard]
interface = "fdwg0"
listen_port = 51820
address = "10.77.0.1/24"
endpoint = "YOUR_SERVER_PUBLIC_IP_OR_DNS:51820"
private_key_file = "/etc/firedoze/wg.key"

[[wireguard.peers]]
name = "alice-laptop"
public_key = "PASTE_CLIENT_PUBLIC_KEY_HERE"
allowed_ips = ["10.77.0.2/32"]

[vm_network]
subnet = "10.88.0.0/16"

[caddy]
http_port = 80
https_port = 443
internal_proxy_port = 18082

[dns]
port = 53

[metadata]
path = "/var/lib/firedoze/firedoze.db"

[ssh]
user = "ubuntu"
authorized_key_files = ["/etc/firedoze/authorized_keys"]
wake_proxy_port = 18022

[idle]
check_interval_seconds = 30
default_sleep_after_seconds = 1800

[firecracker]
binary_path = "/usr/local/bin/firecracker"
base_kernel_path = "/var/lib/firedoze/images/vmlinux.bin"
base_initrd_path = "/var/lib/firedoze/images/initrd.img"
base_rootfs_path = "/var/lib/firedoze/images/rootfs.ext4"
default_vcpus = 1
default_memory_mib = 512
default_disk_bytes = 4294967296
```

## Upgrade or Uninstall

To upgrade from a newer checkout, run the installer again:

```sh
git pull
mise install
./scripts/install.sh
sudo systemctl restart firedozed
```

The installer leaves existing config and VM state untouched.

To remove installed binaries and the systemd unit while keeping config, images, VMs, snapshots, and logs:

```sh
sudo ./scripts/uninstall.sh
```

To remove everything, including config and all VM state:

```sh
sudo ./scripts/uninstall.sh --purge
```
