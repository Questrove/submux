#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
kit_readme="$script_dir/README.md"

fail() {
  echo "submux airgap builder: $*" >&2
  exit 1
}

bundle_version=
control_version=
runtime_version=
mihomo_version=
architecture=
control_binary=
control_installer=
runtime_bundle=
offline_verification_bundle=
airgap_installer=
output_dir=

while (($# > 0)); do
  case "$1" in
    --bundle-version) bundle_version=${2:-}; shift 2 ;;
    --control-version) control_version=${2:-}; shift 2 ;;
    --runtime-version) runtime_version=${2:-}; shift 2 ;;
    --mihomo-version) mihomo_version=${2:-}; shift 2 ;;
    --arch) architecture=${2:-}; shift 2 ;;
    --control) control_binary=${2:-}; shift 2 ;;
    --control-installer) control_installer=${2:-}; shift 2 ;;
    --runtime-bundle) runtime_bundle=${2:-}; shift 2 ;;
    --offline-verification-bundle) offline_verification_bundle=${2:-}; shift 2 ;;
    --airgap-installer) airgap_installer=${2:-}; shift 2 ;;
    --output) output_dir=${2:-}; shift 2 ;;
    *) fail "unsupported option $1" ;;
  esac
done

for value in "$bundle_version" "$control_version" "$runtime_version" "$mihomo_version"; do
  [[ $value =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
    fail "versions must be exact stable vX.Y.Z values"
done
case "$architecture" in amd64 | arm64) ;; *) fail "--arch must be amd64 or arm64" ;; esac
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
}

metadata_value() {
  local file=$1
  local key=$2
  local count value
  count=$(grep -c "^${key}=" "$file" || true)
  [[ $count == 1 ]] || fail "$file must contain exactly one $key"
  value=$(sed -n "s/^${key}=//p" "$file")
  [[ -n $value && $value != *$'\n'* ]] || fail "$file has an invalid $key"
  printf '%s' "$value"
}

require_regular "--control" "$control_binary"
require_regular "--control-installer" "$control_installer"
require_regular "--offline-verification-bundle" "$offline_verification_bundle"
require_regular "--airgap-installer" "$airgap_installer"
require_regular "airgap README" "$kit_readme"
require_tree "--runtime-bundle" "$runtime_bundle"
require_regular "Runtime install.sh" "$runtime_bundle/install.sh"
[[ -x $runtime_bundle/install.sh ]] || fail "Runtime install.sh must be executable"
require_regular "Runtime PACKAGE-METADATA" "$runtime_bundle/PACKAGE-METADATA"
[[ -d $runtime_bundle/root && ! -L $runtime_bundle/root ]] || fail "Runtime bundle root is missing"
require_regular "Runtime artifact manifest" \
  "$runtime_bundle/root/usr/lib/submux-runtime/ARTIFACT-MANIFEST.sha256"

unexpected=
while IFS= read -r -d '' link; do
  relative=${link#"$runtime_bundle"/}
  case "$relative" in
    root/usr/bin/submux-runtime | root/usr/bin/submux-runtime-gui) ;;
    *) unexpected=$relative; break ;;
  esac
done < <(find "$runtime_bundle" -type l -print0)
[[ -z $unexpected ]] || fail "Runtime bundle contains an unexpected link: $unexpected"
non_regular=$(find "$runtime_bundle" ! -type d ! -type f ! -type l -print -quit)
[[ -z $non_regular ]] || fail "Runtime bundle contains a non-regular entry"
[[ $(readlink "$runtime_bundle/root/usr/bin/submux-runtime") == \
    ../lib/submux-runtime/submux-runtime ]] ||
  fail "Runtime bundle CLI link is invalid"
if [[ -e $runtime_bundle/root/usr/lib/submux-runtime/submux-runtime-gui ||
      -L $runtime_bundle/root/usr/bin/submux-runtime-gui ]]; then
  [[ -f $runtime_bundle/root/usr/lib/submux-runtime/submux-runtime-gui &&
     $(readlink "$runtime_bundle/root/usr/bin/submux-runtime-gui") == \
       ../lib/submux-runtime/submux-runtime-gui ]] ||
    fail "Runtime bundle GUI payload or link is invalid"
fi

[[ $(metadata_value "$runtime_bundle/PACKAGE-METADATA" VERSION) == "$runtime_version" ]] ||
  fail "Runtime bundle version does not match --runtime-version"
[[ $(metadata_value "$runtime_bundle/PACKAGE-METADATA" ARCH) == "$architecture" ]] ||
  fail "Runtime bundle architecture does not match --arch"
[[ $(metadata_value "$runtime_bundle/PACKAGE-METADATA" KIND) == offline ]] ||
  fail "Runtime bundle must be a complete offline package"
(
  cd "$runtime_bundle/root"
  sha256sum --strict --check usr/lib/submux-runtime/ARTIFACT-MANIFEST.sha256 >/dev/null
) || fail "Runtime bundle artifact verification failed"
mkdir -p "$output_dir"
output_dir=$(cd -- "$output_dir" && pwd -P)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT HUP INT TERM
package_name="submux-airgap_${bundle_version#v}_linux_${architecture}"
root="$work/$package_name"
archive="$output_dir/$package_name.tar.gz"
checksum="$archive.sha256"
[[ ! -e $archive && ! -L $archive ]] || fail "output archive already exists: $archive"
[[ ! -e $checksum && ! -L $checksum ]] || fail "output checksum already exists: $checksum"

install -d -m 0755 "$root/control" "$root/runtime" "$root/trust"
install -m 0755 "$airgap_installer" "$root/install.sh"
install -m 0644 "$kit_readme" "$root/README.md"
install -m 0755 "$control_installer" "$root/control/install-submux.sh"
install -m 0755 "$control_binary" "$root/control/submux-linux-$architecture"
case "$(uname -m)" in
  x86_64 | amd64) build_arch=amd64 ;;
  aarch64 | arm64) build_arch=arm64 ;;
  *) build_arch=unknown ;;
esac
if [[ $build_arch == "$architecture" ]]; then
  "$root/control/submux-linux-$architecture" --version |
    grep -F " $control_version (" >/dev/null ||
    fail "control binary version does not match --control-version"
fi
(
  cd "$root/control"
  sha256sum "submux-linux-$architecture" >checksums.txt
)
chmod 0644 "$root/control/checksums.txt"
cp -a "$runtime_bundle/." "$root/runtime/"
install -m 0644 "$offline_verification_bundle" \
  "$root/trust/offline-verification-bundle.zip"

cat >"$root/AIRGAP-METADATA" <<EOF
SCHEMA=1
BUNDLE_VERSION=$bundle_version
CONTROL_VERSION=$control_version
RUNTIME_VERSION=$runtime_version
MIHOMO_VERSION=$mihomo_version
PLATFORM=linux
ARCH=$architecture
COMPONENTS=control,runtime
EOF
chmod 0644 "$root/AIRGAP-METADATA"

(
  cd "$root"
  find . -type f ! -path ./ARTIFACT-MANIFEST.sha256 -print0 |
    LC_ALL=C sort -z |
    xargs -0 sha256sum >ARTIFACT-MANIFEST.sha256
  sha256sum --strict --check ARTIFACT-MANIFEST.sha256 >/dev/null
)
chmod 0644 "$root/ARTIFACT-MANIFEST.sha256"

(
  cd "$work"
  tar --sort=name --mtime='UTC 2020-01-01' --owner=0 --group=0 --numeric-owner \
    -cf - "$package_name"
) | gzip -n -9 >"$archive"
chmod 0644 "$archive"
(
  cd "$output_dir"
  sha256sum "$(basename "$archive")" >"$(basename "$checksum")"
  sha256sum --strict --check "$(basename "$checksum")" >/dev/null
)

mapfile -t roots < <(tar -tzf "$archive" | sed 's#/$##' | cut -d/ -f1 | LC_ALL=C sort -u)
[[ ${#roots[@]} -eq 1 && ${roots[0]} == "$package_name" ]] ||
  fail "airgap archive does not contain exactly one expected root directory"
echo "built $archive"
