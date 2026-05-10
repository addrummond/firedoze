# TODO

- Add real tests for API handlers, SQLite migrations, WireGuard config generation, and Firecracker lifecycle edge cases.
- Add structured migrations instead of ad hoc `alter table` checks.
- Add clearer diagnostics for missing host dependencies such as `/dev/kvm`, Firecracker, `iptables`, `debugfs`, `ssh-keygen`, and kernel WireGuard.
- Consider best default idle timeout.
- Make sure that all API 404 responses are explicit about WHAT doesn't exist.
- Explore adopting running Firecracker processes across daemon restarts if less disruptive upgrades become important.
- Show some kind of progress bar for snapshot import/export.
- Can I map arbitrarily nested subdomains to endpoints?
- What if I try to create a VM but the default subdomain is already mapped to another VM? I think we took care of that, but make sure.
