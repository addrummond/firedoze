# Quickstart

firedoze runs shared, persistent Firecracker dev VMs on one Linux host. The management API and VM SSH access are only reachable through WireGuard.

This is early dev software. Use it for shared development environments, not production.

## 1. Host Requirements

Use an x86_64 Linux box with:

- KVM available at `/dev/kvm`.
- Kernel WireGuard support.
- `iptables`, `debugfs`, and `ssh-keygen`.
- Firecracker installed at `/usr/local/bin/firecracker`.
- A Firecracker-compatible kernel image.
- A Firecracker-compatible initrd image if your kernel needs one.
- A Firecracker-compatible ext4 root filesystem image with SSH enabled.
- Go and a C compiler if building from source.

On Ubuntu, the host packages are roughly:

```sh
sudo apt-get update
sudo apt-get install -y build-essential git iptables wireguard-tools e2fsprogs openssh-client
```

## 2. Build and Install

If you use mise, install the pinned project tools first:

```sh
mise install
```

On the host, build and install the daemon:

```sh
task build:daemon
sudo install -m 0755 firedozed /usr/local/bin/firedozed
```

On your laptop, build and install the client command:

```sh
task build:client
sudo install -m 0755 firedoze /usr/local/bin/firedoze
```

Create the config and state directories:

```sh
sudo mkdir -p /etc/firedoze /var/lib/firedoze/images
```

Install the systemd unit:

```sh
sudo mkdir -p /usr/local/share/doc/firedoze
sudo install -m 0644 Quickstart.md /usr/local/share/doc/firedoze/Quickstart.md
sudo install -m 0644 contrib/systemd/firedozed.service /etc/systemd/system/firedozed.service
sudo systemctl daemon-reload
```

## 3. Build and Install Base Images

The easiest path is to build a firedoze Ubuntu base image on your laptop, then copy the artifacts to the Linux host. The builder is native Go; it does not require Docker, Podman, root, mounting, or host ext4 support.

On your laptop, run:

```sh
task image:build
```

The builder downloads pinned Ubuntu cloud image artifacts, verifies their SHA-256 checksums, turns the root tarball into a raw ext4 root filesystem, and adds the small firedoze guest configuration needed for SSH and Firecracker networking.

Copy the generated files to the host:

```sh
rsync -aSv dist/base-image/rootfs.ext4 dist/base-image/vmlinux.bin dist/base-image/initrd.img HOST:/tmp/firedoze-base-image/
```

On the host, install them here:

```text
/var/lib/firedoze/images/vmlinux.bin
/var/lib/firedoze/images/initrd.img
/var/lib/firedoze/images/rootfs.ext4
```

For example:

```sh
sudo mkdir -p /var/lib/firedoze/images
sudo install -m 0644 /tmp/firedoze-base-image/vmlinux.bin /var/lib/firedoze/images/vmlinux.bin
sudo install -m 0644 /tmp/firedoze-base-image/initrd.img /var/lib/firedoze/images/initrd.img
sudo install -m 0644 /tmp/firedoze-base-image/rootfs.ext4 /var/lib/firedoze/images/rootfs.ext4
```

The generated image uses the normal Ubuntu `ubuntu` user for SSH.

## 4. Create SSH Keys for Guests

firedoze injects a shared authorized keys file into new VM disks.

```sh
sudo mkdir -p /etc/firedoze
cat ~/.ssh/id_ed25519.pub | sudo tee /etc/firedoze/authorized_keys
```

## 5. Create a WireGuard Peer Key

On your laptop, use `firedozed` to generate a WireGuard client key pair. You can use a locally built `firedozed` binary for this; it does not need root for key generation.

```sh
firedozed -wg-gen-client-key
```

Save the private key somewhere safe. Copy the public key into the server config.

## 6. Configure firedoze

Start from the resolved default config:

```sh
sudo /usr/local/bin/firedozed -print-config | sudo tee /etc/firedoze/firedoze.toml
```

Edit `/etc/firedoze/firedoze.toml`.

The addresses below use one example WireGuard subnet:

- `10.77.0.1` is the firedoze host's WireGuard address.
- `10.77.0.2` is Alice's laptop WireGuard address. Give each peer a unique `/32` address in the WireGuard subnet.

Minimal fields to change:

```toml
base_domain = "dev.example.com"
default_http_port = 8080
state_dir = "/var/lib/firedoze"

[wireguard]
interface = "fdwg0"
listen_port = 51820
address = "10.77.0.1/24"
endpoint = "YOUR_SERVER_PUBLIC_IP_OR_DNS:51820"
private_key_file = "/etc/firedoze/wg.key"

[[wireguard.peers]]
name = "alice-laptop"
public_key = "PASTE_CLIENT_PUBLIC_KEY_HERE"
# This is Alice's WireGuard client address. Use a different /32 for each peer.
allowed_ips = ["10.77.0.2/32"]

[vm_network]
subnet = "10.88.0.0/16"

[caddy]
http_port = 80
https_port = 443
internal_proxy_port = 18082

[ssh]
user = "ubuntu"
authorized_key_files = ["/etc/firedoze/authorized_keys"]
wake_proxy_port = 18022

[firecracker]
binary_path = "/usr/local/bin/firecracker"
base_kernel_path = "/var/lib/firedoze/images/vmlinux.bin"
base_initrd_path = "/var/lib/firedoze/images/initrd.img"
base_rootfs_path = "/var/lib/firedoze/images/rootfs.ext4"
default_vcpus = 1
default_memory_mib = 512
default_disk_bytes = 4294967296
```

## 7. Firewall and DNS

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

## 8. Start firedozed

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

## 9. Connect WireGuard

Once firedozed is running, fetch a client config template:

```sh
firedozed -config /etc/firedoze/firedoze.toml -wg-peer-config alice-laptop
```

Replace `<client-private-key>` with the private key from `firedozed -wg-gen-client-key`, then save the config in your WireGuard client.

If you need to create the client config manually, use these values:

```ini
[Interface]
PrivateKey = PASTE_CLIENT_PRIVATE_KEY_HERE
# This must match the peer's allowed_ips entry in firedoze.toml.
Address = 10.77.0.2/32
DNS = 10.77.0.1

[Peer]
PublicKey = SERVER_PUBLIC_KEY
Endpoint = YOUR_SERVER_PUBLIC_IP_OR_DNS:51820
AllowedIPs = 10.77.0.1/32, 10.88.0.0/16
PersistentKeepalive = 25
```

The server public key is derived from `/etc/firedoze/wg.key`. On the server, print it with:

```sh
sudo firedozed -config /etc/firedoze/firedoze.toml -wg-server-public-key
```

Bring the tunnel up on your laptop with `wg-quick` or your WireGuard client.

## 10. Use the API

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
