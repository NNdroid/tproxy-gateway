#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/tproxy-version.XXXXXX")"
# Delete only the directory created by mktemp, using this shell end-to-end.
[[ "$SCRATCH" == "${TMPDIR:-/tmp}/tproxy-version."* ]]
trap 'rm -rf -- "$SCRATCH"' EXIT
REPO="$SCRATCH/project with spaces"
mkdir -p "$REPO/scripts"
cp "$PROJECT_ROOT/scripts/version.sh" "$PROJECT_ROOT/scripts/build.sh" "$REPO/scripts/"
git init -q -b main "$REPO"
git -C "$REPO" config user.name 'Version Test'
git -C "$REPO" config user.email 'version-test@example.invalid'

commit() {
    GIT_AUTHOR_DATE="$1" GIT_COMMITTER_DATE="$1" git -C "$REPO" commit -q --allow-empty -m "$2"
}
commit '2020-01-01T00:30:00+1400' first
HASH="$(git -C "$REPO" rev-parse HEAD)"
EXPECTED="v1.0.20191231.1-${HASH:0:7}"
test "$(bash "$REPO/scripts/version.sh")" = "$EXPECTED"
test "$(TZ=Pacific/Honolulu bash "$REPO/scripts/version.sh")" = "$EXPECTED"
test "$(TZ=Asia/Shanghai bash "$REPO/scripts/version.sh" "$EXPECTED")" = "$EXPECTED"
if bash "$REPO/scripts/version.sh" 'v1.0.20200101' > "$SCRATCH/output" 2> "$SCRATCH/error"; then
    echo 'Mismatched version was accepted' >&2; exit 1
fi
test ! -s "$SCRATCH/output"
if bash "$REPO/scripts/version.sh" "$EXPECTED" extra > /dev/null 2>&1; then
    echo 'Extra version arguments were accepted' >&2; exit 1
fi

git -C "$REPO" switch -q -c feature
commit '2020-01-01T12:00:00Z' feature
git -C "$REPO" switch -q main
commit '2020-01-02T12:00:00Z' main
GIT_AUTHOR_DATE='2020-01-03T00:30:00+1400' GIT_COMMITTER_DATE='2020-01-03T00:30:00+1400' \
    git -C "$REPO" merge -q --no-ff feature -m merge
HASH="$(git -C "$REPO" rev-parse HEAD)"
EXPECTED="v1.0.20200102.4-${HASH:0:7}"
git -C "$REPO" tag -a "$EXPECTED" -m version
git -C "$REPO" checkout -q --detach "$EXPECTED"
test "$(bash "$REPO/scripts/version.sh")" = "$EXPECTED"

git clone -q --no-local --depth 1 "$REPO" "$SCRATCH/shallow"
mkdir -p "$SCRATCH/shallow/scripts"
cp "$PROJECT_ROOT/scripts/version.sh" "$SCRATCH/shallow/scripts/"
if bash "$SCRATCH/shallow/scripts/version.sh" > "$SCRATCH/output" 2> "$SCRATCH/error"; then
    echo 'Shallow history was accepted' >&2; exit 1
fi
test ! -s "$SCRATCH/output"
grep -q 'git fetch --unshallow' "$SCRATCH/error"

# A fake Go executable checks build argument/environment wiring without a cross compiler.
# CI separately builds the real gateway and executes its --version flag.
mkdir -p "$SCRATCH/fakebin"
export EXPECTED FAKE_GO_LOG="$SCRATCH/go-calls"
cat > "$SCRATCH/fakebin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
test "$GO111MODULE" = on
test "$CGO_ENABLED" = 0
test "$GOOS" = linux
test "$1" = build
test "$2" = -trimpath
test "$3" = "-ldflags=-s -w -X main.version=$EXPECTED"
test "$4" = -o
test "$6" = .
test "$PWD/bin/tproxy-gateway-$GOOS-$GOARCH${GOAMD64:+-$GOAMD64}" = "$5"
printf '%s\n' "$GOARCH/${GOAMD64:-}" >> "$FAKE_GO_LOG"
printf 'test-only build placeholder\n' > "$5"
EOF
chmod +x "$SCRATCH/fakebin/go"
PATH="$SCRATCH/fakebin:$PATH" GO111MODULE=off bash "$REPO/scripts/build.sh" "$EXPECTED" > "$SCRATCH/build-log"
test "$(wc -l < "$FAKE_GO_LOG" | tr -d '[:space:]')" = 10
test "$(find "$REPO/bin" -type f | wc -l | tr -d '[:space:]')" = 10
# Validation must fail before deleting previous binaries.
printf 'preserve\n' > "$REPO/bin/preserve"
if PATH="$SCRATCH/fakebin:$PATH" bash "$REPO/scripts/build.sh" wrong > /dev/null 2>&1; then
    echo 'Build accepted a mismatched version' >&2; exit 1
fi
test "$(cat "$REPO/bin/preserve")" = preserve
echo 'Version tests passed: UTC, full/merged history, detached tags, shallow rejection and build wiring.'
