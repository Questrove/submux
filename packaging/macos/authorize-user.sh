#!/bin/sh
set -eu

fail() {
  echo "submux-runtime macOS authorization: $*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "desktop authorization requires root"
[ "$#" -eq 2 ] && [ "$1" = --confirm ] ||
  fail "usage: submux-runtime-authorize-user --confirm USER"
user_name=$2
case "$user_name" in *[!A-Za-z0-9_.-]* | -* | .*) fail "user name is invalid" ;; esac
uid=$(id -u "$user_name" 2>/dev/null) || fail "user does not exist"
home=$(dscl . -read "/Users/$user_name" NFSHomeDirectory 2>/dev/null |
  sed 's/^[^:]*:[[:space:]]*//')
case "$home" in /*) ;; *) fail "user home is invalid" ;; esac
[ -d "$home" ] && [ ! -L "$home" ] || fail "user home must be a real directory"

members=$(dscl . -read /Groups/submux-runtime-operators GroupMembership 2>/dev/null |
  sed 's/^GroupMembership:[[:space:]]*//' || true)
was_member=false
for member in $members; do
  [ "$member" = "$user_name" ] && was_member=true
done
agent_dir="$home/Library/LaunchAgents"
agent="$agent_dir/com.byworld.submux.gui.plist"
backup=
if [ -e "$agent" ]; then
  [ -f "$agent" ] && [ ! -L "$agent" ] ||
    fail "existing Runtime GUI LaunchAgent is not a regular file"
  backup=$(mktemp /var/tmp/submux-runtime-gui-agent.XXXXXX)
  cp -p "$agent" "$backup"
fi
completed=false
rollback() {
  if [ "$completed" != true ]; then
    launchctl bootout "gui/$uid/com.byworld.submux.gui" >/dev/null 2>&1 || true
    if [ -n "$backup" ]; then
      cp -p "$backup" "$agent" || true
    else
      rm -f "$agent"
    fi
    if [ "$was_member" != true ]; then
      dseditgroup -o edit -d "$user_name" -t user submux-runtime-operators >/dev/null 2>&1 || true
    fi
  fi
  [ -z "$backup" ] || rm -f "$backup"
}
trap rollback EXIT HUP INT TERM

dseditgroup -o edit -a "$user_name" -t user submux-runtime-operators
[ ! -L "$agent_dir" ] || fail "LaunchAgents directory must not be a symbolic link"
install -d -m 0755 -o "$user_name" -g "$(id -gn "$user_name")" "$agent_dir"
[ ! -L "$agent" ] || fail "Runtime GUI LaunchAgent must not be a symbolic link"
install -m 0644 -o "$user_name" -g "$(id -gn "$user_name")" \
  /Library/PrivilegedHelperTools/com.byworld.submux.gui.plist "$agent"
launchctl bootout "gui/$uid/com.byworld.submux.gui" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$uid" "$agent"
completed=true
trap - EXIT HUP INT TERM
[ -z "$backup" ] || rm -f "$backup"
echo "Authorized $user_name for Runtime IPC and installed its normal-user GUI login item."
