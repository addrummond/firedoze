package guestfiles

const NetworkScript = `#!/bin/sh
set -eu

dev="${1:-}"
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if [ -z "$dev" ]; then
    for candidate in /sys/class/net/*; do
      name="${candidate##*/}"
      [ "$name" = "lo" ] && continue
      [ -e "$candidate/address" ] || continue
      mac="$(cat "$candidate/address")"
      case "$mac" in
        06:00:*) dev="$name"; break ;;
      esac
    done
  fi
  if [ -n "$dev" ]; then
    break
  fi
  sleep 1
done

if [ -z "$dev" ]; then
  for candidate in /sys/class/net/*; do
    name="${candidate##*/}"
    [ "$name" = "lo" ] && continue
    echo "seen network interface $name mac=$(cat "$candidate/address" 2>/dev/null || true)" >&2
  done
fi

if [ -z "$dev" ]; then
  echo "could not find firedoze network interface" >&2
  exit 1
fi

guest_ip=""
host_ip=""
guest_ipv4=""
host_ipv4=""
dns_ip=""
dns_domain="firedoze"
for arg in $(cat /proc/cmdline 2>/dev/null || true); do
  case "$arg" in
    firedoze.guest_ip=*) guest_ip="${arg#firedoze.guest_ip=}" ;;
    firedoze.host_ip=*) host_ip="${arg#firedoze.host_ip=}" ;;
    firedoze.guest_ipv4=*) guest_ipv4="${arg#firedoze.guest_ipv4=}" ;;
    firedoze.host_ipv4=*) host_ipv4="${arg#firedoze.host_ipv4=}" ;;
    firedoze.dns_ip=*) dns_ip="${arg#firedoze.dns_ip=}" ;;
    firedoze.dns_domain=*) dns_domain="${arg#firedoze.dns_domain=}" ;;
  esac
done

if [ -z "$guest_ip" ] || [ -z "$host_ip" ]; then
  echo "missing firedoze.guest_ip or firedoze.host_ip kernel arg" >&2
  exit 1
fi

/bin/ip addr flush dev "$dev"
/bin/ip -6 addr flush dev "$dev" scope global || true
/bin/ip link set "$dev" up
/sbin/sysctl -w "net.ipv6.conf.$dev.accept_dad=0" "net.ipv6.conf.$dev.dad_transmits=0" >/dev/null 2>&1 || true
/bin/ip -6 addr add "$guest_ip/127" dev "$dev"
/bin/ip -6 route replace default via "$host_ip" dev "$dev"
if [ -n "$guest_ipv4" ] && [ -n "$host_ipv4" ]; then
  /bin/ip addr add "$guest_ipv4/31" dev "$dev"
  /bin/ip route replace default via "$host_ipv4" dev "$dev"
fi
if [ -n "$dns_ip" ]; then
  /bin/ip -6 route replace "$dns_ip/128" via "$host_ip" dev "$dev"
fi
echo "configured firedoze network interface $dev guest_ip=$guest_ip host_ip=$host_ip guest_ipv4=$guest_ipv4 host_ipv4=$host_ipv4" >&2

rm -f /etc/resolv.conf
if [ -n "$dns_ip" ]; then
  cat >/etc/resolv.conf <<RESOLV
search $dns_domain
nameserver $dns_ip
RESOLV
else
  cat >/etc/resolv.conf <<RESOLV
nameserver 1.1.1.1
nameserver 8.8.8.8
RESOLV
fi
`
