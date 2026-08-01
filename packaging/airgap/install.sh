#!/usr/bin/env bash
set -euo pipefail
umask 077

fail() {
  echo "submux airgap installer: $*" >&2
  exit 1
}

say() {
  echo "submux airgap installer: $*"
}

usage() {
  cat <<'EOF'
Usage:
  ./install.sh verify
  sudo ./install.sh control [options]
  sudo ./install.sh runtime [options]
  sudo ./install.sh all [options]

Options:
  --authorize-desktop USER  authorize one local Runtime operator
  --repair                  repair a same-version or unreadable installation
  --allow-downgrade         allow an older component version
  --database-compatible     confirm state compatibility for a downgrade
  --help                    show this help

The installer never deletes submux or Runtime state. Control plane and Runtime
remain independent installations; "all" runs both installers in order.
EOF
}

script_source=${BASH_SOURCE[0]}
[[ -f $script_source && ! -L $script_source ]] ||
  fail "install.sh must be a regular non-linked file"
script_dir=$(cd -- "$(dirname -- "$script_source")" && pwd -P)
metadata="$script_dir/AIRGAP-METADATA"
manifest="$script_dir/ARTIFACT-MANIFEST.sha256"

require_regular() {
  local name=$1
  local value=$2
  [[ -f $value && ! -L $value ]] || fail "$name is missing or is not a regular file"
}

metadata_value() {
  local key=$1
  local count value
  count=$(grep -c "^${key}=" "$metadata" || true)
  [[ $count == 1 ]] || fail "AIRGAP-METADATA must contain exactly one $key"
  value=$(sed -n "s/^${key}=//p" "$metadata")
  [[ -n $value && $value != *$'\n'* ]] || fail "AIRGAP-METADATA has an invalid $key"
  printf '%s' "$value"
}

package_metadata_value() {
  local key=$1
  local package_metadata="$script_dir/runtime/PACKAGE-METADATA"
  local count value
  count=$(grep -c "^${key}=" "$package_metadata" || true)
  [[ $count == 1 ]] || fail "Runtime PACKAGE-METADATA must contain exactly one $key"
  value=$(sed -n "s/^${key}=//p" "$package_metadata")
  [[ -n $value && $value != *$'\n'* ]] || fail "Runtime PACKAGE-METADATA has an invalid $key"
  printf '%s' "$value"
}

normalize_arch() {
  case "$1" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) fail "unsupported host architecture: $1" ;;
  esac
}

validate_manifest_paths() {
  local digest path extra count=0
  while read -r digest path extra; do
    [[ -z ${extra:-} ]] || fail "artifact manifest contains a path with whitespace"
    [[ $digest =~ ^[0-9a-f]{64}$ ]] || fail "artifact manifest contains an invalid SHA-256"
    [[ $path == ./* && $path != */../* && $path != ../* && $path != */.. &&
       $path != *//* && $path != ./ARTIFACT-MANIFEST.sha256 ]] ||
      fail "artifact manifest contains an unsafe path: $path"
    count=$((count + 1))
  done <"$manifest"
  [[ $count -gt 0 ]] || fail "artifact manifest is empty"
}

verify_bundle() {
  command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"
  command -v find >/dev/null 2>&1 || fail "find is required"
  require_regular "AIRGAP-METADATA" "$metadata"
  require_regular "ARTIFACT-MANIFEST.sha256" "$manifest"
  validate_manifest_paths
  (
    cd "$script_dir"
    sha256sum --strict --check ARTIFACT-MANIFEST.sha256 >/dev/null
  ) || fail "airgap artifact verification failed"

  local -a expected_files actual_files
  mapfile -t expected_files < <(
    awk '{print $2}' "$manifest" | LC_ALL=C sort
  )
  mapfile -t actual_files < <(
    cd "$script_dir"
    find . -type f ! -path ./ARTIFACT-MANIFEST.sha256 -print | LC_ALL=C sort
  )
  [[ ${#expected_files[@]} -eq ${#actual_files[@]} ]] ||
    fail "airgap artifact manifest does not describe every regular file"
  local index
  for index in "${!expected_files[@]}"; do
    [[ ${expected_files[$index]} == "${actual_files[$index]}" ]] ||
      fail "airgap artifact manifest does not match the file inventory"
  done

  local unexpected non_regular relative
  while IFS= read -r -d '' unexpected; do
    relative=.${unexpected#"$script_dir"}
    case "$relative" in
      ./runtime/root/usr/bin/submux-runtime | ./runtime/root/usr/bin/submux-runtime-gui) ;;
      *) fail "airgap kit contains an unexpected symbolic link: $relative" ;;
    esac
  done < <(find "$script_dir" -type l -print0)
  non_regular=$(find "$script_dir" ! -type d ! -type f ! -type l -print -quit)
  [[ -z $non_regular ]] || fail "airgap kit contains a non-regular entry"
  [[ $(readlink "$script_dir/runtime/root/usr/bin/submux-runtime") == \
      ../lib/submux-runtime/submux-runtime ]] ||
    fail "Runtime CLI link is invalid"
  if [[ -e $script_dir/runtime/root/usr/lib/submux-runtime/submux-runtime-gui ||
        -L $script_dir/runtime/root/usr/bin/submux-runtime-gui ]]; then
    [[ -f $script_dir/runtime/root/usr/lib/submux-runtime/submux-runtime-gui &&
       $(readlink "$script_dir/runtime/root/usr/bin/submux-runtime-gui") == \
         ../lib/submux-runtime/submux-runtime-gui ]] ||
      fail "Runtime GUI payload or link is invalid"
  fi

  schema=$(metadata_value SCHEMA)
  bundle_version=$(metadata_value BUNDLE_VERSION)
  control_version=$(metadata_value CONTROL_VERSION)
  runtime_version=$(metadata_value RUNTIME_VERSION)
  mihomo_version=$(metadata_value MIHOMO_VERSION)
  platform=$(metadata_value PLATFORM)
  architecture=$(metadata_value ARCH)
  components=$(metadata_value COMPONENTS)

  [[ $schema == 1 ]] || fail "unsupported airgap metadata schema: $schema"
  [[ $platform == linux ]] || fail "airgap kit platform must be linux"
  case "$architecture" in amd64 | arm64) ;; *) fail "airgap kit architecture is invalid" ;; esac
  [[ $components == control,runtime ]] || fail "airgap kit component set is invalid"
  for value in "$bundle_version" "$control_version" "$runtime_version" "$mihomo_version"; do
    [[ $value =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
      fail "airgap kit contains an invalid version: $value"
  done
  control_asset="$script_dir/control/submux-linux-$architecture"
  require_regular "control installer" "$script_dir/control/install-submux.sh"
  require_regular "control binary" "$control_asset"
  require_regular "control checksums" "$script_dir/control/checksums.txt"
  require_regular "Runtime installer" "$script_dir/runtime/install.sh"
  [[ -x $script_dir/runtime/install.sh ]] || fail "Runtime installer is not executable"
  require_regular "Runtime package metadata" "$script_dir/runtime/PACKAGE-METADATA"
  [[ -d $script_dir/runtime/root && ! -L $script_dir/runtime/root ]] ||
    fail "Runtime package root is missing"
  require_regular "Runtime artifact manifest" \
    "$script_dir/runtime/root/usr/lib/submux-runtime/ARTIFACT-MANIFEST.sha256"
  require_regular "offline verification bundle" \
    "$script_dir/trust/offline-verification-bundle.zip"

  (
    cd "$script_dir/control"
    grep "  submux-linux-${architecture}$" checksums.txt |
      sha256sum --strict --check >/dev/null
  ) || fail "control-plane checksum verification failed"
  [[ $(package_metadata_value VERSION) == "$runtime_version" ]] ||
    fail "Runtime package version does not match $runtime_version"
  [[ $(package_metadata_value ARCH) == "$architecture" ]] ||
    fail "Runtime package architecture does not match $architecture"
  [[ $(package_metadata_value KIND) == offline ]] ||
    fail "Runtime package is not a complete offline package"
  (
    cd "$script_dir/runtime/root"
    sha256sum --strict --check usr/lib/submux-runtime/ARTIFACT-MANIFEST.sha256 >/dev/null
  ) || fail "Runtime package artifact verification failed"
}

verify_host() {
  [[ $(uname -s) == Linux ]] || fail "installation requires Linux"
  host_arch=$(normalize_arch "$(uname -m)")
  [[ $host_arch == "$architecture" ]] ||
    fail "airgap kit architecture $architecture does not match host $host_arch"
  "$control_asset" --version | grep -F " $control_version (" >/dev/null ||
    fail "control-plane binary version does not match $control_version"
}

version_is_lower() {
  local candidate=${1#v}
  local current=${2#v}
  [[ $candidate != "$current" ]] &&
    printf '%s\n%s\n' "$candidate" "$current" | sort -V -C
}

installed_control_version() {
  [[ -x /usr/local/bin/submux ]] || return 0
  /usr/local/bin/submux --version 2>/dev/null |
    sed -n 's/^[^ ]* \(v[0-9][^ ]*\) .*/\1/p'
}

installed_runtime_version() {
  local receipt=/var/lib/submux-runtime/install-receipt.json
  [[ -f $receipt ]] || return 0
  sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)"[,]*[[:space:]]*$/\1/p' \
    "$receipt"
}

current_mihomo_version() {
  runtime_cli status --json 2>/dev/null |
    sed -n 's/.*"mihomo":{[^}]*"version":"\([^"]*\)".*/\1/p'
}

runtime_cli() {
  /usr/bin/submux-runtime "$@"
}

install_control() {
  local current
  current=$(installed_control_version)
  if [[ -n $current && $current != "$control_version" ]] &&
     version_is_lower "$control_version" "$current"; then
    [[ $allow_downgrade == true && $database_compatible == true ]] ||
      fail "control-plane downgrade requires --allow-downgrade and --database-compatible"
  fi
  if [[ -e /usr/local/bin/submux && -z $current && $repair != true ]]; then
    fail "an unreadable control-plane installation requires --repair"
  fi
  local args=(
    --version "$control_version"
    --offline-dir "$script_dir/control"
    --service
  )
  [[ -x /usr/local/bin/submux ]] && args+=(--upgrade)
  bash "$script_dir/control/install-submux.sh" "${args[@]}"
  say "control plane $control_version is installed; /var/lib/submux was preserved"
}

activate_mihomo() {
  local current plan_file plan_id status
  current=$(current_mihomo_version)
  if [[ $current == "$mihomo_version" ]]; then
    say "official Mihomo core $mihomo_version is already installed"
    return 0
  fi
  plan_file=$(mktemp)
  if ! runtime_cli mihomo import \
      --json --version "$mihomo_version" \
      "$script_dir/trust/offline-verification-bundle.zip" >"$plan_file"; then
    if [[ -s $plan_file ]]; then
      sed -n '1,20p' "$plan_file" >&2
    fi
    rm -f "$plan_file"
    fail "offline Mihomo verification failed"
  fi
  plan_id=$(sed -n 's/.*"plan_id":"\([^"]*\)".*/\1/p' "$plan_file")
  rm -f "$plan_file"
  [[ $plan_id =~ ^plan_[0-9a-f]{32}$ ]] || fail "Runtime returned an invalid Mihomo plan"
  runtime_cli mihomo install \
    --json --plan "$plan_id" --trust tuf --confirm --wait
  status=$(current_mihomo_version)
  [[ $status == "$mihomo_version" ]] ||
    fail "Mihomo activation completed without the expected version"
  say "official Mihomo core $mihomo_version is installed through TUF verification"
}

install_runtime() {
  local current
  current=$(installed_runtime_version)
  if [[ $current == "$runtime_version" && $repair != true ]]; then
    systemctl is-active --quiet submux-runtime.service ||
      fail "same-version Runtime is unhealthy; rerun with --repair"
    systemctl is-active --quiet submux-runtime-net.service ||
      fail "same-version privileged network process is unhealthy; rerun with --repair"
    runtime_cli status --json >/dev/null ||
      fail "same-version Runtime IPC is unhealthy; rerun with --repair"
    if [[ -n $authorize_user ]]; then
      /usr/sbin/submux-runtime-authorize-user --confirm "$authorize_user"
    fi
    say "Submux Runtime $runtime_version is already installed and healthy"
  else
    local args=()
    [[ $repair == true ]] && args+=(--repair)
    [[ $allow_downgrade == true ]] && args+=(--allow-downgrade)
    [[ $database_compatible == true ]] && args+=(--database-compatible)
    [[ -z $authorize_user ]] || args+=(--authorize-desktop "$authorize_user")
    "$script_dir/runtime/install.sh" "${args[@]}"
    runtime_cli status --json >/dev/null ||
      fail "Runtime installation completed without healthy local IPC"
  fi
  activate_mihomo
  say "Submux Runtime $runtime_version is installed with official Mihomo core $mihomo_version"
}

if [[ ${BASH_SOURCE[0]} != "$0" ]]; then
  return 0
fi

command=${1:-}
case "$command" in
  --help | -h | help)
    usage
    exit 0
    ;;
  verify)
    shift
    [[ $# -eq 0 ]] || fail "verify does not accept options"
    verify_bundle
    say "verified bundle $bundle_version for linux/$architecture"
    exit 0
    ;;
  control | runtime | all) shift ;;
  "") usage; exit 2 ;;
  *) fail "unknown command: $command" ;;
esac

repair=false
allow_downgrade=false
database_compatible=false
authorize_user=
while (($# > 0)); do
  case "$1" in
    --repair) repair=true; shift ;;
    --allow-downgrade) allow_downgrade=true; shift ;;
    --database-compatible) database_compatible=true; shift ;;
    --authorize-desktop)
      (($# >= 2)) || fail "--authorize-desktop requires a user"
      authorize_user=$2
      shift 2
      ;;
    --help | -h) usage; exit 0 ;;
    *) fail "unsupported option: $1" ;;
  esac
done
if [[ $allow_downgrade != "$database_compatible" ]]; then
  fail "--allow-downgrade and --database-compatible must be used together"
fi
if [[ $command == control && -n $authorize_user ]]; then
  fail "--authorize-desktop applies only to Runtime"
fi

verify_bundle
verify_host
[[ $(id -u) -eq 0 ]] || fail "installation requires root"
[[ -d /run/systemd/system ]] || fail "installation requires a running systemd system"
getconf GNU_LIBC_VERSION >/dev/null 2>&1 || fail "installation requires glibc"

case "$command" in
  control) install_control ;;
  runtime) install_runtime ;;
  all)
    install_control
    install_runtime
    ;;
esac
say "$command installation completed"
