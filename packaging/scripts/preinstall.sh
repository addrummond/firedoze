#!/bin/sh
set -eu

service_user=firedoze
service_group=firedoze
statedir=/var/lib/firedoze

if ! getent group "$service_group" >/dev/null 2>&1; then
  groupadd --system "$service_group"
fi

if ! id -u "$service_user" >/dev/null 2>&1; then
  useradd --system --gid "$service_group" --home-dir "$statedir" --shell /usr/sbin/nologin "$service_user"
fi

if getent group kvm >/dev/null 2>&1; then
  usermod -a -G kvm "$service_user"
fi
