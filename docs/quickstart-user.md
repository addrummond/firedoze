# User Quickstart

This guide is for someone using an existing Firedoze server. It does not cover installing or administering `firedozed`.

Firedoze gives you persistent dev VMs that you can create, sleep, wake, SSH into, copy files to, and expose over public HTTPS when needed.

## 1. What You Need

Ask the Firedoze admin for:

- A WireGuard peer config template for your laptop.

Install these local tools:

- The `firedoze` client, built from this repo
- WireGuard, either the WireGuard app or `wg-quick`
- `ssh`
- `rsync`, if you want to use `firedoze cp`

## 2. Generate Your WireGuard Key

Generate a key pair on your laptop:

```sh
firedoze wg keygen
```

Send only the `public_key` to the admin. Keep the `private_key` on your laptop.

The admin will send you a WireGuard config template. Replace this line:

```ini
PrivateKey = <client-private-key>
```

with your generated private key.

Do not change the `Address` value unless the admin tells you to. That address has to match the server-side peer config.

## 3. Connect

Save the WireGuard config somewhere private, then connect with your WireGuard app or with `wg-quick`:

```sh
sudo wg-quick up /path/to/firedoze.conf
```

The generated WireGuard config includes a commented `firedoze server add ...` command. Run that command once after connecting. It saves the server's API URL in your local Firedoze client config:

```sh
firedoze server add firedoze http://[fdxx:xxxx:xxxx:xxxx::1] -default
```

If you use more than one Firedoze server, add each one with a different name:

```sh
firedoze server list
firedoze server use work
```

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

Inspect one VM:

```sh
firedoze vm inspect demo
```

Create one or more VMs (hidden from public web by default, not started):

```sh
firedoze vm create demo
firedoze vm create app1 app2 app3 -memory-mib 1024
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

### OpenSSH And Editor Integrations

Tools such as VS Code Remote SSH usually want a normal OpenSSH host entry. Use
`firedoze ssh-proxy <vm>` as a `ProxyCommand`. The proxy starts or wakes the VM
if needed, waits for guest SSH, then pipes OpenSSH to the VM private address.

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
  ProxyCommand /usr/local/bin/firedoze ssh-proxy demo
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

## 8. Sleep And Autowake

Firedoze VMs are meant to be cheap to forget about. When a VM is inactive for long enough, Firedoze can sleep it automatically. A sleeping VM keeps its disk and suspended runtime state, but it stops using CPU and memory until it wakes again.

The server has a default idle timeout (6 hours, in the default configuration). Your VM can override it:

```sh
firedoze vm settings demo -idle-sleep-after 3600 # 1 hour
```

The value is in seconds.

Autowake controls whether passive network activity is allowed to wake a sleeping VM. It is enabled by default for newly created VMs.

When autowake is enabled:

- `firedoze ssh demo`, `firedoze exec demo -- ...`, and `firedoze cp ... demo:...` will start the VM if needed before connecting.
- A request to a published HTTPS URL can wake the VM (guarded by a captcha check to stop nuisance traffic from waking sleeping VMs).

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
firedoze snapshot restore demo-base bigger-demo -memory-mib 2048 -vcpus 2
```

Delete a snapshot:

```sh
firedoze snapshot delete demo-base
```

## 10. Disconnect

When you are done, you can leave WireGuard connected or bring it down:

```sh
sudo wg-quick down /path/to/firedoze.conf
```

## Troubleshooting

If `firedoze health` fails:

- Check that WireGuard is connected.
- Run `firedoze server current` and check that a server is configured.
- Ask the admin whether the server is up and whether your peer is configured.

If `firedoze ssh demo` hangs:

- Run `firedoze vm inspect demo` and check the VM state.
- Try `firedoze vm start demo`.
- If WireGuard was reconfigured recently, disconnect and reconnect the tunnel.

If a public URL shows a human check, complete it in the browser. Firedoze uses that check to avoid waking sleeping VMs for ordinary scanner traffic.
