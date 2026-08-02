#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

BUILDER=${DALEC_HOMEBREW_LIVE_BUILDER:-}
REGISTRY=${DALEC_HOMEBREW_LIVE_REGISTRY:-}
PLATFORM=${DALEC_HOMEBREW_LIVE_PLATFORM:-}
SPEC=${DALEC_HOMEBREW_LIVE_SPEC:-examples/live-test.yaml}
FINAL_IMAGE=${DALEC_HOMEBREW_LIVE_IMAGE:-dalec-homebrew-live:dev}
FINAL_OUTPUT=${DALEC_HOMEBREW_LIVE_OUTPUT:-load}
PROGRESS=${DALEC_HOMEBREW_LIVE_PROGRESS:-plain}
SOURCE_DATE_EPOCH=${DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH:-1781049600}
METADATA_NOT_BEFORE=${DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE:-}
BASE_REF=${DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF:-}
MATERIALIZER_REF=${DALEC_HOMEBREW_LIVE_MATERIALIZER_REF:-}
FRONTEND_REF=${DALEC_HOMEBREW_LIVE_FRONTEND_REF:-}

fail_usage() {
  echo "$*" >&2
  exit 64
}

USE_PUBLISHED_COMPONENTS=0
if [[ -n "$BASE_REF" || -n "$MATERIALIZER_REF" || -n "$FRONTEND_REF" ]]; then
  USE_PUBLISHED_COMPONENTS=1
fi
if (( USE_PUBLISHED_COMPONENTS == 1 )) || [[ -n "$METADATA_NOT_BEFORE" ]]; then
  command -v go >/dev/null 2>&1 ||
    fail_usage "go is required to validate published component references and metadata timestamps"
  if ! GOWORK=off GOFLAGS='' go run ./cmd/live-input-verify \
    --runtime-base-ref "$BASE_REF" \
    --materializer-ref "$MATERIALIZER_REF" \
    --frontend-ref "$FRONTEND_REF" \
    --metadata-not-before "$METADATA_NOT_BEFORE"; then
    exit 64
  fi
fi

if [[ -z "$BUILDER" || -z "$PLATFORM" || ( "$USE_PUBLISHED_COMPONENTS" -eq 0 && -z "$REGISTRY" ) ]]; then
  cat >&2 <<'USAGE'
usage: rebuild components:
       DALEC_HOMEBREW_LIVE_BUILDER=<buildx-builder> \
         DALEC_HOMEBREW_LIVE_REGISTRY=<registry-host> \
         DALEC_HOMEBREW_LIVE_PLATFORM=linux/<amd64|arm64> \
         ./scripts/live-test.sh

       test a published component tuple:
       DALEC_HOMEBREW_LIVE_BUILDER=<buildx-builder> \
         DALEC_HOMEBREW_LIVE_PLATFORM=linux/<amd64|arm64> \
         DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=<image@sha256:digest> \
         DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=<image@sha256:digest> \
         DALEC_HOMEBREW_LIVE_FRONTEND_REF=<image@sha256:digest> \
         ./scripts/live-test.sh

Optional settings for either mode:
       DALEC_HOMEBREW_LIVE_SPEC=examples/live-test.yaml
       DALEC_HOMEBREW_LIVE_IMAGE=dalec-homebrew-live:dev
       DALEC_HOMEBREW_LIVE_OUTPUT=load|push
       DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE=<RFC3339 timestamp>

The registry must be reachable from the selected builder and configured as an
insecure HTTP registry when appropriate when rebuilding components. Component
and frontend references are always consumed by digest even though temporary
tags are used for publication.
USAGE
  exit 64
fi

case "$FINAL_OUTPUT" in
  load) FINAL_OUTPUT_FLAG=--load ;;
  push) FINAL_OUTPUT_FLAG=--push ;;
  *) fail_usage "unsupported DALEC_HOMEBREW_LIVE_OUTPUT: $FINAL_OUTPUT (expected load or push)" ;;
esac

repository_from_ref() {
  local ref=${1%@*}
  if [[ "${ref##*/}" == *:* ]]; then
    ref=${ref%:*}
  fi
  printf '%s\n' "$ref"
}

case "$PLATFORM" in
  linux/amd64)
    ARCH=amd64
    RUNTIME_BASE_DEFAULT='docker.io/library/ubuntu@sha256:52df9b1ee71626e0088f7d400d5c6b5f7bb916f8f0c82b474289a4ece6cf3faf'
    ;;
  linux/arm64)
    ARCH=arm64
    RUNTIME_BASE_DEFAULT='docker.io/library/ubuntu@sha256:7f622ca8766bccb22f04242ecb6f19f770b2f08827dc4b8c707de5e78a6da7ab'
    ;;
  *) fail_usage "unsupported DALEC_HOMEBREW_LIVE_PLATFORM: $PLATFORM" ;;
esac

SPEC_HEADER=
if ! IFS= read -r SPEC_HEADER < "$SPEC" || [[ "$SPEC_HEADER" != '# syntax='* ]]; then
  fail_usage "DALEC_HOMEBREW_LIVE_SPEC must start with a # syntax= directive"
fi

TMPDIR_ROOT=${TMPDIR:-/tmp}
WORK=$(mktemp -d "$TMPDIR_ROOT/dalec-homebrew-live.XXXXXX")
cleanup() {
  rm -rf "$WORK"
}
trap cleanup EXIT

BUILDX_ARGS=(
  --builder "$BUILDER"
  --platform "$PLATFORM"
  --provenance=false
  --progress="$PROGRESS"
)

read_digest() {
  jq -er '."containerimage.digest" | select(type == "string" and test("^sha256:[0-9a-f]{64}$"))' "$1"
}

build_component() {
  local output_var=$1
  local label=$2
  local target=$3
  local repository=$4
  shift 4
  local metadata_file="$WORK/$target.json"
  local digest

  echo "==> Building $label for $PLATFORM"
  docker buildx build \
    "${BUILDX_ARGS[@]}" \
    --target "$target" \
    --build-arg "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH" \
    "$@" \
    --tag "$REGISTRY/$repository:$RUN_ID" \
    --metadata-file "$metadata_file" \
    --push \
    .
  digest=$(read_digest "$metadata_file")
  printf -v "$output_var" '%s@%s' "$REGISTRY/$repository" "$digest"
}

docker buildx inspect "$BUILDER" >/dev/null

if (( USE_PUBLISHED_COMPONENTS == 0 )); then
  RUNTIME_BASE=${DALEC_HOMEBREW_LIVE_UBUNTU_BASE:-$RUNTIME_BASE_DEFAULT}
  RUN_ID=${DALEC_HOMEBREW_LIVE_RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$ARCH}

  build_component BASE_REF "runtime base" runtime-base dalec-homebrew-runtime-base \
    --build-arg "RUNTIME_BASE=$RUNTIME_BASE"
  build_component MATERIALIZER_REF materializer materializer dalec-homebrew-materializer \
    --build-arg "RUNTIME_BASE=$RUNTIME_BASE"
  build_component FRONTEND_REF "gateway frontend" frontend dalec-homebrew \
    --build-arg "RUNTIME_BASE_REF=$BASE_REF" \
    --build-arg "MATERIALIZER_REF=$MATERIALIZER_REF"
else
  echo "==> Using published component tuple for $PLATFORM"
fi

{
  printf '# syntax=%s\n' "$FRONTEND_REF"
  tail -n +2 "$SPEC"
} > "$WORK/spec.yaml"

FINAL_BUILD_ARGS=(
  --build-arg "DALEC_HOMEBREW_RUNTIME_BASE=$BASE_REF"
  --build-arg "DALEC_HOMEBREW_MATERIALIZER=$MATERIALIZER_REF"
  --build-arg "DALEC_HOMEBREW_FRONTEND_REF=$FRONTEND_REF"
)
if [[ -n "$METADATA_NOT_BEFORE" ]]; then
  FINAL_BUILD_ARGS+=(--build-arg "DALEC_HOMEBREW_METADATA_NOT_BEFORE=$METADATA_NOT_BEFORE")
fi

echo "==> Building final runtime image $FINAL_IMAGE"
docker buildx build \
  "${BUILDX_ARGS[@]}" \
  --file "$WORK/spec.yaml" \
  --tag "$FINAL_IMAGE" \
  --metadata-file "$WORK/final.json" \
  "${FINAL_BUILD_ARGS[@]}" \
  "$FINAL_OUTPUT_FLAG" \
  .
FINAL_DIGEST=$(read_digest "$WORK/final.json")
FINAL_REF="$(repository_from_ref "$FINAL_IMAGE")@$FINAL_DIGEST"

cat <<RESULT
DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF
DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF
DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF
DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE=$METADATA_NOT_BEFORE
DALEC_HOMEBREW_LIVE_FINAL_IMAGE=$FINAL_IMAGE
DALEC_HOMEBREW_LIVE_FINAL_DIGEST=$FINAL_DIGEST
DALEC_HOMEBREW_LIVE_FINAL_REF=$FINAL_REF
RESULT
