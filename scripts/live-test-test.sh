#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
LIVE_TEST="$ROOT/scripts/live-test.sh"
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/dalec-homebrew-live-test.XXXXXX")
cleanup() {
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

FAKE_BIN="$TEST_ROOT/bin"
DOCKER_LOG="$TEST_ROOT/docker.log"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail

printf '%q ' "$@" >> "$DOCKER_LOG"
printf '\n' >> "$DOCKER_LOG"
case "$1 $2" in
  "buildx inspect") exit 0 ;;
  "buildx build") ;;
  *) echo "unexpected docker command: $*" >&2; exit 1 ;;
esac

metadata_file=
target=final
spec_file=
while (( $# > 0 )); do
  case "$1" in
    --metadata-file)
      metadata_file=$2
      shift 2
      ;;
    --target)
      target=$2
      shift 2
      ;;
    --file)
      spec_file=$2
      shift 2
      ;;
    *) shift ;;
  esac
done

case "$target" in
  runtime-base) digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  materializer) digest=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
  frontend) digest=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc ;;
  final) digest=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd ;;
  *) echo "unexpected build target: $target" >&2; exit 1 ;;
esac
printf '{"containerimage.digest":"sha256:%s"}\n' "$digest" > "$metadata_file"
if [[ -n "$spec_file" && -n "${CAPTURED_SPEC:-}" ]]; then
  cp "$spec_file" "$CAPTURED_SPEC"
fi
DOCKER
chmod +x "$FAKE_BIN/docker"

ORIGINAL_PATH=$PATH
ORIGINAL_HOME=${HOME:-}
ORIGINAL_GOCACHE=$(go env GOCACHE)
ORIGINAL_GOMODCACHE=$(go env GOMODCACHE)
DIGEST_A=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DIGEST_B=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
DIGEST_C=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
DIGEST_D=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
BASE_REF=ghcr.io/example/runtime-base@$DIGEST_A
MATERIALIZER_REF=ghcr.io/example/materializer@$DIGEST_B
FRONTEND_REF=ghcr.io/example/frontend@$DIGEST_C
COMMON_ENV=(
  DALEC_HOMEBREW_LIVE_BUILDER=test-builder
  DALEC_HOMEBREW_LIVE_PLATFORM=linux/amd64
)
PUBLISHED_ENV=(
  "${COMMON_ENV[@]}"
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF"
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF"
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"
)

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  local file=$1
  local expected=$2
  grep -Fq -- "$expected" "$file" || fail "$file does not contain: $expected"
}

assert_not_contains() {
  local file=$1
  local unexpected=$2
  if grep -Fq -- "$unexpected" "$file"; then
    fail "$file unexpectedly contains: $unexpected"
  fi
}

run_live_test() {
  local output=$1
  shift
  : > "$DOCKER_LOG"
  env -i \
    PATH="$FAKE_BIN:$ORIGINAL_PATH" \
    HOME="$ORIGINAL_HOME" \
    GOCACHE="$ORIGINAL_GOCACHE" \
    GOMODCACHE="$ORIGINAL_GOMODCACHE" \
    TMPDIR="$TEST_ROOT" \
    DOCKER_LOG="$DOCKER_LOG" \
    "$@" \
    "$LIVE_TEST" > "$output" 2>&1
}

expect_rejected() {
  local name=$1
  local expected=$2
  shift 2
  local output="$TEST_ROOT/$name.out"
  local rc

  if run_live_test "$output" "$@"; then
    fail "$name was accepted"
  else
    rc=$?
  fi
  [[ $rc -eq 64 ]] || fail "$name exited with $rc instead of 64"
  assert_contains "$output" "$expected"
  [[ ! -s "$DOCKER_LOG" ]] || fail "$name invoked Docker before rejecting invalid input"
}

expect_rejected partial-one "must be set together" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF"

expect_rejected partial-two "must be set together" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF"

expect_rejected mutable-base "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF must be a digest-pinned OCI reference" \
  "${COMMON_ENV[@]}" \
  DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=ghcr.io/example/runtime-base:latest \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"

expect_rejected invalid-materializer "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF must be a digest-pinned OCI reference" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=ghcr.io/example/materializer@sha512:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"

expect_rejected malformed-frontend "DALEC_HOMEBREW_LIVE_FRONTEND_REF must be a digest-pinned OCI reference" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=ghcr.io/example/front|end@$DIGEST_C"

expect_rejected scheme-frontend "DALEC_HOMEBREW_LIVE_FRONTEND_REF must be a digest-pinned OCI reference" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=https://ghcr.io/example/frontend@$DIGEST_C"

expect_rejected empty-reference-segment "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF must be a digest-pinned OCI reference" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=ghcr.io//runtime-base@$DIGEST_A" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"

expect_rejected malformed-reference-tag "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF must be a digest-pinned OCI reference" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=ghcr.io/example/materializer:bad:tag@$DIGEST_B" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"

expect_rejected invalid-metadata-not-before "DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE must be a valid RFC3339 timestamp" \
  "${PUBLISHED_ENV[@]}" \
  DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE=2026-02-31T00:00:00Z

printf 'name: missing-syntax\n' > "$TEST_ROOT/missing-syntax.yaml"
expect_rejected missing-syntax "DALEC_HOMEBREW_LIVE_SPEC must start with a # syntax= directive" \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_SPEC=$TEST_ROOT/missing-syntax.yaml"

published_output="$TEST_ROOT/published.out"
CAPTURED_SPEC="$TEST_ROOT/published-spec.yaml"
run_live_test "$published_output" \
  CAPTURED_SPEC="$CAPTURED_SPEC" \
  "${PUBLISHED_ENV[@]}" \
  DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE=2026-06-01T00:00:00Z
[[ $(grep -c '^buildx build ' "$DOCKER_LOG") -eq 1 ]] || fail "published mode did not build only the final image"
assert_not_contains "$DOCKER_LOG" "--target "
for argument in \
  "DALEC_HOMEBREW_RUNTIME_BASE=$BASE_REF" \
  "DALEC_HOMEBREW_MATERIALIZER=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_FRONTEND_REF=$FRONTEND_REF" \
  "DALEC_HOMEBREW_METADATA_NOT_BEFORE=2026-06-01T00:00:00Z"; do
  assert_contains "$DOCKER_LOG" "--build-arg $argument"
done
[[ $(head -n 1 "$CAPTURED_SPEC") == "# syntax=$FRONTEND_REF" ]] || fail "published frontend reference was not written to the live spec"
diff -u <(tail -n +2 "$ROOT/examples/live-test.yaml") <(tail -n +2 "$CAPTURED_SPEC")
for result in \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF" \
  "DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE=2026-06-01T00:00:00Z" \
  "DALEC_HOMEBREW_LIVE_FINAL_REF=dalec-homebrew-live@$DIGEST_D"; do
  assert_contains "$published_output" "$result"
done

rebuild_output="$TEST_ROOT/rebuild.out"
run_live_test "$rebuild_output" \
  DALEC_HOMEBREW_LIVE_BUILDER=test-builder \
  DALEC_HOMEBREW_LIVE_REGISTRY=registry.example \
  DALEC_HOMEBREW_LIVE_PLATFORM=linux/arm64 \
  DALEC_HOMEBREW_LIVE_RUN_ID=test-run \
  DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH=1700000000
[[ $(grep -c '^buildx build ' "$DOCKER_LOG") -eq 4 ]] || fail "rebuild mode did not retain all four builds"
for target in runtime-base materializer frontend; do
  assert_contains "$DOCKER_LOG" "--target $target"
done
[[ $(grep -Fc -- "--build-arg SOURCE_DATE_EPOCH=1700000000" "$DOCKER_LOG") -eq 3 ]] || fail "rebuild mode did not use one deterministic source date epoch for all components"
for argument in \
  "DALEC_HOMEBREW_RUNTIME_BASE=registry.example/dalec-homebrew-runtime-base@$DIGEST_A" \
  "DALEC_HOMEBREW_MATERIALIZER=registry.example/dalec-homebrew-materializer@$DIGEST_B" \
  "DALEC_HOMEBREW_FRONTEND_REF=registry.example/dalec-homebrew@$DIGEST_C"; do
  assert_contains "$DOCKER_LOG" "--build-arg $argument"
done
assert_not_contains "$DOCKER_LOG" "--build-arg DALEC_HOMEBREW_METADATA_NOT_BEFORE="
for result in \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=registry.example/dalec-homebrew-runtime-base@$DIGEST_A" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=registry.example/dalec-homebrew-materializer@$DIGEST_B" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=registry.example/dalec-homebrew@$DIGEST_C"; do
  assert_contains "$rebuild_output" "$result"
done

echo "live-test shell tests passed"
