# firedoze

Disposable Linux computers for shared development.

firedoze runs persistent Firecracker VMs on one Linux host, gives each VM simple command-line lifecycle controls, and exposes dev services over HTTPS when you want to share them. It is designed for the "create and forget" model: make a VM, use it like a small computer, let it sleep when idle, and only think about the environments that matter today.

**Warning: firedoze is early-stage development software.** It is not production-ready, the interfaces may change, and the security model assumes a trusted shared dev environment. Do not use it for production workloads or hostile multi-tenant isolation.

## Why

Containerized dev environments are useful, but sometimes you want the shape of a real machine: a persistent filesystem, systemd, normal SSH, long-running services, snapshots, and fewer container-specific assumptions.

firedoze aims to make that feel lightweight enough for everyday team development.

## Highlights

- Firecracker-backed Linux VMs that behave like small persistent computers.
- WireGuard-only management access.
- Public HTTPS routes for sharing running dev services.
- Sleeping VMs that keep state while freeing CPU and memory.
- Named snapshots and restores for cloneable environments.
- A simple `firedoze` client for VM lifecycle, SSH, exec, copy, routes, and snapshots.
- Native Go base-image builder; no Docker or Podman required.
- Single-node by design: one beefy box, local SQLite, no scheduler, no cluster.

## Example

```sh
firedoze up demo
firedoze exec demo -- sh -lc 'echo hello from $(hostname)'
firedoze publish demo
firedoze vm sleep demo
```

## Documentation

- [User quickstart](docs/quickstart-user.md): for people using an existing firedoze server.
- [Admin quickstart](docs/quickstart-admin.md): for setting up and operating a firedoze host.
- [AWS guide](docs/aws-guide.md): EC2 notes, nested virtualization, and bastion ideas.
- [ADR](docs/adr.md): design decisions and project scope.
- [TODO](TODO.md): planned improvements and open work.

## Status

firedoze is currently a prototype moving toward basic functional completeness. The intended audience is developers comfortable running early infrastructure software and reading the docs before trusting it.

The license is MIT.
