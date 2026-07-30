#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "submux-runtime Linux package integration test: $*" >&2
  exit 1
}

format=
package=
desktop_user=
while (($# > 0)); do
  case "$1" in
    --format) format=${2:-}; shift 2 ;;
    --package) package=${2:-}; shift 2 ;;
    --desktop-user) desktop_user=${2:-}; shift 2 ;;
    *) fail "unsupported option $1" ;;
  esac
done
case "$format" in deb | rpm | tar) ;; *) fail "--format must be deb, rpm, or tar" ;; esac
[[ -n $package && -f $package && ! -L $package ]] || fail "--package must be a regular file"
[[ -n $desktop_user ]] || fail "--desktop-user is required"
[[ $(id -u) -eq 0 ]] || fail "integration tests require root"
id "$desktop_user" >/dev/null 2>&1 || fail "desktop test user does not exist"
[[ -d /run/systemd/system ]] || fail "integration tests require a running systemd system"
getconf GNU_LIBC_VERSION >/dev/null 2>&1 || fail "integration tests require glibc"

for path in \
  /usr/lib/submux-runtime \
  /var/lib/submux-runtime \
  /var/lib/submux-runtime-privileged \
  /etc/submux-runtime \
  /run/submux-runtime \
  /run/submux-runtime-privileged \
  /usr/lib/systemd/system/submux-runtime.service \
  /usr/lib/systemd/system/submux-runtime-net.service; do
  [[ ! -e $path ]] || fail "test host already contains Runtime state: $path"
done
getent passwd submux-runtime >/dev/null 2>&1 &&
  fail "test host already contains the Runtime account"
getent group submux-runtime >/dev/null 2>&1 &&
  fail "test host already contains the Runtime group"

bundle=
cleanup() {
  set +e
  if [[ -x /usr/lib/submux-runtime/runtime-lifecycle ]]; then
    /usr/lib/submux-runtime/runtime-lifecycle pre-remove >/dev/null 2>&1
  else
    systemctl stop submux-runtime.service >/dev/null 2>&1
    systemctl stop submux-runtime-net.service >/dev/null 2>&1
  fi
  systemctl disable submux-runtime.service submux-runtime-net.service >/dev/null 2>&1
  if dpkg-query -W submux-runtime >/dev/null 2>&1; then
    dpkg --purge submux-runtime >/dev/null 2>&1
  fi
  if rpm -q submux-runtime >/dev/null 2>&1; then
    SUBMUX_RUNTIME_PURGE=1 rpm -e --nodeps submux-runtime >/dev/null 2>&1
  fi
  if [[ -n $bundle && -x $bundle/install.sh && -x /usr/lib/submux-runtime/runtime-lifecycle ]]; then
    "$bundle/install.sh" --uninstall --purge >/dev/null 2>&1
  fi
  systemctl stop submux-runtime.service >/dev/null 2>&1
  systemctl stop submux-runtime-net.service >/dev/null 2>&1
  systemctl reset-failed submux-runtime.service submux-runtime-net.service >/dev/null 2>&1
  for path in \
    /usr/lib/submux-runtime \
    /var/lib/submux-runtime \
    /var/lib/submux-runtime-privileged \
    /etc/submux-runtime \
    /run/submux-runtime \
    /run/submux-runtime-privileged; do
    if [[ -e $path && ! -L $path ]]; then
      rm -rf -- "$path"
    fi
  done
  rm -f \
    /usr/lib/systemd/system/submux-runtime.service \
    /usr/lib/systemd/system/submux-runtime-net.service \
    /usr/lib/tmpfiles.d/submux-runtime.conf \
    /usr/bin/submux-runtime \
    /usr/bin/submux-runtime-gui \
    /usr/sbin/submux-runtime-authorize-user \
    /usr/sbin/submux-runtime-uninstall
  getent passwd submux-runtime >/dev/null 2>&1 && userdel submux-runtime
  getent group submux-runtime >/dev/null 2>&1 && groupdel submux-runtime
  dpkg --purge submux-runtime >/dev/null 2>&1
  [[ -z $bundle ]] || rm -rf -- "$bundle"
  systemctl daemon-reload >/dev/null 2>&1
}
trap cleanup EXIT HUP INT TERM
trap 'echo "FAILED: line $LINENO: $BASH_COMMAND" >&2' ERR

install_package() {
  case "$format" in
    deb) dpkg -i "$package" ;;
    rpm) rpm -ivh --nodeps "$package" ;;
    tar) "$bundle/install.sh" ;;
  esac
}

repair_package() {
  case "$format" in
    deb) SUBMUX_RUNTIME_REPAIR=1 dpkg -i "$package" ;;
    rpm) SUBMUX_RUNTIME_REPAIR=1 rpm -Uvh --replacepkgs --nodeps "$package" ;;
    tar) "$bundle/install.sh" --repair ;;
  esac
}

same_version_without_repair() {
  case "$format" in
    deb) dpkg -i "$package" ;;
    rpm) rpm -Uvh --replacepkgs --nodeps "$package" ;;
    tar) "$bundle/install.sh" ;;
  esac
}

remove_package() {
  purge=$1
  case "$format" in
    deb)
      if [[ $purge == true ]]; then
        dpkg --purge submux-runtime
      else
        dpkg -r submux-runtime
      fi
      ;;
    rpm)
      if [[ $purge == true ]]; then
        SUBMUX_RUNTIME_PURGE=1 rpm -e --nodeps submux-runtime
      else
        rpm -e --nodeps submux-runtime
      fi
      ;;
    tar)
      if [[ $purge == true ]]; then
        "$bundle/install.sh" --uninstall --purge
      else
        "$bundle/install.sh" --uninstall
      fi
      ;;
  esac
}

if [[ $format == tar ]]; then
  bundle=$(mktemp -d /tmp/submux-runtime-bundle.XXXXXX)
  tar --zstd -xf "$package" -C "$bundle"
fi

install_package
systemctl is-active --quiet submux-runtime-net.service
systemctl is-active --quiet submux-runtime.service
[[ $(systemctl show -P User submux-runtime.service) == submux-runtime ]]
[[ $(systemctl show -P User submux-runtime-net.service) == root ]]
[[ $(systemctl show -P Group submux-runtime-net.service) == root ]]
[[ $(stat -c '%a:%U:%G' /var/lib/submux-runtime) == 700:submux-runtime:submux-runtime ]]
[[ $(stat -c '%a:%U:%G' /var/lib/submux-runtime-privileged/network) == 700:root:root ]]
[[ $(stat -c '%a:%U:%G' /run/submux-runtime/runtime.sock) == 660:submux-runtime:submux-runtime ]]
[[ $(stat -c '%a:%U:%G' /run/submux-runtime-privileged/runtime-net.sock) == 660:root:submux-runtime ]]
/usr/bin/submux-runtime status --json >/dev/null
if runuser -u "$desktop_user" -- /usr/bin/submux-runtime status --json >/dev/null 2>&1; then
  fail "unauthorized desktop user reached the Runtime Socket"
fi
/usr/sbin/submux-runtime-authorize-user --confirm "$desktop_user"
runuser -u "$desktop_user" -- sg submux-runtime \
  -c '/usr/bin/submux-runtime status --json >/dev/null'

! ip -o rule show | grep -qi submux
! ip -o route show table all | grep -qi submux
! nft list tables | grep -qi submux
! find /sys/class/net -maxdepth 1 -mindepth 1 -printf '%f\n' |
  grep -Eq '^(submux|tun-submux)'

if same_version_without_repair >/tmp/submux-runtime-unexpected-repair.log 2>&1; then
  fail "same-version replacement succeeded without distinguishing repair"
fi
repair_package

remove_package false
[[ -d /var/lib/submux-runtime ]]
[[ -d /var/lib/submux-runtime-privileged ]]
[[ ! -e /usr/lib/submux-runtime ]]
if id -nG "$desktop_user" | tr ' ' '\n' | grep -qx submux-runtime; then
  fail "default uninstall retained desktop operator authorization"
fi

repair_package
remove_package true
[[ ! -e /var/lib/submux-runtime ]]
[[ ! -e /var/lib/submux-runtime-privileged ]]
[[ ! -e /etc/submux-runtime ]]
! getent passwd submux-runtime >/dev/null
! getent group submux-runtime >/dev/null
! systemctl is-active --quiet submux-runtime.service
! systemctl is-active --quiet submux-runtime-net.service
! ip -o rule show | grep -qi submux
! ip -o route show table all | grep -qi submux
! nft list tables | grep -qi submux

trap - EXIT HUP INT TERM
cleanup
echo "PASS: $format install, permission, repair, authorization, retain, purge, and residual checks"
