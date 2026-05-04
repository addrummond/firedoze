# TODO

- Move privileged host operations behind a small helper or tighter privilege boundary.
- Add real tests for API handlers, SQLite migrations, WireGuard config generation, and Firecracker lifecycle edge cases.
- Add structured migrations instead of ad hoc `alter table` checks.
- Improve idle detection with better per-VM overrides, observability, and race handling during start/stop/sleep.
- Add install packaging for deb/rpm or a single install script.
- Add clearer diagnostics for missing host dependencies such as `/dev/kvm`, Firecracker, `iptables`, `debugfs`, `ssh-keygen`, and kernel WireGuard.
- Add optional auth layers for public routes if shared dev environments need more than WireGuard-only trust.
- Consider best default idle timeout.
- Make sure that all API 404 responses are explicit about WHAT doesn't exist.
- Put this in Readme, but check it's true: Ordinary SSH access, so tools like ssh, scp, rsync, and VS Code Remote SSH work as if it were a normal Linux box
- File import and export of images.
- Will a regular Linux shutdown command inside a VM update its status accordingly?
