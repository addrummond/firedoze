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
- A Firecracker-compatible ext4 root filesystem image with SSH enabled.
- Go and a C compiler if building from source.

On Ubuntu, the host packages are roughly:

```sh
sudo apt-get update
sudo apt-get install -y build-essential git iptables wireguard-tools e2fsprogs openssh-client
```

## 2. Build and Install

From the repo:

```sh
go build -o firedozed ./cmd/firedozed
sudo install -m 0755 firedozed /usr/local/bin/firedozed
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

## 3. Install Base Images

Put your Firecracker kernel and root filesystem here:

```text
/var/lib/firedoze/images/vmlinux.bin
/var/lib/firedoze/images/rootfs.ext4
```

The current default assumes the guest supports root SSH. If your image uses another user, set `ssh.user` in the config.

## 4. Create SSH Keys for Guests

firedoze injects a shared authorized keys file into new VM disks.

```sh
sudo mkdir -p /etc/firedoze
cat ~/.ssh/id_ed25519.pub | sudo tee /etc/firedoze/authorized_keys
```

## 5. Create a WireGuard Peer Key

On your laptop:

```sh
wg genkey | tee firedoze-client.key | wg pubkey > firedoze-client.pub
cat firedoze-client.pub
```

Copy the public key. You will paste it into the server config.

## 6. Configure firedoze

Start from the resolved default config:

```sh
sudo /usr/local/bin/firedozed -print-config | sudo tee /etc/firedoze/firedoze.toml
```

Edit `/etc/firedoze/firedoze.toml`.

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
allowed_ips = ["10.77.0.2/32"]

[vm_network]
subnet = "10.88.0.0/16"

[ssh]
user = "root"
authorized_key_files = ["/etc/firedoze/authorized_keys"]

[firecracker]
binary_path = "/usr/local/bin/firecracker"
base_kernel_path = "/var/lib/firedoze/images/vmlinux.bin"
base_rootfs_path = "/var/lib/firedoze/images/rootfs.ext4"
default_vcpus = 1
default_memory_mib = 128
default_disk_bytes = 536870912
```

## 7. Firewall and DNS

Open these inbound ports to the host:

- UDP `51820` for WireGuard.
- TCP `80` and `443` later for public HTTPS routes. During early local testing, the default Caddy listener is `8080`.

Set public wildcard DNS for HTTP routes:

```text
*.dev.example.com -> your firedoze host public IP
```

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

## 9. Connect WireGuard

Once firedozed is running, fetch a client config template:

```sh
curl http://10.77.0.1:8081/wireguard/peers/alice-laptop/config
```

This only works after you already have some WireGuard route to the API. For the first connection, create the client config manually using the same values:

```ini
[Interface]
PrivateKey = PASTE_CLIENT_PRIVATE_KEY_HERE
Address = 10.77.0.2/32
DNS = 10.77.0.1

[Peer]
PublicKey = SERVER_PUBLIC_KEY
Endpoint = YOUR_SERVER_PUBLIC_IP_OR_DNS:51820
AllowedIPs = 10.77.0.1/32, 10.88.0.0/16
PersistentKeepalive = 25
```

The server public key is derived from `/etc/firedoze/wg.key`. On the server:

```sh
sudo cat /etc/firedoze/wg.key | wg pubkey
```

Bring the tunnel up on your laptop with `wg-quick` or your WireGuard client.

## 10. Use the API

All API commands go over WireGuard:

```sh
curl http://10.77.0.1:8081/
curl http://10.77.0.1:8081/health
curl http://10.77.0.1:8081/config
```

Create and start a VM:

```sh
curl -X POST http://10.77.0.1:8081/vms \
  -H 'Content-Type: application/json' \
  -d '{"name":"demo"}'

curl -X POST http://10.77.0.1:8081/vms/demo/start
```

List VMs:

```sh
curl http://10.77.0.1:8081/vms
```

SSH to the VM:

```sh
ssh root@demo.dev.example.com
```

Sleep or stop a VM:

```sh
curl -X POST http://10.77.0.1:8081/vms/demo/sleep
curl -X POST http://10.77.0.1:8081/vms/demo/stop
```

Save a named snapshot:

```sh
curl -X POST http://10.77.0.1:8081/snapshots \
  -H 'Content-Type: application/json' \
  -d '{"name":"demo-base","vm":"demo"}'
```

Restore a snapshot as a new VM:

```sh
curl -X POST http://10.77.0.1:8081/snapshots/demo-base/restore \
  -H 'Content-Type: application/json' \
  -d '{"vm":"demo-copy"}'
```

Create a public HTTP route alias:

```sh
curl -X POST http://10.77.0.1:8081/routes \
  -H 'Content-Type: application/json' \
  -d '{"name":"app","vm":"demo","port":8080}'
```

That route maps:

```text
https://app.dev.example.com -> demo VM port 8080
```

Early local builds may still be using the configured plain HTTP Caddy port instead of full Auto HTTPS.
