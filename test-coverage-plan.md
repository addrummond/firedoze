# Firedoze Test Coverage Plan

This is a plan for getting firedoze to adequate test coverage as the codebase stabilizes. "Adequate" here does not mean chasing 100% line coverage. It means the cheap, deterministic tests catch ordinary regressions, and the dangerous Linux/Firecracker paths have a small but meaningful integration suite.

## Current Baseline

Measured on 2026-05-04 with `GOTOOLCHAIN=auto go test ./...` and package-level `go test -cover` runs. The normal test suite passes outside the local sandbox.

| Package | Current coverage | Notes |
| --- | ---: | --- |
| `cmd/firedoze` | 39.8% | Good coverage for recent CLI parsing/output fixes; many subcommands still uncovered end to end. |
| `cmd/firedoze-image-builder` | 16.6% | Some artifact parsing helpers are covered; the actual image customization flow is mostly untested. |
| `cmd/firedoze-hello` | no tests | Formatting, favicon, and verbose behavior are currently manual-test only. |
| `cmd/firedozed` | no tests | Startup flags, restart-wake behavior, and systemd-facing paths are untested. |
| `internal/api` | no tests | Highest priority gap: all JSON API behavior currently relies on CLI/manual coverage. |
| `internal/config` | 61.7% | Reasonable start; needs more validation/rendering edge cases. |
| `internal/dns` | 43.1% | VM lookup has tests; forwarding and server lifecycle are uncovered. |
| `internal/firecracker` | 22.1% | Recent snapshot/cold-storage protections are tested; most lifecycle, networking, and process paths are not. |
| `internal/host` | no tests | Netlink/WireGuard host reconciliation has no unit seam yet. |
| `internal/model` | no direct tests | `JSONText` is indirectly tested through `internal/store`; add direct scan tests. |
| `internal/proxy` | 34.6% | Caddy config and HTTP wake have tests; TCP/SSH wake and captcha verification paths are thin. |
| `internal/store` | 33.7% | VM listing/state tests exist; routes, snapshots, delete/update, and migrations need coverage. |
| `internal/systemd` | no tests | Notify/watchdog behavior can be tested with Unix sockets and env isolation. |
| `internal/wireguard` | 69.2% | Strongest package; add key generation/server-key and endpoint edge cases. |

## Coverage Goals

The target should be package-appropriate:

- Fast unit suite: `go test ./...` should stay hermetic, non-root, non-network, and runnable on macOS and Linux.
- Core pure packages (`config`, `store`, `wireguard`, `api`): aim for roughly 75-85% statement coverage.
- Firecracker manager and proxy packages: aim for 60-70% unit coverage, plus integration tests for the paths that cannot be usefully faked.
- Command packages: cover parsing, request bodies, command dispatch, and output contracts rather than trying to execute real `ssh`, `rsync`, or `firecracker` in unit tests.
- Privileged integration suite: small, explicit, Linux-only, and opt-in with build tags.

## Test Infrastructure First

1. Add test tasks:
   - `task test`: existing fast suite.
   - `task test:coverage`: package coverage summary for fast tests.
   - `task test:integration`: Linux-only privileged tests, disabled by default.
   - `task test:all`: fast tests plus integration tests when the host is configured.

2. Add coverage hygiene:
   - Generate coverage profiles under `dist/coverage` or a temp dir, not the repo root.
   - Add a short script or task that prints package coverage sorted from lowest to highest.
   - Do not enforce a global threshold immediately. Start with package targets after the first coverage pass lands.

3. Add shared test helpers:
   - Temporary config/store builder.
   - Fake proxy reconciler.
   - Fake VM manager for API tests.
   - Fake command runner for `ssh`, `rsync`, `iptables`, `debugfs`, and Firecracker process launch paths.
   - Golden JSON/text helpers for CLI and API output.

## Phase 1: Highest Value Hermetic Tests

### `internal/api`

This should be the first major coverage push. The API is the contract between `firedoze` and `firedozed`, and it is currently untested.

Recommended small refactor:

- Change `api.NewServer` to depend on a narrow manager interface instead of `*firecracker.Manager`.
- Keep the concrete manager implementation unchanged.
- Use a fake manager in tests to force success, `store.ErrNotFound`, `firecracker.ErrAlreadyRunning`, `firecracker.ErrRunning`, and validation failures.

Test cases:

- `GET /`, `/health`, and `/config` return stable JSON and do not expose secrets.
- `GET /vms?name=glob` passes all requested name filters through.
- VM create validates names, defaults, `auto_wake`, `public_http`, and bad JSON.
- VM start/stop/sleep/reboot map manager errors to the intended HTTP statuses.
- VM settings patch validates ports, sleep timeout, `auto_wake`, and `public_http`.
- VM delete reconciles the proxy and maps not-found correctly.
- Route create/list/delete validates names, ports, VM aliases, VM-name reservation, and reconcile behavior.
- Snapshot create/restore/delete validates snapshot names and maps running/sleeping/not-found conflicts correctly.
- WireGuard peer list/config returns only public peer material.
- Every mutating endpoint that changes public routing calls `proxy.Reconcile`.

### `internal/store`

Store tests should use real SQLite in temp dirs. They are cheap and catch real schema mistakes.

Add coverage for:

- Route CRUD, including duplicate route names, missing routes, route deletion for a VM, and `VMExists`.
- Snapshot CRUD, including duplicate names and metadata round-tripping.
- `UpdateVM`, including partial updates and unchanged fields.
- `SetVMArchivedDiskPath` and clearing archived paths.
- `DeleteVM` removes only the intended VM row and leaves snapshots alone.
- `CountVMs`, `ListVMs`, and empty-list behavior.
- Migration from older schemas: create a minimal old DB shape, run `Migrate`, and assert new columns/defaults.
- Direct `model.JSONText.Scan` behavior for `nil`, `[]byte`, `string`, invalid types, invalid JSON, and object metadata.

### `cmd/firedoze`

The CLI is now important enough to test by command behavior, not only helper functions.

Add coverage for:

- `vm start`, `vm stop`, `vm sleep`, `vm reboot`, `vm publish`, `vm hide`, `vm up`, and `vm list -names`.
- Snapshot list/create/delete/restore, including restore options for CPU, memory, disk, ports, publish, and auto-wake.
- Route list/create/delete.
- `server add/list/use/remove` and config-file error cases.
- `exec`, `ssh`, `cp`, and `with-vm-ip` with fake API responses and fake exec runners.
- `up` progress output in non-terminal mode and error behavior when create succeeds but start or SSH readiness fails.
- Missing API URL errors: commands that need the API should fail clearly, while local commands should not.
- JSON output contracts where still supported.

Recommended small refactor:

- Inject an `execCommand` function and a `waitForSSH` function into `app` for tests.
- Keep the production path using `os/exec`.

### `internal/firecracker`

Do not try to unit-test KVM itself. Unit-test the decision logic and isolate host operations behind small seams.

Add coverage for pure and filesystem-backed paths first:

- `CreateVM` defaults: CPU, memory, disk, HTTP port, `auto_wake`, private IPv6 assignment, metadata fields.
- `RestoreSnapshot` validation, duplicate target handling, disk-size behavior, metadata copying, and cleanup on copy/create failures.
- `UpdateVM` validation.
- `DeleteSnapshot` removes the snapshot dir and DB row.
- `ReconcileStartup` marks stale `running` VMs as `lost`, ignores stopped/sleeping VMs, and tolerates stale network cleanup errors.
- `RunningVMNames`, `SleepRunningVMs`, and `StartVMs` behavior with multiple VMs and partial failures.
- Base image metadata caching, manifest parsing, missing initrd behavior, and cache invalidation when artifact size/mtime changes.
- `nextPrivateIP`, IPv6 boundary behavior, and duplicate IP avoidance.
- `bootArgs` with and without guest DNS enabled.
- `tapName`, `macForVMName`, `addToIP`, `decrementIP`, and path layout helpers.
- `ensureDisk` clone/copy/truncate behavior.
- More cold-storage cases: not old enough, already archived, disabled config, context cancellation during copy, commit-phase behavior, stale hot disk plus archived path.

Recommended small refactors:

- Introduce a tiny `networkOps` interface for tap creation, address assignment, routes, forwarding, and cleanup.
- Introduce a tiny `processLauncher` interface for Firecracker process launch and socket wait.
- Keep Firecracker SDK calls wrapped in package-level variables or a small interface so sleep/resume/snapshot paths can be tested against a fake Unix-socket server.

### `internal/proxy`

Add coverage for:

- TCP wake proxy: IP-to-VM lookup, unknown destination handling, auto-wake disabled, already-running VM, start failure, wait-for-SSH timeout, and proxy-copy shutdown.
- SSH redirect rule construction, especially IPv6/private subnet behavior.
- Wake gate captcha verification success/failure, signed cookie expiry, wrong host, malformed cookie, key creation permissions, and favicon/nonexistent VM behavior.
- `DefaultHost` and route matching with aliases.
- `Reconcile` behavior should be tested with a caddy adapter seam rather than starting real Caddy in unit tests.

### `cmd/firedoze-image-builder`

The image builder has a lot of important behavior hidden in one file. Test the guest customization contract heavily.

Add coverage for:

- Default URL and checksum selection for supported Ubuntu releases.
- Refusal to download without checksums unless explicitly insecure.
- Local artifact reads and checksum failures.
- BusyBox `.deb` extraction failure modes.
- Kernel extraction failure modes.
- `guestOverlay` skip/capture/apply behavior.
- `populateRootfs` with a small synthetic tar containing dirs, symlinks, hardlinks, device-ish modes, and boot files.
- `customizeGuest` creates the expected files for networking, SSH, `firedoze-hello`, `firedoze-hello-install`, shell prompt, hostname setup, systemd units, and permissions.
- `/etc/passwd`, `/etc/group`, `/etc/shadow`, and sudo membership text helpers.
- Golden tests for generated guest scripts and units.

Recommended follow-up:

- Split the image builder into small internal packages once tests start getting awkward: artifact download/verify, rootfs tar population, guest customization, and CLI.

### `cmd/firedozed`

Add tests around logic that does not need root:

- Flag behavior for `-init-config`, `-print-config`, `-print-api-env`, `-wg-server-public-key`, `-wg-peer-config`, and `-wg-add-peer`.
- Refuses `-serve` unless `-setup-wireguard` is also present.
- `wireGuardBindIP` handles IPv6 CIDR and invalid input.
- Restart-wake file write/remove/parse behavior.
- `wakeRestartVMs` starts the recorded VMs, removes the restart file, and handles malformed JSON.

Recommended small refactor:

- Convert `run()` into `run(args []string, deps daemonDeps) int`, with production `main()` passing real dependencies. This keeps tests from touching real host networking.

### `cmd/firedoze-hello`

Add tests for:

- Plain response formatting.
- `-verbose` behavior.
- Favicon route content type and SVG body.
- User/kernel/uptime/load fallbacks when proc files or commands are unavailable.
- IPv6 address filtering.
- Route output deduplication or fallback behavior.

Recommended small refactor:

- Put host data collection behind package-level variables or a small provider struct so tests do not depend on the developer machine.

### `internal/host` and `internal/systemd`

Add low-cost tests:

- `ensureWireGuardPrivateKey` creates a key file, reuses it, and rejects malformed existing keys.
- `EnsureLoopbackAddress` input validation can be tested if parsing is separated from netlink application.
- `systemd.Notify` returns false without `NOTIFY_SOCKET`.
- `systemd.Notify` sends the expected datagram to a temp Unix socket, including abstract socket handling on Linux.
- `StartWatchdog` ignores missing/bad env values and sends at least one watchdog datagram for a short interval.

## Phase 2: Integration Tests

Keep these out of the default suite. Use build tags such as `//go:build integration && linux`.

### API plus real store integration

Run `internal/api` against a real temp SQLite store and a fake manager/proxy:

- Create VM, publish it, list it, hide it, delete it.
- Create route, list routes, delete route.
- Snapshot create/restore/delete flow with fake manager state.

### Image builder smoke test

On Linux only, with network disabled by default:

- Given pre-fetched test artifacts or a tiny synthetic rootfs tar, build a rootfs.
- Mount/read it through the Go ext4 library and assert key guest files exist.
- Do not download Ubuntu artifacts in the default integration test.

### Firecracker host integration

Run only on a configured Linux host with KVM and Firecracker:

- Build/install binaries.
- Initialize a temp config in an isolated state dir.
- Create VM.
- Start VM.
- Wait for SSH.
- Run `firedoze-hello`.
- Sleep VM.
- Wake via `firedoze vm start`.
- Snapshot stopped VM.
- Restore to a new VM.
- Publish and curl the public URL if DNS/proxy config is present.
- Delete both VMs and assert hot/cold disks and DB rows are gone.

### WireGuard integration

Run only when root/network access is explicitly allowed:

- Initialize WireGuard config.
- Add a peer by public key.
- Bring up the server interface.
- Assert API binds only to the configured WireGuard address.
- Assert peer config contains the expected client address, server public key, endpoint, and `AllowedIPs`.

### Upgrade/restart integration

This is important because daemon restarts are expected to sleep and then restore running VMs:

- Start two VMs.
- Send SIGTERM to `firedozed`.
- Assert restart-wake file is written.
- Restart `firedozed`.
- Assert VMs are started again or reported cleanly as lost if startup fails.

## Regression Checklist To Encode

These are specific bugs/design decisions that should become tests:

- API and CLI must handle object-shaped `base_image_metadata`.
- `firedoze vm sleep a b` must not accidentally wake one VM while sleeping another.
- Stale `running` state after daemon restart becomes `lost`.
- Snapshotting `running` or `sleeping` VMs is rejected.
- Snapshot restore should preserve base image metadata.
- Cold archive copy is canceled by explicit start/delete/snapshot operations.
- Starting an archived VM hydrates exactly that VM's disk.
- Deleting a VM removes hot disk, cold disk, routes, runtime files, and DB row.
- WireGuard peer names and allowed IPs must be unique.
- Peer setup must not require the admin to see the client private key.
- API URL discovery uses the WireGuard server address and has no magic default.
- `firedoze vm list` hides private VM URLs and supports `-names`.
- Lifecycle commands live under `firedoze vm`; top-level lifecycle aliases stay removed.
- `firedoze ssh`, `exec`, and `cp` use the VM private IP and passwordless guest auth options.
- Caddy config only publishes public VMs.
- Missing public route returns a friendly root/default response where intended, not the wake captcha.
- `firedoze-hello` favicon and non-verbose output stay stable.
- Guest image includes BusyBox, passwordless SSH config, useful prompt, `firedoze-hello`, and `firedoze-hello-install`.

## Suggested Order Of Work

1. Add coverage/test tasks and shared test helpers.
2. Add `internal/api` tests with a manager interface seam.
3. Fill out `internal/store` route/snapshot/update/delete/migration coverage.
4. Add CLI command behavior tests for all user-facing commands.
5. Add Firecracker manager unit seams and cover create/restore/start decision logic without launching Firecracker.
6. Add proxy TCP wake and wake-gate tests.
7. Add image-builder guest customization golden tests.
8. Add daemon, hello, host, systemd tests.
9. Add opt-in Linux integration tests.
10. Start enforcing package-level coverage targets once the suite is no longer lopsided.

## Current Coverage Enforcement

`scripts/test-coverage.sh` now enforces conservative package-level floors from `scripts/coverage-thresholds.tsv`. The thresholds are intentionally below the current measured coverage, so they catch accidental regressions without turning coverage into a brittle target.

## Exit Criteria

Firedoze has adequate coverage when:

- `task test` is fast, deterministic, and passes on macOS and Linux without root.
- New API endpoints and CLI commands are expected to arrive with tests.
- Store migrations and metadata round-tripping are covered.
- VM lifecycle state transitions are covered in unit tests where possible and integration tests where necessary.
- Image-builder output contracts are covered by golden tests.
- A documented Linux integration suite exercises create/start/ssh/sleep/wake/snapshot/restore/delete.
- Coverage gaps are intentional and documented when they require KVM, root, or real network setup.
