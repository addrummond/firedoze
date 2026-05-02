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
go build -o "$build_dir/firedoze" ./cmd/firedoze
go build -o "$build_dir/firedozed" ./cmd/firedozed
go build -o "$build_dir/firedoze-image" ./cmd/firedoze-image

echo "installing binaries to $prefix/bin"
as_root install -d -m 0755 "$prefix/bin"
as_root install -m 0755 "$build_dir/firedoze" "$prefix/bin/firedoze"
as_root install -m 0755 "$build_dir/firedozed" "$prefix/bin/firedozed"
as_root install -m 0755 "$build_dir/firedoze-image" "$prefix/bin/firedoze-image"

echo "creating firedoze directories"
as_root install -d -m 0755 "$sysconfdir"
as_root install -d -m 0755 "$statedir"
as_root install -d -m 0755 "$statedir/images"
as_root install -d -m 0755 /var/log/firedoze
as_root install -d -m 0755 "$docdir"

if [ ! -f "$config_dst" ]; then
  echo "installing example config to $config_dst"
  as_root install -m 0644 "$config_src" "$config_dst"
else
  echo "leaving existing config in place: $config_dst"
fi

if [ ! -f "$sysconfdir/authorized_keys" ]; then
  echo "creating empty guest authorized_keys file"
  as_root install -m 0644 /dev/null "$sysconfdir/authorized_keys"
fi

echo "installing documentation and systemd unit"
as_root install -m 0644 Quickstart.md "$docdir/Quickstart.md"
as_root install -m 0644 ADR.md "$docdir/ADR.md"
as_root install -m 0644 "$unit_src" "$unit_dst"

if command -v systemctl >/dev/null 2>&1; then
  as_root systemctl daemon-reload
fi

cat <<EOF

firedoze is installed.

Next steps:
  1. Build the base image with: firedoze-image build
  2. Install the generated image files with:
       mise exec -- task image:install
  3. Add your SSH public key:
       cat ~/.ssh/id_ed25519.pub | sudo tee $sysconfdir/authorized_keys
  4. Edit $config_dst:
       sudoedit $config_dst
  5. Generate a WireGuard peer bundle:
       sudo firedozed -wg-new-peer alice-laptop 10.77.0.2/32
  6. Start the daemon:
       sudo systemctl enable --now firedozed

EOF
