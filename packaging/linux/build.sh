#!/usr/bin/env bash
# shellcheck disable=SC2154
set -euo pipefail

fail() {
  echo "submux-runtime Linux package builder: $*" >&2
  exit 1
}

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
version=
architecture=
package_kind=
runtime_binary=
network_binary=
gui_binary=
sbom_file=
licenses_dir=
tuf_dir=
mihomo_binary=
mihomo_provenance=
mihomo_source=
mihomo_license=
output_dir=
formats=deb,rpm,tar

while (($# > 0)); do
  case "$1" in
    --version) version=${2:-}; shift 2 ;;
    --arch) architecture=${2:-}; shift 2 ;;
    --kind) package_kind=${2:-}; shift 2 ;;
    --runtime) runtime_binary=${2:-}; shift 2 ;;
    --runtime-net) network_binary=${2:-}; shift 2 ;;
    --gui) gui_binary=${2:-}; shift 2 ;;
    --sbom) sbom_file=${2:-}; shift 2 ;;
    --licenses) licenses_dir=${2:-}; shift 2 ;;
    --tuf) tuf_dir=${2:-}; shift 2 ;;
    --mihomo) mihomo_binary=${2:-}; shift 2 ;;
    --mihomo-provenance) mihomo_provenance=${2:-}; shift 2 ;;
    --mihomo-source) mihomo_source=${2:-}; shift 2 ;;
    --mihomo-license) mihomo_license=${2:-}; shift 2 ;;
    --output) output_dir=${2:-}; shift 2 ;;
    --formats) formats=${2:-}; shift 2 ;;
    *) fail "unsupported option $1" ;;
  esac
done

[[ $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
  fail "--version must be an exact stable vX.Y.Z version"
case "$architecture" in amd64 | arm64) ;; *) fail "--arch must be amd64 or arm64" ;; esac
case "$package_kind" in online | offline) ;; *) fail "--kind must be online or offline" ;; esac
[[ -n $output_dir ]] || fail "--output is required"

require_regular() {
  local name=$1
  local value=$2
  [[ -n $value && -f $value && ! -L $value ]] || fail "$name must be a regular file"
}

require_tree() {
  local name=$1
  local value=$2
  [[ -n $value && -d $value && ! -L $value ]] || fail "$name must be a real directory"
  if find "$value" -type l -o \( ! -type d ! -type f \) | grep -q .; then
    fail "$name contains a link or non-regular entry"
  fi
}

require_regular "--runtime" "$runtime_binary"
require_regular "--runtime-net" "$network_binary"
if [[ -n $gui_binary ]]; then
  require_regular "--gui" "$gui_binary"
fi
require_regular "--sbom" "$sbom_file"
require_tree "--licenses" "$licenses_dir"
if [[ $package_kind == offline ]]; then
  require_regular "--mihomo" "$mihomo_binary"
  require_regular "--mihomo-provenance" "$mihomo_provenance"
  require_regular "--mihomo-source" "$mihomo_source"
  require_regular "--mihomo-license" "$mihomo_license"
  require_tree "--tuf" "$tuf_dir"
  for metadata in root.json timestamp.json snapshot.json targets.json; do
    require_regular "offline TUF $metadata" "$tuf_dir/$metadata"
  done
elif [[ -n $mihomo_binary || -n $mihomo_provenance || -n $mihomo_source || -n $mihomo_license || -n $tuf_dir ]]; then
  fail "online packages must not contain Mihomo, its source/provenance, or an offline TUF bundle"
fi

case ",$formats," in
  *,deb,* | *,rpm,* | *,tar,*) ;;
  *) fail "--formats must contain deb, rpm, or tar" ;;
esac
[[ $formats != *" "* && $formats != *",,"* ]] || fail "--formats is invalid"
IFS=, read -r -a requested_formats <<<"$formats"
for format in "${requested_formats[@]}"; do
  case "$format" in deb | rpm | tar) ;; *) fail "unsupported package format $format" ;; esac
done

mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd -P)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
root="$work/root"
install -d \
  "$root/usr/lib/submux-runtime" \
  "$root/usr/lib/submux-runtime/licenses" \
  "$root/usr/lib/systemd/system" \
  "$root/usr/lib/tmpfiles.d" \
  "$root/usr/bin" \
  "$root/usr/sbin"
install -m 0755 "$runtime_binary" "$root/usr/lib/submux-runtime/submux-runtime"
install -m 0755 "$network_binary" "$root/usr/lib/submux-runtime/submux-runtime-net"
if [[ -n $gui_binary ]]; then
  install -m 0755 "$gui_binary" "$root/usr/lib/submux-runtime/submux-runtime-gui"
fi
install -m 0755 "$script_dir/runtime-lifecycle.sh" "$root/usr/lib/submux-runtime/runtime-lifecycle"
install -m 0755 "$script_dir/postremove.sh" "$root/usr/lib/submux-runtime/postremove.sh"
install -m 0755 "$script_dir/uninstall.sh" "$root/usr/sbin/submux-runtime-uninstall"
install -m 0755 "$script_dir/authorize-user.sh" "$root/usr/sbin/submux-runtime-authorize-user"
install -m 0644 "$script_dir/submux-runtime.service" "$root/usr/lib/systemd/system/submux-runtime.service"
install -m 0644 "$script_dir/submux-runtime-net.service" "$root/usr/lib/systemd/system/submux-runtime-net.service"
install -m 0644 "$script_dir/submux-runtime.tmpfiles" "$root/usr/lib/tmpfiles.d/submux-runtime.conf"
install -m 0644 "$sbom_file" "$root/usr/lib/submux-runtime/SBOM.spdx.json"
cp -a "$licenses_dir/." "$root/usr/lib/submux-runtime/licenses/"
find "$root/usr/lib/submux-runtime/licenses" -type d -exec chmod 0755 {} +
find "$root/usr/lib/submux-runtime/licenses" -type f -exec chmod 0644 {} +
ln -s ../lib/submux-runtime/submux-runtime "$root/usr/bin/submux-runtime"
if [[ -n $gui_binary ]]; then
  ln -s ../lib/submux-runtime/submux-runtime-gui "$root/usr/bin/submux-runtime-gui"
fi

if [[ $package_kind == offline ]]; then
  install -d "$root/usr/lib/submux-runtime/offline/tuf"
  install -m 0755 "$mihomo_binary" "$root/usr/lib/submux-runtime/offline/mihomo"
  install -m 0644 "$mihomo_provenance" "$root/usr/lib/submux-runtime/offline/mihomo-provenance.json"
  install -m 0644 "$mihomo_source" "$root/usr/lib/submux-runtime/offline/mihomo-source.tar.gz"
  install -m 0644 "$mihomo_license" "$root/usr/lib/submux-runtime/offline/Mihomo-GPL-3.0.txt"
  for metadata in root.json timestamp.json snapshot.json targets.json; do
    install -m 0644 "$tuf_dir/$metadata" "$root/usr/lib/submux-runtime/offline/tuf/$metadata"
  done
fi

{
  printf 'VERSION=%s\n' "$version"
  printf 'ARCH=%s\n' "$architecture"
  printf 'KIND=%s\n' "$package_kind"
  if [[ -n $gui_binary ]]; then
    printf 'GUI=present\n'
  else
    printf 'GUI=absent\n'
  fi
  printf 'SUPPORT=stable-systemd-glibc\n'
  printf 'MUSL_OPENWRT_NONSYSTEMD=preview-only\n'
} >"$work/PACKAGE-METADATA"
manifest="$work/ARTIFACT-MANIFEST.sha256"
(
  cd "$root"
  find . -type f -print0 |
    sort -z |
    xargs -0 sha256sum
) >"$manifest"
install -m 0644 "$manifest" "$root/usr/lib/submux-runtime/ARTIFACT-MANIFEST.sha256"

package_base="submux-runtime_${version#v}_${architecture}_${package_kind}"

build_tar() {
  command -v tar >/dev/null || fail "tar is required"
  command -v zstd >/dev/null || fail "zstd is required for tar.zst"
  bundle="$work/tar-bundle"
  install -d "$bundle"
  cp -a "$root" "$bundle/root"
  install -m 0755 "$script_dir/tar-install.sh" "$bundle/install.sh"
  install -m 0644 "$work/PACKAGE-METADATA" "$bundle/PACKAGE-METADATA"
  tar --sort=name --mtime='UTC 2020-01-01' --owner=0 --group=0 --numeric-owner \
    -C "$bundle" -cf - . |
    zstd -19 --threads=0 -o "$output_dir/$package_base.tar.zst"
}

build_deb() {
  command -v dpkg-deb >/dev/null || fail "dpkg-deb is required for DEB output"
  debroot="$work/deb"
  cp -a "$root" "$debroot"
  install -d "$debroot/DEBIAN"
  installed_size=$(du -sk "$root" | awk '{print $1}')
  cat >"$debroot/DEBIAN/control" <<EOF
Package: submux-runtime
Version: ${version#v}
Architecture: $architecture
Maintainer: Questrove <release@byworld.cn>
Installed-Size: $installed_size
Depends: systemd, libc6
Section: net
Priority: optional
Description: Local-only Submux Runtime ($package_kind package)
 Machine-level Runtime, CLI/TUI, GUI, and privileged network helper.
EOF
  cat >"$debroot/DEBIAN/preinst" <<EOF
#!/bin/sh
set -eu
[ "\$(id -u)" -eq 0 ] || { echo "Runtime installation requires root" >&2; exit 1; }
[ -d /run/systemd/system ] || { echo "stable packages require systemd" >&2; exit 1; }
getconf GNU_LIBC_VERSION >/dev/null 2>&1 || { echo "stable packages require glibc" >&2; exit 1; }
if [ -e /etc/systemd/system/submux-runtime.service ] && [ ! -L /etc/systemd/system/submux-runtime.service ]; then
  echo "refusing a second or unmanaged Runtime service" >&2
  exit 1
fi
if [ ! -f /var/lib/submux-runtime/install-receipt.json ] &&
   { [ -e /usr/lib/submux-runtime ] ||
     [ -e /var/lib/submux-runtime ] ||
     [ -e /var/lib/submux-runtime-privileged ] ||
     [ -e /usr/lib/systemd/system/submux-runtime.service ] ||
     [ -e /usr/lib/systemd/system/submux-runtime-net.service ]; }; then
  [ "\${SUBMUX_RUNTIME_REPAIR:-0}" = 1 ] || {
    echo "Runtime files without a valid receipt require an explicit repair" >&2
    exit 1
  }
fi
if { [ "\${1:-}" = install ] || [ "\${1:-}" = upgrade ]; } &&
   [ "\${2:-}" = "${version#v}" ]; then
  [ "\${SUBMUX_RUNTIME_REPAIR:-0}" = 1 ] || {
    echo "same-version installation requires SUBMUX_RUNTIME_REPAIR=1" >&2
    exit 1
  }
fi
if [ "\${1:-}" = upgrade ] && [ -n "\${2:-}" ] && dpkg --compare-versions "\$2" gt "${version#v}"; then
  [ "\${SUBMUX_RUNTIME_ALLOW_DOWNGRADE:-0}" = 1 ] &&
    [ "\${SUBMUX_RUNTIME_DATABASE_COMPATIBLE:-0}" = 1 ] || {
      echo "downgrade requires explicit authorization and database compatibility" >&2
      exit 1
    }
fi
if [ -x /usr/lib/submux-runtime/runtime-lifecycle ]; then
  /usr/lib/submux-runtime/runtime-lifecycle pre-remove
fi
EOF
  cat >"$debroot/DEBIAN/postinst" <<EOF
#!/bin/sh
set -eu
if [ -n "\${SUBMUX_RUNTIME_AUTHORIZE_USER:-}" ]; then
  /usr/lib/submux-runtime/runtime-lifecycle post-install "$version" "$architecture" "$package_kind" \
    --authorize-desktop "\$SUBMUX_RUNTIME_AUTHORIZE_USER"
else
  /usr/lib/submux-runtime/runtime-lifecycle post-install "$version" "$architecture" "$package_kind"
fi
EOF
  cat >"$debroot/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -eu
if [ -x /usr/lib/submux-runtime/runtime-lifecycle ]; then
  /usr/lib/submux-runtime/runtime-lifecycle pre-remove
fi
EOF
  if [[ $package_kind == offline ]]; then
    cat >>"$debroot/DEBIAN/postinst" <<'EOF'
test -x /usr/lib/submux-runtime/offline/mihomo
test -f /usr/lib/submux-runtime/offline/tuf/root.json
EOF
  fi
  cp "$script_dir/postremove.sh" "$debroot/DEBIAN/postrm"
  chmod 0755 "$debroot/DEBIAN/preinst" "$debroot/DEBIAN/postinst" \
    "$debroot/DEBIAN/prerm" "$debroot/DEBIAN/postrm"
  fakeroot_command=()
  if command -v fakeroot >/dev/null; then
    fakeroot_command=(fakeroot)
  fi
  "${fakeroot_command[@]}" dpkg-deb --root-owner-group --build "$debroot" \
    "$output_dir/$package_base.deb"
}

build_rpm() {
  command -v rpmbuild >/dev/null || fail "rpmbuild is required for RPM output"
  rpm_arch=x86_64
  [[ $architecture == arm64 ]] && rpm_arch=aarch64
  rpm_license=MIT
  [[ $package_kind == offline ]] && rpm_license='MIT AND GPL-3.0-only'
  rpm_gui_file=
  [[ -z $gui_binary ]] || rpm_gui_file=/usr/bin/submux-runtime-gui
  spec="$work/submux-runtime.spec"
  cat >"$spec" <<EOF
Name: submux-runtime
Version: ${version#v}
Release: 1
Summary: Local-only Submux Runtime
License: $rpm_license
BuildArch: $rpm_arch
Requires: systemd, glibc

%description
Machine-level Runtime, CLI/TUI, GUI, and privileged network helper.

%install
mkdir -p %{buildroot}
cp -a %{_payload}/. %{buildroot}/

%pre
if [ -f /var/lib/submux-runtime/install-receipt.json ]; then
  current_version=\$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\\([^"]*\\)"[,]*[[:space:]]*$/\\1/p' /var/lib/submux-runtime/install-receipt.json)
  current_root=\$(sed -n 's/^[[:space:]]*"install_root":[[:space:]]*"\\([^"]*\\)"[,]*[[:space:]]*$/\\1/p' /var/lib/submux-runtime/install-receipt.json)
  [ "\$current_root" = /usr/lib/submux-runtime ] || { echo "a second Runtime installation target is not allowed" >&2; exit 1; }
  if [ "\$current_version" = "$version" ] &&
     [ "\${SUBMUX_RUNTIME_REPAIR:-0}" != 1 ]; then
    echo "same-version installation requires SUBMUX_RUNTIME_REPAIR=1" >&2
    exit 1
  fi
  if [ -n "\$current_version" ] && [ "\$current_version" != "$version" ] &&
     printf '%s\\n%s\\n' "${version#v}" "\${current_version#v}" | sort -V -C; then
    [ "\${SUBMUX_RUNTIME_ALLOW_DOWNGRADE:-0}" = 1 ] &&
      [ "\${SUBMUX_RUNTIME_DATABASE_COMPATIBLE:-0}" = 1 ] || {
        echo "downgrade requires explicit authorization and database compatibility" >&2
        exit 1
      }
  fi
fi
if [ ! -f /var/lib/submux-runtime/install-receipt.json ] &&
   { [ -e /usr/lib/submux-runtime ] ||
     [ -e /var/lib/submux-runtime ] ||
     [ -e /var/lib/submux-runtime-privileged ] ||
     [ -e /usr/lib/systemd/system/submux-runtime.service ] ||
     [ -e /usr/lib/systemd/system/submux-runtime-net.service ]; }; then
  [ "\${SUBMUX_RUNTIME_REPAIR:-0}" = 1 ] || {
    echo "Runtime files without a valid receipt require an explicit repair" >&2
    exit 1
  }
fi
getent group submux-runtime >/dev/null || groupadd -r submux-runtime
getent passwd submux-runtime >/dev/null || useradd -r -g submux-runtime -d /var/lib/submux-runtime -s /sbin/nologin submux-runtime
if [ -x /usr/lib/submux-runtime/runtime-lifecycle ]; then
  /usr/lib/submux-runtime/runtime-lifecycle pre-remove
fi

%post
if [ -n "\${SUBMUX_RUNTIME_AUTHORIZE_USER:-}" ]; then
  /usr/lib/submux-runtime/runtime-lifecycle post-install "$version" "$architecture" "$package_kind" \
    --authorize-desktop "\$SUBMUX_RUNTIME_AUTHORIZE_USER"
else
  /usr/lib/submux-runtime/runtime-lifecycle post-install "$version" "$architecture" "$package_kind"
fi

%preun
if [ "\$1" -eq 0 ] && [ -x /usr/lib/submux-runtime/runtime-lifecycle ]; then
  /usr/lib/submux-runtime/runtime-lifecycle pre-remove
fi

%postun
if [ "\$1" -eq 0 ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
  # shellcheck disable=SC2154
  residuals=
  for service in submux-runtime.service submux-runtime-net.service; do
    systemctl is-active --quiet "\$service" 2>/dev/null &&
      residuals="\${residuals}\\nservice remains active: \$service" || true
  done
  routes=\$(ip -o rule show 2>/dev/null | grep -i submux || true)
  routes="\${routes}\$(ip -o route show table all 2>/dev/null | grep -i submux || true)"
  firewall=\$(nft list tables 2>/dev/null | grep -i submux || true)
  tun=\$(find /sys/class/net -maxdepth 1 -mindepth 1 -printf '%%f\\n' 2>/dev/null |
    grep -E '^(submux|tun-submux)' || true)
  dns=\$(resolvectl status 2>/dev/null | grep -i submux || true)
  sockets=
  for path in \
    /run/submux-runtime/runtime.sock \
    /run/submux-runtime-privileged/runtime-net.sock; do
    [ ! -e "\$path" ] && [ ! -S "\$path" ] || sockets="\$sockets \$path"
  done
  [ -z "\$routes" ] || residuals="\${residuals}\\nroutes:\\n\$routes"
  [ -z "\$firewall" ] || residuals="\${residuals}\\nfirewall:\\n\$firewall"
  [ -z "\$tun" ] || residuals="\${residuals}\\ninterfaces:\\n\$tun"
  [ -z "\$dns" ] || residuals="\${residuals}\\nDNS:\\n\$dns"
  [ -z "\$sockets" ] || residuals="\${residuals}\\nsockets:\$sockets"
  if [ -n "\$residuals" ]; then
    printf 'Submux Runtime uninstall residuals detected:%%b\\n' "\$residuals" >&2
    exit 1
  else
    echo "No Runtime-owned service, route, DNS, firewall, TUN, or Socket residual was detected."
  fi
  members=\$(getent group submux-runtime 2>/dev/null | awk -F: '{ print \$4 }' | tr ',' ' ' || true)
  for member in \$members; do
    [ "\$member" = submux-runtime ] ||
      gpasswd --delete "\$member" submux-runtime >/dev/null 2>&1 || true
  done
  if [ "\${SUBMUX_RUNTIME_PURGE:-0}" = 1 ]; then
    [ ! -L /run/submux-runtime ] &&
      [ ! -L /run/submux-runtime-privileged ] &&
      [ ! -L /var/lib/submux-runtime ] &&
      [ ! -L /var/lib/submux-runtime-privileged ] &&
      [ ! -L /etc/submux-runtime ] || exit 1
    rm -rf -- \
      /run/submux-runtime \
      /run/submux-runtime-privileged \
      /var/lib/submux-runtime \
      /var/lib/submux-runtime-privileged \
      /etc/submux-runtime
    getent passwd submux-runtime >/dev/null 2>&1 && userdel submux-runtime || true
    getent group submux-runtime >/dev/null 2>&1 && groupdel submux-runtime || true
  else
    echo "Runtime state was retained at /var/lib/submux-runtime."
    echo "Privileged Runtime state was retained at /var/lib/submux-runtime-privileged."
  fi
fi

%files
/usr/lib/submux-runtime
/usr/lib/systemd/system/submux-runtime.service
/usr/lib/systemd/system/submux-runtime-net.service
/usr/lib/tmpfiles.d/submux-runtime.conf
/usr/bin/submux-runtime
$rpm_gui_file
/usr/sbin/submux-runtime-authorize-user
/usr/sbin/submux-runtime-uninstall
EOF
  top="$work/rpmbuild"
  mkdir -p "$top"/{BUILD,BUILDROOT,RPMS,SOURCES,SPECS,SRPMS}
  rpmbuild -bb "$spec" \
    --define "_topdir $top" \
    --define "_payload $root"
  rpm_file=$(find "$top/RPMS" -type f -name '*.rpm' -print -quit)
  [[ -n $rpm_file ]] || fail "rpmbuild did not produce an RPM"
  cp "$rpm_file" "$output_dir/$package_base.rpm"
}

case ",$formats," in *,tar,*) build_tar ;; esac
case ",$formats," in *,deb,*) build_deb ;; esac
case ",$formats," in *,rpm,*) build_rpm ;; esac

(
  cd "$output_dir"
  for artifact in "$package_base".*; do
    [[ -f $artifact && $artifact != *.sha256 ]] || continue
    sha256sum "$artifact"
  done
) >"$output_dir/$package_base.sha256"
