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
