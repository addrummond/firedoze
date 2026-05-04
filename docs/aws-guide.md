# AWS Guide

This is the recommended AWS shape for Firedoze.

Run one large EC2 instance in a private subnet. Put a Network Load Balancer in
front of WireGuard, and put an Application Load Balancer in front of public VM
HTTP traffic. Keep the Firedoze API and VM SSH reachable only through
WireGuard.

```text
developer laptop
  -> NLB UDP 51820
  -> Firedoze host WireGuard
  -> Firedoze API and VM private network

browser
  -> ALB HTTPS 443
  -> Firedoze host HTTP 80
  -> Firedoze wake proxy
  -> VM private HTTP port
```

## Instance

Use a virtual `C8i`, `M8i`, or `R8i` instance with nested virtualization
enabled. Start with `M8i` unless you already know the host should be CPU-heavy
or memory-heavy.

Firedoze needs `/dev/kvm`, because Firecracker uses KVM.

Launch example:

```sh
aws ec2 run-instances \
  --image-id ami-EXAMPLE \
  --instance-type m8i.4xlarge \
  --cpu-options "NestedVirtualization=enabled" \
  --key-name my-key
```

For an existing supported instance, stop it first, then enable nested
virtualization:

```sh
aws ec2 modify-instance-cpu-options \
  --instance-id i-EXAMPLE \
  --core-count 4 \
  --threads-per-core 2 \
  --nested-virtualization enabled
```

After boot:

```sh
test -e /dev/kvm && echo "KVM is present"
```

## Host OS

Use Ubuntu 24.04 LTS for now. That is the host OS currently covered by the
Firedoze admin quickstart.

## Firedoze Config

Use a public wildcard domain for VM web routes:

```toml
base_domain = "dev.example.com"
```

Configure Firedoze's public web listener for TLS termination at the ALB:

```toml
[caddy]
http_port = 80
https_port = 443
tls_mode = "behind_proxy"
```

In `behind_proxy` mode, Firedoze serves plain HTTP on `http_port`. Public users
still see HTTPS because the ALB terminates TLS.

Set the WireGuard endpoint to a stable DNS name for the NLB:

```toml
[wireguard]
endpoint = "wg.example.com:51820"
```

Do not expose the Firedoze API through the ALB. The API should bind only to the
WireGuard address.

## DNS

Create these DNS records:

```text
wg.example.com        -> alias to the Network Load Balancer
*.dev.example.com     -> alias to the Application Load Balancer
```

Use a WireGuard endpoint name outside `base_domain`, so it does not consume a VM
hostname.

## Network Load Balancer

Create an internet-facing Network Load Balancer for WireGuard.

Configure it like this:

- Listener: UDP `51820`.
- Target group: UDP `51820`.
- Target: the Firedoze EC2 instance.
- Health check: HTTP port `80`, path `/`, success codes `200-499`.
- Security group inbound: UDP `51820` from developer networks.
- Firedoze instance inbound: UDP `51820` from the NLB security group.
- Firedoze instance inbound for health checks: TCP `80` from the NLB security
  group.

The NLB only forwards WireGuard packets. It does not expose the Firedoze API.

## Application Load Balancer

Create an internet-facing Application Load Balancer for public VM HTTPS.

Configure it like this:

- Listener: HTTPS `443`.
- Certificate: wildcard certificate for `*.dev.example.com`.
- Target group protocol: HTTP.
- Target group port: `80`.
- Target: the Firedoze EC2 instance.
- Firedoze instance inbound: TCP `80` from the ALB security group.

The ALB forwards all hostnames under `*.dev.example.com` to Firedoze. Firedoze
then routes `https://<vm>.dev.example.com` or route aliases to the right VM.

For the target group health check, use:

```text
Path: /
Success codes: 200-499
```

Firedoze returns `404` for unknown route hostnames. That is a valid proof that
the public web listener is alive.

## Security Groups

Use three security groups:

```text
firedoze-nlb
  inbound  UDP 51820  from developer networks
  outbound UDP 51820  to firedoze-host
  outbound TCP 80     to firedoze-host for health checks

firedoze-alb
  inbound  TCP 443    from the internet
  outbound TCP 80     to firedoze-host

firedoze-host
  inbound  UDP 51820  from firedoze-nlb
  inbound  TCP 80     from firedoze-alb
  inbound  TCP 80     from firedoze-nlb for health checks
  no public inbound SSH
```

Use IAM Session Manager for emergency shell access to the Firedoze host.

## Install

On the EC2 host, follow [quickstart-admin.md](quickstart-admin.md).

The important AWS-specific config choices are:

```toml
base_domain = "dev.example.com"

[caddy]
http_port = 80
tls_mode = "behind_proxy"

[wireguard]
endpoint = "wg.example.com:51820"
```

## Verify

Check KVM:

```sh
test -e /dev/kvm && echo "KVM is present"
```

Check WireGuard from a client laptop:

```sh
firedoze health
```

Check public web routing:

```sh
firedoze vm create demo -publish
firedoze vm start demo
firedoze exec demo -- sudo firedoze-hello-service install 8080
firedoze vm list demo
curl https://demo.dev.example.com/
```

## References

- AWS EC2 nested virtualization announcement, February 16, 2026:
  https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual
- AWS EC2 nested virtualization documentation:
  https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
- AWS Network Load Balancer listeners:
  https://docs.aws.amazon.com/elasticloadbalancing/latest/network/load-balancer-listeners.html
- AWS Network Load Balancer health checks:
  https://docs.aws.amazon.com/elasticloadbalancing/latest/network/target-group-health-checks.html
- AWS Application Load Balancer listeners:
  https://docs.aws.amazon.com/elasticloadbalancing/latest/application/load-balancer-listeners.html
- AWS Application Load Balancer certificates:
  https://docs.aws.amazon.com/elasticloadbalancing/latest/application/https-listener-certificates.html
- AWS Application Load Balancer health checks:
  https://docs.aws.amazon.com/elasticloadbalancing/latest/application/target-group-health-checks.html
- Route 53 alias records for load balancers:
  https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/routing-to-elb-load-balancer.html
