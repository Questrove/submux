#!/usr/bin/env bash
# Install, upgrade, roll back, or uninstall the optional root submux Agent.
set -euo pipefail

readonly REPO="Questrove/submux"
readonly BINARY="submux-agent"
readonly UNIT="submux-agent.service"
readonly UNIT_MARKER="# Managed by submux-agent installer; do not edit."

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
REQUESTED_VERSION="${VERSION:-}"
CHANNEL="stable"
WITH_SERVICE=0
MODE="install"

usage() {
  cat <<'EOF'
Usage: install-agent.sh [options]

  --version VERSION   install an exact release
  --channel CHANNEL   stable (default) or alpha
  --service           install the Linux systemd unit (enrollment starts it)
  --upgrade           require an existing Agent and upgrade it
  --rollback          restore the previous verified Agent binary
  --uninstall         remove the binary and unit; preserve local Agent state
  --help              show this help

Environment: INSTALL_DIR, VERSION
EOF
}

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || die "--version requires a value"; REQUESTED_VERSION="$2"; shift 2 ;;
    --channel) [ "$#" -ge 2 ] || die "--channel requires stable or alpha"; CHANNEL="$2"; shift 2 ;;
    --service) WITH_SERVICE=1; shift ;;
    --upgrade) [ "$MODE" = install ] || die "choose only one operation"; MODE=upgrade; shift ;;
    --rollback) [ "$MODE" = install ] || die "choose only one operation"; MODE=rollback; shift ;;
    --uninstall) [ "$MODE" = install ] || die "choose only one operation"; MODE=uninstall; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

case "$CHANNEL" in stable|alpha) ;; *) die "unsupported channel: $CHANNEL" ;; esac
[ "$(uname -s)" = Linux ] || die "submux-agent v1 is supported only on Linux"

if [ "$(id -u)" -eq 0 ]; then
  run_as_root() { "$@"; }
elif command -v sudo >/dev/null 2>&1; then
  run_as_root() { sudo "$@"; }
else
  run_as_root() { die "root privileges are required for: $*"; }
fi

readonly TARGET="${INSTALL_DIR}/${BINARY}"
readonly PREVIOUS="${INSTALL_DIR}/.${BINARY}.previous"
readonly UNIT_PATH="/etc/systemd/system/${UNIT}"

service_exists() { [ -f "$UNIT_PATH" ] && command -v systemctl >/dev/null 2>&1; }
service_active() { service_exists && systemctl is-active --quiet "$UNIT"; }

assert_managed_unit_if_present() {
  if [ -e "$UNIT_PATH" ] || [ -L "$UNIT_PATH" ]; then
    [ -f "$UNIT_PATH" ] && [ ! -L "$UNIT_PATH" ] || die "refusing to manage a non-regular systemd unit"
    grep -Fqx "$UNIT_MARKER" "$UNIT_PATH" || die "refusing to take over an unmanaged $UNIT"
  fi
}

assert_managed_binary_paths() {
  [ ! -L "$INSTALL_DIR" ] || die "INSTALL_DIR must not be a symbolic link"
  [ ! -L "$TARGET" ] || die "refusing to use a symbolic-link target"
  [ ! -e "$TARGET" ] || [ -f "$TARGET" ] || die "refusing to use a non-regular target"
  [ ! -L "$PREVIOUS" ] || die "refusing to use a symbolic-link rollback target"
  [ ! -e "$PREVIOUS" ] || [ -f "$PREVIOUS" ] || die "refusing to use a non-regular rollback target"
}

restart_and_verify() {
  run_as_root systemctl daemon-reload
  run_as_root systemctl restart "$UNIT"
  run_as_root systemctl is-active --quiet "$UNIT" || return 1
  attempts=0
  while [ "$attempts" -lt 20 ]; do
    if run_as_root "$TARGET" status >/dev/null 2>&1; then return 0; fi
    attempts=$((attempts + 1))
    sleep 1
  done
  return 1
}

rollback_binary() {
	assert_managed_unit_if_present
  [ -f "$PREVIOUS" ] || die "no previous submux-agent binary is available"
  failed="${INSTALL_DIR}/.${BINARY}.failed.$$"
  was_active=0
  service_active && was_active=1
  run_as_root mv -f "$TARGET" "$failed"
  run_as_root mv -f "$PREVIOUS" "$TARGET"
  if [ "$was_active" -eq 1 ] && ! restart_and_verify; then
    run_as_root mv -f "$TARGET" "$PREVIOUS"
    run_as_root mv -f "$failed" "$TARGET"
    die "the previous Agent failed verification; the current binary was restored"
  fi
  run_as_root rm -f "$failed"
  say "restored $("$TARGET" --version)"
}

uninstall() {
	assert_managed_unit_if_present
  if service_exists; then
    run_as_root systemctl disable --now "$UNIT" >/dev/null 2>&1 || true
    run_as_root rm -f "$UNIT_PATH"
    run_as_root systemctl daemon-reload
  fi
  run_as_root rm -f "$TARGET" "$PREVIOUS"
  say "submux-agent was uninstalled; /var/lib/submux-agent and managed Mihomo files were preserved for explicit recovery"
}

assert_managed_binary_paths
case "$MODE" in
  rollback) rollback_binary; exit 0 ;;
  uninstall) uninstall; exit 0 ;;
esac

[ "$MODE" != upgrade ] || [ -x "$TARGET" ] || die "--upgrade requires an existing $TARGET"
command -v curl >/dev/null 2>&1 || die "curl is required"
arch="$(uname -m)"
case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "unsupported architecture: $arch" ;; esac
asset="submux-agent-linux-${arch}"

resolve_version() {
  if [ -n "$REQUESTED_VERSION" ]; then printf '%s' "$REQUESTED_VERSION"; return; fi
  if [ "$CHANNEL" = stable ]; then
    effective="$(curl -fsSIL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")"
    basename "$effective"
    return
  fi
  command -v python3 >/dev/null 2>&1 || die "python3 is required to resolve the alpha channel"
  curl -fsSL "https://api.github.com/repos/${REPO}/releases?per_page=50" | python3 -c '
import json, sys
for item in json.load(sys.stdin):
    if item.get("prerelease") and not item.get("draft"):
        print(item["tag_name"]); break
else: raise SystemExit("no alpha release found")'
}

version="$(resolve_version)"
[ -n "$version" ] || die "could not resolve a release version"
printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' || die "invalid exact release version: $version"
if [ "$CHANNEL" = stable ] && printf '%s' "$version" | grep -Eqi '(alpha|beta|rc|pre)'; then
  die "pre-release $version requires --channel alpha"
fi

tmpdir="$(mktemp -d)"
cleanup() { rm -rf "$tmpdir"; }
trap cleanup EXIT INT TERM
base_url="https://github.com/${REPO}/releases/download/${version}"
say "downloading submux-agent ${version} (linux/${arch})"
curl -fsSL "${base_url}/${asset}" -o "${tmpdir}/${asset}"
curl -fsSL "${base_url}/checksums.txt" -o "${tmpdir}/checksums.txt"
expected="$(awk -v file="$asset" '$2==file || $2=="*"file {print $1}' "${tmpdir}/checksums.txt")"
[ -n "$expected" ] || die "checksums.txt has no entry for $asset"
printf '%s' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || die "checksums.txt has an invalid or duplicate digest for $asset"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmpdir}/${asset}" | awk '{print $1}')"
else
  die "sha256sum is required"
fi
[ "$actual" = "$expected" ] || die "checksum verification failed for $asset"
chmod 0755 "${tmpdir}/${asset}"
reported="$("${tmpdir}/${asset}" --version)"
printf '%s' "$reported" | grep -F " ${version} (" >/dev/null || die "binary version does not match $version: $reported"

[ ! -L "$UNIT_PATH" ] || die "refusing to replace a symbolic-link systemd unit"
run_as_root mkdir -p "$INSTALL_DIR"
assert_managed_unit_if_present
if [ "$WITH_SERVICE" -eq 1 ]; then
  [ ! -L /var/lib/submux-agent ] || die "/var/lib/submux-agent must not be a symbolic link"
  run_as_root mkdir -p /var/lib/submux-agent
  run_as_root chmod 0700 /var/lib/submux-agent
  if ! id submux-mihomo >/dev/null 2>&1; then
    command -v useradd >/dev/null 2>&1 || die "useradd is required to create the low-privilege Mihomo service account"
    run_as_root useradd --system --home-dir /var/lib/submux-agent/mihomo-runtime --shell /usr/sbin/nologin submux-mihomo
  fi
fi
was_active=0
service_active && was_active=1
unit_had_previous=0
if [ -f "$UNIT_PATH" ]; then
  unit_had_previous=1
  run_as_root cp -p "$UNIT_PATH" "${tmpdir}/${UNIT}.previous"
fi
staged="${INSTALL_DIR}/.${BINARY}.staging.$$"
run_as_root install -m 0755 "${tmpdir}/${asset}" "$staged"
had_previous=0
if [ -e "$TARGET" ]; then
  had_previous=1
  run_as_root rm -f "$PREVIOUS"
  run_as_root mv "$TARGET" "$PREVIOUS" || die "could not preserve the current Agent binary"
fi
if ! run_as_root mv "$staged" "$TARGET"; then
  if [ "$had_previous" -eq 1 ]; then run_as_root mv "$PREVIOUS" "$TARGET" || true; fi
  die "could not atomically activate the verified Agent binary"
fi

install_service() {
  command -v systemctl >/dev/null 2>&1 || die "systemd is required for --service"
  unit_tmp="${tmpdir}/${UNIT}"
  printf '%s\n' \
    "$UNIT_MARKER" \
    '[Unit]' \
    'Description=submux host runtime Agent' \
    'After=network-online.target' \
    'Wants=network-online.target' \
    '' \
    '[Service]' \
    'Type=simple' \
    "ExecStart=${TARGET} serve" \
    'User=root' \
    'Group=root' \
    'StateDirectory=submux-agent' \
    'StateDirectoryMode=0700' \
    'RuntimeDirectory=submux-agent' \
    'RuntimeDirectoryMode=0750' \
    'UMask=0077' \
    'NoNewPrivileges=true' \
    'PrivateTmp=true' \
    'ProtectSystem=strict' \
    'ProtectHome=true' \
    'ProtectKernelTunables=true' \
    'ProtectKernelModules=true' \
    'ProtectKernelLogs=true' \
    'RestrictSUIDSGID=true' \
    'RestrictRealtime=true' \
    'LockPersonality=true' \
    'ReadWritePaths=/var/lib/submux-agent /run/submux-agent /etc/docker /etc/systemd/system' \
    'RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6' \
    'Restart=on-failure' \
    'RestartSec=5s' \
    '' \
    '[Install]' \
    'WantedBy=multi-user.target' >"$unit_tmp"
  run_as_root install -m 0644 "$unit_tmp" "$UNIT_PATH"
  run_as_root systemctl daemon-reload
}

restore_installation() {
  if service_exists; then run_as_root systemctl stop "$UNIT" >/dev/null 2>&1 || true; fi
  run_as_root rm -f "$TARGET"
  if [ "$had_previous" -eq 1 ] && [ -f "$PREVIOUS" ]; then run_as_root mv "$PREVIOUS" "$TARGET"; fi
  if [ "$WITH_SERVICE" -eq 1 ]; then
    if [ "$unit_had_previous" -eq 1 ]; then
      run_as_root cp -p "${tmpdir}/${UNIT}.previous" "$UNIT_PATH"
    else
      run_as_root rm -f "$UNIT_PATH"
    fi
    run_as_root systemctl daemon-reload || true
  fi
  if [ "$was_active" -eq 1 ] && [ "$had_previous" -eq 1 ]; then restart_and_verify || true; fi
}

if [ "$WITH_SERVICE" -eq 1 ] && ! install_service; then
  restore_installation
  die "Agent service installation failed; the previous installation was restored"
fi
if [ "$was_active" -eq 1 ] && ! restart_and_verify; then
  say "new Agent failed verification; restoring the previous binary" >&2
  restore_installation
  die "new Agent failed verification; the previous installation was restored"
fi

say "installed $("$TARGET" --version) at $TARGET"
if [ "$WITH_SERVICE" -eq 1 ] && [ "$was_active" -eq 0 ]; then
  say "service unit installed but not started; run 'submux-agent enroll --server https://…' to enroll and start it"
fi
