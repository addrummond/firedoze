#!/bin/sh
set -eu

purge=0
if [ "${1:-}" = "-purge" ] || [ "${1:-}" = "--purge" ]; then
  purge=1
elif [ "${1:-}" != "" ]; then
  echo "usage: sudo ./scripts/uninstall.sh [-purge]" >&2
  exit 1
fi

if [ "$(id -u)" -ne 0 ]; then
  echo "error: run as root, for example: sudo ./scripts/uninstall.sh" >&2
  exit 1
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl disable --now firedozed >/dev/null 2>&1 || true
fi

rm -f /etc/systemd/system/firedozed.service
rm -f /usr/local/bin/firedoze
rm -f /usr/local/bin/firedozed
rm -f /usr/local/bin/firedoze-image
rm -rf /usr/local/share/doc/firedoze

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
fi

if [ "$purge" -eq 1 ]; then
  rm -rf /etc/firedoze
  rm -rf /var/lib/firedoze
  rm -rf /var/log/firedoze
  echo "firedoze binaries, service, config, logs, and state removed"
else
  echo "firedoze binaries and service removed"
  echo "kept /etc/firedoze, /var/lib/firedoze, and /var/log/firedoze"
  echo "run with -purge to remove config, VM state, snapshots, images, and logs"
fi
