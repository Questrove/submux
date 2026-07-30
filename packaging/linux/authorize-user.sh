#!/bin/sh
set -eu

fail() {
  echo "submux-runtime authorization: $*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "authorization requires root"
[ "$#" -eq 2 ] && [ "$1" = "--confirm" ] ||
  fail "usage: submux-runtime-authorize-user --confirm USER"
user_name=$2
case "$user_name" in
  *[!A-Za-z0-9_.-]* | -* | .*) fail "user name is invalid" ;;
esac
id "$user_name" >/dev/null 2>&1 || fail "user does not exist"
getent group submux-runtime >/dev/null 2>&1 || fail "Runtime operator group is unavailable"
usermod --append --groups submux-runtime "$user_name"
echo "Authorized $user_name. Start a new login session before using the management Socket."
