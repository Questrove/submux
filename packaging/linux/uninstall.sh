#!/bin/sh
set -eu

install_root=/usr/lib/submux-runtime
unit_root=/usr/lib/systemd/system
tmpfiles_root=/usr/lib/tmpfiles.d

fail() {
  echo "submux-runtime uninstaller: $*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "machine uninstallation requires root"
purge=false
if [ "$#" -gt 0 ]; then
  [ "$#" -eq 1 ] && [ "$1" = "--purge" ] ||
    fail "usage: submux-runtime-uninstall [--purge]"
  purge=true
fi
[ -x "$install_root/runtime-lifecycle" ] || fail "Runtime lifecycle helper is unavailable"

lifecycle_copy=$(mktemp)
trap 'rm -f "$lifecycle_copy"' EXIT HUP INT TERM
cp "$install_root/runtime-lifecycle" "$lifecycle_copy"
chmod 0700 "$lifecycle_copy"
"$lifecycle_copy" pre-remove
systemctl disable submux-runtime.service submux-runtime-net.service >/dev/null 2>&1 || true
rm -f \
  "$unit_root/submux-runtime.service" \
  "$unit_root/submux-runtime-net.service" \
  "$tmpfiles_root/submux-runtime.conf" \
  /usr/bin/submux-runtime \
  /usr/bin/submux-runtime-gui \
  /usr/sbin/submux-runtime-authorize-user \
  /usr/sbin/submux-runtime-uninstall
rm -rf -- "$install_root"
systemctl daemon-reload
if [ "$purge" = true ]; then
  "$lifecycle_copy" post-remove --purge
else
  "$lifecycle_copy" post-remove
fi
