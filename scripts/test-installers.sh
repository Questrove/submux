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
for required in 'useradd --create-home' 'loginctl enable-linger' 'runuser -u' 'systemctl --user is-active' '--local-binary'; do
  grep -F -- "$required" "$bootstrap_script" >/dev/null || {
    printf 'server bootstrap is missing: %s\n' "$required" >&2
    exit 1
  }
done

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
