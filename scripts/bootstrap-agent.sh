#!/usr/bin/env bash
# Provision or upgrade submux-agent under a dedicated unprivileged Linux user.
set -euo pipefail

readonly REPO="Questrove/submux"
readonly DEFAULT_INSTALLER_URL="https://raw.githubusercontent.com/${REPO}/main/scripts/install-agent.sh"

AGENT_USER="submuxagent"
SERVER_URL=""
PAIRING_CODE=""
REQUESTED_VERSION=""
CHANNEL="stable"
INSTALLER_PATH=""
LOCAL_BINARY=""
LOCAL_SHA256=""
REQUIRE_BUNDLE=0
MODE="install"

usage() {
  cat <<'EOF'
Usage: bootstrap-agent.sh (--server URL --code CODE | --upgrade) [options]

  --server URL          submux control-plane URL
  --code CODE           short-lived one-time pairing code
  --user USER           dedicated account to create or reuse (default: submuxagent)
  --version VERSION     exact Agent release; latest stable when omitted
  --channel CHANNEL     stable (default) or alpha
  --upgrade             upgrade an existing active Agent without re-enrolling
  --installer PATH      use a local install-agent.sh instead of downloading it
  --local-binary PATH   install a prebuilt development Agent binary
  --sha256 DIGEST       required SHA-256 for --local-binary
  --require-bundle      fail unless a root-owned local development bundle exists
  --help                show this help

Install mode requires root to provision the account and lingering user service.
Upgrade mode only enters the existing account and never re-enrolls the Agent.
submux-agent and Mihomo always run as the dedicated user.
EOF
}

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --server) [ "$#" -ge 2 ] || die "--server requires a URL"; SERVER_URL="$2"; shift 2 ;;
    --code) [ "$#" -ge 2 ] || die "--code requires a pairing code"; PAIRING_CODE="$2"; shift 2 ;;
    --user) [ "$#" -ge 2 ] || die "--user requires an account name"; AGENT_USER="$2"; shift 2 ;;
    --version) [ "$#" -ge 2 ] || die "--version requires a value"; REQUESTED_VERSION="$2"; shift 2 ;;
    --channel) [ "$#" -ge 2 ] || die "--channel requires stable or alpha"; CHANNEL="$2"; shift 2 ;;
    --upgrade) [ "$MODE" = install ] || die "choose only one operation"; MODE=upgrade; shift ;;
    --installer) [ "$#" -ge 2 ] || die "--installer requires a path"; INSTALLER_PATH="$2"; shift 2 ;;
    --local-binary) [ "$#" -ge 2 ] || die "--local-binary requires a path"; LOCAL_BINARY="$2"; shift 2 ;;
    --sha256) [ "$#" -ge 2 ] || die "--sha256 requires a digest"; LOCAL_SHA256="$2"; shift 2 ;;
    --require-bundle) REQUIRE_BUNDLE=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(uname -s)" = Linux ] || die "server bootstrap is supported only on Linux"
[ "$(id -u)" -eq 0 ] || die "bootstrap-agent.sh must run as root"
case "$CHANNEL" in stable|alpha) ;; *) die "unsupported channel: $CHANNEL" ;; esac
printf '%s' "$AGENT_USER" | grep -Eq '^[a-z_][a-z0-9_-]{0,31}$' || die "invalid dedicated user name"
if [ -n "$REQUESTED_VERSION" ]; then
  printf '%s' "$REQUESTED_VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' || die "invalid exact release version: $REQUESTED_VERSION"
fi
if [ "$MODE" = upgrade ]; then
  [ -z "$SERVER_URL" ] && [ -z "$PAIRING_CODE" ] || die "--server and --code are not accepted with --upgrade"
  [ -z "$LOCAL_BINARY" ] && [ -z "$LOCAL_SHA256" ] && [ "$REQUIRE_BUNDLE" -eq 0 ] || die "local development bundles are not accepted with --upgrade"
else
  [ -n "$SERVER_URL" ] && [ -n "$PAIRING_CODE" ] || die "--server and --code are required"
fi
required_commands=(getent runuser systemctl stat)
if [ "$MODE" = install ]; then required_commands+=(loginctl useradd); fi
for command_name in "${required_commands[@]}"; do
  command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"
done

bundle_dir="${SUBMUX_AGENT_BUNDLE_DIR:-/usr/local/lib/submux-agent-bootstrap}"
if [ "$MODE" = install ] && [ -z "$LOCAL_BINARY" ] && [ -f "${bundle_dir}/submux-agent" ] && [ -f "${bundle_dir}/version" ] && [ -f "${bundle_dir}/sha256" ]; then
  for bundle_file in "${bundle_dir}/submux-agent" "${bundle_dir}/version" "${bundle_dir}/sha256"; do
    [ ! -L "$bundle_file" ] && [ "$(stat -c %U "$bundle_file")" = root ] || die "local development bundle must contain root-owned regular files"
  done
  LOCAL_BINARY="${bundle_dir}/submux-agent"
  REQUESTED_VERSION="$(tr -d '\r\n' <"${bundle_dir}/version")"
  LOCAL_SHA256="$(tr -d '\r\n' <"${bundle_dir}/sha256")"
  if [ -z "$INSTALLER_PATH" ] && [ -f "${bundle_dir}/install-agent.sh" ] && [ ! -L "${bundle_dir}/install-agent.sh" ]; then
    [ "$(stat -c %U "${bundle_dir}/install-agent.sh")" = root ] || die "bundled installer must be root-owned"
    INSTALLER_PATH="${bundle_dir}/install-agent.sh"
  fi
  say "using the root-owned local Agent development bundle"
elif [ "$REQUIRE_BUNDLE" -eq 1 ]; then
  die "the required local Agent development bundle is unavailable"
fi

if [ -n "$INSTALLER_PATH" ]; then
  [ -f "$INSTALLER_PATH" ] && [ ! -L "$INSTALLER_PATH" ] || die "--installer must be a regular file and not a symbolic link"
fi
if [ -n "$LOCAL_BINARY" ] || [ -n "$LOCAL_SHA256" ]; then
  [ -n "$LOCAL_BINARY" ] && [ -n "$LOCAL_SHA256" ] || die "--local-binary and --sha256 must be supplied together"
  [ -n "$REQUESTED_VERSION" ] || die "--local-binary requires --version"
  [ -f "$LOCAL_BINARY" ] && [ ! -L "$LOCAL_BINARY" ] || die "--local-binary must be a regular file and not a symbolic link"
fi

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT INT TERM
installer="$INSTALLER_PATH"
if [ -z "$installer" ]; then
  command -v curl >/dev/null 2>&1 || die "curl is required"
  installer="${work}/install-agent.sh"
  say "downloading the unprivileged Agent installer"
  installer_url="$DEFAULT_INSTALLER_URL"
  if [ -n "$REQUESTED_VERSION" ] && [ -z "$LOCAL_BINARY" ]; then
    installer_url="https://raw.githubusercontent.com/${REPO}/${REQUESTED_VERSION}/scripts/install-agent.sh"
  fi
  curl -fsSL "$installer_url" -o "$installer"
fi
bash -n "$installer" || die "Agent installer has invalid shell syntax"

entry="$(getent passwd "$AGENT_USER" || true)"
if [ -z "$entry" ]; then
  [ "$MODE" = install ] || die "dedicated user ${AGENT_USER} does not exist; use enrollment mode for the first installation"
  say "creating dedicated user ${AGENT_USER}"
  useradd --create-home --user-group --shell /bin/bash "$AGENT_USER"
  entry="$(getent passwd "$AGENT_USER")"
fi
IFS=: read -r account _ uid _ _ agent_home _ <<<"$entry"
[ "$account" = "$AGENT_USER" ] || die "could not resolve the dedicated user"
[ "$uid" -gt 0 ] || die "refusing to run Agent as root"
[ -n "$agent_home" ] && [ "$agent_home" != / ] && [ "${agent_home#/}" != "$agent_home" ] || die "dedicated user has an unsafe home directory"
[ -d "$agent_home" ] && [ ! -L "$agent_home" ] || die "dedicated user home must be a real directory"
[ "$(stat -c %U "$agent_home")" = "$AGENT_USER" ] || die "dedicated user does not own its home directory"

runtime_dir="/run/user/${uid}"
if [ "$MODE" = install ]; then
  say "enabling the user service manager for ${AGENT_USER}"
  loginctl enable-linger "$AGENT_USER"
  systemctl start "user@${uid}.service"
  attempts=0
  while [ ! -S "${runtime_dir}/bus" ] && [ "$attempts" -lt 20 ]; do
    attempts=$((attempts + 1))
    sleep 1
  done
  [ -S "${runtime_dir}/bus" ] || die "dedicated user service manager did not become ready"
else
  systemctl is-active --quiet "user@${uid}.service" || die "dedicated user service manager is not active"
fi

run_as_agent=(
  runuser -u "$AGENT_USER" -- env
  "HOME=${agent_home}"
  "USER=${AGENT_USER}"
  "LOGNAME=${AGENT_USER}"
  "XDG_RUNTIME_DIR=${runtime_dir}"
  "DBUS_SESSION_BUS_ADDRESS=unix:path=${runtime_dir}/bus"
)
for proxy_variable in http_proxy https_proxy all_proxy no_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY; do
  proxy_value="${!proxy_variable:-}"
  if [ -n "$proxy_value" ]; then run_as_agent+=("${proxy_variable}=${proxy_value}"); fi
done

installer_args=(--channel "$CHANNEL" --service)
if [ "$MODE" = upgrade ]; then
  "${run_as_agent[@]}" test -x "${agent_home}/.local/bin/submux-agent" || die "existing Agent binary is unavailable"
  "${run_as_agent[@]}" systemctl --user is-active --quiet submux-agent.service || die "Agent user service must be active before --upgrade"
  installer_args+=(--upgrade)
else
  installer_args+=(--server "$SERVER_URL" --code "$PAIRING_CODE")
fi
if [ -n "$REQUESTED_VERSION" ]; then installer_args+=(--version "$REQUESTED_VERSION"); fi
if [ -n "$LOCAL_BINARY" ]; then
  "${run_as_agent[@]}" test -r "$LOCAL_BINARY" || die "dedicated user cannot read --local-binary"
  installer_args+=(--local-binary "$LOCAL_BINARY" --sha256 "$LOCAL_SHA256")
fi

if [ "$MODE" = upgrade ]; then
  say "upgrading Agent as ${AGENT_USER}"
else
  say "installing and enrolling Agent as ${AGENT_USER}"
fi
(
  cd "$agent_home"
  "${run_as_agent[@]}" bash -s -- "${installer_args[@]}" <"$installer"
)

"${run_as_agent[@]}" systemctl --user is-active --quiet submux-agent.service || die "Agent user service is not active"
"${run_as_agent[@]}" "${agent_home}/.local/bin/submux-agent" status >/dev/null || die "Agent local status check failed"

if [ "$MODE" = upgrade ]; then
  say "submux-agent is upgraded, active, and running as ${AGENT_USER} (uid ${uid})"
else
  say "submux-agent is enrolled, active, and running as ${AGENT_USER} (uid ${uid})"
fi
say "root privileges are not retained by the Agent or Mihomo"
