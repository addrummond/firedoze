#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
out_dir="${2:-dist/release}"

if [[ -z "$version" ]]; then
  echo "usage: scripts/build-release-artifacts.sh <version> [out-dir]" >&2
  exit 2
fi

version="${version#v}"
repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

mkdir -p "$out_dir"
rm -f "$out_dir"/*

build_binary() {
  local goos="$1"
  local goarch="$2"
  local pkg="$3"
  local out="$4"

  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -o "$out" "$pkg"
}

build_bundle() {
  local goos="$1"
  local goarch="$2"
  local bundle="firedoze_${version}_${goos}_${goarch}"
  local root="$tmp_dir/$bundle"
  local exe=""

  if [[ "$goos" == "windows" ]]; then
    exe=".exe"
  fi

  mkdir -p "$root"
  build_binary "$goos" "$goarch" ./cmd/firedoze "$root/firedoze$exe"
  build_binary "$goos" "$goarch" ./cmd/firedoze-image-builder "$root/firedoze-image-builder$exe"

  if [[ "$goos" == "linux" && "$goarch" == "amd64" ]]; then
    build_binary "$goos" "$goarch" ./cmd/firedozed "$root/firedozed"
    mkdir -p "$root/config" "$root/systemd"
    cp "$repo_root/config/firedoze.example.toml" "$root/config/"
    cp "$repo_root/systemd/firedozed.service" "$root/systemd/"
  fi

  cp "$repo_root/LICENSE" "$root/"
  cp "$repo_root/Readme.md" "$root/"

  tar -C "$tmp_dir" -czf "$out_dir/$bundle.tar.gz" "$bundle"
}

build_bundle darwin amd64
build_bundle darwin arm64
build_bundle linux amd64
build_bundle linux arm64

(
  cd "$out_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum *.tar.gz > "firedoze_${version}_checksums.txt"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 *.tar.gz > "firedoze_${version}_checksums.txt"
  else
    echo "sha256sum or shasum is required to write checksums" >&2
    exit 1
  fi
)

echo "wrote release artifacts to $out_dir"
