#!/bin/sh
set -eu

prefix=/usr/local
sysconfdir=/etc/firedoze
statedir=/var/lib/firedoze
docdir="$prefix/share/doc/firedoze"
unit_src=systemd/firedozed.service
unit_dst=/etc/systemd/system/firedozed.service
config_src=config/firedoze.example.toml
config_dst="$sysconfdir/firedoze.toml"
config_example_dst="$sysconfdir/firedoze.example.toml"
service_user=firedoze
service_group=firedoze

if [ "$(id -u)" -eq 0 ]; then
  sudo_cmd=
elif command -v sudo >/dev/null 2>&1; then
  sudo_cmd=sudo
else
  echo "error: this script needs root privileges for installation; install sudo or run as root" >&2
  exit 1
fi

if [ ! -f go.mod ] || [ ! -f "$unit_src" ] || [ ! -f "$config_src" ]; then
  echo "error: run this script from the firedoze repository root" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "error: go is required to build firedoze" >&2
  exit 1
fi

as_root() {
  if [ -n "$sudo_cmd" ]; then
    sudo "$@"
  else
    "$@"
  fi
}

build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT INT TERM

echo "building firedoze binaries"
CGO_ENABLED=0 go build -o "$build_dir/firedoze" ./cmd/firedoze
CGO_ENABLED=0 go build -o "$build_dir/firedozed" ./cmd/firedozed
CGO_ENABLED=0 go build -o "$build_dir/firedoze-image-builder" ./cmd/firedoze-image-builder

echo "installing binaries to $prefix/bin"
as_root install -d -m 0755 "$prefix/bin"
as_root install -m 0755 "$build_dir/firedoze" "$prefix/bin/firedoze"
as_root install -m 0755 "$build_dir/firedozed" "$prefix/bin/firedozed"
as_root install -m 0755 "$build_dir/firedoze-image-builder" "$prefix/bin/firedoze-image-builder"

echo "creating firedoze service user"
if ! getent group "$service_group" >/dev/null 2>&1; then
  as_root groupadd --system "$service_group"
fi
if ! id -u "$service_user" >/dev/null 2>&1; then
  as_root useradd --system --gid "$service_group" --home-dir "$statedir" --shell /usr/sbin/nologin "$service_user"
fi
if getent group kvm >/dev/null 2>&1; then
  as_root usermod -a -G kvm "$service_user"
fi

echo "creating firedoze directories"
as_root install -d -o root -g "$service_group" -m 2770 "$sysconfdir"
as_root install -d -o "$service_user" -g "$service_group" -m 0750 "$statedir"
as_root install -d -o "$service_user" -g "$service_group" -m 0755 "$statedir/images"
as_root install -d -o "$service_user" -g "$service_group" -m 0750 /var/log/firedoze
as_root install -d -m 0755 "$docdir"

echo "installing example config to $config_example_dst"
as_root install -o root -g "$service_group" -m 0640 "$config_src" "$config_example_dst"
if [ -f "$config_dst" ]; then
  echo "leaving existing config in place: $config_dst"
fi
as_root chown -R "$service_user:$service_group" "$statedir" /var/log/firedoze
as_root chgrp -R "$service_group" "$sysconfdir"
as_root chmod 2770 "$sysconfdir"
as_root find "$sysconfdir" -type f -name '*.toml' -exec chmod 0640 {} +
if [ -f "$sysconfdir/wg.key" ]; then
  as_root chown "$service_user:$service_group" "$sysconfdir/wg.key"
  as_root chmod 0600 "$sysconfdir/wg.key"
fi

echo "installing documentation and systemd unit"
as_root install -d -m 0755 "$docdir/developer"
as_root install -m 0644 docs/quickstart-admin.md "$docdir/quickstart-admin.md"
as_root install -m 0644 docs/quickstart-user.md "$docdir/quickstart-user.md"
as_root install -m 0644 docs/adr.md "$docdir/adr.md"
as_root install -m 0644 docs/developer/client-wireguard-broker.md "$docdir/developer/client-wireguard-broker.md"
as_root install -m 0644 "$unit_src" "$unit_dst"

if command -v systemctl >/dev/null 2>&1; then
  as_root systemctl daemon-reload
fi

cat <<EOF

firedoze is installed.

The firedozed systemd service runs as the '$service_user' system user with
limited network/bind capabilities.

Next steps:
  1. To run the full host setup from scratch, use: task setup:host
     To do only the remaining image steps now, use:
       task firecracker:install
       task image:build
       task image:install
  2. Create $config_dst:
     If you have a domain name:
       sudo firedozed -init-config -init-host <DOMAIN_NAME>
     If you have only an IP address:
       sudo firedozed -init-config -init-sslip-host \$(curl -4 https://ifconfig.me)
  3. Ask the client to run 'firedoze server request alice-laptop', then add
     their public key and send the printed import config back to them:
       sudo firedozed -wg-add-peer alice-laptop <ALICE_PUBLIC_KEY>
  4. Start the daemon:
       sudo systemctl enable --now firedozed

EOF
