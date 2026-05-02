#!/usr/bin/env bash
set -euo pipefail

release="noble"
arch="amd64"
size="4G"
out_dir="dist/base-image"
image_url=""
container_tool=""

usage() {
  cat <<'USAGE'
Usage: scripts/build-base-image.sh [options]

Build a Firecracker-ready Ubuntu root filesystem and matching boot artifacts.

Options:
  --out DIR        Output directory. Default: dist/base-image
  --release NAME   Ubuntu cloud image release. Default: noble
  --arch ARCH      Ubuntu architecture. Default: amd64
  --size SIZE      Root filesystem image size. Default: 4G
  --url URL        Override the Ubuntu root tarball URL
  --tool NAME      Container tool: docker or podman
  -h, --help       Show this help

The script uses Docker or Podman so it can run from macOS or Linux without
requiring host ext4 mount support.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      out_dir="${2:?missing value for --out}"
      shift 2
      ;;
    --release)
      release="${2:?missing value for --release}"
      shift 2
      ;;
    --arch)
      arch="${2:?missing value for --arch}"
      shift 2
      ;;
    --size)
      size="${2:?missing value for --size}"
      shift 2
      ;;
    --url)
      image_url="${2:?missing value for --url}"
      shift 2
      ;;
    --tool)
      container_tool="${2:?missing value for --tool}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ "$arch" != "amd64" ]]; then
  echo "only amd64 is supported for now; firedoze currently targets x86_64 hosts" >&2
  exit 2
fi

if [[ -z "$image_url" ]]; then
  case "$release" in
    noble)
      image_url="https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64-root.tar.xz"
      ;;
    *)
      image_url="https://cloud-images.ubuntu.com/${release}/current/${release}-server-cloudimg-amd64-root.tar.xz"
      ;;
  esac
fi

if [[ -z "$container_tool" ]]; then
  if command -v docker >/dev/null 2>&1; then
    container_tool="docker"
  elif command -v podman >/dev/null 2>&1; then
    container_tool="podman"
  else
    echo "docker or podman is required" >&2
    exit 1
  fi
fi

mkdir -p "$out_dir"
abs_out="$(cd "$out_dir" && pwd)"

"$container_tool" run --rm --platform linux/amd64 \
  -e FIREDOZE_RELEASE="$release" \
  -e FIREDOZE_ARCH="$arch" \
  -e FIREDOZE_SIZE="$size" \
  -e FIREDOZE_IMAGE_URL="$image_url" \
  -v "$abs_out:/out" \
  ubuntu:24.04 bash -s <<'CONTAINER'
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl e2fsprogs tar xz-utils

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
root="$work/root"
mkdir -p "$root"

curl -fsSL "$FIREDOZE_IMAGE_URL" -o "$work/rootfs.tar.xz"
tar -C "$root" -xJf "$work/rootfs.tar.xz"

mkdir -p "$root/etc/cloud/cloud.cfg.d" "$root/etc/firedoze" "$root/etc/ssh/sshd_config.d" "$root/etc/systemd/system" "$root/usr/local/sbin"

cat >"$root/etc/ssh/sshd_config.d/99-firedoze.conf" <<'EOF'
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
AuthorizedKeysFile /etc/firedoze/authorized_keys .ssh/authorized_keys
EOF

cat >"$root/usr/local/sbin/firedoze-guest-network" <<'EOF'
#!/bin/sh
set -eu

dev="${1:-eth0}"
mac="$(cat "/sys/class/net/$dev/address")"
IFS=: set -- $mac

if [ "$1:$2" != "06:00" ]; then
  echo "unexpected firedoze MAC prefix: $mac" >&2
  exit 1
fi

o1="$(printf "%d" "0x$3")"
o2="$(printf "%d" "0x$4")"
o3="$(printf "%d" "0x$5")"
o4="$(printf "%d" "0x$6")"
guest_ip="$o1.$o2.$o3.$o4"
host_ip="$o1.$o2.$o3.$((o4 - 1))"

ip addr flush dev "$dev"
ip addr add "$guest_ip/30" dev "$dev"
ip link set "$dev" up
ip route replace default via "$host_ip" dev "$dev"

cat >/etc/resolv.conf <<RESOLV
nameserver 1.1.1.1
nameserver 8.8.8.8
RESOLV
EOF
chmod 0755 "$root/usr/local/sbin/firedoze-guest-network"

cat >"$root/etc/systemd/system/firedoze-network.service" <<'EOF'
[Unit]
Description=Configure firedoze Firecracker guest networking
DefaultDependencies=no
Before=network-pre.target
Wants=network-pre.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/firedoze-guest-network eth0
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF

mkdir -p "$root/etc/systemd/system/multi-user.target.wants"
ln -sf /etc/systemd/system/firedoze-network.service "$root/etc/systemd/system/multi-user.target.wants/firedoze-network.service"

for unit in cloud-init.service cloud-init-local.service cloud-config.service cloud-final.service; do
  ln -sf /dev/null "$root/etc/systemd/system/$unit"
done

cat >"$root/etc/cloud/cloud.cfg.d/99-firedoze.cfg" <<'EOF'
datasource_list: [ None ]
preserve_hostname: true
manage_etc_hosts: false
ssh_pwauth: false
disable_root: false
EOF

kernel="$(find "$root/boot" -maxdepth 1 -type f -name 'vmlinuz-*' | sort -V | tail -n 1 || true)"
initrd="$(find "$root/boot" -maxdepth 1 -type f -name 'initrd.img-*' | sort -V | tail -n 1 || true)"

if [[ -z "$kernel" ]]; then
  echo "no /boot/vmlinuz-* found in Ubuntu root tarball" >&2
  exit 1
fi
if [[ -z "$initrd" ]]; then
  echo "no /boot/initrd.img-* found in Ubuntu root tarball" >&2
  exit 1
fi

rm -f /out/rootfs.ext4 /out/vmlinux.bin /out/initrd.img /out/manifest.txt
truncate -s "$FIREDOZE_SIZE" /out/rootfs.ext4
mkfs.ext4 -F -L firedoze-rootfs -d "$root" /out/rootfs.ext4
cp "$kernel" /out/vmlinux.bin
cp "$initrd" /out/initrd.img

cat >/out/manifest.txt <<EOF
release=$FIREDOZE_RELEASE
arch=$FIREDOZE_ARCH
source=$FIREDOZE_IMAGE_URL
rootfs=rootfs.ext4
kernel=vmlinux.bin
initrd=initrd.img
size=$FIREDOZE_SIZE
ssh_authorized_keys=/etc/firedoze/authorized_keys
network=06:00:<guest-ip-octets> with guest /30 and host at guest_ip-1
EOF
CONTAINER

cat <<EOF
Built firedoze base image artifacts in $abs_out:
  rootfs.ext4
  vmlinux.bin
  initrd.img
  manifest.txt
EOF
