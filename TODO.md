# TODO

- Move privileged host operations behind a small helper or tighter privilege boundary.
- Add real tests for API handlers, SQLite migrations, WireGuard config generation, and Firecracker lifecycle edge cases.
- Add structured migrations instead of ad hoc `alter table` checks.
- Improve idle detection with better per-VM overrides, observability, and race handling during start/stop/sleep.
- Add clearer diagnostics for missing host dependencies such as `/dev/kvm`, Firecracker, `iptables`, `debugfs`, `ssh-keygen`, and kernel WireGuard.
- Add optional auth layers for public routes if shared dev environments need more than WireGuard-only trust.
- Consider best default idle timeout.
- Make sure that all API 404 responses are explicit about WHAT doesn't exist.
- Explore adopting running Firecracker processes across daemon restarts if less disruptive upgrades become important.
- Consider a snapshot-capable Ubuntu mirror or lockfile for stronger base image rebuild reproducibility.
