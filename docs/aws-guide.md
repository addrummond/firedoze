# AWS EC2 Notes

These notes describe the moving parts for running Firedoze on AWS EC2.

## Instance Types

Firedoze needs KVM on the host because Firecracker uses `/dev/kvm`.

As of May 2026, AWS has two relevant EC2 paths:

- **Virtual EC2 instances with nested virtualization enabled.** AWS supports
  nested virtualization on virtual `C8i`, `M8i`, and `R8i` instances. Enable it
  at launch or while the instance is stopped by setting the EC2 CPU option
  `NestedVirtualization=enabled`.
- **EC2 bare metal instances.** Nitro bare metal instances expose the underlying
  hardware directly and have historically been the AWS answer for KVM-on-EC2.
  They are still the conservative choice for performance-sensitive or
  low-latency nested virtualization workloads.

For Firedoze, start with a virtual `C8i`, `M8i`, or `R8i` instance unless you
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

Think of the Firedoze host as a shared pool of CPUs, RAM, disk, and network.
Sleeping VMs mostly consume disk. Running VMs consume memory and CPU.

Useful starting points:

- `m8i` for a balanced shared dev host.
- `c8i` if users run mostly CPU-heavy compile/test jobs.
- `r8i` if users need many running VMs or memory-heavy development workloads.
- local NVMe instance families or large EBS volumes if VM disk I/O matters.

## Network Shape

Firedoze has two different network surfaces:

- **Management and VM SSH:** WireGuard-only.
- **Public web apps:** Caddy on `80/443`, usually `https://<vm>.<base_domain>`.

In the simplest direct setup, the Firedoze EC2 security group allows:

```text
UDP 51820  from developer IPs, or from anywhere if relying only on WireGuard keys
TCP 80     from the internet
TCP 443    from the internet
SSH 22     from admin IPs, or use SSM instead
```

The Firedoze API should still bind only to the WireGuard address. Do not expose
the management API directly through an AWS load balancer or public security
group unless the security model is deliberately changed.

## DNS

Set `base_domain` to a wildcard-capable domain:

```toml
base_domain = "dev.example.com"
```

Then create DNS:

```text
*.dev.example.com -> Firedoze public address or load balancer
```

If the host has a stable Elastic IP, a wildcard `A` record is enough. If using a
load balancer, point the wildcard record at the load balancer DNS name.

## Public Web Traffic

The simplest public web path is:

```text
internet -> EC2 TCP 80/443 -> embedded Caddy -> Firedoze wake proxy -> VM
```

In direct-internet mode:

```toml
[caddy]
tls_mode = "auto"
```

Caddy obtains certificates and serves HTTPS itself.

If an AWS load balancer terminates TLS before forwarding to the Firedoze host,
use:

```toml
[caddy]
tls_mode = "behind_proxy"
```

Then have the load balancer forward plain HTTP to the host. Public users still
see HTTPS at the load balancer.

## WireGuard Through A Network Load Balancer

The recommended AWS shape is to put an internet-facing Network Load Balancer in
front of the Firedoze WireGuard UDP listener:

```text
developer laptop
  -> internet-facing NLB UDP 51820
  -> private Firedoze EC2 UDP 51820
  -> kernel WireGuard
  -> Firedoze API and VM private subnet
```

This keeps the large Firedoze host away from direct public UDP ingress without
running a separate forwarding instance. The load balancer only forwards
WireGuard packets; it does not expose the Firedoze HTTP API. The API should
still bind only to the Firedoze WireGuard address.

Recommended setup:

- Create an internet-facing Network Load Balancer.
- Add a UDP listener on `51820`.
- Add a UDP target group on `51820` with the Firedoze EC2 instance as the
  target.
- Attach a security group to the NLB when creating it.
- Allow inbound UDP `51820` to the NLB from developer IP ranges, or from the
  internet if you are relying only on WireGuard keys.
- Allow inbound UDP `51820` to the Firedoze EC2 instance from the NLB security
  group.
- Put the Firedoze instance in a private subnet if possible, and use IAM
  Session Manager for emergency/admin shell access.
- Set `wireguard.endpoint` to the NLB DNS name or to a stable DNS name that
  points at the NLB.

This is usually a better default than documenting a manual UDP forwarding
instance. A tiny forwarding box can be cheaper, but it adds another Linux host,
another patching surface, and hand-maintained routing or NAT rules. If someone
already wants that pattern, they probably know enough AWS networking to build it
without Firedoze-specific instructions.

Cost note: a Network Load Balancer has its own hourly charge, NLCU usage charge,
data transfer costs, and public IPv4 address charges. For a small team this is
often still a modest monthly cost, but it is normally more expensive than the
smallest possible EC2 forwarding instance. Treat the NLB as the managed,
lower-operations option rather than the cheapest possible option.

## Private Firedoze Host

With the WireGuard listener behind an NLB, the Firedoze host does not need a
public IP for management access. Public web traffic can be handled separately:

```text
internet -> ALB/CloudFront/NLB/direct EC2 80/443 -> Firedoze web listener -> VM
```

For the simplest setup, expose `80/443` directly on the Firedoze host and let
Caddy manage HTTPS:

```toml
[caddy]
tls_mode = "auto"
```

For a more AWS-native private-host setup, terminate public HTTPS at an ALB or
CloudFront distribution and forward plain HTTP to Firedoze:

```toml
[caddy]
tls_mode = "behind_proxy"
```

Keep these two surfaces separate in security groups:

- WireGuard UDP ingress to the NLB.
- Public web ingress to the chosen web load balancer or host listener.
- No direct public access to the Firedoze management API.

## Operational Notes

- Prefer IAM Session Manager for emergency/admin shell access where possible,
  instead of exposing SSH broadly.
- Keep the Firedoze management API WireGuard-only.
- Use security groups to express intent: public web, WireGuard ingress, and
  private Firedoze management should be separate rules.
- Use Elastic IPs or stable DNS for WireGuard endpoints; client configs include
  the endpoint host/port.
- Back up `/etc/firedoze` and `/var/lib/firedoze` if the environments matter.
- If using an NLB for WireGuard, keep the target security group limited to UDP
  `51820` from the NLB security group.

## References

- AWS EC2 nested virtualization announcement, February 16, 2026:
  https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual
- AWS EC2 nested virtualization documentation:
  https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
- AWS Nitro and bare metal instance documentation:
  https://docs.aws.amazon.com/ec2/latest/instancetypes/ec2-nitro-instances.html
- AWS Network Load Balancer target group documentation:
  https://docs.aws.amazon.com/elasticloadbalancing/latest/network/load-balancer-target-groups.html
- AWS Network Load Balancer security group documentation:
  https://docs.aws.amazon.com/elasticloadbalancing/latest/network/load-balancer-security-groups.html
- AWS Elastic Load Balancing pricing:
  https://aws.amazon.com/elasticloadbalancing/pricing/
