#!/usr/bin/env bash
# Install, upgrade, roll back, or uninstall the unprivileged submux Agent.
set -euo pipefail

readonly REPO="Questrove/submux"
readonly BINARY="submux-agent"
readonly UNIT="submux-agent.service"
readonly UNIT_MARKER="# Managed by submux-agent installer; do not edit."

INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"
REQUESTED_VERSION="${VERSION:-}"
CHANNEL="stable"
WITH_SERVICE=0
MODE="install"
ENROLL_SERVER=""
ENROLL_CODE=""
LOCAL_BINARY=""
LOCAL_SHA256=""

usage() {
  cat <<'EOF'
Usage: install-agent.sh [options]

  --version VERSION   install an exact release
  --channel CHANNEL   stable (default) or alpha
  --service           install a systemd user unit (no root; enrollment starts it)
  --server URL        enroll with this control-plane URL after installation
  --code CODE         one-time pairing code used with --server
  --local-binary PATH install a prebuilt development binary instead of downloading
  --sha256 DIGEST     required SHA-256 for --local-binary
  --upgrade           require an existing Agent and upgrade it
  --rollback          restore the previous verified Agent binary
  --uninstall         remove the binary and user unit; preserve Agent user data
  --help              show this help

Environment: INSTALL_DIR, VERSION, XDG_CONFIG_HOME
EOF
}

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || die "--version requires a value"; REQUESTED_VERSION="$2"; shift 2 ;;
    --channel) [ "$#" -ge 2 ] || die "--channel requires stable or alpha"; CHANNEL="$2"; shift 2 ;;
    --service) WITH_SERVICE=1; shift ;;
    --server) [ "$#" -ge 2 ] || die "--server requires a URL"; ENROLL_SERVER="$2"; shift 2 ;;
    --code) [ "$#" -ge 2 ] || die "--code requires a pairing code"; ENROLL_CODE="$2"; shift 2 ;;
    --local-binary) [ "$#" -ge 2 ] || die "--local-binary requires a path"; LOCAL_BINARY="$2"; shift 2 ;;
    --sha256) [ "$#" -ge 2 ] || die "--sha256 requires a digest"; LOCAL_SHA256="$2"; shift 2 ;;
    --upgrade) [ "$MODE" = install ] || die "choose only one operation"; MODE=upgrade; shift ;;
    --rollback) [ "$MODE" = install ] || die "choose only one operation"; MODE=rollback; shift ;;
    --uninstall) [ "$MODE" = install ] || die "choose only one operation"; MODE=uninstall; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

case "$CHANNEL" in stable|alpha) ;; *) die "unsupported channel: $CHANNEL" ;; esac
[ "$(uname -s)" = Linux ] || die "submux-agent is currently packaged for Linux and Windows"
[ "$(id -u)" -ne 0 ] || die "do not run the Agent installer as root; use bootstrap-agent.sh for a dedicated user"
[ -n "${HOME:-}" ] && [ -d "$HOME" ] || die "a real user HOME directory is required"

if [ -n "$ENROLL_SERVER" ] || [ -n "$ENROLL_CODE" ]; then
  [ -n "$ENROLL_SERVER" ] && [ -n "$ENROLL_CODE" ] || die "--server and --code must be supplied together"
  [ "$MODE" = install ] || die "enrollment is supported only for a fresh install"
  [ "$WITH_SERVICE" -eq 1 ] || die "one-command enrollment requires --service"
fi
if [ -n "$LOCAL_BINARY" ] || [ -n "$LOCAL_SHA256" ]; then
  [ "$MODE" = install ] || die "--local-binary is supported only for a fresh install"
  [ -n "$LOCAL_BINARY" ] && [ -n "$LOCAL_SHA256" ] || die "--local-binary and --sha256 must be supplied together"
  [ -n "$REQUESTED_VERSION" ] || die "--local-binary requires --version"
  printf '%s' "$LOCAL_SHA256" | grep -Eq '^[0-9a-fA-F]{64}$' || die "--sha256 must contain exactly 64 hexadecimal characters"
  [ -f "$LOCAL_BINARY" ] && [ ! -L "$LOCAL_BINARY" ] || die "--local-binary must be a regular file and not a symbolic link"
fi

readonly TARGET="${INSTALL_DIR}/${BINARY}"
readonly PREVIOUS="${INSTALL_DIR}/.${BINARY}.previous"
readonly USER_CONFIG_HOME="${XDG_CONFIG_HOME:-${HOME}/.config}"
readonly UNIT_DIR="${USER_CONFIG_HOME}/systemd/user"
readonly UNIT_PATH="${UNIT_DIR}/${UNIT}"

service_exists() { [ -f "$UNIT_PATH" ] && command -v systemctl >/dev/null 2>&1; }
service_active() { service_exists && systemctl --user is-active --quiet "$UNIT"; }

assert_managed_unit_if_present() {
  if [ -e "$UNIT_PATH" ] || [ -L "$UNIT_PATH" ]; then
    [ -f "$UNIT_PATH" ] && [ ! -L "$UNIT_PATH" ] || die "refusing to manage a non-regular systemd user unit"
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
  systemctl --user daemon-reload
  systemctl --user restart "$UNIT"
  systemctl --user is-active --quiet "$UNIT" || return 1
  attempts=0
  while [ "$attempts" -lt 20 ]; do
    if "$TARGET" status >/dev/null 2>&1; then return 0; fi
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
  mv -f "$TARGET" "$failed"
  mv -f "$PREVIOUS" "$TARGET"
  if [ "$was_active" -eq 1 ] && ! restart_and_verify; then
    mv -f "$TARGET" "$PREVIOUS"
    mv -f "$failed" "$TARGET"
    die "the previous Agent failed verification; the current binary was restored"
  fi
  rm -f "$failed"
  say "restored $("$TARGET" --version)"
}

uninstall() {
  assert_managed_unit_if_present
  if service_exists; then
    systemctl --user disable --now "$UNIT" >/dev/null 2>&1 || true
    rm -f "$UNIT_PATH"
    systemctl --user daemon-reload || true
  fi
  rm -f "$TARGET" "$PREVIOUS"
  say "submux-agent was uninstalled; user-owned state and managed Mihomo files were preserved for explicit recovery"
}

assert_managed_binary_paths
case "$MODE" in
  rollback) rollback_binary; exit 0 ;;
  uninstall) uninstall; exit 0 ;;
esac

[ "$MODE" != upgrade ] || [ -x "$TARGET" ] || die "--upgrade requires an existing $TARGET"
[ -n "$LOCAL_BINARY" ] || command -v curl >/dev/null 2>&1 || die "curl is required"
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
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
if [ -n "$LOCAL_BINARY" ]; then
  say "installing local submux-agent ${version} (linux/${arch})"
  cp "$LOCAL_BINARY" "${tmpdir}/${asset}"
  expected="$(printf '%s' "$LOCAL_SHA256" | tr '[:upper:]' '[:lower:]')"
else
  base_url="https://github.com/${REPO}/releases/download/${version}"
  say "downloading submux-agent ${version} (linux/${arch})"
  curl -fsSL "${base_url}/${asset}" -o "${tmpdir}/${asset}"
  curl -fsSL "${base_url}/checksums.txt" -o "${tmpdir}/checksums.txt"
  expected="$(awk -v file="$asset" '$2==file || $2=="*"file {print $1}' "${tmpdir}/checksums.txt")"
  [ -n "$expected" ] || die "checksums.txt has no entry for $asset"
  printf '%s' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || die "checksums.txt has an invalid or duplicate digest for $asset"
  expected="$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')"
fi
actual="$(sha256sum "${tmpdir}/${asset}" | awk '{print $1}')"
[ "$actual" = "$expected" ] || die "checksum verification failed for $asset"
chmod 0755 "${tmpdir}/${asset}"
reported="$("${tmpdir}/${asset}" --version)"
printf '%s' "$reported" | grep -F " ${version} (" >/dev/null || die "binary version does not match $version: $reported"

mkdir -p "$INSTALL_DIR"
chmod 0755 "$INSTALL_DIR"
assert_managed_unit_if_present
was_active=0
service_active && was_active=1
unit_had_previous=0
if [ -f "$UNIT_PATH" ]; then
  unit_had_previous=1
  cp -p "$UNIT_PATH" "${tmpdir}/${UNIT}.previous"
fi
staged="${INSTALL_DIR}/.${BINARY}.staging.$$"
install -m 0755 "${tmpdir}/${asset}" "$staged"
had_previous=0
if [ -e "$TARGET" ]; then
  had_previous=1
  rm -f "$PREVIOUS"
  mv "$TARGET" "$PREVIOUS" || die "could not preserve the current Agent binary"
fi
if ! mv "$staged" "$TARGET"; then
  if [ "$had_previous" -eq 1 ]; then mv "$PREVIOUS" "$TARGET" || true; fi
  die "could not atomically activate the verified Agent binary"
fi

install_service() {
  command -v systemctl >/dev/null 2>&1 || die "systemd is required for --service"
  mkdir -p "$UNIT_DIR"
  [ ! -L "$UNIT_DIR" ] || die "systemd user unit directory must not be a symbolic link"
  unit_tmp="${tmpdir}/${UNIT}"
  printf '%s\n' \
    "$UNIT_MARKER" \
    '[Unit]' \
    'Description=submux user runtime Agent' \
    'After=network-online.target' \
    'Wants=network-online.target' \
    '' \
    '[Service]' \
    'Type=simple' \
    "ExecStart=${TARGET} serve" \
    'UMask=0077' \
    'NoNewPrivileges=true' \
    'PrivateTmp=true' \
    'ProtectKernelTunables=true' \
    'RestrictSUIDSGID=true' \
    'RestrictRealtime=true' \
    'LockPersonality=true' \
    'RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6' \
    'Restart=on-failure' \
    'RestartSec=5s' \
    '' \
    '[Install]' \
    'WantedBy=default.target' >"$unit_tmp"
  install -m 0644 "$unit_tmp" "$UNIT_PATH"
  systemctl --user daemon-reload
}

restore_installation() {
  if service_exists; then systemctl --user stop "$UNIT" >/dev/null 2>&1 || true; fi
  rm -f "$TARGET"
  if [ "$had_previous" -eq 1 ] && [ -f "$PREVIOUS" ]; then mv "$PREVIOUS" "$TARGET"; fi
  if [ "$WITH_SERVICE" -eq 1 ]; then
    if [ "$unit_had_previous" -eq 1 ]; then
      cp -p "${tmpdir}/${UNIT}.previous" "$UNIT_PATH"
    else
      rm -f "$UNIT_PATH"
    fi
    systemctl --user daemon-reload || true
  fi
  if [ "$was_active" -eq 1 ] && [ "$had_previous" -eq 1 ]; then restart_and_verify || true; fi
}

if [ "$WITH_SERVICE" -eq 1 ] && ! install_service; then
  restore_installation
  die "Agent user service installation failed; the previous installation was restored"
fi
if [ "$was_active" -eq 1 ] && ! restart_and_verify; then
  say "new Agent failed verification; restoring the previous binary" >&2
  restore_installation
  die "new Agent failed verification; the previous installation was restored"
fi

say "installed $("$TARGET" --version) at $TARGET"
if [ "$WITH_SERVICE" -eq 1 ] && [ "$was_active" -eq 0 ]; then
  say "systemd user unit installed but not started; enroll this user to start it"
  say "for unattended servers, your administrator may need to enable lingering for this user"
fi
if [ -n "$ENROLL_SERVER" ]; then
  "$TARGET" enroll --server "$ENROLL_SERVER" --code "$ENROLL_CODE"
  attempts=0
  until "$TARGET" status >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    [ "$attempts" -lt 20 ] || die "Agent enrolled but its local Socket did not become ready"
    sleep 1
  done
  say "Agent enrolled and running as $(id -un)"
fi
