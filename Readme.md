# firedoze

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](go.mod)
[![Status](https://img.shields.io/badge/status-early_stage-orange)](#status)
[![Platform](https://img.shields.io/badge/platform-linux%20x86__64-lightgrey)](docs/quickstart-admin.md)

**Disposable Linux computers for your dev team, backed by [Firecracker](https://firecracker-microvm.github.io/).**

firedoze runs persistent Linux VMs on a single Linux host. Each one behaves like a small computer: its own filesystem, systemd, SSH, and long-running processes.

**Spin a VM up in seconds. Work in it normally. When it goes idle, it sleeps automatically, consuming only disk space. If it has a public link, a click weeks later can wake it right back up.**

> ⚠️ firedoze is early-stage software. Don't use it for production workloads, hostile multi-tenant isolation, or infrastructure you'd be upset to lose.

---

## Why not just use containers?

Containers are great, but sometimes you want:

- A persistent filesystem that survives restarts without volume gymnastics
- `systemd` running like normal
- Ordinary SSH without any container runtime in the way
- Snapshots you can clone and hand to a teammate
- A place to run services that keep running

firedoze puts that behind one simple model. One beefy box. One CLI. One WireGuard tunnel to keep the management plane private.

Unlike container workflows, firedoze does not impose a single blessed shape for a dev environment. Prefer a single hand-tended VM? Fine. Prefer small per-service VMs built from scripts and snapshots? Also fine.

> 💡 **AWS quietly made this easier.** In February 2026, AWS enabled nested virtualization on C8i, M8i, and R8i instances. You no longer need bare-metal EC2 to run KVM-backed VMs on AWS. See the [AWS guide](docs/aws-guide.md). Other low-cost options include small dedicated servers from providers like [Hetzner](https://www.hetzner.com), or VPS providers with nested virtualization support, such as [DigitalOcean](https://www.digitalocean.com).

## What you get

- **Firecracker-backed VMs** that boot fast and act like real Linux machines
- **WireGuard-only management access** — if you're not in the tunnel, you can't reach the management endpoint
- **Automatic public HTTPS routes** for sharing a dev service with someone outside the tunnel
- **Idle sleep with full state preservation** — sleeping VMs cost nothing but disk
- **Named snapshots and clones** — script a golden environment, clone it for everyone on the team
- **A single `firedoze` CLI** covering the full lifecycle: create, SSH, exec, copy files, manage routes, snapshot, restore
- **Native Go image builder** — no Docker or Podman required
- **Deliberately single-node** — one box, local SQLite, no scheduler, no cluster to babysit

## Quick example

Create a VM, publish it, and drop in a tiny web app:

```sh
firedoze vm create launchpad -publish
firedoze start launchpad
firedoze exec launchpad -- sh -lc 'cat > app.html <<EOF
<h1>hello from $(hostname)</h1>
<p>this is a whole disposable computer, not a container</p>
EOF
busybox httpd -p 8080 -h .'
firedoze vm list launchpad
```

Start a second VM and call the first one by name over the private VM network:

```sh
firedoze vm create cockpit
firedoze start cockpit
firedoze exec cockpit -- wget -qO- http://launchpad.firedoze:8080
```

Done for the day? Put them to sleep. They keep everything and wake again when traffic arrives:

```sh
firedoze vm sleep cockpit
firedoze vm sleep launchpad
```

Or don't bother — firedoze will sleep idle VMs automatically after a configurable timeout.

## Scope (what firedoze is not)

firedoze is a tool for a small, high-trust team.

- **No clustering.** No live migration, scheduler, or HA.
- **One shared trust boundary.** Access is gated by WireGuard. No built-in users, teams, or ACLs.
- **Fixed image.** One non-configurable Ubuntu-based VM image for now.
- **HTTPS ingress only.** Public routes are for HTTP services, not arbitrary TCP.

## Documentation

- [User quickstart](docs/quickstart-user.md) — for people using an existing firedoze server
- [Admin quickstart](docs/quickstart-admin.md) — for setting up and operating a firedoze host
- [AWS guide](docs/aws-guide.md) — EC2 notes, nested virtualization, and bastion ideas
- [ADR](docs/adr.md) — design decisions and the reasoning behind the scope

## Status

firedoze is a prototype working toward basic functional completeness. It is for developers who are comfortable running early-stage infrastructure software and reading the docs before trusting it with their workflow.

Contributions and feedback welcome.
