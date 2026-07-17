#!/usr/bin/env bash
# Install, upgrade, roll back, or uninstall the submux control plane.
set -euo pipefail

readonly REPO="Questrove/submux"
readonly BINARY="submux"
readonly UNIT="submux.service"
readonly UNIT_MARKER="# Managed by submux installer; do not edit."

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/healthz}"
REQUESTED_VERSION="${VERSION:-}"
CHANNEL="stable"
WITH_SERVICE=0
MODE="install"

usage() {
  cat <<'EOF'
Usage: install.sh [options]

  --version VERSION   install an exact release (for example v0.4.0)
  --channel CHANNEL   stable (default) or alpha
  --service           install or update the Linux systemd service
  --upgrade           require an existing installation and upgrade it
  --rollback          restore the previous verified binary
  --uninstall         remove the binary and, when present, its systemd unit
  --help              show this help

Environment: INSTALL_DIR, VERSION, HEALTH_URL
EOF
}

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || die "--version requires a value"
      REQUESTED_VERSION="$2"; shift 2 ;;
    --channel)
      [ "$#" -ge 2 ] || die "--channel requires stable or alpha"
      CHANNEL="$2"; shift 2 ;;
    --service) WITH_SERVICE=1; shift ;;
    --upgrade) [ "$MODE" = install ] || die "choose only one operation"; MODE=upgrade; shift ;;
    --rollback) [ "$MODE" = install ] || die "choose only one operation"; MODE=rollback; shift ;;
    --uninstall) [ "$MODE" = install ] || die "choose only one operation"; MODE=uninstall; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

case "$CHANNEL" in stable|alpha) ;; *) die "unsupported channel: $CHANNEL" ;; esac

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
  command -v curl >/dev/null 2>&1 || die "curl is required for the service health check"
  attempts=0
  while [ "$attempts" -lt 20 ]; do
    if curl -fsS --max-time 2 "$HEALTH_URL" >/dev/null; then return 0; fi
    attempts=$((attempts + 1))
    sleep 1
  done
  return 1
}

rollback_binary() {
	assert_managed_unit_if_present
  [ -f "$PREVIOUS" ] || die "no previous submux binary is available"
  failed="${INSTALL_DIR}/.${BINARY}.failed.$$"
  run_as_root mv -f "$TARGET" "$failed"
  run_as_root mv -f "$PREVIOUS" "$TARGET"
  if service_active; then
    if ! restart_and_verify; then
      run_as_root mv -f "$TARGET" "$PREVIOUS"
      run_as_root mv -f "$failed" "$TARGET"
      die "the previous binary failed its health check; the current binary was restored"
    fi
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
  say "submux was uninstalled; /var/lib/submux was preserved"
}

assert_managed_binary_paths
case "$MODE" in
  rollback) rollback_binary; exit 0 ;;
  uninstall) uninstall; exit 0 ;;
esac

[ "$MODE" != upgrade ] || [ -x "$TARGET" ] || die "--upgrade requires an existing $TARGET"
command -v curl >/dev/null 2>&1 || die "curl is required"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "unsupported architecture: $arch" ;; esac
case "$os" in linux|darwin) ;; *) die "unsupported OS: $os" ;; esac
[ "$WITH_SERVICE" -eq 0 ] || [ "$os" = linux ] || die "--service is supported only on Linux"
asset="submux-${os}-${arch}"

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
items=json.load(sys.stdin)
for item in items:
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
say "downloading submux ${version} (${os}/${arch})"
curl -fsSL "${base_url}/${asset}" -o "${tmpdir}/${asset}"
curl -fsSL "${base_url}/checksums.txt" -o "${tmpdir}/checksums.txt"

expected="$(awk -v file="$asset" '$2==file || $2=="*"file {print $1}' "${tmpdir}/checksums.txt")"
[ -n "$expected" ] || die "checksums.txt has no entry for $asset"
printf '%s' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || die "checksums.txt has an invalid or duplicate digest for $asset"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmpdir}/${asset}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "${tmpdir}/${asset}" | awk '{print $1}')"
else
  die "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] || die "checksum verification failed for $asset"
chmod 0755 "${tmpdir}/${asset}"
reported="$("${tmpdir}/${asset}" --version)"
printf '%s' "$reported" | grep -F " ${version} (" >/dev/null || die "binary version does not match $version: $reported"

run_as_root mkdir -p "$INSTALL_DIR"
assert_managed_unit_if_present
was_active=0
service_active && was_active=1
unit_had_previous=0
if [ -L "$UNIT_PATH" ]; then die "refusing to replace a symbolic-link systemd unit"; fi
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
  run_as_root mv "$TARGET" "$PREVIOUS" || die "could not preserve the current binary"
fi
if ! run_as_root mv "$staged" "$TARGET"; then
  if [ "$had_previous" -eq 1 ]; then run_as_root mv "$PREVIOUS" "$TARGET" || true; fi
  die "could not atomically activate the verified binary"
fi

install_service() {
  command -v systemctl >/dev/null 2>&1 || die "systemd is required for --service"
  if ! id submux >/dev/null 2>&1; then
    run_as_root useradd --system --home-dir /var/lib/submux --shell /usr/sbin/nologin submux
  fi
  unit_tmp="${tmpdir}/${UNIT}"
  printf '%s\n' \
    "$UNIT_MARKER" \
    '[Unit]' \
    'Description=submux configuration control plane' \
    'After=network-online.target' \
    'Wants=network-online.target' \
    '' \
    '[Service]' \
    'Type=simple' \
    "ExecStart=${TARGET}" \
    'Environment=SUBMUX_DB=/var/lib/submux/submux.db' \
    'User=submux' \
    'Group=submux' \
    'StateDirectory=submux' \
    'WorkingDirectory=/var/lib/submux' \
    'UMask=0077' \
    'NoNewPrivileges=true' \
    'PrivateTmp=true' \
    'ProtectSystem=strict' \
    'ProtectHome=true' \
    'ReadWritePaths=/var/lib/submux' \
    'RestrictSUIDSGID=true' \
    'LockPersonality=true' \
    'Restart=on-failure' \
    'RestartSec=3s' \
    '' \
    '[Install]' \
    'WantedBy=multi-user.target' >"$unit_tmp"
  run_as_root install -m 0644 "$unit_tmp" "$UNIT_PATH"
  run_as_root systemctl daemon-reload
  run_as_root systemctl enable "$UNIT" >/dev/null
}

restore_installation() {
  if service_exists; then run_as_root systemctl stop "$UNIT" >/dev/null 2>&1 || true; fi
  run_as_root rm -f "$TARGET"
  if [ "$had_previous" -eq 1 ] && [ -f "$PREVIOUS" ]; then run_as_root mv "$PREVIOUS" "$TARGET"; fi
  if [ "$WITH_SERVICE" -eq 1 ]; then
    if [ "$unit_had_previous" -eq 1 ]; then
      run_as_root cp -p "${tmpdir}/${UNIT}.previous" "$UNIT_PATH"
    else
      run_as_root systemctl disable "$UNIT" >/dev/null 2>&1 || true
      run_as_root rm -f "$UNIT_PATH"
    fi
    run_as_root systemctl daemon-reload || true
  fi
  if [ "$was_active" -eq 1 ] && [ "$had_previous" -eq 1 ]; then restart_and_verify || true; fi
}

if [ "$WITH_SERVICE" -eq 1 ] && ! install_service; then
  restore_installation
  die "systemd service installation failed; the previous installation was restored"
fi

needs_health=0
[ "$WITH_SERVICE" -eq 1 ] && needs_health=1
[ "$was_active" -eq 1 ] && needs_health=1
if [ "$needs_health" -eq 1 ]; then
  if ! restart_and_verify; then
    say "new service failed its health check; restoring the previous binary" >&2
    restore_installation
    die "new service failed its health check; the previous installation was restored"
  fi
fi

say "installed $("$TARGET" --version) at $TARGET"
if [ "$needs_health" -eq 1 ]; then say "service is healthy at $HEALTH_URL"; fi
