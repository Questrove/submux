#!/bin/sh
set -eu

state_root=/var/lib/submux-runtime
privileged_state_root=/var/lib/submux-runtime-privileged
config_root=/etc/submux-runtime
run_root=/run/submux-runtime
privileged_run_root=/run/submux-runtime-privileged

systemctl daemon-reload >/dev/null 2>&1 || true
residuals=
for service in submux-runtime.service submux-runtime-net.service; do
  if systemctl is-active --quiet "$service" 2>/dev/null; then
    residuals="${residuals}
service remains active: $service"
  fi
done
for path in "$run_root/runtime.sock" "$privileged_run_root/runtime-net.sock"; do
  if [ -e "$path" ] || [ -S "$path" ]; then
    residuals="${residuals}
Socket remains: $path"
  fi
done
routes=$(ip -o rule show 2>/dev/null | grep -i submux || true)
routes="${routes}$(ip -o route show table all 2>/dev/null | grep -i submux || true)"
firewall=$(nft list tables 2>/dev/null | grep -i submux || true)
tun=$(find /sys/class/net -maxdepth 1 -mindepth 1 -printf '%f\n' 2>/dev/null |
  grep -E '^(submux|tun-submux)' || true)
dns=$(resolvectl status 2>/dev/null | grep -i submux || true)
[ -z "$routes" ] || residuals="${residuals}
routes:
$routes"
[ -z "$firewall" ] || residuals="${residuals}
firewall:
$firewall"
[ -z "$tun" ] || residuals="${residuals}
interfaces:
$tun"
[ -z "$dns" ] || residuals="${residuals}
DNS:
$dns"
if [ -n "$residuals" ]; then
  printf 'Submux Runtime uninstall residuals detected:%s\n' "$residuals" >&2
  exit 1
else
  echo "No Runtime-owned service, route, DNS, firewall, TUN, or Socket residual was detected."
fi

members=$(getent group submux-runtime 2>/dev/null |
  awk -F: '{ print $4 }' | tr ',' ' ' || true)
for member in $members; do
  [ "$member" = submux-runtime ] ||
    gpasswd --delete "$member" submux-runtime >/dev/null 2>&1 || true
done

if [ "${1:-}" = purge ]; then
  for path in \
    "$run_root" \
    "$privileged_run_root" \
    "$state_root" \
    "$privileged_state_root" \
    "$config_root"; do
    [ ! -L "$path" ] || {
      echo "refusing to purge symbolic-link Runtime path: $path" >&2
      exit 1
    }
  done
  rm -rf -- \
    "$run_root" \
    "$privileged_run_root" \
    "$state_root" \
    "$privileged_state_root" \
    "$config_root"
  getent passwd submux-runtime >/dev/null 2>&1 && userdel submux-runtime || true
  getent group submux-runtime >/dev/null 2>&1 && groupdel submux-runtime || true
else
  echo "Runtime state was retained at $state_root."
  echo "Privileged Runtime state was retained at $privileged_state_root."
fi
