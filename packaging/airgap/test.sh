#!/usr/bin/env bash
set -euo pipefail

if [[ $(uname -s) != Linux ]]; then
  echo "airgap archive tests require a Linux host; skipped"
  exit 0
fi

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT HUP INT TERM

case "$(uname -m)" in
  x86_64 | amd64) architecture=amd64 ;;
  aarch64 | arm64) architecture=arm64 ;;
  *) echo "unsupported test architecture" >&2; exit 1 ;;
esac

mkdir -p \
  "$work/input/runtime/root/usr/lib/submux-runtime" \
  "$work/input/runtime/root/usr/bin" \
  "$work/output-one" \
  "$work/output-two"

cat >"$work/input/control" <<'EOF'
#!/usr/bin/env bash
printf 'submux v3.4.5 (airgap-test)\n'
EOF
cat >"$work/input/control-installer" <<'EOF'
#!/usr/bin/env bash
echo "fake control installer should not run during verification" >&2
exit 99
EOF
cat >"$work/input/runtime/install.sh" <<'EOF'
#!/bin/sh
echo "fake Runtime installer should not run during verification" >&2
exit 99
EOF
cat >"$work/input/runtime/PACKAGE-METADATA" <<EOF
VERSION=v3.4.5
ARCH=$architecture
KIND=offline
GUI=absent
SUPPORT=preview-test
EOF
printf 'runtime-test\n' >"$work/input/runtime/root/usr/lib/submux-runtime/submux-runtime"
ln -s ../lib/submux-runtime/submux-runtime \
  "$work/input/runtime/root/usr/bin/submux-runtime"
printf 'bundle-test\n' >"$work/input/offline-verification-bundle.zip"
chmod 0644 "$work/input/control"
chmod 0755 \
  "$work/input/control-installer" \
  "$work/input/runtime/install.sh" \
  "$work/input/runtime/root/usr/lib/submux-runtime/submux-runtime"
(
  cd "$work/input/runtime/root"
  sha256sum usr/lib/submux-runtime/submux-runtime > \
    usr/lib/submux-runtime/ARTIFACT-MANIFEST.sha256
)

build() {
  local output=$1
  local target_arch=${2:-$architecture}
  "$repo_root/packaging/airgap/build.sh" \
    --bundle-version v3.4.5 \
    --control-version v3.4.5 \
    --runtime-version v3.4.5 \
    --mihomo-version v1.2.3 \
    --arch "$target_arch" \
    --control "$work/input/control" \
    --control-installer "$work/input/control-installer" \
    --runtime-bundle "$work/input/runtime" \
    --offline-verification-bundle "$work/input/offline-verification-bundle.zip" \
    --airgap-installer "$repo_root/packaging/airgap/install.sh" \
    --output "$output"
}

build "$work/output-one"
build "$work/output-two"
archive="submux-airgap_3.4.5_linux_${architecture}.tar.gz"
cmp "$work/output-one/$archive" "$work/output-two/$archive"
cmp "$work/output-one/$archive.sha256" "$work/output-two/$archive.sha256"

mkdir "$work/extracted"
tar -xzf "$work/output-one/$archive" -C "$work/extracted"
kit="$work/extracted/submux-airgap_3.4.5_linux_${architecture}"
bash "$kit/install.sh" verify >/dev/null
[[ -x $kit/control/submux-linux-$architecture ]] || {
  echo "airgap builder did not normalize the control binary mode" >&2
  exit 1
}

printf 'unlisted\n' >"$kit/unlisted.txt"
if bash "$kit/install.sh" verify >/dev/null 2>&1; then
  echo "airgap verification accepted an unlisted file" >&2
  exit 1
fi
rm "$kit/unlisted.txt"

printf 'tampered\n' >>"$kit/control/submux-linux-$architecture"
if bash "$kit/install.sh" verify >/dev/null 2>&1; then
  echo "airgap verification accepted a tampered control binary" >&2
  exit 1
fi

case "$architecture" in amd64) cross_arch=arm64 ;; arm64) cross_arch=amd64 ;; esac
sed -i "s/^ARCH=.*/ARCH=$cross_arch/" "$work/input/runtime/PACKAGE-METADATA"
mkdir "$work/output-cross" "$work/extracted-cross"
build "$work/output-cross" "$cross_arch"
cross_archive="submux-airgap_3.4.5_linux_${cross_arch}.tar.gz"
tar -xzf "$work/output-cross/$cross_archive" -C "$work/extracted-cross"
bash "$work/extracted-cross/submux-airgap_3.4.5_linux_${cross_arch}/install.sh" \
  verify >/dev/null
sed -i "s/^ARCH=.*/ARCH=$architecture/" "$work/input/runtime/PACKAGE-METADATA"

sed -i 's/^KIND=offline$/KIND=online/' "$work/input/runtime/PACKAGE-METADATA"
if "$repo_root/packaging/airgap/build.sh" \
    --bundle-version v3.4.6 \
    --control-version v3.4.5 \
    --runtime-version v3.4.5 \
    --mihomo-version v1.2.3 \
    --arch "$architecture" \
    --control "$work/input/control" \
    --control-installer "$work/input/control-installer" \
    --runtime-bundle "$work/input/runtime" \
    --offline-verification-bundle "$work/input/offline-verification-bundle.zip" \
    --airgap-installer "$repo_root/packaging/airgap/install.sh" \
    --output "$work/invalid" >/dev/null 2>&1; then
  echo "airgap builder accepted an online Runtime bundle" >&2
  exit 1
fi

mkdir -p "$work/runtime-install/runtime"
cat >"$work/runtime-install/runtime/install.sh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod 0755 "$work/runtime-install/runtime/install.sh"
if failure_output=$(
  (
    source "$repo_root/packaging/airgap/install.sh"
    script_dir="$work/runtime-install"
    runtime_version=v2.0.1
    mihomo_version=v1.19.29
    repair=false
    allow_downgrade=false
    database_compatible=false
    authorize_user=
    installed_runtime_version() { return 0; }
    runtime_cli() {
      if [[ $1 == status ]]; then
        printf '{"mihomo":{"state":"not_installed"}}\n'
        return 0
      fi
      if [[ $1 == mihomo && $2 == import ]]; then
        printf '{"protocol_version":1,"error":{"code":"invalid_request","message":"asset contract mismatch","retryable":false}}\n'
        return 1
      fi
      return 99
    }
    install_runtime
  ) 2>&1
); then
  echo "airgap Runtime failure harness unexpectedly succeeded" >&2
  exit 1
fi
if ! grep -F '"message":"asset contract mismatch"' <<<"$failure_output" >/dev/null; then
  echo "airgap installer hid the Runtime JSON failure" >&2
  exit 1
fi
if grep -F 'Submux Runtime v2.0.1 is installed' <<<"$failure_output" >/dev/null; then
  echo "airgap installer reported Runtime success before Mihomo activation" >&2
  exit 1
fi

if ! detected_version=$(
  (
    source "$repo_root/packaging/airgap/install.sh"
    runtime_cli() {
      printf '%s\n' '{"updates":{"mihomo_available":true,"mihomo_current_version":"v1.19.29"}}'
    }
    current_mihomo_version
  )
) || [[ $detected_version != v1.19.29 ]]; then
  echo "airgap installer did not read Mihomo version from Runtime update status" >&2
  exit 1
fi

echo "airgap build, reproducibility, verification and tamper tests passed"
