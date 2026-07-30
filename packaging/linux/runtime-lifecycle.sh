#!/bin/sh
set -eu

install_root=/usr/lib/submux-runtime
config_root=/etc/submux-runtime
state_root=/var/lib/submux-runtime
privileged_state_root=/var/lib/submux-runtime-privileged
run_root=/run/submux-runtime
privileged_run_root=/run/submux-runtime-privileged
receipt="$state_root/install-receipt.json"
runtime_user=submux-runtime
runtime_group=submux-runtime
runtime_service=submux-runtime.service
network_service=submux-runtime-net.service

fail() {
  echo "submux-runtime installer: $*" >&2
  exit 1
}

require_root() {
  [ "$(id -u)" -eq 0 ] || fail "machine installation requires root"
}

require_systemd_glibc() {
  [ -d /run/systemd/system ] || fail "stable Linux packages require a running systemd system"
  getconf GNU_LIBC_VERSION >/dev/null 2>&1 ||
    fail "stable Linux packages require glibc; musl and OpenWrt remain preview-only"
}

require_fixed_directory() {
  path=$1
  [ ! -L "$path" ] || fail "refusing symbolic-link path $path"
  if [ -e "$path" ]; then
    [ -d "$path" ] || fail "required directory is not a directory: $path"
  fi
}

create_identity() {
  if ! getent group "$runtime_group" >/dev/null 2>&1; then
    groupadd --system "$runtime_group"
  fi
  if ! getent passwd "$runtime_user" >/dev/null 2>&1; then
    useradd \
      --system \
      --gid "$runtime_group" \
      --home-dir "$state_root" \
      --no-create-home \
      --shell /usr/sbin/nologin \
      "$runtime_user"
  fi
  user_gid=$(id -gn "$runtime_user")
  [ "$user_gid" = "$runtime_group" ] ||
    fail "existing Runtime account has an unexpected primary group"
}

prepare_directories() {
  for path in \
    "$install_root" \
    "$config_root" \
    "$state_root" \
    "$privileged_state_root" \
    "$run_root" \
    "$privileged_run_root"; do
    require_fixed_directory "$path"
  done
  install -d -m 0755 -o root -g root "$install_root"
  install -d -m 0700 -o "$runtime_user" -g "$runtime_group" "$config_root"
  install -d -m 0700 -o "$runtime_user" -g "$runtime_group" "$state_root"
  install -d -m 0700 -o root -g root "$privileged_state_root"
  install -d -m 0700 -o root -g root "$privileged_state_root/network"
  install -d -m 0750 -o "$runtime_user" -g "$runtime_group" "$run_root"
  install -d -m 2750 -o root -g "$runtime_group" "$privileged_run_root"
}

authorize_desktop_user() {
  user_name=${1:-}
  [ -n "$user_name" ] || return 0
  case "$user_name" in
    *[!A-Za-z0-9_.-]* | -* | .*) fail "desktop user name is invalid" ;;
  esac
  id "$user_name" >/dev/null 2>&1 || fail "desktop user does not exist"
  usermod --append --groups "$runtime_group" "$user_name"
  echo "Authorized $user_name for the Runtime management Socket. A new login session may be required."
}

write_receipt() {
  version=$1
  architecture=$2
  package_kind=$3
  printf '%s\n' "$version" |
    grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' ||
    fail "package version must be vX.Y.Z"
  case "$architecture" in amd64 | arm64) ;; *) fail "unsupported Linux architecture" ;; esac
  case "$package_kind" in online | offline) ;; *) fail "package kind must be online or offline" ;; esac
  temporary=$(mktemp "$state_root/.install-receipt.XXXXXX")
  trap 'rm -f "$temporary"' EXIT HUP INT TERM
  installed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  printf '%s\n' \
    '{' \
    '  "format_version": 1,' \
    '  "product_id": "com.byworld.submux.runtime",' \
    "  \"version\": \"$version\"," \
    '  "install_root": "/usr/lib/submux-runtime",' \
    '  "platform": "linux",' \
    "  \"arch\": \"$architecture\"," \
    "  \"package_kind\": \"$package_kind\"," \
    "  \"installed_at\": \"$installed_at\"" \
    '}' >"$temporary"
  chown "$runtime_user:$runtime_group" "$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$receipt"
  trap - EXIT HUP INT TERM
}

start_services() {
  systemctl daemon-reload
  systemctl enable "$network_service" "$runtime_service"
  systemctl restart "$network_service"
  systemctl restart "$runtime_service"
  systemctl is-active --quiet "$network_service"
  systemctl is-active --quiet "$runtime_service"
  attempts=0
  while [ "$attempts" -lt 30 ]; do
    if systemctl is-active --quiet "$network_service" &&
       systemctl is-active --quiet "$runtime_service" &&
       [ -S "$privileged_run_root/runtime-net.sock" ] &&
       [ -S "$run_root/runtime.sock" ]; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  fail "Runtime management Socket did not become ready"
}

stop_fail_open() {
  command -v systemctl >/dev/null 2>&1 ||
    fail "systemctl is required to stop an installed Runtime safely"
  stop_error=
  if systemctl list-unit-files "$runtime_service" >/dev/null 2>&1 ||
     systemctl is-active --quiet "$runtime_service" 2>/dev/null; then
    if ! systemctl stop "$runtime_service"; then
      stop_error="Runtime service stop command failed"
    fi
    if systemctl is-active --quiet "$runtime_service"; then
      stop_error="${stop_error:+$stop_error; }Runtime service remains active"
    fi
  fi
  if systemctl list-unit-files "$network_service" >/dev/null 2>&1 ||
     systemctl is-active --quiet "$network_service" 2>/dev/null; then
    if ! systemctl stop "$network_service"; then
      stop_error="${stop_error:+$stop_error; }privileged network service stop command failed"
    fi
    if systemctl is-active --quiet "$network_service"; then
      stop_error="${stop_error:+$stop_error; }privileged network service remains active"
    fi
  fi
  [ -z "$stop_error" ] ||
    fail "$stop_error; direct networking could not be confirmed"
  return 0
}

residual_report() {
  active_services=
  for service in "$runtime_service" "$network_service"; do
    if systemctl is-active --quiet "$service" 2>/dev/null; then
      active_services="$active_services $service"
    fi
  done
  routes=$(ip -o rule show 2>/dev/null | grep -i submux || true)
  routes="$routes$(ip -o route show table all 2>/dev/null | grep -i submux || true)"
  firewall=$(nft list tables 2>/dev/null | grep -i submux || true)
  tun=$(find /sys/class/net -maxdepth 1 -mindepth 1 -printf '%f\n' 2>/dev/null |
    grep -E '^(submux|tun-submux)' || true)
  dns=$(resolvectl status 2>/dev/null | grep -i submux || true)
  files=
  for path in "$run_root/runtime.sock" "$privileged_run_root/runtime-net.sock"; do
    [ ! -e "$path" ] && [ ! -S "$path" ] || files="$files $path"
  done
  if [ -n "$active_services$routes$firewall$tun$dns$files" ]; then
    echo "Submux Runtime uninstall residuals detected:" >&2
    [ -z "$active_services" ] || echo "  services:$active_services" >&2
    [ -z "$routes" ] || printf '  routes:\n%s\n' "$routes" >&2
    [ -z "$firewall" ] || printf '  firewall:\n%s\n' "$firewall" >&2
    [ -z "$tun" ] || printf '  interfaces:\n%s\n' "$tun" >&2
    [ -z "$dns" ] || printf '  DNS:\n%s\n' "$dns" >&2
    [ -z "$files" ] || echo "  sockets:$files" >&2
    return 1
  fi
  echo "No Runtime-owned service, route, DNS, firewall, TUN, or Socket residual was detected."
}

revoke_operator_members() {
  members=$(getent group "$runtime_group" 2>/dev/null |
    awk -F: '{ print $4 }' | tr ',' ' ' || true)
  for member in $members; do
    [ "$member" = "$runtime_user" ] ||
      gpasswd --delete "$member" "$runtime_group" >/dev/null 2>&1 || true
  done
}

purge_state() {
  for path in \
    "$run_root" \
    "$privileged_run_root" \
    "$state_root" \
    "$privileged_state_root" \
    "$config_root"; do
    require_fixed_directory "$path"
  done
  rm -rf -- \
    "$run_root" \
    "$privileged_run_root" \
    "$state_root" \
    "$privileged_state_root" \
    "$config_root"
  if getent passwd "$runtime_user" >/dev/null 2>&1; then
    userdel "$runtime_user"
  fi
  if getent group "$runtime_group" >/dev/null 2>&1; then
    groupdel "$runtime_group"
  fi
}

command=${1:-}
shift || true
require_root
case "$command" in
  post-install)
    [ "$#" -ge 3 ] && [ "$#" -le 5 ] ||
      fail "post-install requires VERSION ARCH KIND [--authorize-desktop USER]"
    version=$1
    architecture=$2
    package_kind=$3
    shift 3
    desktop_user=
    if [ "$#" -gt 0 ]; then
      [ "$#" -eq 2 ] && [ "$1" = "--authorize-desktop" ] ||
        fail "desktop authorization must be explicit"
      desktop_user=$2
    fi
    require_systemd_glibc
    create_identity
    prepare_directories
    authorize_desktop_user "$desktop_user"
    start_services
    write_receipt "$version" "$architecture" "$package_kind"
    ;;
  pre-remove)
    [ "$#" -eq 0 ] || fail "pre-remove does not accept parameters"
    stop_fail_open
    ;;
  post-remove)
    [ "$#" -le 1 ] || fail "post-remove accepts only --purge"
    purge=false
    if [ "$#" -eq 1 ]; then
      [ "$1" = "--purge" ] || fail "post-remove accepts only --purge"
      purge=true
    fi
    systemctl daemon-reload
    revoke_operator_members
    residual_report
    if [ "$purge" = true ]; then
      purge_state
    else
      echo "Runtime state was retained at $state_root."
      echo "Privileged Runtime state was retained at $privileged_state_root."
    fi
    ;;
  residuals)
    [ "$#" -eq 0 ] || fail "residuals does not accept parameters"
    residual_report
    ;;
  *)
    fail "usage: runtime-lifecycle.sh post-install|pre-remove|post-remove|residuals"
    ;;
esac
