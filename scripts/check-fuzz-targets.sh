#!/usr/bin/env bash
# Checks that .clusterfuzzlite/build.sh builds every Go fuzz target, so a
# new target is not silently left out of continuous fuzzing.
set -euo pipefail
cd "$(dirname "$0")/.."
missing=0
while IFS=: read -r file _ line; do
  name=$(sed -E 's/^func (Fuzz[A-Za-z0-9_]+).*/\1/' <<<"$line")
  pkg=$(dirname "$file")
  if ! grep -Eq "^fuzz ${pkg}[[:space:]]+${name}[[:space:]]" .clusterfuzzlite/build.sh; then
    echo "missing from .clusterfuzzlite/build.sh: ${pkg} ${name}"
    missing=1
  fi
done < <(grep -rn --include='*_test.go' -E '^func Fuzz[A-Za-z0-9_]+\(f \*testing\.F\)' internal cmd | sed 's#^\./##')
exit $missing
