#!/bin/sh
set -eu

service_user=firedoze
service_group=firedoze
sysconfdir=/etc/firedoze
statedir=/var/lib/firedoze
logdir=/var/log/firedoze

install -d -o root -g "$service_group" -m 2770 "$sysconfdir"
install -d -o "$service_user" -g "$service_group" -m 0750 "$statedir"
install -d -o "$service_user" -g "$service_group" -m 0755 "$statedir/images"
install -d -o "$service_user" -g "$service_group" -m 0750 "$logdir"

chgrp -R "$service_group" "$sysconfdir"
chmod 2770 "$sysconfdir"
find "$sysconfdir" -type f -name '*.toml' -exec chmod 0640 {} +

if [ -f "$sysconfdir/wg.key" ]; then
  chown "$service_user:$service_group" "$sysconfdir/wg.key"
  chmod 0600 "$sysconfdir/wg.key"
fi

if getent group kvm >/dev/null 2>&1; then
  usermod -a -G kvm "$service_user"
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload || true
fi
