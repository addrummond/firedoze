# User Quickstart

This guide is for someone using an existing Firedoze server. It does not cover installing or administering `firedozed`.

## 1. What You Need

Ask the Firedoze admin for:

- Permission to add your laptop as a Firedoze WireGuard peer.

Install these local tools:

- The `firedoze` client, built from this repo
- `ssh`
- `rsync`, if you want to use `firedoze cp`

## 2. Request Access

Create a local access request:

```sh
firedoze server request alice-laptop
```

This generates a WireGuard key pair on your laptop and stores the private key in
your local Firedoze client config. `alice-laptop` is the name of this access
request and the WireGuard peer the admin will add on the server. Send only the
printed public key, or the printed admin command, to the Firedoze admin.

Do not send your private key to anyone. The admin does not need it.

If you have already created the request and need to print the public key again:

```sh
firedoze wg pubkey alice-laptop
```

Use the same name you passed to `firedoze server request`.

## 3. Import The Server Config

The admin will send back a Firedoze client import config. Save it to a local
file named after what you want to call this server locally, then import it:

```sh
firedoze server import /path/to/team-dev.conf -default
```

That creates a local server profile named `team-dev`. If you want a different
name, rename the file before importing it or pass `-name`.

You can also import from stdin:

```sh
firedoze server import - -name team-dev -default < /path/to/team-dev.conf
```

For normal `firedoze` commands, you do not need to bring up WireGuard manually.
The client uses the imported WireGuard details internally for API calls, SSH,
`exec`, and `cp`.

If you use more than one Firedoze server, import each one with a different name:

```sh
firedoze server list
firedoze server use team-dev
```

This guide uses `alice-laptop` as the example laptop/peer name and `team-dev` as
the example local server profile name.

Check that everything is reachable:

```sh
firedoze health
```

## 4. Create And Enter A VM

The quickest way to get a VM is:

```sh
firedoze vm up demo
```

`vm up` creates the VM if needed, publishes its default HTTPS URL, starts it, waits for SSH, and connects you.

If you only want to wake an existing VM, use:

```sh
firedoze vm start demo
firedoze ssh demo
```

This avoids accidentally creating a new VM because of a typo.

## 5. Daily Commands

List your VMs:

```sh
firedoze vm list
```

Filter the list with globs:

```sh
firedoze vm list 'demo*'
```

Print just the matching VM names, one per line, for scripts:

```sh
firedoze vm list -names 'demo*'
```

Print just matching VM UUIDs:

```sh
firedoze vm list -ids 'demo*'
```

Inspect one VM:

```sh
firedoze vm inspect demo
```

Get one VM's UUID:

```sh
firedoze vm id demo
```

Commands that take `<vm>` accept either the VM name or its UUID. Names are
usually easier for humans; UUIDs are useful for scripts that need a stable
identity across renames.

Check resource usage across VMs:

```sh
firedoze vm usage
firedoze vm usage 'demo*'
```

`MEMORY` is the configured min-max range. `HOTPLUG` shows how much extra
virtio-mem memory is currently plugged/requested. `HOST MEM`, `HOST CPU`, and
`HOST IO` are host-side metrics. `HOST MEM` uses the best host-side value
Firedoze has, usually process RSS when that is larger than cgroup memory
accounting. `HOST IO` is read/write bytes. `GUEST DISK FREE/TOTAL` is reported
from inside the VM, so it shows the filesystem space the VM user can actually
use.

### Resource Allocation

VM memory is elastic when the host supports Firecracker virtio-mem. The minimum
memory is what the VM boots with. The maximum memory is the cap Firedoze can grow
to under pressure. When pressure goes away, Firedoze gradually asks the VM to
give hotplugged memory back, down to the configured minimum.

VM disk size is different. The configured disk size is the capacity the guest
sees, not necessarily the amount of host disk consumed immediately. On a server
using XFS or another reflink/sparse-file capable filesystem, new VM disks should
be cheap to create and mostly consume host space as the VM writes new data. If
the guest deletes files, guest free space increases, but host disk allocation may
not shrink automatically.
Create one or more VMs (hidden from public web by default, not started):

```sh
firedoze vm create demo
firedoze vm create app1 app2 app3 -memory-min-mib 256 -memory-max-mib 1024
```

Start, reboot, sleep, stop, or delete VMs:

```sh
firedoze vm start demo
firedoze vm reboot demo
firedoze vm sleep demo
firedoze vm stop demo
firedoze vm delete demo
```

`sleep` keeps the VM's exact suspended state. `stop` shuts down the Firecracker process and keeps only the disk. If the server has cold storage enabled, long-stopped VM disks may be moved to slower storage and restored automatically the next time you start them. `reboot` restarts from disk; if the VM is sleeping, it discards the suspended runtime state rather than resuming it.

Inside the VM, use `firedoze-stop` if you want to stop the VM from its own
shell:

```sh
firedoze-stop
```

On x86_64 Firecracker this is implemented with the guest `reboot` command,
because that exits the microVM cleanly. Firedoze will then mark the VM as
`stopped`. Avoid using `shutdown -h now`, `poweroff`, or `halt` as a way to stop
a Firedoze VM: those commands can stop the guest OS while leaving the
Firecracker process alive, so Firedoze may still see the VM as `running`.

## 6. SSH, Commands, And Files

Open a shell:

```sh
firedoze ssh demo
```

Run a command and wait for it to finish:

```sh
firedoze exec demo -- sh -lc 'uname -a && uptime'
```

Copy files to and from the VM:

```sh
firedoze cp ./app/ demo:/home/ubuntu/app/
firedoze cp demo:/home/ubuntu/app/results.log ./results.log
```

Run a local command with the VM private IP available as `FIREDOZE_VM_IP`:

```sh
firedoze with-vm-ip demo sh -c 'printf "%s\n" "$FIREDOZE_VM_IP"'
```

`with-vm-ip` only gives a command the address. It does not create operating
system routes. For file copy and shell access, prefer `firedoze cp`, `ssh`, and
`exec`, which use Firedoze's built-in WireGuard transport.

### OpenSSH And Editor Integrations

Tools such as VS Code Remote SSH usually want a normal OpenSSH host entry. Use
`firedoze ssh-proxy <vm>` as a `ProxyCommand`. The proxy starts or wakes the VM
if needed, waits for guest SSH, then pipes OpenSSH to the VM private address.
See [ssh-proxy.md](ssh-proxy.md) for the concise reference guide.

First find the absolute path to your trusted `firedoze` binary:

```sh
command -v firedoze
```

Then add a host entry like this to `~/.ssh/config`, replacing the
`ProxyCommand` path with the absolute path from the previous command:

```sshconfig
Host demo.firedoze
  HostName demo.firedoze
  User ubuntu
  ProxyCommand /usr/local/bin/firedoze -server team-dev ssh-proxy demo
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
  PubkeyAuthentication no
  PreferredAuthentications none,password
  NumberOfPasswordPrompts 1
```

Then connect with standard SSH tooling:

```sh
ssh demo.firedoze
```

Use the same `Host` name in VS Code Remote SSH. The `ProxyCommand` is a local
program execution hook, so use an absolute path to a `firedoze` binary you
trust.

### Containers Inside A VM

Firedoze VMs are normal Ubuntu machines. If a project benefits from containers,
install a daemonless runtime such as Podman inside the VM and use it there:

```sh
firedoze ssh demo
sudo apt-get update
sudo apt-get install -y podman buildah crun
podman run --rm hello-world
```

That is an optional in-VM workflow. Firedoze itself does not require Docker,
Podman, or any container runtime.

## 7. Public Web Access

VMs created with `firedoze vm create` are ‘hidden‘ by default (i.e. they do not get a public HTTPS URL).

Publish or hide the default VM URL:

```sh
firedoze vm publish demo
firedoze vm hide demo
```

`firedoze vm up demo` publishes by default. To use `vm up` without publishing:

```sh
firedoze vm up demo -publish=false
```

The default public route proxies to port `8080` inside the VM. Custom services should listen on IPv6, for example:

```sh
my-server -listen '[::]:8080'
```

To test quickly inside the VM:

```sh
firedoze ssh demo
firedoze-hello
```

Then open the public URL shown by:

```sh
firedoze vm list
```

Create another public route to a specific VM port:

```sh
firedoze route create app demo 3000
```

Your admin's domain decides the final hostname, for example:

```text
https://app.dev.example.com -> demo port 3000
```

Protect a public hostname when you want only people with a signed access URL to get through:

```sh
firedoze route protect app.dev.example.com
firedoze route get-signed-url app.dev.example.com
```

Signed access URLs last 24 hours by default. Use `-ttl seconds` to choose a shorter or longer lifetime.

Unprotect it later:

```sh
firedoze route unprotect app.dev.example.com
```

Protection is independent of route creation. You can protect a hostname before the VM or route exists.

## 8. Sleep And Autowake

Firedoze VMs are meant to be cheap to forget about. When a VM is inactive for long enough, Firedoze can sleep it automatically. A sleeping VM keeps its disk and suspended runtime state, but it stops using CPU and memory until it wakes again.

The server has a default idle timeout (6 hours, in the default configuration). Your VM can override it:

```sh
firedoze vm settings demo -idle-sleep-after 3600 # 1 hour
```

The value is in seconds.

Firedoze counts user-facing activity, such as public HTTPS requests and client sessions opened with `firedoze ssh`, `firedoze exec`, `firedoze cp`, or the SSH proxy. Internal VM management traffic, such as guest resource reporting, does not keep a VM awake.

Autowake controls whether passive network activity is allowed to wake a sleeping VM. It is enabled by default for newly created VMs.

When autowake is enabled:

- `firedoze ssh demo`, `firedoze exec demo -- ...`, and `firedoze cp ... demo:...` will start the VM if needed before connecting.
- A request to a published HTTPS URL can wake the VM after the browser passes a small human check. Protected routes require a signed access URL first.

When autowake is disabled:

- Public HTTPS requests will not wake the VM.
- Start the VM explicitly with `firedoze vm start demo`.
- `firedoze ssh`, `firedoze exec`, and `firedoze cp` still try to make the VM ready because they are explicit client commands.

Disable autowake when creating a VM:

```sh
firedoze vm create demo -no-auto-wake
```

Disable or re-enable autowake later:

```sh
firedoze vm settings demo -auto-wake false
firedoze vm settings demo -auto-wake true
```

Check the current setting:

```sh
firedoze vm inspect demo
```

Use `firedoze vm start` when you definitely mean "wake this existing VM":

```sh
firedoze vm start demo
```

Use `firedoze vm up` when you want the more convenient workflow: create if missing, publish by default, start, and SSH.

## 9. Snapshots

Snapshots are named cloneable disk images you can restore later.

Stop the VM first, then save a snapshot:

```sh
firedoze vm stop demo
firedoze snapshot save demo-base demo
```

Firedoze does not allow snapshotting a running or sleeping VM. Use `sleep` for
exact suspend/resume of the same VM, and `stop` before creating a cloneable
snapshot.

A sleeping VM includes live memory and device state that belongs to that exact
VM identity. A snapshot restore creates a new VM identity, so it uses a clean
disk snapshot and boots normally.

List and inspect snapshots:

```sh
firedoze snapshot list
firedoze snapshot inspect demo-base
```

Export a snapshot to a portable file:

```sh
firedoze snapshot export demo-base demo-base.firedoze-snapshot.tgz
```

Import that file as a snapshot on another Firedoze server:

```sh
firedoze snapshot import demo-base demo-base.firedoze-snapshot.tgz
```

The imported snapshot name does not have to match the original name.

Restore a snapshot as a new VM:

```sh
firedoze snapshot restore demo-base demo-copy
```

Restore with larger resources:

```sh
firedoze snapshot restore demo-base bigger-demo -memory-min-mib 512 -memory-max-mib 2048 -vcpus 2
```

Delete a snapshot:

```sh
firedoze snapshot delete demo-base
```

## 10. Connection Lifecycle

For normal Firedoze commands there is no manual tunnel to disconnect. The client
starts or reuses a local per-server WireGuard broker when a command needs the
server. The broker exits automatically after it has been idle for several
minutes.

## Troubleshooting

If `firedoze health` fails:

- Run `firedoze server current` and check that a server is configured.
- Check that your imported server config has `WIREGUARD` set to `yes` in `firedoze server list`.
- Ask the admin whether the server is up and whether your peer is configured.

If `firedoze ssh demo` hangs:

- Run `firedoze vm inspect demo` and check the VM state.
- Try `firedoze vm start demo`.
- If your peer was reconfigured recently, ask the admin to send a fresh import config and run `firedoze server import <file>`.

If a public URL shows a human check, complete it in the browser. Firedoze uses that check to avoid waking sleeping VMs for ordinary scanner traffic.
