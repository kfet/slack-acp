#!/usr/bin/env bash
# Enforce 100% statement coverage on every Go package that ships test
# files. Packages with no *_test.go are skipped (they would report 0.0%
# under `go test -cover`, which is meaningless for an entry-point /
# wiring package like cmd/slack-acp).
#
# Fails on the first package below 100%, or when the suite fails.

set -euo pipefail

cd "$(dirname "$0")/.."

mkdir -p bin
PROFILE=bin/coverage.out

# Build the list of packages that have at least one *_test.go file.
mapfile -t PKGS < <(go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./...)

if [[ ${#PKGS[@]} -eq 0 ]]; then
  echo "coverage: no packages with tests"
  exit 1
fi

# Run the suite once with a combined profile so per-package totals are
# self-consistent.
go test -coverprofile="$PROFILE" -covermode=set "${PKGS[@]}" >/dev/null

# Compute per-package coverage straight from the profile. Each non-mode
# line is "<file>:<startLine>.<startCol>,<endLine>.<endCol> <numStmts> <count>".
declare -A pkg_stmts pkg_hit
while read -r line; do
  [[ -z "$line" ]] && continue
  [[ "$line" == mode:* ]] && continue
  file="${line%%:*}"
  rest="${line#*:}"            # "startLine.startCol,endLine.endCol numStmts count"
  numstmts="$(awk '{print $2}' <<< "$rest")"
  count="$(awk '{print $3}' <<< "$rest")"
  pkg="$(dirname "$file")"
  pkg_stmts["$pkg"]=$(( ${pkg_stmts["$pkg"]:-0} + numstmts ))
  if [[ "$count" -gt 0 ]]; then
    pkg_hit["$pkg"]=$(( ${pkg_hit["$pkg"]:-0} + numstmts ))
  fi
done < "$PROFILE"

OK=1
FAILED=()
printf "%-60s %s\n" "package" "coverage"
for pkg in $(printf '%s\n' "${!pkg_stmts[@]}" | sort); do
  total=${pkg_stmts["$pkg"]}
  hit=${pkg_hit["$pkg"]:-0}
  pct="$(awk -v h="$hit" -v t="$total" 'BEGIN { printf "%.1f", (t==0?0:100*h/t) }')"
  status=""
  if [[ "$hit" -ne "$total" ]]; then
    status=" ✗"
    OK=0
    FAILED+=("$pkg")
  fi
  printf "%-60s %5s%%%s\n" "$pkg" "$pct" "$status"
done

if [[ "$OK" -ne 1 ]]; then
  echo
  echo "coverage: the following packages are below 100%:"
  for p in "${FAILED[@]}"; do
    echo "  $p"
    # Show only the uncovered functions in this package.
    go tool cover -func="$PROFILE" \
      | awk -v pkg="$p" '
        index($1, pkg)==1 {
          # The last whitespace-separated field is the percentage.
          n = split($0, f, /[ \t]+/);
          if (f[n] != "100.0%") print "    " $0;
        }'
  done
  exit 1
fi
