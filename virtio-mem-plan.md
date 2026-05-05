# virtio-mem Plan

Firedoze should move from fixed-memory VMs to elastic-memory VMs with a minimum, a maximum, and a host-controlled target. The guest may provide pressure/reclaim hints, but the host remains authoritative.

## Goals

- Let each VM start with `memory_min_mib`.
- Let each VM grow up to `memory_max_mib` via Firecracker `virtio-mem`.
- Let Firedoze shrink VMs again when memory is no longer needed.
- Keep the security model simple: guest agents send hints only; they do not command the host.
- Surface useful live memory data in `firedoze vm usage`.

## Model

- `memory_min_mib`: base memory configured directly in Firecracker `machine-config.mem_size_mib`.
- `memory_max_mib`: upper bound for guest memory.
- `hotplug_total_mib`: `memory_max_mib - memory_min_mib`.
- `hotplug_requested_mib`: current requested size of the virtio-mem region.
- `effective_memory_mib`: `memory_min_mib + hotplug_plugged_mib`.

The existing `memory_mib` field is treated as the maximum memory during transition.

## Firecracker Integration

- Add a `memory-hotplug` section to the Firecracker config file when `memory_max_mib > memory_min_mib`.
- Boot with `machine-config.mem_size_mib = memory_min_mib`.
- Configure:
  - `total_size_mib = memory_max_mib - memory_min_mib`
  - conservative default `slot_size_mib`
  - conservative default `block_size_mib`
- Use Firecracker's Unix-socket API directly for `/hotplug/memory` until the Go SDK exposes generated bindings for the current API.

## Guest Integration

- Add kernel args for automatic memory onlining so newly plugged memory becomes usable without a userspace race.
- Install a small guest-side Firedoze memory agent later if host-side metrics alone are not enough.
- If added, the guest agent reports pressure/reclaim hints to a host-only endpoint over the VM private link. Hints are authenticated by source VM mapping and constrained by the VM's configured min/max.

## Controller

- Start with a conservative host-side controller:
  - periodically read Firecracker hotplug status;
  - grow quickly when a guest hint reports pressure;
  - shrink slowly after sustained low pressure;
  - never request below min or above max;
  - tolerate shrink failure by observing `plugged_size_mib` and retrying later.
- Treat unplug as best-effort. Linux may be unable to offline some memory blocks immediately.

## User Interface

- Replace `-memory-mib` with:
  - `-memory-min-mib`
  - `-memory-max-mib`
- Keep `-memory-mib` temporarily as shorthand for setting both min and max while this is still unreleased/development-stage.
- Extend `firedoze vm usage` with min/max/effective/hotplug status.

## Implementation Steps

1. Add schema/model/API/CLI support for min/max memory.
2. Add Firecracker config support for `memory-hotplug`.
3. Add direct Unix-socket helpers for `/hotplug/memory`.
4. Extend `vm usage` with hotplug status.
5. Add guest kernel args for automatic memory onlining.
6. Validate on the NUC or another Linux host with a real VM.
7. Add a guest hint agent only after the host-side path is proven.
