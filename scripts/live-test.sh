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

if [[ -z "$BUILDER" || -z "$REGISTRY" || -z "$PLATFORM" ]]; then
  cat >&2 <<'USAGE'
usage: DALEC_HOMEBREW_LIVE_BUILDER=<buildx-builder> \
       DALEC_HOMEBREW_LIVE_REGISTRY=<registry-host> \
       DALEC_HOMEBREW_LIVE_PLATFORM=linux/<amd64|arm64> \
       [DALEC_HOMEBREW_LIVE_SPEC=examples/live-test.yaml] \
       [DALEC_HOMEBREW_LIVE_IMAGE=dalec-homebrew-live:dev] \
       [DALEC_HOMEBREW_LIVE_OUTPUT=load|push] \
       ./scripts/live-test.sh

The registry must be reachable from the selected builder and configured as an
insecure HTTP registry when appropriate. Component and frontend references are
always consumed by digest even though temporary tags are used for publication.
USAGE
  exit 64
fi

case "$FINAL_OUTPUT" in
  load) FINAL_OUTPUT_FLAG=--load ;;
  push) FINAL_OUTPUT_FLAG=--push ;;
  *)
    echo "unsupported DALEC_HOMEBREW_LIVE_OUTPUT: $FINAL_OUTPUT (expected load or push)" >&2
    exit 64
    ;;
esac

repository_from_ref() {
  local ref=${1%@*}
  local last=${ref##*/}
  if [[ "$last" == *:* ]]; then
    printf '%s\n' "${ref%:*}"
  else
    printf '%s\n' "$ref"
  fi
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
  *)
    echo "unsupported DALEC_HOMEBREW_LIVE_PLATFORM: $PLATFORM" >&2
    exit 64
    ;;
esac

RUNTIME_BASE=${DALEC_HOMEBREW_LIVE_UBUNTU_BASE:-$RUNTIME_BASE_DEFAULT}
RUN_ID=${DALEC_HOMEBREW_LIVE_RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$ARCH}
TMPDIR_ROOT=${TMPDIR:-/tmp}
WORK=$(mktemp -d "$TMPDIR_ROOT/dalec-homebrew-live.XXXXXX")
cleanup() {
  rm -f "$WORK/base.json" "$WORK/materializer.json" "$WORK/frontend.json" "$WORK/final.json" "$WORK/spec.yaml"
  rmdir "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

docker buildx inspect "$BUILDER" >/dev/null

echo "==> Building runtime base for $PLATFORM"
docker buildx build \
  --builder "$BUILDER" \
  --platform "$PLATFORM" \
  --target runtime-base \
  --build-arg "RUNTIME_BASE=$RUNTIME_BASE" \
  --build-arg "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH" \
  --tag "$REGISTRY/dalec-homebrew-runtime-base:$RUN_ID" \
  --metadata-file "$WORK/base.json" \
  --provenance=false \
  --progress="$PROGRESS" \
  --push \
  .
BASE_REF="$REGISTRY/dalec-homebrew-runtime-base@$(jq -er '."containerimage.digest"' "$WORK/base.json")"

echo "==> Building materializer for $PLATFORM"
docker buildx build \
  --builder "$BUILDER" \
  --platform "$PLATFORM" \
  --target materializer \
  --build-arg "RUNTIME_BASE=$RUNTIME_BASE" \
  --build-arg "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH" \
  --tag "$REGISTRY/dalec-homebrew-materializer:$RUN_ID" \
  --metadata-file "$WORK/materializer.json" \
  --provenance=false \
  --progress="$PROGRESS" \
  --push \
  .
MATERIALIZER_REF="$REGISTRY/dalec-homebrew-materializer@$(jq -er '."containerimage.digest"' "$WORK/materializer.json")"

echo "==> Building gateway frontend for $PLATFORM"
docker buildx build \
  --builder "$BUILDER" \
  --platform "$PLATFORM" \
  --target frontend \
  --build-arg "RUNTIME_BASE_REF=$BASE_REF" \
  --build-arg "MATERIALIZER_REF=$MATERIALIZER_REF" \
  --build-arg "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH" \
  --tag "$REGISTRY/dalec-homebrew:$RUN_ID" \
  --metadata-file "$WORK/frontend.json" \
  --provenance=false \
  --progress="$PROGRESS" \
  --push \
  .
FRONTEND_REF="$REGISTRY/dalec-homebrew@$(jq -er '."containerimage.digest"' "$WORK/frontend.json")"

sed "1s|.*|# syntax=$FRONTEND_REF|" "$SPEC" > "$WORK/spec.yaml"

echo "==> Building final runtime image $FINAL_IMAGE"
docker buildx build \
  --builder "$BUILDER" \
  --platform "$PLATFORM" \
  --file "$WORK/spec.yaml" \
  --tag "$FINAL_IMAGE" \
  --metadata-file "$WORK/final.json" \
  --provenance=false \
  --progress="$PROGRESS" \
  "$FINAL_OUTPUT_FLAG" \
  .
FINAL_DIGEST=$(jq -er '."containerimage.digest"' "$WORK/final.json")
FINAL_REF="$(repository_from_ref "$FINAL_IMAGE")@$FINAL_DIGEST"

cat <<RESULT
DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF=$BASE_REF
DALEC_HOMEBREW_LIVE_MATERIALIZER_REF=$MATERIALIZER_REF
DALEC_HOMEBREW_LIVE_FRONTEND_REF=$FRONTEND_REF
DALEC_HOMEBREW_LIVE_FINAL_IMAGE=$FINAL_IMAGE
DALEC_HOMEBREW_LIVE_FINAL_DIGEST=$FINAL_DIGEST
DALEC_HOMEBREW_LIVE_FINAL_REF=$FINAL_REF
RESULT
