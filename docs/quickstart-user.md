# User Quickstart

This guide is for someone using an existing firedoze server. It does not cover installing or administering `firedozed`.

firedoze gives you persistent dev VMs that you can create, sleep, wake, SSH into, copy files to, and expose over public HTTPS when needed.

## 1. What You Need

Ask the firedoze admin for:

- A WireGuard peer config template for your laptop.

Install these local tools:

- The `firedoze` client, built from this repo.
- WireGuard, either the WireGuard app or `wg-quick`.
- `ssh`.
- `rsync`, if you want to use `firedoze cp`.

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

The generated WireGuard config includes a commented `firedoze server add ...` command. Run that command once after connecting. It saves the server's API URL in your local firedoze client config:

```sh
firedoze server add firedoze http://[fdxx:xxxx:xxxx:xxxx::1] -default
```

The client adds the default API port, `8081`, when the URL has no port. If the generated command includes a URL with a port, use it exactly.

If you use more than one firedoze server, add each one with a different name:

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
firedoze up demo
```

`up` creates the VM if needed, publishes its default HTTPS URL, starts it, waits for SSH, and connects you.

If you only want to wake an existing VM, use:

```sh
firedoze start demo
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
firedoze start demo
firedoze reboot demo
firedoze vm sleep demo
firedoze vm stop demo
firedoze vm delete demo
```

`sleep` keeps the VM's exact suspended state. `stop` shuts down the Firecracker process and keeps only the disk. `reboot` restarts from disk; if the VM is sleeping, it discards the suspended runtime state rather than resuming it.

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

## 7. Public Web Access

VMs created with `firedoze vm create` are ‘hidden‘ by default (i.e. they do not get a public HTTPS URL).

Publish or hide the default VM URL:

```sh
firedoze publish demo
firedoze hide demo
```

`firedoze up demo` publishes by default. To use `up` without publishing:

```sh
firedoze up demo -publish=false
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

firedoze VMs are meant to be cheap to forget about. When a VM is inactive for long enough, firedoze can sleep it automatically. A sleeping VM keeps its disk and suspended runtime state, but it stops using CPU and memory until it wakes again.

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
- Start the VM explicitly with `firedoze start demo`.
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

Use `firedoze start` when you definitely mean "wake this existing VM":

```sh
firedoze start demo
```

Use `firedoze up` when you want the more convenient workflow: create if missing, publish by default, start, and SSH.

## 9. Snapshots

Snapshots are named VM images you can restore later.

Stop the VM first, then save a snapshot:

```sh
firedoze vm stop demo
firedoze snapshot save demo-base demo
```

firedoze does not allow snapshotting a running or sleeping VM. Use `sleep` for
exact suspend/resume of the same VM, and `stop` before creating a cloneable
snapshot.

List and inspect snapshots:

```sh
firedoze snapshot list
firedoze snapshot inspect demo-base
```

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
- Try `firedoze start demo`.
- If WireGuard was reconfigured recently, disconnect and reconnect the tunnel.

If a public URL shows a human check, complete it in the browser. firedoze uses that check to avoid waking sleeping VMs for ordinary scanner traffic.
