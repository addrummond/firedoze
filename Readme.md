# firedoze

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**Disposable Linux computers for your dev team, backed by [Firecracker](https://firecracker-microvm.github.io/).**

firedoze runs persistent VMs on a single Linux host. Each one behaves like a small computer: its own filesystem, its own systemd, its own SSH, its own long-running processes.

Spin one up in seconds. Work in it normally. When it goes idle, it sleeps automatically — keeping all its state while consuming only disk space. Wake it again by sending it traffic.

> ⚠️ firedoze is early-stage software. Don't use it for production workloads, hostile multi-tenant isolation, or on infrastructure you'd be upset to lose.

---

## Why not just use containers?

Containers are great, but sometimes you want:

- A real persistent filesystem that survives restarts without volume gymnastics
- `systemd` running like normal
- Ordinary SSH without any container runtime in the way
- Snapshots you can clone and hand to a teammate
- A place to run services that just stay running

firedoze makes all of that lightweight enough for everyday team development. One beefy box to run the VMs, a simple CLI, and a WireGuard tunnel to keep things honest.

> 💡 **AWS quietly made this easier.** In February 2026, AWS enabled nested virtualization on C8i, M8i, and R8i instances. You no longer need bare-metal EC2 to run KVM-backed VMs on AWS. See the [AWS guide](docs/aws-guide.md). Other low-cost options include [Hetzner](https://www.hetzner.com/) dedicated servers and [Digital Ocean droplets](https://www.digitalocean.com/community/questions/does-digitalocean-support-kvm-or-nested-virtulzation).

## What you get

- **Firecracker-backed VMs** that boot fast and act like real Linux machines
- **WireGuard-only management access** — if you're not in the tunnel, you can't access the management endpoint
- **Automatic public HTTPS routes** for sharing a dev service with someone outside the tunnel
- **Idle sleep with full state preservation** — sleeping VMs cost you nothing but disk
- **Named snapshots and clones** — script a golden environment, clone it for everyone on the team
- **A single `firedoze` CLI** covering the full lifecycle: create, SSH, exec, copy files, manage routes, snapshot, restore
- **Native Go image builder** — no Docker or Podman required
- **Deliberately single-node** — one box, local SQLite, no scheduler, no cluster to babysit

## Quick example

Spin up a VM, drop in a tiny web app, publish it:

```sh
firedoze up launchpad
firedoze exec launchpad -- sh -lc 'cat > app.html <<EOF
<h1>hello from $(hostname)</h1>
<p>this is a whole disposable computer, not a container</p>
EOF
busybox httpd -f -p 8080 -h .'
firedoze vm list launchpad
```

Start a second VM and call the first one by name over the private VM network:

```sh
firedoze up cockpit --publish=false
firedoze exec cockpit -- wget -qO- http://launchpad.firedoze:8080
```

Done for the day? Put them to sleep. They'll keep everything and wake up the next time traffic arrives:

```sh
firedoze vm sleep cockpit
firedoze vm sleep launchpad
```

Or don't bother — firedoze will sleep idle VMs automatically after a configurable timeout.

## Scope (what firedoze is not)

firedoze is deliberately narrow. It's a tool for a small high-trust team or squad, not a platform:

- **No clustering.** No live migration, no scheduler, no HA.
- **One shared trust boundary.** Access is gated purely by WireGuard. No built-in users, teams, or ACLs.
- **Fixed image.** Single non-configurable Ubuntu-based VM image.
- **HTTPS ingress only.** Public routes are for sharing HTTP services, not arbitrary TCP exposure.

## Documentation

- [User quickstart](docs/quickstart-user.md) — for people using an existing firedoze server
- [Admin quickstart](docs/quickstart-admin.md) — for setting up and operating a firedoze host
- [AWS guide](docs/aws-guide.md) — EC2 notes, nested virtualization, and bastion ideas
- [ADR](docs/adr.md) — design decisions and the reasoning behind the scope

## Status

firedoze is a prototype working toward basic functional completeness. The intended audience is developers who are comfortable running early-stage infrastructure software and reading the docs before trusting something with their workflow.

Contributions and feedback welcome.
