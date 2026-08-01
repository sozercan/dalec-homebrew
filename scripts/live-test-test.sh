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
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >> "$DOCKER_LOG"
if [[ "$1 $2" == "buildx inspect" ]]; then
  exit 0
fi
if [[ "$1 $2" != "buildx build" ]]; then
  echo "unexpected docker command: $*" >&2
  exit 1
fi

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
    *)
      shift
      ;;
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
DIGEST_A=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DIGEST_B=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
DIGEST_C=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
BASE_REF=ghcr.io/example/runtime-base@$DIGEST_A
MATERIALIZER_REF=ghcr.io/example/materializer@$DIGEST_B
FRONTEND_REF=ghcr.io/example/frontend@$DIGEST_C

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
  (
    unset \
      DALEC_HOMEBREW_LIVE_BUILDER \
      DALEC_HOMEBREW_LIVE_REGISTRY \
      DALEC_HOMEBREW_LIVE_PLATFORM \
      DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF \
      DALEC_HOMEBREW_LIVE_MATERIALIZER_REF \
      DALEC_HOMEBREW_LIVE_FRONTEND_REF \
      DALEC_HOMEBREW_LIVE_RUN_ID \
      DALEC_HOMEBREW_LIVE_SPEC \
      DALEC_HOMEBREW_LIVE_IMAGE \
      DALEC_HOMEBREW_LIVE_OUTPUT \
      DALEC_HOMEBREW_LIVE_PROGRESS \
      DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH \
      DALEC_HOMEBREW_LIVE_UBUNTU_BASE
    export PATH="$FAKE_BIN:$ORIGINAL_PATH"
    export TMPDIR="$TEST_ROOT"
    export DOCKER_LOG="$TEST_ROOT/docker.log"
    : > "$DOCKER_LOG"
    for assignment in "$@"; do
      export "${assignment?}"
    done
    "$LIVE_TEST"
  ) > "$output" 2>&1
}

partial_output="$TEST_ROOT/partial.out"
if run_live_test "$partial_output" \
  DALEC_HOMEBREW_LIVE_BUILDER=test-builder \
  DALEC_HOMEBREW_LIVE_REGISTRY=registry.example \
  DALEC_HOMEBREW_LIVE_PLATFORM=linux/amd64 \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF"; then
  fail "a partial component tuple was accepted"
else
  status=$?
fi
[[ $status -eq 64 ]] || fail "partial tuple exited with $status instead of 64"
assert_contains "$partial_output" "must be set together"

mutable_output="$TEST_ROOT/mutable.out"
if run_live_test "$mutable_output" \
  DALEC_HOMEBREW_LIVE_BUILDER=test-builder \
  DALEC_HOMEBREW_LIVE_PLATFORM=linux/amd64 \
  DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=ghcr.io/example/runtime-base:latest \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"; then
  fail "a mutable component reference was accepted"
else
  status=$?
fi
[[ $status -eq 64 ]] || fail "mutable reference exited with $status instead of 64"
assert_contains "$mutable_output" "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF must be a digest-pinned OCI reference"

published_output="$TEST_ROOT/published.out"
CAPTURED_SPEC="$TEST_ROOT/published-spec.yaml"
export CAPTURED_SPEC
run_live_test "$published_output" \
  DALEC_HOMEBREW_LIVE_BUILDER=test-builder \
  DALEC_HOMEBREW_LIVE_PLATFORM=linux/amd64 \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"
[[ $(grep -c '^buildx build ' "$TEST_ROOT/docker.log") -eq 1 ]] || fail "published mode did not build only the final image"
assert_not_contains "$TEST_ROOT/docker.log" "--target runtime-base"
assert_not_contains "$TEST_ROOT/docker.log" "--target materializer"
assert_not_contains "$TEST_ROOT/docker.log" "--target frontend"
assert_contains "$TEST_ROOT/docker.log" "--build-arg DALEC_HOMEBREW_RUNTIME_BASE=$BASE_REF"
assert_contains "$TEST_ROOT/docker.log" "--build-arg DALEC_HOMEBREW_MATERIALIZER=$MATERIALIZER_REF"
assert_contains "$TEST_ROOT/docker.log" "--build-arg DALEC_HOMEBREW_FRONTEND_REF=$FRONTEND_REF"
[[ $(head -n 1 "$CAPTURED_SPEC") == "# syntax=$FRONTEND_REF" ]] || fail "published frontend reference was not written to the live spec"
assert_contains "$published_output" "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF"
assert_contains "$published_output" "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF"
assert_contains "$published_output" "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"

rebuild_output="$TEST_ROOT/rebuild.out"
unset CAPTURED_SPEC
run_live_test "$rebuild_output" \
  DALEC_HOMEBREW_LIVE_BUILDER=test-builder \
  DALEC_HOMEBREW_LIVE_REGISTRY=registry.example \
  DALEC_HOMEBREW_LIVE_PLATFORM=linux/arm64 \
  DALEC_HOMEBREW_LIVE_RUN_ID=test-run
[[ $(grep -c '^buildx build ' "$TEST_ROOT/docker.log") -eq 4 ]] || fail "rebuild mode did not retain all four builds"
assert_contains "$TEST_ROOT/docker.log" "--target runtime-base"
assert_contains "$TEST_ROOT/docker.log" "--target materializer"
assert_contains "$TEST_ROOT/docker.log" "--target frontend"
assert_not_contains "$TEST_ROOT/docker.log" "--build-arg DALEC_HOMEBREW_RUNTIME_BASE="
assert_contains "$rebuild_output" "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=registry.example/dalec-homebrew-runtime-base@$DIGEST_A"
assert_contains "$rebuild_output" "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=registry.example/dalec-homebrew-materializer@$DIGEST_B"
assert_contains "$rebuild_output" "DALEC_HOMEBREW_LIVE_FRONTEND_REF=registry.example/dalec-homebrew@$DIGEST_C"

echo "live-test shell tests passed"
