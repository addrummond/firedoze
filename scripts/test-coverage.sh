#!/usr/bin/env bash
set -euo pipefail

out_dir="${1:-dist/coverage}"
profile_dir="$out_dir/profiles"
summary="$out_dir/summary.tsv"

mkdir -p "$profile_dir"
: > "$summary"

status=0
repo_root="$(pwd)"

while IFS=$'\t' read -r pkg pkg_dir test_files xtest_files; do
  safe_pkg="$(printf '%s' "$pkg" | sed 's#[^A-Za-z0-9_.-]#_#g')"
  rel_dir="${pkg_dir#$repo_root/}"
  if [[ "$rel_dir" == "$pkg_dir" ]]; then
    package_arg="$pkg_dir"
  elif [[ "$rel_dir" == "." ]]; then
    package_arg="."
  else
    package_arg="./$rel_dir"
  fi

  if (( test_files + xtest_files == 0 )); then
    printf 'no tests\t%s\n' "$pkg" >> "$summary"
    continue
  fi

  profile="$profile_dir/$safe_pkg.out"
  if output="$(CGO_ENABLED="${CGO_ENABLED:-0}" go test -coverprofile="$profile" "$package_arg" 2>&1)"; then
    printf '%s\n' "$output"
    coverage="$(printf '%s\n' "$output" | awk -F'coverage: ' '/coverage:/ { split($2, a, " "); print a[1] }' | tail -n 1)"
    if [[ -z "$coverage" ]]; then
      coverage="unknown"
    fi
    printf '%s\t%s\n' "$coverage" "$pkg" >> "$summary"
  else
    printf '%s\n' "$output" >&2
    printf 'failed\t%s\n' "$pkg" >> "$summary"
    status=1
  fi
done < <(go list -f '{{.ImportPath}}{{printf "\t"}}{{.Dir}}{{printf "\t"}}{{len .TestGoFiles}}{{printf "\t"}}{{len .XTestGoFiles}}' ./...)

printf '\nCoverage summary, lowest first:\n'
awk -F'\t' '
  $1 == "failed" {
    printf "-001.000\t%s\t%s\n", $1, $2
    next
  }
  $1 ~ /%$/ {
    value = $1
    sub(/%$/, "", value)
    printf "%08.3f\t%s\t%s\n", value + 0, $1, $2
    next
  }
  $1 == "unknown" {
    printf "9998.000\t%s\t%s\n", $1, $2
    next
  }
  {
    printf "9999.000\t%s\t%s\n", $1, $2
  }
' "$summary" | sort -n | awk -F'\t' '{ printf "%8s  %s\n", $2, $3 }'

printf '\nWrote coverage profiles to %s\n' "$profile_dir"
exit "$status"
