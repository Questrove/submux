#!/bin/sh
set -eu

fail() {
  echo "submux-runtime macOS uninstall: $*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "uninstall requires root"
purge=false
if [ "$#" -gt 0 ]; then
  [ "$#" -eq 1 ] && [ "$1" = --purge ] ||
    fail "usage: submux-runtime-uninstall [--purge]"
  purge=true
fi

lifecycle=/Library/PrivilegedHelperTools/submux-runtime-lifecycle
[ -x "$lifecycle" ] || fail "installed Runtime lifecycle helper is missing"
temporary=$(mktemp /var/tmp/submux-runtime-lifecycle.XXXXXX)
trap 'rm -f "$temporary"' EXIT HUP INT TERM
cp "$lifecycle" "$temporary"
chmod 0700 "$temporary"
"$temporary" pre-remove

for path in \
  '/Applications/Submux Runtime.app' \
  /Library/PrivilegedHelperTools/SubmuxRuntimeOffline \
  /Library/PrivilegedHelperTools/SubmuxRuntimeLicenses; do
  [ ! -L "$path" ] || fail "refusing symbolic-link installed path: $path"
  if [ -e "$path" ]; then
    [ -d "$path" ] || fail "installed path is not a directory: $path"
  fi
done

rm -f \
  /Library/LaunchDaemons/com.byworld.submux.runtime.plist \
  /Library/LaunchDaemons/com.byworld.submux.runtime-net.plist \
  /usr/local/bin/submux-runtime \
  /usr/local/bin/submux-runtime-gui \
  /usr/local/sbin/submux-runtime-authorize-user \
  /usr/local/sbin/submux-runtime-uninstall
rm -rf -- \
  '/Applications/Submux Runtime.app' \
  /Library/PrivilegedHelperTools/SubmuxRuntimeOffline
rm -f \
  /Library/PrivilegedHelperTools/submux-runtime \
  /Library/PrivilegedHelperTools/submux-runtime-net \
  /Library/PrivilegedHelperTools/submux-runtime-lifecycle \
  /Library/PrivilegedHelperTools/com.byworld.submux.gui.plist \
  /Library/PrivilegedHelperTools/SBOM.spdx.json \
  /Library/PrivilegedHelperTools/ARTIFACT-MANIFEST.sha256
rm -rf -- /Library/PrivilegedHelperTools/SubmuxRuntimeLicenses

if [ "$purge" = true ]; then
  "$temporary" post-remove --purge
else
  "$temporary" post-remove
fi
