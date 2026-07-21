#!/usr/bin/env bash
set -euo pipefail
PATH="/usr/bin:/bin:$PATH"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT INT TERM
mkdir -p "$work/bin" "$work/control" "$work/agent"

cat >"$work/bin/id" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = "-u" ]; then printf '%s\n' "${FAKE_UID:-0}"; exit 0; fi
exec /usr/bin/id "$@"
EOF
cat >"$work/bin/uname" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in -s) printf 'Linux\n' ;; -m) printf 'x86_64\n' ;; *) printf 'Linux\n' ;; esac
EOF
cat >"$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
output=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
[ -n "$output" ] && [ -n "$url" ]
if [ "${url##*/}" = "checksums.txt" ]; then
  asset="$(find "$(dirname "$output")" -maxdepth 1 -type f ! -name checksums.txt | head -n 1)"
  digest="$(sha256sum "$asset" | awk '{print $1}')"
  if [ "${FAKE_BAD_CHECKSUM:-0}" = "1" ]; then digest="$(printf '0%.0s' {1..64})"; fi
  printf '%s  %s\n' "$digest" "$(basename "$asset")" >"$output"
  exit 0
fi
version="$(printf '%s' "$url" | sed -E 's#^.*/download/([^/]+)/.*$#\1#')"
binary="$(basename "$url")"
printf '#!/usr/bin/env bash\nprintf "%%s %%s (installer-test)\\n" "%s" "%s"\n' "$binary" "$version" >"$output"
EOF
chmod +x "$work/bin/id" "$work/bin/uname" "$work/bin/curl"

run_installer_cycle() {
  local script="$1" install_dir="$2" binary="$3" fake_uid="$4"
  PATH="$work/bin:$PATH" FAKE_UID="$fake_uid" INSTALL_DIR="$install_dir" bash "$script" --version v1.2.3
  "$install_dir/$binary" --version | grep -F 'v1.2.3' >/dev/null
  if PATH="$work/bin:$PATH" FAKE_UID="$fake_uid" FAKE_BAD_CHECKSUM=1 INSTALL_DIR="$install_dir" bash "$script" --version v1.2.4 --upgrade 2>/dev/null; then
    printf 'bad checksum was accepted by %s\n' "$script" >&2
    return 1
  fi
  "$install_dir/$binary" --version | grep -F 'v1.2.3' >/dev/null
  PATH="$work/bin:$PATH" FAKE_UID="$fake_uid" INSTALL_DIR="$install_dir" bash "$script" --version v1.2.4 --upgrade
  "$install_dir/$binary" --version | grep -F 'v1.2.4' >/dev/null
  PATH="$work/bin:$PATH" FAKE_UID="$fake_uid" INSTALL_DIR="$install_dir" bash "$script" --rollback
  "$install_dir/$binary" --version | grep -F 'v1.2.3' >/dev/null
  if PATH="$work/bin:$PATH" FAKE_UID="$fake_uid" INSTALL_DIR="$install_dir" bash "$script" --version 'v1/../../invalid' 2>/dev/null; then
    printf 'unsafe version was accepted by %s\n' "$script" >&2
    return 1
  fi
  PATH="$work/bin:$PATH" FAKE_UID="$fake_uid" INSTALL_DIR="$install_dir" bash "$script" --uninstall
  [ ! -e "$install_dir/$binary" ]
}

if [ -e /etc/systemd/system/submux.service ]; then
  printf 'skipping control-plane lifecycle test because a real submux.service exists\n'
else
  run_installer_cycle "$repo_root/scripts/install.sh" "$work/control" submux 0
fi
run_installer_cycle "$repo_root/scripts/install-agent.sh" "$work/agent" submux-agent 1000

local_agent="$work/submux-agent-local"
printf '#!/usr/bin/env bash\nprintf "submux-agent v2.0.0 (local-test)\\n"\n' >"$local_agent"
chmod 0755 "$local_agent"
local_digest="$(sha256sum "$local_agent" | awk '{print $1}')"
PATH="$work/bin:$PATH" FAKE_UID=1000 INSTALL_DIR="$work/agent-local" bash "$repo_root/scripts/install-agent.sh" \
  --version v2.0.0 --local-binary "$local_agent" --sha256 "$local_digest"
"$work/agent-local/submux-agent" --version | grep -F 'v2.0.0' >/dev/null
if PATH="$work/bin:$PATH" FAKE_UID=1000 INSTALL_DIR="$work/agent-bad" bash "$repo_root/scripts/install-agent.sh" \
  --version v2.0.0 --local-binary "$local_agent" --sha256 "$(printf '0%.0s' {1..64})" 2>/dev/null; then
  printf 'bad local binary checksum was accepted by install-agent.sh\n' >&2
  exit 1
fi

agent_script="$repo_root/scripts/install-agent.sh"
for forbidden in 'run_as_root' '/etc/systemd/system' 'User=root' 'useradd' '/var/lib/submux-agent' 'ProtectKernelModules=true' 'ProtectKernelLogs=true'; do
  if grep -F "$forbidden" "$agent_script" >/dev/null; then
    printf 'unprivileged Agent installer retains forbidden host-level operation: %s\n' "$forbidden" >&2
    exit 1
  fi
done
for required in '${HOME}/.local/bin' 'systemctl --user' 'WantedBy=default.target' 'local Socket did not become ready'; do
  grep -F "$required" "$agent_script" >/dev/null || {
    printf 'unprivileged Agent installer is missing: %s\n' "$required" >&2
    exit 1
  }
done

bootstrap_script="$repo_root/scripts/bootstrap-agent.sh"
for forbidden in '/etc/systemd/system' 'User=root' '/var/lib/submux-agent' 'CAP_NET_ADMIN'; do
  if grep -F "$forbidden" "$bootstrap_script" >/dev/null; then
    printf 'server bootstrap retains forbidden privileged Agent operation: %s\n' "$forbidden" >&2
    exit 1
  fi
done
for required in 'useradd --create-home' 'loginctl enable-linger' 'runuser -u' 'systemctl --user is-active' '--local-binary' '--upgrade'; do
  grep -F -- "$required" "$bootstrap_script" >/dev/null || {
    printf 'server bootstrap is missing: %s\n' "$required" >&2
    exit 1
  }
done

bootstrap_bin="$work/bootstrap-bin"
bootstrap_home="$work/bootstrap-home"
bootstrap_calls="$work/bootstrap-calls.log"
bootstrap_args="$work/bootstrap-installer-args.log"
bootstrap_env="$work/bootstrap-installer-env.log"
mkdir -p "$bootstrap_bin" "$bootstrap_home/.local/bin"
: >"$bootstrap_calls"

cat >"$bootstrap_bin/getent" <<'EOF'
#!/usr/bin/env bash
if [ "${FAKE_BOOTSTRAP_USER_MISSING:-0}" = 1 ]; then exit 2; fi
[ "${1:-}" = passwd ] && [ "${2:-}" = submuxagent ] || exit 2
printf 'submuxagent:x:4242:4242::%s:/bin/bash\n' "$BOOTSTRAP_AGENT_HOME"
EOF
cat >"$bootstrap_bin/stat" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = -c ] && [ "${2:-}" = %U ]; then printf 'submuxagent\n'; exit 0; fi
exec /usr/bin/stat "$@"
EOF
cat >"$bootstrap_bin/runuser" <<'EOF'
#!/usr/bin/env bash
[ "${1:-}" = -u ] && [ "${2:-}" = submuxagent ] && [ "${3:-}" = -- ] || exit 2
printf 'runuser %s\n' "$2" >>"$BOOTSTRAP_CALL_LOG"
shift 3
exec env -u http_proxy -u https_proxy -u all_proxy -u no_proxy \
  -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u NO_PROXY "$@"
EOF
cat >"$bootstrap_bin/systemctl" <<'EOF'
#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >>"$BOOTSTRAP_CALL_LOG"
case "$*" in
  'is-active --quiet user@4242.service') exit 0 ;;
  '--user is-active --quiet submux-agent.service') [ "${FAKE_BOOTSTRAP_AGENT_INACTIVE:-0}" = 0 ] ;;
  *) exit 97 ;;
esac
EOF
for command_name in loginctl useradd; do
  cat >"$bootstrap_bin/$command_name" <<'EOF'
#!/usr/bin/env bash
printf '%s %s\n' "$(basename "$0")" "$*" >>"$BOOTSTRAP_CALL_LOG"
exit 97
EOF
done
cat >"$bootstrap_home/.local/bin/submux-agent" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  --version) printf 'submux-agent v1.2.3 (bootstrap-test)\n' ;;
  status) exit 0 ;;
  *) exit 0 ;;
esac
EOF
bootstrap_installer="$work/bootstrap-install-agent.sh"
cat >"$bootstrap_installer" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$BOOTSTRAP_INSTALLER_ARGS"
printf 'HOME=%s\nXDG_RUNTIME_DIR=%s\nDBUS_SESSION_BUS_ADDRESS=%s\nhttps_proxy=%s\n' \
  "$HOME" "$XDG_RUNTIME_DIR" "$DBUS_SESSION_BUS_ADDRESS" "${https_proxy:-}" \
  >"$BOOTSTRAP_INSTALLER_ENV"
EOF
chmod +x "$bootstrap_bin"/* "$bootstrap_home/.local/bin/submux-agent" "$bootstrap_installer"

PATH="$bootstrap_bin:$work/bin:$PATH" \
  FAKE_UID=0 \
  BOOTSTRAP_AGENT_HOME="$bootstrap_home" \
  BOOTSTRAP_CALL_LOG="$bootstrap_calls" \
  BOOTSTRAP_INSTALLER_ARGS="$bootstrap_args" \
  BOOTSTRAP_INSTALLER_ENV="$bootstrap_env" \
  https_proxy=http://127.0.0.1:17890 \
  bash "$bootstrap_script" --upgrade --user submuxagent --version v1.2.4 --installer "$bootstrap_installer"

for required_arg in '--channel' stable '--service' '--upgrade' '--version' v1.2.4; do
  grep -Fx -- "$required_arg" "$bootstrap_args" >/dev/null || {
    printf 'bootstrap upgrade did not forward installer argument: %s\n' "$required_arg" >&2
    exit 1
  }
done
[ "$(wc -l <"$bootstrap_args")" -eq 6 ] || {
  printf 'bootstrap upgrade forwarded unexpected installer arguments\n' >&2
  exit 1
}
for forbidden_arg in '--server' '--code' '--local-binary' '--sha256'; do
  if grep -Fx -- "$forbidden_arg" "$bootstrap_args" >/dev/null; then
    printf 'bootstrap upgrade forwarded forbidden installer argument: %s\n' "$forbidden_arg" >&2
    exit 1
  fi
done
if grep -Eq '^(useradd|loginctl|systemctl start )' "$bootstrap_calls"; then
  printf 'bootstrap upgrade changed dedicated-user provisioning state\n' >&2
  exit 1
fi
grep -Fx "HOME=$bootstrap_home" "$bootstrap_env" >/dev/null
grep -Fx 'XDG_RUNTIME_DIR=/run/user/4242' "$bootstrap_env" >/dev/null
grep -Fx 'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/4242/bus' "$bootstrap_env" >/dev/null
grep -Fx 'https_proxy=http://127.0.0.1:17890' "$bootstrap_env" >/dev/null

: >"$bootstrap_calls"
if PATH="$bootstrap_bin:$work/bin:$PATH" \
  FAKE_UID=0 \
  FAKE_BOOTSTRAP_USER_MISSING=1 \
  BOOTSTRAP_AGENT_HOME="$bootstrap_home" \
  BOOTSTRAP_CALL_LOG="$bootstrap_calls" \
  bash "$bootstrap_script" --upgrade --user submuxagent --version v1.2.4 --installer "$bootstrap_installer" 2>/dev/null; then
  printf 'bootstrap upgrade accepted a missing dedicated user\n' >&2
  exit 1
fi
if grep -Eq '^(useradd|loginctl)' "$bootstrap_calls"; then
  printf 'bootstrap upgrade tried to provision a missing dedicated user\n' >&2
  exit 1
fi

: >"$bootstrap_calls"
rm -f "$bootstrap_args"
if PATH="$bootstrap_bin:$work/bin:$PATH" \
  FAKE_UID=0 \
  FAKE_BOOTSTRAP_AGENT_INACTIVE=1 \
  BOOTSTRAP_AGENT_HOME="$bootstrap_home" \
  BOOTSTRAP_CALL_LOG="$bootstrap_calls" \
  BOOTSTRAP_INSTALLER_ARGS="$bootstrap_args" \
  bash "$bootstrap_script" --upgrade --user submuxagent --version v1.2.4 \
    --installer "$bootstrap_installer" 2>/dev/null; then
  printf 'bootstrap upgrade accepted an inactive Agent service\n' >&2
  exit 1
fi
[ ! -e "$bootstrap_args" ] || {
  printf 'bootstrap upgrade invoked the installer while the Agent service was inactive\n' >&2
  exit 1
}

if PATH="$bootstrap_bin:$work/bin:$PATH" \
  FAKE_UID=0 \
  BOOTSTRAP_AGENT_HOME="$bootstrap_home" \
  BOOTSTRAP_CALL_LOG="$bootstrap_calls" \
  bash "$bootstrap_script" --upgrade --server http://127.0.0.1:8080 --code test \
    --installer "$bootstrap_installer" 2>/dev/null; then
  printf 'bootstrap upgrade accepted enrollment arguments\n' >&2
  exit 1
fi

windows_agent_script="$repo_root/scripts/install-agent.ps1"
for forbidden in 'Assert-Administrator' '$env:ProgramFiles' 'service install' 'LocalSystem'; do
  if grep -F "$forbidden" "$windows_agent_script" >/dev/null; then
    printf 'Windows user Agent installer retains elevated operation: %s\n' "$forbidden" >&2
    exit 1
  fi
done
for required in '$env:LOCALAPPDATA' "GetFolderPath('Startup')"; do
  grep -F "$required" "$windows_agent_script" >/dev/null || {
    printf 'Windows user Agent installer is missing: %s\n' "$required" >&2
    exit 1
  }
done
printf 'installer lifecycle smoke tests passed\n'
