#!/bin/sh
set -eu

runtime_user=_submux-runtime
runtime_group=_submux-runtime
operator_group=submux-runtime-operators
state_root='/Library/Application Support/SubmuxRuntime'
privileged_state_root='/Library/Application Support/SubmuxRuntimePrivileged'
program_root='/Library/PrivilegedHelperTools'
runtime_label=com.byworld.submux.runtime
network_label=com.byworld.submux.runtime-net
runtime_plist="/Library/LaunchDaemons/$runtime_label.plist"
network_plist="/Library/LaunchDaemons/$network_label.plist"
receipt="$state_root/install-receipt.json"
identity_receipt="$privileged_state_root/install-identity"

fail() {
  echo "submux-runtime macOS installer: $*" >&2
  exit 1
}

require_root() {
  [ "$(id -u)" -eq 0 ] || fail "machine installation requires root"
}

require_real_directory() {
  path=$1
  [ ! -L "$path" ] || fail "refusing symbolic-link directory $path"
  if [ -e "$path" ]; then
    [ -d "$path" ] || fail "required directory is not a directory: $path"
  fi
}

next_system_id() {
  used=$(
    {
      dscl . -list /Users UniqueID 2>/dev/null || true
      dscl . -list /Groups PrimaryGroupID 2>/dev/null || true
    } | awk '{ print $NF }'
  )
  candidate=499
  while [ "$candidate" -ge 400 ]; do
    if ! printf '%s\n' "$used" | grep -qx "$candidate"; then
      printf '%s' "$candidate"
      return 0
    fi
    candidate=$((candidate - 1))
  done
  fail "no free macOS system identity is available in the reserved 400-499 range"
}

ensure_group() {
  name=$1
  field=$2
  if dscl . -read "/Groups/$name" PrimaryGroupID >/dev/null 2>&1; then
    gid=$(dscl . -read "/Groups/$name" PrimaryGroupID | awk '{ print $2 }')
    case "$gid" in '' | *[!0-9]*) fail "existing group $name has an invalid GID" ;; esac
    expected=$(identity_field "$field") ||
      fail "refusing to adopt existing group $name without the protected installation identity"
    [ "$gid" = "$expected" ] ||
      fail "existing group $name does not match the protected installation identity"
    printf '%s' "$gid"
    return 0
  fi
  gid=$(next_system_id)
  dscl . -create "/Groups/$name"
  dscl . -create "/Groups/$name" PrimaryGroupID "$gid"
  dscl . -create "/Groups/$name" RealName "$name"
  printf '%s' "$gid"
}

ensure_runtime_user() {
  primary_gid=$1
  if dscl . -read "/Users/$runtime_user" UniqueID >/dev/null 2>&1; then
    current_gid=$(dscl . -read "/Users/$runtime_user" PrimaryGroupID | awk '{ print $2 }')
    [ "$current_gid" = "$primary_gid" ] ||
      fail "existing Runtime account has an unexpected primary group"
    uid=$(dscl . -read "/Users/$runtime_user" UniqueID | awk '{ print $2 }')
    case "$uid" in '' | *[!0-9]*) fail "existing Runtime account has an invalid UID" ;; esac
    expected=$(identity_field runtime_uid) ||
      fail "refusing to adopt an existing Runtime account without the protected installation identity"
    [ "$uid" = "$expected" ] ||
      fail "existing Runtime account does not match the protected installation identity"
    printf '%s' "$uid"
    return 0
  fi
  uid=$(next_system_id)
  dscl . -create "/Users/$runtime_user"
  dscl . -create "/Users/$runtime_user" UniqueID "$uid"
  dscl . -create "/Users/$runtime_user" PrimaryGroupID "$primary_gid"
  dscl . -create "/Users/$runtime_user" UserShell /usr/bin/false
  dscl . -create "/Users/$runtime_user" NFSHomeDirectory "$state_root"
  dscl . -create "/Users/$runtime_user" RealName "Submux Runtime service"
  dscl . -create "/Users/$runtime_user" IsHidden 1
  printf '%s' "$uid"
}

identity_field() {
  field=$1
  [ -f "$identity_receipt" ] && [ ! -L "$identity_receipt" ] || return 1
  [ "$(stat -f '%u:%g:%Lp' "$identity_receipt")" = 0:0:600 ] ||
    fail "protected installation identity has unsafe ownership or permissions"
  value=$(awk -F= -v name="$field" '$1 == name { print $2 }' "$identity_receipt")
  case "$value" in '' | *[!0-9]*) return 1 ;; esac
  printf '%s' "$value"
}

write_identity_receipt() {
  runtime_uid=$1
  runtime_gid=$2
  operator_gid=$3
  temporary=$(mktemp "$privileged_state_root/.install-identity.XXXXXX")
  trap 'rm -f "$temporary"' EXIT HUP INT TERM
  printf 'runtime_uid=%s\nruntime_gid=%s\noperator_gid=%s\n' \
    "$runtime_uid" "$runtime_gid" "$operator_gid" >"$temporary"
  chown root:wheel "$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$identity_receipt"
  trap - EXIT HUP INT TERM
}

prepare_directories() {
  runtime_uid=$1
  runtime_gid=$2
  operator_gid=$3
  for path in "$state_root" "$privileged_state_root"; do
    require_real_directory "$path"
  done
  install -d -m 0700 -o "$runtime_uid" -g "$runtime_gid" "$state_root"
  install -d -m 0700 -o root -g wheel "$privileged_state_root"
  run_parent=$(CDPATH='' cd /var/run && pwd -P)
  management_root="$run_parent/submux-runtime"
  privileged_root="$run_parent/submux-runtime-privileged"
  require_real_directory "$management_root"
  require_real_directory "$privileged_root"
  install -d -m 0750 -o "$runtime_uid" -g "$operator_gid" "$management_root"
  install -d -m 0750 -o root -g "$runtime_gid" "$privileged_root"
}

secure_payload() {
  chown root:wheel \
    "$program_root/submux-runtime" \
    "$program_root/submux-runtime-net" \
    "$program_root/submux-runtime-lifecycle" \
    "$runtime_plist" \
    "$network_plist"
  chmod 0755 \
    "$program_root/submux-runtime" \
    "$program_root/submux-runtime-net" \
    "$program_root/submux-runtime-lifecycle"
  chmod 0644 "$runtime_plist" "$network_plist"
  if [ -d "$program_root/SubmuxRuntimeOffline" ]; then
    chown -R root:wheel "$program_root/SubmuxRuntimeOffline"
    find "$program_root/SubmuxRuntimeOffline" -type d -exec chmod 0755 {} +
    find "$program_root/SubmuxRuntimeOffline" -type f -exec chmod 0644 {} +
    chmod 0755 "$program_root/SubmuxRuntimeOffline/mihomo"
  fi
}

stop_label() {
  label=$1
  launchctl bootout "system/$label" >/dev/null 2>&1 || true
  attempts=0
  while launchctl print "system/$label" >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    [ "$attempts" -lt 30 ] || fail "LaunchDaemon did not stop: $label"
    sleep 1
  done
}

stop_fail_open() {
  stop_label "$runtime_label"
  stop_label "$network_label"
}

start_services() {
  launchctl enable "system/$network_label"
  launchctl enable "system/$runtime_label"
  if ! launchctl bootstrap system "$network_plist" ||
     ! launchctl kickstart -k "system/$network_label" ||
     ! launchctl bootstrap system "$runtime_plist" ||
     ! launchctl kickstart -k "system/$runtime_label"; then
    stop_fail_open
    fail "Runtime LaunchDaemon activation failed"
  fi
  attempts=0
  while [ "$attempts" -lt 30 ]; do
    if launchctl print "system/$network_label" >/dev/null 2>&1 &&
       launchctl print "system/$runtime_label" >/dev/null 2>&1 &&
       [ -S /var/run/submux-runtime/runtime.sock ]; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  stop_fail_open
  fail "Runtime LaunchDaemons did not become ready"
}

write_receipt() {
  version=$1
  kind=$2
  installed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  temporary=$(mktemp "$state_root/.install-receipt.XXXXXX")
  trap 'rm -f "$temporary"' EXIT HUP INT TERM
  printf '%s\n' \
    '{' \
    '  "format_version": 1,' \
    '  "product_id": "com.byworld.submux.runtime",' \
    "  \"version\": \"$version\"," \
    '  "install_root": "/Library/PrivilegedHelperTools",' \
    '  "platform": "darwin",' \
    '  "arch": "universal",' \
    "  \"package_kind\": \"$kind\"," \
    "  \"installed_at\": \"$installed_at\"" \
    '}' >"$temporary"
  chown "$runtime_user:$runtime_group" "$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$receipt"
  trap - EXIT HUP INT TERM
}

residual_report() {
  residuals=
  for label in "$runtime_label" "$network_label"; do
    if launchctl print "system/$label" >/dev/null 2>&1; then
      residuals="${residuals}
LaunchDaemon: $label"
    fi
  done
  routes=$(netstat -rn 2>/dev/null | grep -i submux || true)
  dns=$(scutil --dns 2>/dev/null | grep -i submux || true)
  firewall=$(pfctl -sr 2>/dev/null | grep -i submux || true)
  tun=$(ifconfig -l 2>/dev/null | tr ' ' '\n' | grep -E '^(submux|tun-submux)' || true)
  sockets=
  for path in \
    /var/run/submux-runtime/runtime.sock \
    /var/run/submux-runtime-privileged/runtime-net.sock; do
    [ ! -e "$path" ] && [ ! -S "$path" ] || sockets="$sockets $path"
  done
  [ -z "$routes" ] || residuals="${residuals}
routes:
$routes"
  [ -z "$dns" ] || residuals="${residuals}
DNS:
$dns"
  [ -z "$firewall" ] || residuals="${residuals}
packet filter:
$firewall"
  [ -z "$tun" ] || residuals="${residuals}
interfaces:
$tun"
  [ -z "$sockets" ] || residuals="${residuals}
sockets:$sockets"
  if [ -n "$residuals" ]; then
    printf 'Submux Runtime uninstall residuals detected:%s\n' "$residuals" >&2
    return 1
  fi
  echo "No Runtime-owned service, route, DNS, packet-filter rule, TUN, or Socket residual was detected."
}

remove_authorizations() {
  members=$(dscl . -read "/Groups/$operator_group" GroupMembership 2>/dev/null |
    sed 's/^GroupMembership:[[:space:]]*//' || true)
  for member in $members; do
    uid=$(id -u "$member" 2>/dev/null || true)
    home=$(dscl . -read "/Users/$member" NFSHomeDirectory 2>/dev/null | sed 's/^[^:]*:[[:space:]]*//' || true)
    case "$uid" in '' | *[!0-9]*) continue ;; esac
    case "$home" in /*) ;; *) continue ;; esac
    launchctl bootout "gui/$uid/com.byworld.submux.gui" >/dev/null 2>&1 || true
    agent="$home/Library/LaunchAgents/com.byworld.submux.gui.plist"
    [ -L "$agent" ] && fail "refusing symbolic-link Runtime GUI LaunchAgent"
    rm -f "$agent"
  done
}

remove_owned_identities() {
  runtime_uid=$(identity_field runtime_uid) ||
    fail "cannot remove Runtime identities without a protected installation identity"
  runtime_gid=$(identity_field runtime_gid) ||
    fail "protected Runtime group identity is incomplete"
  operator_gid=$(identity_field operator_gid) ||
    fail "protected Runtime operator-group identity is incomplete"
  actual_uid=$(dscl . -read "/Users/$runtime_user" UniqueID 2>/dev/null | awk '{ print $2 }' || true)
  actual_runtime_gid=$(dscl . -read "/Groups/$runtime_group" PrimaryGroupID 2>/dev/null |
    awk '{ print $2 }' || true)
  actual_operator_gid=$(dscl . -read "/Groups/$operator_group" PrimaryGroupID 2>/dev/null |
    awk '{ print $2 }' || true)
  [ -z "$actual_uid" ] || [ "$actual_uid" = "$runtime_uid" ] ||
    fail "Runtime account no longer matches the protected installation identity"
  [ -z "$actual_runtime_gid" ] || [ "$actual_runtime_gid" = "$runtime_gid" ] ||
    fail "Runtime group no longer matches the protected installation identity"
  [ -z "$actual_operator_gid" ] || [ "$actual_operator_gid" = "$operator_gid" ] ||
    fail "operator group no longer matches the protected installation identity"
  [ -z "$actual_uid" ] || dscl . -delete "/Users/$runtime_user"
  [ -z "$actual_runtime_gid" ] || dscl . -delete "/Groups/$runtime_group"
  [ -z "$actual_operator_gid" ] || dscl . -delete "/Groups/$operator_group"
}

purge_state() {
  for path in "$state_root" "$privileged_state_root"; do
    require_real_directory "$path"
  done
  rm -rf -- "$state_root" "$privileged_state_root"
}

command=${1:-}
shift || true
require_root
case "$command" in
  post-install)
    [ "$#" -eq 2 ] || fail "post-install requires VERSION KIND"
    version=$1
    kind=$2
    printf '%s\n' "$version" |
      grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' ||
      fail "package version must be vX.Y.Z"
    case "$kind" in online | offline) ;; *) fail "package kind must be online or offline" ;; esac
    runtime_gid=$(ensure_group "$runtime_group" runtime_gid)
    operator_gid=$(ensure_group "$operator_group" operator_gid)
    runtime_uid=$(ensure_runtime_user "$runtime_gid")
    prepare_directories "$runtime_uid" "$runtime_gid" "$operator_gid"
    write_identity_receipt "$runtime_uid" "$runtime_gid" "$operator_gid"
    secure_payload
    start_services
    write_receipt "$version" "$kind"
    ;;
  pre-remove)
    [ "$#" -eq 0 ] || fail "pre-remove does not accept parameters"
    stop_fail_open
    ;;
  post-remove)
    [ "$#" -le 1 ] || fail "post-remove accepts only --purge"
    purge=false
    if [ "$#" -eq 1 ]; then
      [ "$1" = --purge ] || fail "post-remove accepts only --purge"
      purge=true
    fi
    remove_authorizations
    residual_report
    if [ "$purge" = true ]; then
      remove_owned_identities
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
    fail "usage: runtime-lifecycle post-install|pre-remove|post-remove|residuals"
    ;;
esac
