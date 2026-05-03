# firedoze

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Disposable Linux computers for shared dev.

firedoze runs persistent Firecracker VMs on one Linux host. You get:

- WireGuard-gated management access.
- Simple command-line lifecycle controls.
- Optional public HTTPS for dev services.

Create and forget! Make a VM. Use it like a small computer. When it goes idle, it sleeps. Sleeping VMs keep their state and consume only disk space.

Reproducibility is optional. Fire and forget – or script creation of named snapshots for cloneable environments. 

⚠️ _firedoze is early-stage software. Do not use it for production, hostile multi-tenant isolation, or sensitive infrastructure accounts._

## Why

Containers are useful. Sometimes you want a real machine: a persistent filesystem, systemd, normal SSH, long-running services, snapshots, and fewer container-specific assumptions.

firedoze makes that lightweight enough for everyday team development.

💡 **Did you know?** AWS enabled nested virtualization on virtual EC2 instances in February 2026. It started with C8i, M8i, and R8i. You no longer need bare-metal EC2 just to run KVM-backed dev VMs on AWS. See the [AWS guide](docs/aws-guide.md) for notes.

## Highlights

- Firecracker-backed Linux VMs that behave like small persistent computers.
- WireGuard-only management access.
- Automatic public HTTPS routes for sharing running dev services.
- Sleeping VMs that keep state while freeing CPU and memory.
- Named snapshots and restores for cloneable environments.
- A simple `firedoze` client for VM lifecycle, SSH, exec, copy, routes, and snapshots.
- Native Go base-image builder; no Docker or Podman required.
- Single-node by design: one beefy box, local SQLite, no scheduler, no cluster.

## Current Scope

firedoze is deliberately narrow in scope:

- One host only; no clustering, scheduling, live migration, or high availability features.
- One WireGuard-gated shared trust boundary per server; no built-in users, teams, ACLs, or non-WireGuard management access.
- A fixed Ubuntu-based VM image.
- Public ingress is focused on HTTPS routes for dev services, not arbitrary TCP exposure.

## Example

Spin up a fresh Linux VM, drop in a tiny web app, publish it, and hand someone
the HTTPS URL:

```sh
firedoze up launchpad

firedoze exec launchpad -- sh -lc 'cat > app.html <<EOF
<h1>hello from $(hostname)</h1>
<p>this is a whole disposable computer, not a container</p>
EOF
busybox httpd -f -p 8080 -h .'

firedoze vm list launchpad
```

Start another VM and call the first one by name on the private VM network:

```sh
firedoze up cockpit -publish=false
firedoze exec cockpit -- wget -qO- http://launchpad.firedoze:8080
```

When you are done, sleep it. It keeps its state, but stops burning CPU and
memory:

```sh
firedoze vm sleep cockpit
firedoze vm sleep launchpad
```

Too lazy to run the sleep command? Don't worry! firedoze will sleep the VM automatically after a configurable idle timeout.
The VM wakes again when network traffic arrives, so the next (verifiably human) request to
its HTTPS URL brings it back.

## Documentation

- [User quickstart](docs/quickstart-user.md): for people using an existing firedoze server.
- [Admin quickstart](docs/quickstart-admin.md): for setting up and operating a firedoze host.
- [AWS guide](docs/aws-guide.md): EC2 notes, nested virtualization, and bastion ideas.
- [ADR](docs/adr.md): design decisions and project scope.

## Status

firedoze is currently a prototype moving toward basic functional completeness. The intended audience is developers comfortable running early infrastructure software and reading the docs before trusting it.
