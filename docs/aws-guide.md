# AWS EC2 Notes

These notes describe the moving parts for running firedoze on AWS EC2. They are
not a full Terraform module or production architecture. firedoze is a
shared development environment tool, not a production isolation boundary.

## Instance Types

firedoze needs KVM on the host because Firecracker uses `/dev/kvm`.

As of May 2026, AWS has two relevant EC2 paths:

- **Virtual EC2 instances with nested virtualization enabled.** AWS supports
  nested virtualization on virtual `C8i`, `M8i`, and `R8i` instances. Enable it
  at launch or while the instance is stopped by setting the EC2 CPU option
  `NestedVirtualization=enabled`.
- **EC2 bare metal instances.** Nitro bare metal instances expose the underlying
  hardware directly and have historically been the AWS answer for KVM-on-EC2.
  They are still the conservative choice for performance-sensitive or
  low-latency nested virtualization workloads.

For firedoze, start with a virtual `C8i`, `M8i`, or `R8i` instance unless you
know you need bare metal. Bare metal is more expensive and slower to start, but
it removes one layer of virtualization and gives direct access to hardware
virtualization features.

## Enabling Nested Virtualization

When launching with the AWS CLI, the shape is:

```sh
aws ec2 run-instances \
  --image-id ami-EXAMPLE \
  --instance-type m8i.4xlarge \
  --cpu-options "NestedVirtualization=enabled" \
  --key-name my-key
```

For an existing supported instance, stop it first, then modify CPU options:

```sh
aws ec2 modify-instance-cpu-options \
  --instance-id i-EXAMPLE \
  --core-count 4 \
  --threads-per-core 2 \
  --nested-virtualization enabled
```

After boot, verify KVM:

```sh
test -e /dev/kvm && echo "KVM is present"
lscpu | grep -E 'Virtualization|Hypervisor'
```

If `/dev/kvm` is missing, check:

- the instance family is `C8i`, `M8i`, or `R8i`, or a suitable bare metal type
- nested virtualization was enabled
- the guest OS has KVM modules available
- the instance was stopped/restarted after changing CPU options

## Sizing

Think of the firedoze host as a shared pool of CPUs, RAM, disk, and network.
Sleeping VMs mostly consume disk. Running VMs consume memory and CPU.

Useful starting points:

- `m8i` for a balanced shared dev host.
- `c8i` if users run mostly CPU-heavy compile/test jobs.
- `r8i` if users need many running VMs or memory-heavy development workloads.
- local NVMe instance families or large EBS volumes if VM disk I/O matters.

Use EBS gp3/io2 deliberately. VM images and snapshots can consume a lot of
space, and future ZFS-backed clone support would benefit from predictable disk
throughput.

## Network Shape

firedoze has two different network surfaces:

- **Management and VM SSH:** WireGuard-only.
- **Public web apps:** Caddy on `80/443`, usually `https://<vm>.<base_domain>`.

In the simplest direct setup, the firedoze EC2 security group allows:

```text
UDP 51820  from developer IPs, or from anywhere if relying only on WireGuard keys
TCP 80     from the internet
TCP 443    from the internet
SSH 22     from admin IPs, or use SSM instead
```

The firedoze API should still bind only to the WireGuard address. Do not expose
the management API directly through an AWS load balancer or public security
group unless the security model is deliberately changed.

## DNS

Set `base_domain` to a wildcard-capable domain:

```toml
base_domain = "dev.example.com"
```

Then create DNS:

```text
*.dev.example.com -> firedoze public address or load balancer
```

If the host has a stable Elastic IP, a wildcard `A` record is enough. If using a
load balancer, point the wildcard record at the load balancer DNS name.

## Public Web Traffic

The simplest public web path is:

```text
internet -> EC2 TCP 80/443 -> embedded Caddy -> firedoze wake proxy -> VM
```

In direct-internet mode:

```toml
[caddy]
tls_mode = "auto"
```

Caddy obtains certificates and serves HTTPS itself.

If an AWS load balancer terminates TLS before forwarding to the firedoze host,
use:

```toml
[caddy]
tls_mode = "behind_proxy"
```

Then have the load balancer forward plain HTTP to the host. Public users still
see HTTPS at the load balancer.

## Bastion Pattern

A small bastion can keep the large firedoze host away from direct public
WireGuard ingress.

Instead of:

```text
developer laptop -> big firedoze EC2 UDP 51820
```

use:

```text
developer laptop
  -> WireGuard UDP 51820
  -> small public bastion
  -> private AWS network
  -> large firedoze EC2
```

Recommended AWS shape:

- **Bastion instance**
  - small EC2 instance
  - public IP or Elastic IP
  - security group allows UDP `51820`
  - runs WireGuard
  - enables IP forwarding
  - routes developer traffic to firedoze private addresses

- **Firedoze instance**
  - large EC2 instance
  - private subnet, or at least no public WireGuard ingress
  - security group allows management/API/private-VM traffic from the bastion
  - optionally allows public `80/443` directly, or receives web traffic through
    an ALB/reverse proxy

This reduces the public attack surface of the expensive machine. The bastion is
small, replaceable, and can be monitored separately.

## Bastion Routing Sketch

On the bastion:

```sh
sudo sysctl -w net.ipv6.conf.all.forwarding=1
sudo sysctl -w net.ipv4.ip_forward=1
```

WireGuard peers get routes for:

```text
firedoze WireGuard/API address
firedoze VM private subnet
```

The bastion needs routes toward the firedoze host over the VPC private network.
The firedoze host and VM traffic need a return path back through the bastion.
There are two broad ways to do that:

- Add explicit routes in the VPC route tables and host networking.
- SNAT/MASQUERADE developer WireGuard traffic on the bastion so the firedoze
  host sees it as coming from the bastion.

SNAT is simpler to operate; explicit routing is cleaner for observability.

Security groups should allow only the minimum required private traffic from the
bastion to the firedoze host. For example:

```text
firedoze API port       from bastion security group
VM private subnet/ports from bastion security group as needed
```

## Public Web With A Bastion

Public web links do not have to go through the WireGuard bastion.

Possible layouts:

```text
internet -> firedoze EC2 80/443 -> Caddy -> VM
```

or:

```text
internet -> ALB/CloudFront -> firedoze private HTTP -> Caddy/wake proxy -> VM
```

or:

```text
internet -> small proxy/bastion 80/443 -> firedoze private HTTP -> VM
```

The first is simplest. The second and third reduce direct exposure of the large
host but add more moving parts.

## Operational Notes

- Prefer IAM Session Manager for emergency/admin shell access where possible,
  instead of exposing SSH broadly.
- Keep the firedoze management API WireGuard-only.
- Use security groups to express intent: public web, WireGuard ingress, and
  private firedoze management should be separate rules.
- Use Elastic IPs or stable DNS for WireGuard endpoints; client configs include
  the endpoint host/port.
- Back up `/etc/firedoze` and `/var/lib/firedoze` if the environments matter.
- If using a bastion, document whether it routes or SNATs. Debugging is much
  easier when that choice is explicit.

## References

- AWS EC2 nested virtualization announcement, February 16, 2026:
  https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual
- AWS EC2 nested virtualization documentation:
  https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
- AWS Nitro and bare metal instance documentation:
  https://docs.aws.amazon.com/ec2/latest/instancetypes/ec2-nitro-instances.html
