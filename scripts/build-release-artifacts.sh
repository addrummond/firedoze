#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
out_dir="${2:-dist/release}"

if [[ -z "$version" ]]; then
  echo "usage: scripts/build-release-artifacts.sh <version> [out-dir]" >&2
  exit 2
fi

version="${version#v}"
nfpm_version="${NFPM_VERSION:-v2.46.1}"
firecracker_version="${FIRECRACKER_VERSION:-v1.15.1}"
firecracker_x86_64_sha256="${FIRECRACKER_X86_64_SHA256:-d4a32ab2322d887ca1bc4a4e7afa9cc35393e6362dfc2b3becb389d362e4275a}"
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

run_nfpm() {
  if command -v nfpm >/dev/null 2>&1; then
    nfpm "$@"
  else
    go run "github.com/goreleaser/nfpm/v2/cmd/nfpm@$nfpm_version" "$@"
  fi
}

verify_sha256() {
  local expected="$1"
  local path="$2"

  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "$expected" "$path" | sha256sum -c -
  elif command -v shasum >/dev/null 2>&1; then
    printf '%s  %s\n' "$expected" "$path" | shasum -a 256 -c -
  else
    echo "sha256sum or shasum is required to verify Firecracker" >&2
    exit 1
  fi
}

stage_firecracker_linux_amd64() {
  local package_root="$1"
  local arch="x86_64"
  local tarball="firecracker-$firecracker_version-$arch.tgz"
  local work="$tmp_dir/firecracker-$firecracker_version-$arch"

  mkdir -p "$work" "$package_root/usr/lib/firedoze"
  curl -fsSL \
    "https://github.com/firecracker-microvm/firecracker/releases/download/$firecracker_version/$tarball" \
    -o "$work/$tarball"
  verify_sha256 "$firecracker_x86_64_sha256" "$work/$tarball"
  tar -xzf "$work/$tarball" -C "$work"
  install -m 0755 \
    "$work/release-$firecracker_version-$arch/firecracker-$firecracker_version-$arch" \
    "$package_root/usr/lib/firedoze/firecracker"
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

build_linux_packages() {
  local package_root="$tmp_dir/package-root-linux-amd64"

  mkdir -p "$package_root/usr/bin"
  mkdir -p "$package_root/usr/lib/firedoze"
  build_binary linux amd64 ./cmd/firedoze "$package_root/usr/bin/firedoze"
  build_binary linux amd64 ./cmd/firedozed "$package_root/usr/bin/firedozed"
  build_binary linux amd64 ./cmd/firedoze-image-builder "$package_root/usr/bin/firedoze-image-builder"
  build_binary linux amd64 ./cmd/firedoze-hello "$package_root/usr/lib/firedoze/firedoze-hello-linux-amd64"
  stage_firecracker_linux_amd64 "$package_root"

  export FIREDOZE_VERSION="$version"
  export FIREDOZE_PACKAGE_RELEASE="${FIREDOZE_PACKAGE_RELEASE:-1}"
  export FIREDOZE_PACKAGE_ARCH=amd64
  export FIREDOZE_PACKAGE_ROOT="$package_root"

  run_nfpm package --config packaging/nfpm.yaml --packager deb --target "$out_dir/firedoze_${version}_linux_amd64.deb"
  run_nfpm package --config packaging/nfpm.yaml --packager rpm --target "$out_dir/firedoze_${version}_linux_amd64.rpm"
}

build_bundle darwin amd64
build_bundle darwin arm64
build_bundle linux amd64
build_bundle linux arm64
build_linux_packages

(
  cd "$out_dir"
  artifacts=( *.deb *.rpm *.tar.gz )
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${artifacts[@]}" > "firedoze_${version}_checksums.txt"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${artifacts[@]}" > "firedoze_${version}_checksums.txt"
  else
    echo "sha256sum or shasum is required to write checksums" >&2
    exit 1
  fi
)

echo "wrote release artifacts to $out_dir"
