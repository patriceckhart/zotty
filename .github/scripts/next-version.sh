#!/usr/bin/env bash
# Prints the tag that follows the given vX.Y.Z tag (or v0.0.0 when empty):
# the patch number counts up to 99, then rolls over to the next minor.
#   v0.0.1 -> v0.0.2, v0.0.99 -> v0.1.0, v0.1.99 -> v0.2.0
set -euo pipefail

latest="${1:-}"
[ -n "$latest" ] || latest="v0.0.0"
if ! [[ "$latest" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  echo "not a vX.Y.Z tag: $latest" >&2
  exit 1
fi
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"
# Force base 10 so a value like 08 is not read as octal.
if [ "$((10#$patch))" -ge 99 ]; then
  minor=$((10#$minor + 1))
  patch=0
else
  patch=$((10#$patch + 1))
fi
echo "v$((10#$major)).${minor}.${patch}"
