#!/usr/bin/env bash
set -euo pipefail

# One source of truth for local builds, CI and release tags.
PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"
if (( $# > 1 )); then
    echo "Usage: bash scripts/version.sh [expected-version]" >&2
    exit 1
fi
if [[ "$(git rev-parse --is-shallow-repository)" == "true" ]]; then
    echo "Version generation requires complete Git history. Run git fetch --unshallow first." >&2
    exit 1
fi

COMMIT="$(git rev-parse --verify HEAD)"
COUNT="$(git rev-list --count "$COMMIT")"
# Committer date in UTC makes rebuilding the same commit independent of build time/zone.
COMMIT_DATE="$(TZ=UTC git show -s --date=format-local:%Y%m%d --format=%cd "$COMMIT")"
VERSION="v1.0.${COMMIT_DATE}.${COUNT}-${COMMIT:0:7}"
if [[ -n "${1:-}" && "$1" != "$VERSION" ]]; then
    echo "Requested version '$1' does not match this commit: $VERSION" >&2
    exit 1
fi
printf '%s\n' "$VERSION"
