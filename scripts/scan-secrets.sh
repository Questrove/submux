#!/usr/bin/env bash

set -euo pipefail

readonly gitleaks_version="8.30.1"
readonly gitleaks_sha256="551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb"
readonly gitleaks_archive="gitleaks_${gitleaks_version}_linux_x64.tar.gz"
readonly gitleaks_url="https://github.com/gitleaks/gitleaks/releases/download/v${gitleaks_version}/${gitleaks_archive}"

if [[ "$(uname -s)" != "Linux" || "$(uname -m)" != "x86_64" ]]; then
  echo "secret scan requires a Linux x86_64 runner" >&2
  exit 1
fi

if [[ "$(git rev-parse --is-shallow-repository)" != "false" ]]; then
  echo "secret scan requires the complete Git history; use actions/checkout with fetch-depth: 0" >&2
  exit 1
fi

scan_dir="$(mktemp -d)"
trap 'rm -rf -- "$scan_dir"' EXIT

curl \
  --fail \
  --location \
  --proto '=https' \
  --retry 3 \
  --show-error \
  --silent \
  --tlsv1.2 \
  "$gitleaks_url" \
  --output "$scan_dir/$gitleaks_archive"

printf '%s  %s\n' \
  "$gitleaks_sha256" \
  "$scan_dir/$gitleaks_archive" |
  sha256sum --check --strict -

tar -xzf "$scan_dir/$gitleaks_archive" -C "$scan_dir" gitleaks
"$scan_dir/gitleaks" version
"$scan_dir/gitleaks" git \
  --exit-code 1 \
  --log-opts="--all" \
  --redact \
  --verbose \
  .
