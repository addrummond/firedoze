#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: ./release.sh <version> [--push] [--skip-tests]

Creates an annotated Git tag for a Firedoze release.

Examples:
  ./release.sh v0.1.0
  ./release.sh v0.1.0 --push
EOF
}

version=""
push=0
run_tests=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    --push)
      push=1
      shift
      ;;
    --skip-tests)
      run_tests=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "unknown option: $1" >&2
      usage
      exit 2
      ;;
    *)
      if [[ -n "$version" ]]; then
        echo "unexpected argument: $1" >&2
        usage
        exit 2
      fi
      version="$1"
      shift
      ;;
  esac
done

if [[ -z "$version" ]]; then
  usage
  exit 2
fi

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "version must look like v0.1.0 or v0.1.0-rc.1" >&2
  exit 2
fi

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "working tree is not clean; commit or stash changes first" >&2
  exit 1
fi

if git rev-parse -q --verify "refs/tags/$version" >/dev/null; then
  echo "tag already exists: $version" >&2
  exit 1
fi

if [[ "$run_tests" -eq 1 ]]; then
  CGO_ENABLED=0 go test ./...
fi

git tag -a "$version" -m "Release $version"

echo "created tag $version"
if [[ "$push" -eq 1 ]]; then
  git push origin "$version"
else
  echo "push it with: git push origin $version"
fi
