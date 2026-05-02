# TODO

- Move privileged host operations behind a small helper or tighter privilege boundary.
- Add real tests for API handlers, SQLite migrations, DNS responses, WireGuard config generation, and Firecracker lifecycle edge cases.
- Add structured migrations instead of ad hoc `alter table` checks.
- Add image management: base image versioning, image import, image export, and slow-storage archival for old disks/snapshots.
- Improve idle detection with better per-VM overrides, observability, and race handling during start/stop/sleep.
- Improve snapshot/restore semantics for exact clones versus identity-rewritten clones.
- Add install packaging for deb/rpm or a single install script.
- Add clearer diagnostics for missing host dependencies such as `/dev/kvm`, Firecracker, `iptables`, `debugfs`, `ssh-keygen`, and kernel WireGuard.
- Add optional IPv6/public-address support if the v1 private-network model proves too limiting.
- Add optional auth layers for public routes if shared dev environments need more than WireGuard-only trust.
- Way of copying files in and out of VMs, such as `firedoze cp <vm>:<src> <dst>` and `firedoze cp <src> <vm>:<dst>`.
- Install a 'firedoze-sleep' command within the VM. If possible, this should be a static linux x86_64 binary that can be copied into any VM and invoked to trigger a clean sleep with the same semantics as `firedoze vm sleep`.
- Figure out why exactly we need chmodIfExists in the image builder.
- Check if we guard against multiple wireguard peers with the same IP or name.
