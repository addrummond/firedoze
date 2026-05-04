# SSH Proxy

Firedoze VMs run ordinary `sshd` on their private WireGuard-routed address.
`firedoze ssh <vm>` is the simplest way to connect interactively, but some
tools expect a normal OpenSSH `Host` entry. VS Code Remote SSH is the main
example.

`firedoze ssh-proxy <vm>` exists for those tools.

## What It Does

`ssh-proxy` is designed for OpenSSH `ProxyCommand`.

When OpenSSH starts it, the proxy:

- Looks up the VM through the Firedoze API.
- Starts the VM if it is stopped or sleeping.
- Waits until guest SSH is reachable.
- Opens a TCP connection to the VM private address on port 22.
- Copies bytes between OpenSSH and that TCP connection.

It does not replace SSH, terminate SSH, or handle SSH authentication itself.
OpenSSH still talks to the guest `sshd`; the proxy is just the connection pipe.

## Use Cases

Use `ssh-proxy` when a tool wants a normal SSH host name but you still want
Firedoze to handle VM wakeup and private-IP lookup.

This is useful for:

- VS Code Remote SSH.
- Any tool that reads `~/.ssh/config`.
- Scripts that need normal `ssh <host>` behavior.

## Example

Find the absolute path to the trusted `firedoze` binary:

```sh
command -v firedoze
```

Add this to `~/.ssh/config`, replacing `/usr/local/bin/firedoze` with that
absolute path:

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

Then connect with standard OpenSSH:

```sh
ssh demo.firedoze
```

Use the same host name in VS Code Remote SSH.

## Security Note

`ProxyCommand` runs a local program on your laptop. That is normal OpenSSH
behavior, but it means the config should use an absolute path to a `firedoze`
binary you trust.

The SSH session itself is still encrypted by OpenSSH. Firedoze's management API
traffic and the VM private network are protected by WireGuard.
