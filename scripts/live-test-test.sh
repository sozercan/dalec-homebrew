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
  homebrew/image) digest=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd ;;
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
DIGEST_E=sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
DIGEST_F=sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
DIGEST_G=sha256:1111111111111111111111111111111111111111111111111111111111111111
BASE_REF=ghcr.io/example/runtime-base@$DIGEST_A
MATERIALIZER_REF=ghcr.io/example/materializer@$DIGEST_B
FRONTEND_REF=ghcr.io/example/frontend@$DIGEST_C
FRONTEND_INDEX_REF=ghcr.io/example/frontend@$DIGEST_D
DALEC_FRONTEND_REF=ghcr.io/project-dalec/dalec/frontend@$DIGEST_E
DALEC_PIN="$TEST_ROOT/dalec-frontend.json"
cat > "$DALEC_PIN" <<EOF_PIN
{
  "schema_version": "dalec-homebrew-dalec-frontend/v1",
  "module": {
    "path": "github.com/project-dalec/dalec",
    "version": "v0.21.5"
  },
  "index": "$DALEC_FRONTEND_REF",
  "platforms": {
    "linux/amd64": "ghcr.io/project-dalec/dalec/frontend@$DIGEST_F",
    "linux/arm64": "ghcr.io/project-dalec/dalec/frontend@$DIGEST_G"
  },
  "route": "homebrew/image"
}
EOF_PIN
COMMON_ENV=(
  DALEC_HOMEBREW_LIVE_BUILDER=test-builder
  DALEC_HOMEBREW_LIVE_PLATFORM=linux/amd64
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN=$DALEC_PIN"
)
PUBLISHED_ENV=(
  "${COMMON_ENV[@]}"
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF"
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF"
  "DALEC_HOMEBREW_LIVE_FRONTEND_INDEX_REF=$FRONTEND_INDEX_REF"
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

expect_rejected missing-frontend-index "must be set together" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"

expect_rejected scheme-frontend "DALEC_HOMEBREW_LIVE_FRONTEND_REF must be a digest-pinned OCI reference" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_INDEX_REF=$FRONTEND_INDEX_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=https://ghcr.io/example/frontend@$DIGEST_C"

expect_rejected cross-repository-frontend "DALEC_HOMEBREW_LIVE_FRONTEND_INDEX_REF and DALEC_HOMEBREW_LIVE_FRONTEND_REF must use the same repository" \
  "${COMMON_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_INDEX_REF=ghcr.io/other/frontend@$DIGEST_D" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF"

expect_rejected invalid-metadata-not-before "DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE must be a valid RFC3339 timestamp" \
  "${PUBLISHED_ENV[@]}" \
  DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE=2026-02-31T00:00:00Z

expect_rejected partial-dalec-override "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF and DALEC_HOMEBREW_LIVE_TARGET must be set together" \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF=ghcr.io/project-dalec/dalec/frontend@$DIGEST_F"

expect_rejected mutable-dalec-override "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF must be a digest-pinned OCI reference" \
  "${PUBLISHED_ENV[@]}" \
  DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF=ghcr.io/project-dalec/dalec/frontend:latest \
  DALEC_HOMEBREW_LIVE_TARGET=homebrew/image

MUTABLE_DALEC_PIN="$TEST_ROOT/mutable-dalec-frontend.json"
sed 's#@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee#:latest#' \
  "$DALEC_PIN" > "$MUTABLE_DALEC_PIN"
expect_rejected mutable-dalec-index "index must be a digest-pinned OCI reference" \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN=$MUTABLE_DALEC_PIN"

PARTIAL_DALEC_PIN="$TEST_ROOT/partial-dalec-frontend.json"
jq 'del(.platforms["linux/arm64"])' "$DALEC_PIN" > "$PARTIAL_DALEC_PIN"
expect_rejected partial-dalec-platforms 'missing platform "linux/arm64"' \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN=$PARTIAL_DALEC_PIN"

WRONG_ROUTE_PIN="$TEST_ROOT/wrong-route-dalec-frontend.json"
jq '.route = "homebrew/debug"' "$DALEC_PIN" > "$WRONG_ROUTE_PIN"
expect_rejected wrong-dalec-route 'route must be exactly "homebrew/image"' \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN=$WRONG_ROUTE_PIN"

printf 'name: missing-syntax\n' > "$TEST_ROOT/missing-syntax.yaml"
expect_rejected missing-syntax "must start with a # syntax= directive" \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_SPEC=$TEST_ROOT/missing-syntax.yaml"

cat > "$TEST_ROOT/list-runtime.yaml" <<'EOF_SPEC'
# syntax=example/frontend@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
dependencies:
  runtime: [hello, jq]
EOF_SPEC
expect_rejected list-runtime "dependencies.runtime must use map form" \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_SPEC=$TEST_ROOT/list-runtime.yaml"

cat > "$TEST_ROOT/existing-targets.yaml" <<'EOF_SPEC'
# syntax=example/frontend@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
dependencies:
  runtime:
    hello: {}
targets: {}
EOF_SPEC
expect_rejected existing-targets "live helper reserves targets.homebrew for forwarding" \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_SPEC=$TEST_ROOT/existing-targets.yaml"

published_output="$TEST_ROOT/published.out"
CAPTURED_SPEC="$TEST_ROOT/published-spec.yaml"
run_live_test "$published_output" \
  CAPTURED_SPEC="$CAPTURED_SPEC" \
  "${PUBLISHED_ENV[@]}" \
  DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE=2026-06-01T00:00:00Z
[[ $(grep -c '^buildx build ' "$DOCKER_LOG") -eq 1 ]] || fail "published mode did not build only the final image"
assert_contains "$DOCKER_LOG" "--target homebrew/image"
for argument in \
  "DALEC_HOMEBREW_RUNTIME_BASE=$BASE_REF" \
  "DALEC_HOMEBREW_MATERIALIZER=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$FRONTEND_INDEX_REF" \
  "DALEC_HOMEBREW_FRONTEND_REF=$FRONTEND_REF" \
  "DALEC_HOMEBREW_METADATA_NOT_BEFORE=2026-06-01T00:00:00Z"; do
  assert_contains "$DOCKER_LOG" "--build-arg $argument"
done
[[ $(head -n 1 "$CAPTURED_SPEC") == "# syntax=$DALEC_FRONTEND_REF" ]] || fail "upstream Dalec frontend reference was not written to the live spec"
EXPECTED_SPEC="$TEST_ROOT/expected-published-spec.yaml"
{
  printf '# syntax=%s\n' "$DALEC_FRONTEND_REF"
  tail -n +2 "$ROOT/examples/live-test.yaml"
  printf '\ntargets:\n'
  printf '  homebrew:\n'
  printf '    frontend:\n'
  printf '      image: %s\n' "$FRONTEND_REF"
} > "$EXPECTED_SPEC"
diff -u "$EXPECTED_SPEC" "$CAPTURED_SPEC"
assert_not_contains "$CAPTURED_SPEC" "x-dalec-homebrew"
assert_not_contains "$CAPTURED_SPEC" "runtime_dependency_order"
for result in \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_INDEX_REF=$FRONTEND_INDEX_REF" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF" \
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF=$DALEC_FRONTEND_REF" \
  "DALEC_HOMEBREW_LIVE_TARGET=homebrew/image" \
  "DALEC_HOMEBREW_LIVE_DALEC_ROUTE=homebrew/image" \
  "DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE=2026-06-01T00:00:00Z" \
  "DALEC_HOMEBREW_LIVE_FINAL_REF=dalec-homebrew-live@$DIGEST_D"; do
  assert_contains "$published_output" "$result"
done

explicit_output="$TEST_ROOT/explicit-parent.out"
explicit_spec="$TEST_ROOT/explicit-parent-spec.yaml"
run_live_test "$explicit_output" \
  CAPTURED_SPEC="$explicit_spec" \
  "${PUBLISHED_ENV[@]}" \
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF=ghcr.io/project-dalec/dalec/frontend@$DIGEST_F" \
  DALEC_HOMEBREW_LIVE_TARGET=homebrew/image
[[ $(head -n 1 "$explicit_spec") == "# syntax=ghcr.io/project-dalec/dalec/frontend@$DIGEST_F" ]] || fail "release-bound Dalec platform child override was not written to the live spec"
assert_contains "$explicit_output" "DALEC_HOMEBREW_LIVE_TARGET=homebrew/image"

rebuild_output="$TEST_ROOT/rebuild.out"
run_live_test "$rebuild_output" \
  DALEC_HOMEBREW_LIVE_BUILDER=test-builder \
  DALEC_HOMEBREW_LIVE_REGISTRY=registry.example \
  DALEC_HOMEBREW_LIVE_PLATFORM=linux/arm64 \
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN=$DALEC_PIN" \
  DALEC_HOMEBREW_LIVE_RUN_ID=test-run \
  DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH=1700000000
[[ $(grep -c '^buildx build ' "$DOCKER_LOG") -eq 4 ]] || fail "rebuild mode did not retain all four builds"
for target in runtime-base materializer frontend; do
  assert_contains "$DOCKER_LOG" "--target $target"
done
assert_contains "$DOCKER_LOG" "--target homebrew/image"
[[ $(grep -Fc -- "--build-arg SOURCE_DATE_EPOCH=1700000000" "$DOCKER_LOG") -eq 3 ]] || fail "rebuild mode did not use one deterministic source date epoch for all components"
for argument in \
  "DALEC_HOMEBREW_RUNTIME_BASE=registry.example/dalec-homebrew-runtime-base@$DIGEST_A" \
  "DALEC_HOMEBREW_MATERIALIZER=registry.example/dalec-homebrew-materializer@$DIGEST_B" \
  "DALEC_HOMEBREW_FRONTEND_REF=registry.example/dalec-homebrew@$DIGEST_C"; do
  assert_contains "$DOCKER_LOG" "--build-arg $argument"
done
assert_not_contains "$DOCKER_LOG" "--build-arg DALEC_HOMEBREW_METADATA_NOT_BEFORE="
assert_not_contains "$DOCKER_LOG" "--build-arg DALEC_HOMEBREW_FRONTEND_INDEX_REF="
for result in \
  "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=registry.example/dalec-homebrew-runtime-base@$DIGEST_A" \
  "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=registry.example/dalec-homebrew-materializer@$DIGEST_B" \
  "DALEC_HOMEBREW_LIVE_FRONTEND_REF=registry.example/dalec-homebrew@$DIGEST_C" \
  "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF=$DALEC_FRONTEND_REF" \
  "DALEC_HOMEBREW_LIVE_TARGET=homebrew/image" \
  "DALEC_HOMEBREW_LIVE_DALEC_ROUTE=homebrew/image"; do
  assert_contains "$rebuild_output" "$result"
done

echo "live-test shell tests passed"
