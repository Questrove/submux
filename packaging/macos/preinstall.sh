#!/bin/sh
set -eu

version='@VERSION@'
kind='@KIND@'
install_root=/Library/PrivilegedHelperTools
state_root='/Library/Application Support/SubmuxRuntime'
receipt="$state_root/install-receipt.json"

fail() {
  echo "submux-runtime PKG preinstall: $*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "machine installation requires root"
major=$(sw_vers -productVersion | awk -F. '{ print $1 }')
[ "$major" -ge 13 ] || fail "macOS 13 or newer is required"

receipt_value() {
  key=$1
  [ -f "$receipt" ] || return 0
  sed -n "s/^[[:space:]]*\"${key}\":[[:space:]]*\"\\([^\"]*\\)\"[,]*[[:space:]]*$/\\1/p" "$receipt"
}

compare_versions() {
  left=${1#v}
  right=${2#v}
  awk -v left="$left" -v right="$right" 'BEGIN {
    split(left, l, "."); split(right, r, ".");
    for (i = 1; i <= 3; i++) {
      if ((l[i] + 0) < (r[i] + 0)) { print -1; exit }
      if ((l[i] + 0) > (r[i] + 0)) { print 1; exit }
    }
    print 0
  }'
}

if [ ! -f "$receipt" ]; then
  conflict=
  for path in \
    "$install_root/submux-runtime" \
    "$install_root/submux-runtime-net" \
    /Library/LaunchDaemons/com.byworld.submux.runtime.plist \
    /Library/LaunchDaemons/com.byworld.submux.runtime-net.plist; do
    [ ! -e "$path" ] || conflict=$path
  done
  if [ -n "$conflict" ] && [ "${SUBMUX_RUNTIME_REPAIR:-0}" != 1 ]; then
    fail "Runtime files without a valid receipt require explicit repair"
  fi
else
  current_root=$(receipt_value install_root)
  current_platform=$(receipt_value platform)
  current_arch=$(receipt_value arch)
  current_version=$(receipt_value version)
  [ "$current_root" = "$install_root" ] &&
    [ "$current_platform" = darwin ] &&
    [ "$current_arch" = universal ] &&
    [ -n "$current_version" ] ||
    fail "installed Runtime receipt is invalid or names a second installation target"
  comparison=$(compare_versions "$version" "$current_version")
  if [ "$comparison" -eq 0 ] && [ "${SUBMUX_RUNTIME_REPAIR:-0}" != 1 ]; then
    fail "same-version installation requires explicit repair"
  fi
  if [ "$comparison" -lt 0 ]; then
    [ "${SUBMUX_RUNTIME_ALLOW_DOWNGRADE:-0}" = 1 ] &&
      [ "${SUBMUX_RUNTIME_DATABASE_COMPATIBLE:-0}" = 1 ] ||
      fail "downgrade requires explicit authorization and database compatibility"
  fi
fi

if [ -x "$install_root/submux-runtime-lifecycle" ]; then
  "$install_root/submux-runtime-lifecycle" pre-remove
fi

case "$kind" in online | offline) ;; *) fail "package kind is invalid" ;; esac
exit 0
