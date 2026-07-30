#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "submux-runtime macOS package builder: $*" >&2
  exit 1
}

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
version=
kind=
runtime_amd64=
runtime_arm64=
network_amd64=
network_arm64=
gui_amd64=
gui_arm64=
mihomo_amd64=
mihomo_arm64=
mihomo_provenance=
mihomo_source=
mihomo_license=
tuf_dir=
sbom=
license=
output_dir=

while (($# > 0)); do
  case "$1" in
    --version) version=${2:-}; shift 2 ;;
    --kind) kind=${2:-}; shift 2 ;;
    --runtime-amd64) runtime_amd64=${2:-}; shift 2 ;;
    --runtime-arm64) runtime_arm64=${2:-}; shift 2 ;;
    --runtime-net-amd64) network_amd64=${2:-}; shift 2 ;;
    --runtime-net-arm64) network_arm64=${2:-}; shift 2 ;;
    --gui-amd64) gui_amd64=${2:-}; shift 2 ;;
    --gui-arm64) gui_arm64=${2:-}; shift 2 ;;
    --mihomo-amd64) mihomo_amd64=${2:-}; shift 2 ;;
    --mihomo-arm64) mihomo_arm64=${2:-}; shift 2 ;;
    --mihomo-provenance) mihomo_provenance=${2:-}; shift 2 ;;
    --mihomo-source) mihomo_source=${2:-}; shift 2 ;;
    --mihomo-license) mihomo_license=${2:-}; shift 2 ;;
    --tuf) tuf_dir=${2:-}; shift 2 ;;
    --sbom) sbom=${2:-}; shift 2 ;;
    --license) license=${2:-}; shift 2 ;;
    --output) output_dir=${2:-}; shift 2 ;;
    *) fail "unsupported option $1" ;;
  esac
done

[[ $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
  fail "--version must be an exact stable vX.Y.Z version"
case "$kind" in online | offline) ;; *) fail "--kind must be online or offline" ;; esac
[[ -n $output_dir ]] || fail "--output is required"

require_regular() {
  local name=$1 value=$2
  [[ -n $value && -f $value && ! -L $value ]] || fail "$name must be a regular file"
}
for input in \
  "runtime amd64:$runtime_amd64" \
  "runtime arm64:$runtime_arm64" \
  "runtime-net amd64:$network_amd64" \
  "runtime-net arm64:$network_arm64" \
  "GUI amd64:$gui_amd64" \
  "GUI arm64:$gui_arm64" \
  "SBOM:$sbom" \
  "license:$license"; do
  require_regular "${input%%:*}" "${input#*:}"
done
if [[ $kind == offline ]]; then
  require_regular "Mihomo amd64" "$mihomo_amd64"
  require_regular "Mihomo arm64" "$mihomo_arm64"
  require_regular "Mihomo provenance" "$mihomo_provenance"
  require_regular "Mihomo source" "$mihomo_source"
  require_regular "Mihomo license" "$mihomo_license"
  [[ -n $tuf_dir && -d $tuf_dir && ! -L $tuf_dir ]] || fail "--tuf must be a real directory"
  if find "$tuf_dir" -type l -o \( ! -type d ! -type f \) | grep -q .; then
    fail "--tuf contains a link or non-regular entry"
  fi
  for metadata in root.json timestamp.json snapshot.json targets.json; do
    require_regular "TUF $metadata" "$tuf_dir/$metadata"
    plutil -lint "$tuf_dir/$metadata" >/dev/null
  done
elif [[ -n $mihomo_amd64 || -n $mihomo_arm64 || -n $mihomo_provenance || -n $mihomo_source || -n $mihomo_license || -n $tuf_dir ]]; then
  fail "online PKG files must not contain Mihomo, its source/provenance, or offline TUF metadata"
fi

for tool in lipo pkgbuild productbuild plutil shasum; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
verify_arch() {
  local architecture=$1 binary=$2
  lipo -verify_arch "$architecture" "$binary" ||
    fail "$binary does not contain $architecture"
}
for pair in \
  "x86_64:$runtime_amd64" "arm64:$runtime_arm64" \
  "x86_64:$network_amd64" "arm64:$network_arm64" \
  "x86_64:$gui_amd64" "arm64:$gui_arm64"; do
  verify_arch "${pair%%:*}" "${pair#*:}"
done
if [[ $kind == offline ]]; then
  verify_arch x86_64 "$mihomo_amd64"
  verify_arch arm64 "$mihomo_arm64"
fi

mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd -P)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
root="$work/root"
scripts="$work/scripts"
install -d \
  "$root/Library/PrivilegedHelperTools" \
  "$root/Library/LaunchDaemons" \
  "$root/Applications/Submux Runtime.app/Contents/MacOS" \
  "$root/usr/local/bin" \
  "$root/usr/local/sbin" \
  "$scripts"

lipo -create "$runtime_amd64" "$runtime_arm64" \
  -output "$root/Library/PrivilegedHelperTools/submux-runtime"
lipo -create "$network_amd64" "$network_arm64" \
  -output "$root/Library/PrivilegedHelperTools/submux-runtime-net"
lipo -create "$gui_amd64" "$gui_arm64" \
  -output "$root/Applications/Submux Runtime.app/Contents/MacOS/submux-runtime-gui"
install -m 0755 "$script_dir/runtime-lifecycle.sh" \
  "$root/Library/PrivilegedHelperTools/submux-runtime-lifecycle"
install -m 0644 "$script_dir/com.byworld.submux.runtime.plist" \
  "$root/Library/LaunchDaemons/com.byworld.submux.runtime.plist"
install -m 0644 "$script_dir/com.byworld.submux.runtime-net.plist" \
  "$root/Library/LaunchDaemons/com.byworld.submux.runtime-net.plist"
install -m 0644 "$script_dir/com.byworld.submux.gui.plist" \
  "$root/Library/PrivilegedHelperTools/com.byworld.submux.gui.plist"
install -m 0644 "$sbom" "$root/Library/PrivilegedHelperTools/SBOM.spdx.json"
install -d "$root/Library/PrivilegedHelperTools/SubmuxRuntimeLicenses"
install -m 0644 "$license" \
  "$root/Library/PrivilegedHelperTools/SubmuxRuntimeLicenses/NOTICE.txt"
install -m 0755 "$script_dir/authorize-user.sh" \
  "$root/usr/local/sbin/submux-runtime-authorize-user"
install -m 0755 "$script_dir/uninstall.sh" \
  "$root/usr/local/sbin/submux-runtime-uninstall"
ln -s /Library/PrivilegedHelperTools/submux-runtime "$root/usr/local/bin/submux-runtime"
ln -s '/Applications/Submux Runtime.app/Contents/MacOS/submux-runtime-gui' \
  "$root/usr/local/bin/submux-runtime-gui"

product_version=${version#v}
sed "s/@PRODUCT_VERSION@/$product_version/g" "$script_dir/Info.plist" \
  >"$root/Applications/Submux Runtime.app/Contents/Info.plist"
plutil -lint "$root/Applications/Submux Runtime.app/Contents/Info.plist" >/dev/null

if [[ $kind == offline ]]; then
  offline="$root/Library/PrivilegedHelperTools/SubmuxRuntimeOffline"
  install -d "$offline/tuf"
  lipo -create "$mihomo_amd64" "$mihomo_arm64" -output "$offline/mihomo"
  install -m 0644 "$mihomo_provenance" "$offline/mihomo-provenance.json"
  install -m 0644 "$mihomo_source" "$offline/mihomo-source.tar.gz"
  install -m 0644 "$mihomo_license" "$offline/Mihomo-GPL-3.0.txt"
  for metadata in root.json timestamp.json snapshot.json targets.json; do
    install -m 0644 "$tuf_dir/$metadata" "$offline/tuf/$metadata"
  done
fi

manifest="$root/Library/PrivilegedHelperTools/ARTIFACT-MANIFEST.sha256"
(
  cd "$root"
  find . -type f -print0 | sort -z | xargs -0 shasum -a 256
) >"$manifest"

sed -e "s/@VERSION@/$version/g" -e "s/@KIND@/$kind/g" \
  "$script_dir/preinstall.sh" >"$scripts/preinstall"
sed -e "s/@VERSION@/$version/g" -e "s/@KIND@/$kind/g" \
  "$script_dir/postinstall.sh" >"$scripts/postinstall"
chmod 0755 "$scripts/preinstall" "$scripts/postinstall"

component="$work/submux-runtime-component.pkg"
pkgbuild \
  --root "$root" \
  --scripts "$scripts" \
  --identifier com.byworld.submux.runtime \
  --version "$product_version" \
  --install-location / \
  "$component"
sed "s/@PRODUCT_VERSION@/$product_version/g" "$script_dir/Distribution.xml" \
  >"$work/Distribution.xml"
artifact="$output_dir/submux-runtime_${product_version}_universal_${kind}_unsigned.pkg"
productbuild \
  --distribution "$work/Distribution.xml" \
  --package-path "$work" \
  "$artifact"

(
  cd "$output_dir"
  shasum -a 256 "$(basename "$artifact")"
) >"$artifact.sha256"
cat >"$artifact.release-metadata" <<EOF
VERSION=$version
PLATFORM=darwin
ARCH=universal
KIND=$kind
SIGNED=false
NOTARIZED=false
SUPPORT=preview
REASON=native-install-ipc-tun-update-rollback-uninstall-matrix-not-complete
EOF
printf '%s\n' "$artifact"
