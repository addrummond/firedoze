# Firedoze

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](go.mod)
[![Status](https://img.shields.io/badge/status-early_stage-orange)](#status)
[![Platform](https://img.shields.io/badge/platform-linux%20x86__64-lightgrey)](docs/quickstart-admin.md)

**Disposable Linux computers for your dev team, backed by [Firecracker](https://firecracker-microvm.github.io/).**

Firedoze runs persistent Linux VMs on a single Linux host. Each one behaves like a small computer: its own filesystem, systemd, SSH, and long-running processes.

✨ **Spin up a Linux VM in seconds and access it via SSH.**

✨ **Idle VMs sleep automatically, consuming only disk space.**

✨ **Give your VMs public https URLs with one command (certs managed automatically via [Caddy](https://caddyserver.com/)).**

✨ **Hit a sleeping VM's public https URL and it automatically resumes after a lightweight human check.**

✨ **Snapshot and clone VMs to quickly instantiate dev environments.**

✨ **Management interface secured via [Wireguard](https://www.wireguard.com/).**

⚠️ _Firedoze is early-stage software. Don't use it for production workloads or in production infrastructure accounts._

## Why not just use containers?

Because sometimes you want:

- A persistent root filesystem, without deciding up front which paths need volumes
- Systemd running in its normal role as PID 1
- VM snapshots you can clone and hand to a teammate
- Long-running background services managed like they would be on a server

Firedoze puts all that behind a simple model. One beefy box to run your VMs. One CLI. WireGuard authentication built into the client, so the management plane stays private.

Like modern container workflows, Firedoze does not force a single shape for a dev environment. You can run a whole stack together in one long-lived VM, or split services into smaller VMs built from scripts and snapshots. The difference is less about topology than substrate: VM isolation and full machine semantics rather than container boundaries.

Firedoze is heavily inspired by [Sprites](https://sprites.dev/). It borrows the idea of a persistent computer that can sleep cheaply when idle, then narrows the target to shared dev environments. This enables a much simpler implementation – no need to worry about a global fleet, production networking, durable object-storage layer, or a hosted platform.

## What you get

- **Firecracker-backed VMs** that boot fast and act like real Linux machines.
- **WireGuard** for securing the management endpoint.
- **Public HTTPS** for sharing a dev service with someone outside the tunnel.
- **Idle sleep with full state preservation** — sleeping VMs cost nothing but disk.
- **Optional cold storage** for long-stopped VMs.
- **Named snapshots and clones** — script a golden environment, clone it for everyone on the team.
- **Ordinary SSH** — use `ssh`, `scp`, `rsync`, and VS Code Remote SSH against VMs like normal Linux boxes.
- **One CLI** covering the full lifecycle: create, ssh, copy files, manage routes, snapshot, restore.
- **Dynamic resourcing**: VM RAM allocation grows and shrinks with demand; disk space grows with demand.

## Quick example

Create a VM, publish it, and drop in a tiny web app:

```sh
firedoze vm create launchpad -publish
firedoze vm start launchpad

firedoze exec launchpad -- sh -lc 'cat > index.html <<EOF
<h1>hello from $(hostname)</h1>
<p>this is a whole disposable computer, not a container</p>
EOF
busybox httpd -p 8080 -h .'

firedoze vm list launchpad
```

Open the `PUBLIC URL` from `firedoze vm list` in your browser. It is a real https URL for the service running inside that VM.

Start a second VM, then run a command inside it that calls the first VM by name
over the private VM network:

```sh
firedoze vm create cockpit
firedoze vm start cockpit
firedoze exec cockpit -- hostname
firedoze exec cockpit -- curl -fsS http://launchpad.firedoze:8080
```

Done for the day? Put your VMs to sleep. They keep everything and wake again when traffic arrives:

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

Nested virtualization tends not to be cost effective. It is useful for testing small Firedoze deployments without the commitment of a dedicated server, but is not recommended for sustained use.

Each VM requires around 512MB RAM to boot reliably.

## Documentation

- [User quickstart](docs/quickstart-user.md) — for people using an existing Firedoze server
- [Admin quickstart](docs/quickstart-admin.md) — for setting up and operating a Firedoze host
- [Release packages](docs/release-packages.md) — installing and verifying `.deb` / `.rpm` artifacts
- [Client WireGuard broker](docs/developer/client-wireguard-broker.md) — developer notes on embedded WireGuard and SSH proxying
- [ADR](docs/adr.md) — design decisions and the reasoning behind the scope

## Status

Firedoze is a prototype. It is for developers who are comfortable running early-stage infrastructure software and trusting it with their workflow.

Contributions and feedback welcome.
