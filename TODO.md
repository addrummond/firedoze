# TODO

- Add real tests for API handlers, SQLite migrations, WireGuard config generation, and Firecracker lifecycle edge cases.
- Add structured migrations instead of ad hoc `alter table` checks.
- Improve idle detection with better per-VM overrides, observability, and race handling during start/stop/sleep.
- Add clearer diagnostics for missing host dependencies such as `/dev/kvm`, Firecracker, `iptables`, `debugfs`, `ssh-keygen`, and kernel WireGuard.
- Consider best default idle timeout.
- Make sure that all API 404 responses are explicit about WHAT doesn't exist.
- Explore adopting running Firecracker processes across daemon restarts if less disruptive upgrades become important.
- Consider a snapshot-capable Ubuntu mirror or lockfile for stronger base image rebuild reproducibility.
- Show some kind of progress bar for snapshot import/export.
