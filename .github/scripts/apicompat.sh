#!/usr/bin/env bash
# apicompat.sh [NEXT]: the module's public API at HEAD against the latest
# release (the highest vX.Y.Z tag, or $API_BASE), compared with apidiff.
# Internal packages are not compared.
#
# An incompatible change fails, unless NEXT (the version HEAD is to be
# released as) is a major bump over that release, or, while the major
# version is 0, a minor one. Without NEXT, .github/api-next names it when
# it holds a version newer than the latest release (one already released
# is stale, and ignored).
#
# Run it from the module's root, with every tag fetched.
set -euo pipefail

# apidiff, pinned: golang.org/x/exp has no tagged releases.
APIDIFF=golang.org/x/exp/cmd/apidiff@v0.0.0-20261109185158-a52e3aef5920

export GOWORK=off
semver='^v[0-9]+\.[0-9]+\.[0-9]+$'
base=${API_BASE:-$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | grep -E "$semver" | head -1 || true)}
if [ -z "$base" ]; then
  echo "apicompat: no release tag to compare with"
  exit 0
fi

# newer A B: version A is newer than B.
newer() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$1" ]; }

next=${1:-}
if [ -z "$next" ] && [ -f .github/api-next ]; then
  declared=$(tr -d '[:space:]' < .github/api-next)
  if [[ "$declared" =~ $semver ]] && newer "$declared" "$base"; then
    next=$declared
  elif [ -n "$declared" ]; then
    echo "apicompat: .github/api-next names $declared, not newer than $base: ignored"
  fi
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
GOBIN="$work/bin" go install "$APIDIFF"
apidiff="$work/bin/apidiff"
module=$(go list -m)

mkdir "$work/base"
git archive "$base" | tar -x -C "$work/base"
(cd "$work/base" && "$apidiff" -m -w "$work/base.api" "$module") 2>/dev/null
"$apidiff" -m -w "$work/head.api" "$module" 2>/dev/null
"$apidiff" -m "$work/base.api" "$work/head.api" 2>/dev/null | tee "$work/report"
broken=$("$apidiff" -m -incompatible "$work/base.api" "$work/head.api" 2>/dev/null || true)

if [ -z "$broken" ]; then
  echo "apicompat: compatible with $base"
  exit 0
fi

IFS=. read -r bma bmi _ <<< "${base#v}"
if [ -n "$next" ]; then
  IFS=. read -r nma nmi _ <<< "${next#v}"
  if [ "$nma" -gt "$bma" ] || { [ "$bma" -eq 0 ] && [ "$nma" -eq 0 ] && [ "$nmi" -gt "$bmi" ]; }; then
    echo "apicompat: incompatible with $base, allowed in $next"
    exit 0
  fi
fi
rule="a major release"
[ "$bma" -eq 0 ] && rule="a minor release (v0.$((bmi + 1)).0) while v0"
echo "::error::incompatible API changes since $base${next:+ for $next}; they need $rule:"
echo "$broken"
exit 1
