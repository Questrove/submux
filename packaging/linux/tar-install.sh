#!/bin/sh
set -eu

fail() {
  echo "submux-runtime tar installer: $*" >&2
  exit 1
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
metadata="$script_dir/PACKAGE-METADATA"
payload="$script_dir/root"
install_root=/usr/lib/submux-runtime
state_root=/var/lib/submux-runtime
unit_root=/usr/lib/systemd/system
tmpfiles_root=/usr/lib/tmpfiles.d
receipt="$state_root/install-receipt.json"

[ "$(id -u)" -eq 0 ] || fail "machine installation requires root"
if [ ! -f "$metadata" ] || [ -L "$metadata" ]; then
  fail "package metadata is missing"
fi
if [ ! -d "$payload" ] || [ -L "$payload" ]; then
  fail "package payload is missing"
fi

metadata_value() {
  key=$1
  value=$(sed -n "s/^${key}=//p" "$metadata")
  [ -n "$value" ] || fail "package metadata omits $key"
  [ "$(printf '%s\n' "$value" | wc -l)" -eq 1 ] || fail "package metadata repeats $key"
  printf '%s' "$value"
}

version=$(metadata_value VERSION)
architecture=$(metadata_value ARCH)
package_kind=$(metadata_value KIND)

verify_payload() {
  manifest="$payload/usr/lib/submux-runtime/ARTIFACT-MANIFEST.sha256"
  if [ ! -f "$manifest" ] || [ -L "$manifest" ]; then
    fail "artifact manifest is missing"
  fi
  (
    cd "$payload"
    sha256sum --strict --check "usr/lib/submux-runtime/ARTIFACT-MANIFEST.sha256"
  ) || fail "artifact manifest verification failed"
  [ "$(readlink "$payload/usr/bin/submux-runtime")" = ../lib/submux-runtime/submux-runtime ] ||
    fail "Runtime CLI link is invalid"
  if [ -e "$payload/usr/lib/submux-runtime/submux-runtime-gui" ] ||
     [ -L "$payload/usr/bin/submux-runtime-gui" ]; then
    if [ ! -f "$payload/usr/lib/submux-runtime/submux-runtime-gui" ] ||
       [ "$(readlink "$payload/usr/bin/submux-runtime-gui")" != ../lib/submux-runtime/submux-runtime-gui ]; then
      fail "Runtime GUI payload or link is invalid"
    fi
  fi
  unexpected=$(find "$payload" -type l \
    ! -path "$payload/usr/bin/submux-runtime" \
    ! -path "$payload/usr/bin/submux-runtime-gui" -print -quit)
  [ -z "$unexpected" ] || fail "package payload contains an unexpected link"
  non_regular=$(find "$payload" ! -type d ! -type f ! -type l -print -quit)
  [ -z "$non_regular" ] || fail "package payload contains a non-regular entry"
}

receipt_value() {
  key=$1
  [ -f "$receipt" ] || return 0
  sed -n "s/^[[:space:]]*\"${key}\":[[:space:]]*\"\\([^\"]*\\)\"[,]*[[:space:]]*$/\\1/p" "$receipt"
}

validate_state() {
  repair=$1
  allow_downgrade=$2
  database_compatible=$3
  if [ ! -f "$receipt" ]; then
    for path in \
      /etc/systemd/system/submux-runtime.service \
      "$install_root" \
      "$state_root" \
      /var/lib/submux-runtime-privileged \
      "$unit_root/submux-runtime.service" \
      "$unit_root/submux-runtime-net.service"; do
      if [ -e "$path" ]; then
        [ "$repair" = true ] ||
          fail "Runtime files without a valid receipt require --repair"
      fi
    done
    return 0
  fi
  current_root=$(receipt_value install_root)
  current_version=$(receipt_value version)
  [ "$current_root" = "$install_root" ] ||
    fail "a second Runtime installation target is not allowed"
  [ -n "$current_version" ] || fail "installed Runtime receipt is invalid"
  if [ "$current_version" = "$version" ]; then
    [ "$repair" = true ] || fail "same-version installation requires --repair"
    return 0
  fi
  current_number=${current_version#v}
  target_number=${version#v}
  if printf '%s\n%s\n' "$target_number" "$current_number" | sort -V -C; then
    [ "$allow_downgrade" = true ] ||
      fail "downgrade requires --allow-downgrade"
    [ "$database_compatible" = true ] ||
      fail "downgrade requires --database-compatible"
  fi
}

stop_existing() {
  if [ -x "$install_root/runtime-lifecycle" ]; then
    "$install_root/runtime-lifecycle" pre-remove
  fi
}

install_payload() {
  backup=$(mktemp -d /tmp/submux-runtime-installer.XXXXXX)
  cleanup() {
    rm -rf -- "$backup"
  }
  trap cleanup EXIT HUP INT TERM
  if [ -d "$install_root" ]; then
    cp -a "$install_root/." "$backup/install-root"
  fi
  for item in submux-runtime.service submux-runtime-net.service; do
    if [ -f "$unit_root/$item" ]; then
      mkdir -p "$backup/units"
      cp -a "$unit_root/$item" "$backup/units/$item"
    fi
  done
  if [ -f "$tmpfiles_root/submux-runtime.conf" ]; then
    cp -a "$tmpfiles_root/submux-runtime.conf" "$backup/tmpfiles.conf"
  fi
  stop_existing
  if ! cp -a --no-preserve=ownership "$payload/." /; then
    restore_payload "$backup"
    fail "copying the Runtime payload failed"
  fi
  chown -R root:root "$install_root"
  chown root:root \
    "$unit_root/submux-runtime.service" \
    "$unit_root/submux-runtime-net.service" \
    "$tmpfiles_root/submux-runtime.conf"
  lifecycle_args="post-install $version $architecture $package_kind"
  if [ -n "$authorize_user" ]; then
    lifecycle_args="$lifecycle_args --authorize-desktop $authorize_user"
  fi
  # User names are strictly validated by runtime-lifecycle before use.
  # shellcheck disable=SC2086
  if ! "$install_root/runtime-lifecycle" $lifecycle_args; then
    restore_payload "$backup"
    fail "Runtime service activation failed; previous files were restored"
  fi
  trap - EXIT HUP INT TERM
  cleanup
}

restore_payload() {
  backup=$1
  systemctl stop submux-runtime.service submux-runtime-net.service >/dev/null 2>&1 || true
  rm -rf -- "$install_root"
  if [ -d "$backup/install-root" ]; then
    install -d -m 0755 -o root -g root "$install_root"
    cp -a "$backup/install-root/." "$install_root"
  fi
  for item in submux-runtime.service submux-runtime-net.service; do
    if [ -f "$backup/units/$item" ]; then
      cp -a "$backup/units/$item" "$unit_root/$item"
    else
      rm -f "$unit_root/$item"
    fi
  done
  if [ -f "$backup/tmpfiles.conf" ]; then
    cp -a "$backup/tmpfiles.conf" "$tmpfiles_root/submux-runtime.conf"
  else
    rm -f "$tmpfiles_root/submux-runtime.conf"
  fi
  systemctl daemon-reload
  if [ -x "$install_root/runtime-lifecycle" ]; then
    current_version=$(receipt_value version)
    current_kind=$(receipt_value package_kind)
    if [ -n "$current_version" ]; then
      [ -n "$current_kind" ] || current_kind=online
      "$install_root/runtime-lifecycle" post-install "$current_version" "$architecture" "$current_kind" || true
    fi
  fi
}

uninstall_payload() {
  purge=$1
  if [ -x "$install_root/runtime-lifecycle" ]; then
    "$install_root/runtime-lifecycle" pre-remove
  fi
  systemctl disable submux-runtime.service submux-runtime-net.service >/dev/null 2>&1 || true
  rm -f \
    "$unit_root/submux-runtime.service" \
    "$unit_root/submux-runtime-net.service" \
    "$tmpfiles_root/submux-runtime.conf" \
    /usr/bin/submux-runtime \
    /usr/bin/submux-runtime-gui \
    /usr/sbin/submux-runtime-authorize-user \
    /usr/sbin/submux-runtime-uninstall
  lifecycle_copy=$(mktemp)
  if [ -x "$install_root/runtime-lifecycle" ]; then
    cp "$install_root/runtime-lifecycle" "$lifecycle_copy"
    chmod 0700 "$lifecycle_copy"
  fi
  rm -rf -- "$install_root"
  systemctl daemon-reload
  if [ -x "$lifecycle_copy" ]; then
    if [ "$purge" = true ]; then
      "$lifecycle_copy" post-remove --purge
    else
      "$lifecycle_copy" post-remove
    fi
  fi
  rm -f "$lifecycle_copy"
}

command=install
repair=false
allow_downgrade=false
database_compatible=false
purge=false
authorize_user=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --install) command=install ;;
    --uninstall) command=uninstall ;;
    --repair) repair=true ;;
    --allow-downgrade) allow_downgrade=true ;;
    --database-compatible) database_compatible=true ;;
    --purge) purge=true ;;
    --authorize-desktop)
      shift
      [ "$#" -gt 0 ] || fail "--authorize-desktop requires a user"
      authorize_user=$1
      ;;
    *) fail "unsupported option $1" ;;
  esac
  shift
done

case "$command" in
  install)
    verify_payload
    validate_state "$repair" "$allow_downgrade" "$database_compatible"
    install_payload
    ;;
  uninstall)
    uninstall_payload "$purge"
    ;;
esac
