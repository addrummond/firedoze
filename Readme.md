# Firedoze

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](go.mod)
[![Status](https://img.shields.io/badge/status-early_stage-orange)](#status)
[![Platform](https://img.shields.io/badge/platform-linux%20x86__64-lightgrey)](docs/quickstart-admin.md)

**Disposable Linux computers for your dev team, backed by [Firecracker](https://firecracker-microvm.github.io/).**

Firedoze runs persistent Linux VMs on a single Linux host. Each one behaves like a small computer: its own filesystem, systemd, SSH, and long-running processes.

🛠️ **Spin up a Linux VM in seconds and access it via SSH.**

🛠️ **Idle VMs sleep automatically, consuming only disk space.**

🛠️ **Give your VMs public https URLs with one command (certs managed automatically via [Caddy](https://caddyserver.com/)).**

🛠️ **Hit A sleeping VM's public https URL and it automatically resumes (subject to captcha).**

🛠️ **Snapshot and clone VMs to quickly create reproducible dev environments.**

🛠️ **Management interface secured via [Wireguard](https://www.wireguard.com/).**

⚠️ _Firedoze is early-stage software. Don't use it for production workloads or in production infrastructure accounts._

## Why not just use containers?

Because sometimes you want:

- A persistent filesystem that survives restarts without volume gymnastics
- `systemd` running like normal
- Ordinary SSH without any container runtime in the way
- Snapshots you can clone and hand to a teammate
- A place to run services that keep running

Firedoze puts all that behind a simple model. One beefy box to run your VMs. One CLI. WireGuard authentication built into the client, so the management plane stays private.

Unlike container workflows, Firedoze does not urge any particular shape for a dev environment. Prefer a single hand-tended VM running multiple services together? Fine. Prefer small per-service VMs built from scripts and snapshots? Also fine.

Firedoze is heavily inspired by [Sprites](https://sprites.dev/). It borrows the idea of a persistent computer that can sleep cheaply when idle, then narrows the target to shared dev environments. This enables a much simpler implementation – no need to worry about a global fleet, production networking, durable object-storage layer, or a hosted platform.

## What you get

- **Firecracker-backed VMs** that boot fast and act like real Linux machines.
- **WireGuard-only management access** — if your client is not authenticated, it cannot reach the management endpoint.
- **Public HTTPS** for sharing a dev service with someone outside the tunnel.
- **Idle sleep with full state preservation** — sleeping VMs cost nothing but disk.
- **Optional cold storage** for long-stopped VMs.
- **Named snapshots and clones** — script a golden environment, clone it for everyone on the team.
- **A single `firedoze` CLI** covering the full lifecycle: create, SSH, exec, copy files, manage routes, snapshot, restore.
- **Deliberately single-node** — one box, local SQLite, no scheduler, no cluster to babysit.

## Quick example

Create a VM, publish it, and drop in a tiny web app:

```sh
firedoze vm create launchpad -publish
firedoze vm start launchpad
firedoze exec launchpad -- sh -lc 'cat > app.html <<EOF
<h1>hello from $(hostname)</h1>
<p>this is a whole disposable computer, not a container</p>
EOF
busybox httpd -p 8080 -h .'
firedoze vm list launchpad
```

Open the `PUBLIC URL` from `firedoze vm list` in your browser. It is a real https URL for the service running inside that VM.

Start a second VM and call the first one by name over the private VM network:

```sh
firedoze vm create cockpit
firedoze vm start cockpit
firedoze exec cockpit -- wget -qO- http://launchpad.firedoze:8080
```

Done for the day? Put them to sleep. They keep everything and wake again when traffic arrives:

```sh
firedoze vm sleep cockpit
firedoze vm sleep launchpad
```

Or don't bother — Firedoze will sleep idle VMs automatically after a configurable timeout.

## Scope (what Firedoze is not)

Firedoze is a tool for a small, high-trust team.

- **No clustering.** No live migration, scheduler, or HA.
- **One shared trust boundary.** Access is gated by WireGuard. No built-in users, teams, or ACLs.
- **Fixed image.** One non-configurable Ubuntu-based VM image.
- **HTTPS ingress only.** Public routes are for HTTP services, not arbitrary TCP.

## Where can I host it?

Firedoze needs a Linux host that can run KVM-backed Firecracker VMs. That means either dedicated hardware or a cloud VM with nested virtualization support.

Low-cost options include small dedicated servers from providers like [Hetzner](https://www.hetzner.com), or VPS providers with nested virtualization support, such as [DigitalOcean](https://www.digitalocean.com).

_Note that while some VPSs in the $1-5 range may support nested virtualization, they are too RAM limited to usefully run multiple full Linux VMs. To reliably run a significant number of non-trivial VMs, you are probably looking at least in the $30/month range._

## Documentation

- [User quickstart](docs/quickstart-user.md) — for people using an existing Firedoze server
- [Admin quickstart](docs/quickstart-admin.md) — for setting up and operating a Firedoze host
- [Release packages](docs/release-packages.md) — installing and verifying `.deb` / `.rpm` artifacts
- [Client WireGuard broker](docs/developer/client-wireguard-broker.md) — developer notes on embedded WireGuard and SSH proxying
- [ADR](docs/adr.md) — design decisions and the reasoning behind the scope

## Status

Firedoze is a prototype. It is for developers who are comfortable running early-stage infrastructure software and trusting it with their workflow.

Contributions and feedback welcome.
