# ZFS Plan

Firedoze should use ZFS as a required host storage layer, not as an optional
backend. The main product value is fast VM creation and cloning, so maintaining
both plain-file and ZFS storage paths is not worth the complexity.

## Target Model

The guest filesystem stays ext4. The host stores each VM root disk as a ZFS
zvol.

```text
base rootfs.ext4
  -> imported into base zvol
  -> snapshotted as firedoze/base/rootfs@current
  -> cloned per VM as firedoze/vms/<vm>/rootfs
  -> exposed to Firecracker as /dev/zvol/firedoze/vms/<vm>/rootfs
```

The Linux host does not need to use ZFS for its root filesystem. It only needs
a ZFS pool/dataset/zvol namespace for firedoze VM storage.

## Installation Model

The setup guide must include a deliberate "create or choose a ZFS pool" step.
Pool creation can destroy disks, so `scripts/install.sh` should not
automatically run `zpool create`.

Recommended path for a fresh server with an extra disk:

```sh
sudo apt install zfsutils-linux
lsblk
ls -l /dev/disk/by-id/
sudo zpool create firedoze /dev/disk/by-id/<DISK_ID>
```

Path for a server that already has a ZFS pool:

```sh
sudo zfs create existingpool/firedoze
```

The installer should be allowed to validate prerequisites and fail clearly:

- `zfs` and `zpool` commands exist.
- the configured pool exists.
- the configured firedoze datasets/zvol namespace exists or can be created by
  an explicit firedoze setup/import command.

The installation instructions should emphasize that admins must choose the disk
or pool intentionally. Firedoze can create datasets/zvols inside a chosen pool,
but should not silently initialize a new pool from an arbitrary block device.

## Image Builder Impact

The pure Go image builder can remain mostly unchanged.

`firedoze-image build` should continue producing:

```text
dist/base-image/rootfs.ext4
dist/base-image/vmlinux.bin
dist/base-image/initrd.img
dist/base-image/manifest.txt
```

The change is the install/import step after the ext4 image is built:

```text
rootfs.ext4
  -> dd/import into /dev/zvol/firedoze/base/rootfs
  -> zfs snapshot firedoze/base/rootfs@current
```

The Go ext4 library does not need to understand ZFS.

## Config Direction

Add mandatory storage configuration:

```toml
[storage]
pool = "firedoze"
base_volume = "firedoze/base/rootfs"
base_snapshot = "current"
```

Keep kernel/initrd as normal host files:

```toml
[firecracker]
base_kernel_path = "/var/lib/firedoze/images/vmlinux.bin"
base_initrd_path = "/var/lib/firedoze/images/initrd.img"
```

`firecracker.base_rootfs_path` should stop being a runtime dependency. It may
remain temporarily as an image import source, then be removed or moved under an
image install command.

## Startup Checks

`firedozed` should fail clearly at startup unless ZFS is ready:

- `zfs` command exists.
- configured pool exists.
- configured base zvol exists.
- configured base snapshot exists.
- corresponding `/dev/zvol/...` path exists or appears after a short wait.

## VM Creation

VM create should clone the base snapshot instead of copying a rootfs file:

```sh
zfs clone firedoze/base/rootfs@current firedoze/vms/<vm>/rootfs
```

Firecracker should use:

```text
/dev/zvol/firedoze/vms/<vm>/rootfs
```

After cloning, firedoze should run the existing guest identity rewrite against
the zvol device path.

## Snapshots

Running VM snapshots are disallowed.

Stopped VM snapshot:

- snapshot the VM zvol
- preserve disk state only

Sleeping VM snapshot:

- snapshot the VM zvol
- copy Firecracker sleep memory/state files into snapshot metadata storage

To allow VM deletion without being blocked by named snapshots, named snapshots
should live in their own ZFS namespace, not only as snapshots attached to VM
zvols.

Snapshot save should do roughly:

```sh
zfs snapshot firedoze/vms/<vm>/rootfs@tmp-<snapshot>
zfs clone firedoze/vms/<vm>/rootfs@tmp-<snapshot> firedoze/snapshots/<snapshot>/rootfs
zfs snapshot firedoze/snapshots/<snapshot>/rootfs@frozen
zfs destroy firedoze/vms/<vm>/rootfs@tmp-<snapshot>
```

SQLite should record the snapshot zvol/snapshot name, plus existing metadata:

```text
snapshot_name
source_vm
zfs_snapshot = firedoze/snapshots/<snapshot>/rootfs@frozen
state_path
mem_path
base_image metadata
kernel metadata
```

## Restore

Restore should clone the saved snapshot zvol:

```sh
zfs clone firedoze/snapshots/<snapshot>/rootfs@frozen firedoze/vms/<newvm>/rootfs
```

Then firedoze should rewrite guest identity on:

```text
/dev/zvol/firedoze/vms/<newvm>/rootfs
```

The restored VM should start in `stopped` state.

## Sleep And Resume

Sleep/resume stays conceptually the same:

- sleep writes Firecracker memory/state files under the VM state directory
- resume loads those files
- Firecracker always uses the VM zvol as the root disk

## Delete

VM delete should destroy only the VM zvol and metadata/state directory:

```sh
zfs destroy firedoze/vms/<vm>/rootfs
rm -rf /var/lib/firedoze/vms/<vm>
```

Snapshot delete should destroy the snapshot namespace:

```sh
zfs destroy -r firedoze/snapshots/<snapshot>/rootfs
rm -rf /var/lib/firedoze/snapshots/<snapshot>
```

## Code Changes

Replace the plain-file disk lifecycle:

- remove rootfs copy on VM create
- remove disk copy on snapshot save
- remove disk copy on snapshot restore
- replace `ensureDisk`
- make VM layout compute a zvol device path instead of `rootfs.ext4`
- add small wrappers around `zfs create`, `zfs snapshot`, `zfs clone`,
  `zfs destroy`, and zvol path waiting

Using the `zfs` CLI is acceptable initially. The commands are admin-facing and
stable, and a small wrapper is easier to audit than maintaining two storage
backends.

## Manual Spike Before Refactor

Verify on a Linux host:

1. Firecracker can boot from `/dev/zvol/...`.
2. `debugfs` can rewrite guest identity on `/dev/zvol/...`.
3. `zfs snapshot -> zfs clone -> Firecracker boot` works.

If these pass, the migration is mostly plumbing.
