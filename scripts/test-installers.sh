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
if [ "${1:-}" = "-u" ]; then printf '0\n'; exit 0; fi
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
  local script="$1" install_dir="$2" binary="$3"
  PATH="$work/bin:$PATH" INSTALL_DIR="$install_dir" bash "$script" --version v1.2.3
  "$install_dir/$binary" --version | grep -F 'v1.2.3' >/dev/null
  if PATH="$work/bin:$PATH" FAKE_BAD_CHECKSUM=1 INSTALL_DIR="$install_dir" bash "$script" --version v1.2.4 --upgrade 2>/dev/null; then
    printf 'bad checksum was accepted by %s\n' "$script" >&2
    return 1
  fi
  "$install_dir/$binary" --version | grep -F 'v1.2.3' >/dev/null
  PATH="$work/bin:$PATH" INSTALL_DIR="$install_dir" bash "$script" --version v1.2.4 --upgrade
  "$install_dir/$binary" --version | grep -F 'v1.2.4' >/dev/null
  PATH="$work/bin:$PATH" INSTALL_DIR="$install_dir" bash "$script" --rollback
  "$install_dir/$binary" --version | grep -F 'v1.2.3' >/dev/null
  if PATH="$work/bin:$PATH" INSTALL_DIR="$install_dir" bash "$script" --version 'v1/../../invalid' 2>/dev/null; then
    printf 'unsafe version was accepted by %s\n' "$script" >&2
    return 1
  fi
  PATH="$work/bin:$PATH" INSTALL_DIR="$install_dir" bash "$script" --uninstall
  [ ! -e "$install_dir/$binary" ]
}

run_installer_cycle "$repo_root/scripts/install.sh" "$work/control" submux
run_installer_cycle "$repo_root/scripts/install-agent.sh" "$work/agent" submux-agent
printf 'installer lifecycle smoke tests passed\n'
