#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

BUILDKIT_IMAGE=${DALEC_HOMEBREW_E2E_BUILDKIT_IMAGE:-}
REGISTRY_IMAGE=${DALEC_HOMEBREW_E2E_REGISTRY_IMAGE:-}
RUN_ID=${DALEC_HOMEBREW_E2E_RUN_ID:-$(date -u +%Y%m%d%H%M%S)}
FINAL_IMAGE=${DALEC_HOMEBREW_E2E_IMAGE:-dalec-homebrew-noncore-e2e:dev}
SPEC=${DALEC_HOMEBREW_E2E_SPEC:-examples/ci-noncore-multi-package.yaml}
PROGRESS=${DALEC_HOMEBREW_E2E_PROGRESS:-plain}
PLATFORM=${DALEC_HOMEBREW_E2E_PLATFORM:-linux/amd64}
KEEP_WORK=${DALEC_HOMEBREW_E2E_KEEP_WORK:-0}
DALEC_FRONTEND_PIN=${DALEC_HOMEBREW_E2E_DALEC_FRONTEND_PIN:-release/dalec-frontend.json}

fail_usage() {
  echo "$*" >&2
  exit 64
}

for tool in docker jq go curl; do
  command -v "$tool" >/dev/null 2>&1 || fail_usage "required tool is unavailable: $tool"
done
[[ -n "$BUILDKIT_IMAGE" ]] || fail_usage "DALEC_HOMEBREW_E2E_BUILDKIT_IMAGE is required"
[[ -n "$REGISTRY_IMAGE" ]] || fail_usage "DALEC_HOMEBREW_E2E_REGISTRY_IMAGE is required"
[[ "$PLATFORM" == linux/amd64 ]] || fail_usage "only linux/amd64 is supported by the CI non-core E2E"
[[ "$RUN_ID" =~ ^[0-9A-Za-z][0-9A-Za-z_.-]{0,63}$ ]] || fail_usage "invalid E2E run ID"
if ! DALEC_SELECTION=$(GOWORK=off GOFLAGS='' go run ./cmd/live-input-verify \
  --dalec-frontend-file "$DALEC_FRONTEND_PIN" \
  --base-spec-file "$SPEC" \
  --pinned-ref "DALEC_HOMEBREW_E2E_BUILDKIT_IMAGE=$BUILDKIT_IMAGE" \
  --pinned-ref "DALEC_HOMEBREW_E2E_REGISTRY_IMAGE=$REGISTRY_IMAGE"); then
  exit 64
fi
DALEC_FRONTEND_REF=$(jq -er '.index | select(type == "string" and length > 0)' <<<"$DALEC_SELECTION") ||
  fail_usage "validated upstream Dalec frontend pin did not contain an index"
DALEC_ROUTE=$(jq -er '.route | select(type == "string" and length > 0)' <<<"$DALEC_SELECTION") ||
  fail_usage "validated upstream Dalec frontend pin did not contain a route"

WORK=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dalec-homebrew-noncore-e2e.XXXXXX")
NETWORK="dalec-homebrew-e2e-${RUN_ID}"
REGISTRY_CONTAINER="dalec-homebrew-e2e-registry-${RUN_ID}"
BUILDER="dalec-homebrew-e2e-${RUN_ID}"
REGISTRY="e2e-registry:5000"
HOST_REGISTRY="localhost:5000"

cleanup() {
  status=$?
  trap - EXIT
  set +e
  docker buildx rm --force "$BUILDER" >/dev/null 2>&1 || true
  docker rm --force "$REGISTRY_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  if [[ "$KEEP_WORK" == 1 ]]; then
    echo "E2E work directory retained at $WORK" >&2
  else
    rm -rf "$WORK"
  fi
  exit "$status"
}
trap cleanup EXIT

read_digest() {
  jq -er '."containerimage.digest" | select(type == "string" and test("^sha256:[0-9a-f]{64}$"))' "$1"
}

build_component() {
  local label=$1 target=$2 repository=$3
  shift 3
  local metadata="$WORK/${target}.metadata.json"
  echo "==> Building $label" >&2
  if ! docker buildx build \
    --builder "$BUILDER" \
    --platform "$PLATFORM" \
    --provenance=false \
    --progress="$PROGRESS" \
    --target "$target" \
    --build-arg "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH" \
    "$@" \
    --tag "$REGISTRY/$repository:$RUN_ID" \
    --metadata-file "$metadata" \
    --push \
    . >&2; then
    return 1
  fi
  local digest
  if ! digest=$(read_digest "$metadata"); then
    echo "component metadata for $target does not contain an image digest:" >&2
    cat "$metadata" >&2 || true
    return 1
  fi
  printf '%s/%s@%s\n' "$REGISTRY" "$repository" "$digest"
}


inputs="$WORK/release-inputs.json"
./scripts/release-inputs.sh > "$inputs"
SOURCE_DATE_EPOCH=$(jq -er .source_date_epoch "$inputs")
RUNTIME_BASE=$(jq -er '.ubuntu_base["linux/amd64"]' "$inputs")
HOMEBREW_COMMIT=$(jq -er .homebrew_commit "$inputs")

cat > "$WORK/buildkitd.toml" <<EOF_CONFIG
[registry."$REGISTRY"]
  http = true
  insecure = true
EOF_CONFIG

docker network create "$NETWORK" >/dev/null
docker run --detach \
  --name "$REGISTRY_CONTAINER" \
  --network "$NETWORK" \
  --network-alias e2e-registry \
  --publish 127.0.0.1:5000:5000 \
  "$REGISTRY_IMAGE" >/dev/null
for _ in $(seq 1 30); do
  curl --fail --silent http://127.0.0.1:5000/v2/ >/dev/null && break
  sleep 1
done
curl --fail --silent http://127.0.0.1:5000/v2/ >/dev/null

docker buildx create \
  --name "$BUILDER" \
  --driver docker-container \
  --driver-opt "image=$BUILDKIT_IMAGE" \
  --driver-opt "network=$NETWORK" \
  --buildkitd-config "$WORK/buildkitd.toml" \
  --use \
  --bootstrap >/dev/null

RUNTIME_BASE_REF=$(build_component "runtime base" runtime-base dalec-homebrew-runtime-base \
  --build-arg "RUNTIME_BASE=$RUNTIME_BASE")
BOTTLE_FETCHER_REF=$(build_component "bottle fetcher" bottle-fetcher dalec-homebrew-bottle-fetcher)
EXTRACTOR_REF=$(build_component "catalog extractor" catalog-extractor dalec-homebrew-catalog-extractor \
  --build-arg "RUNTIME_BASE=$RUNTIME_BASE")

go run ./cmd/v2-bindings \
  --catalog-extractor-ref "$EXTRACTOR_REF" \
  --output "$WORK/v2-bindings.json"

TAP_POLICY_DIGEST=$(jq -er .tap_policy_digest "$WORK/v2-bindings.json")
RUNTIME_POLICY_DIGEST=$(jq -er .executable_runtime_policy_digest "$WORK/v2-bindings.json")
CATALOG_POLICY_VERSIONS=$(jq -er .supported_catalog_policy_versions "$WORK/v2-bindings.json")
FETCH_POLICY_VERSIONS=$(jq -er .supported_fetch_policy_versions "$WORK/v2-bindings.json")
PROVENANCE_POLICY_VERSIONS=$(jq -er .supported_provenance_policy_versions "$WORK/v2-bindings.json")

V2_BUILD_ARGS=(
  --build-arg "BOTTLE_FETCHER_REF=$BOTTLE_FETCHER_REF"
  --build-arg "CATALOG_EXTRACTOR_REF=$EXTRACTOR_REF"
  --build-arg "TAP_POLICY_DIGEST=$TAP_POLICY_DIGEST"
  --build-arg "EXECUTABLE_RUNTIME_POLICY_DIGEST=$RUNTIME_POLICY_DIGEST"
  --build-arg "SUPPORTED_CATALOG_POLICY_VERSIONS=$CATALOG_POLICY_VERSIONS"
  --build-arg "SUPPORTED_FETCH_POLICY_VERSIONS=$FETCH_POLICY_VERSIONS"
  --build-arg "SUPPORTED_PROVENANCE_POLICY_VERSIONS=$PROVENANCE_POLICY_VERSIONS"
)

MATERIALIZER_REF=$(build_component materializer materializer dalec-homebrew-materializer \
  --build-arg "RUNTIME_BASE=$RUNTIME_BASE" \
  "${V2_BUILD_ARGS[@]}")
FRONTEND_REF=$(build_component "V2 frontend" frontend dalec-homebrew \
  --build-arg "RUNTIME_BASE_REF=$RUNTIME_BASE_REF" \
  --build-arg "MATERIALIZER_REF=$MATERIALIZER_REF" \
  "${V2_BUILD_ARGS[@]}")

cat > "$WORK/list-form-spec.yaml" <<EOF_LIST_SPEC
# syntax=$DALEC_FRONTEND_REF
dependencies:
  runtime: [hello]
targets:
  homebrew:
    frontend:
      image: $FRONTEND_REF
EOF_LIST_SPEC
if docker buildx build \
  --builder "$BUILDER" \
  --platform "$PLATFORM" \
  --progress="$PROGRESS" \
  --target "$DALEC_ROUTE" \
  --file "$WORK/list-form-spec.yaml" \
  . >"$WORK/list-form.log" 2>&1; then
  echo "upstream Dalec accepted unsupported list-form runtime dependencies" >&2
  exit 1
fi
grep -F "target $PLATFORM has no applicable runtime roots" \
  "$WORK/list-form.log" >/dev/null || {
    cat "$WORK/list-form.log" >&2
    echo "list-form rejection did not fail at the expected empty-root boundary" >&2
    exit 1
  }

DALEC_HOMEBREW_LIVE_BUILDER="$BUILDER" \
DALEC_HOMEBREW_LIVE_PLATFORM="$PLATFORM" \
DALEC_HOMEBREW_LIVE_SPEC="$SPEC" \
DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF="$RUNTIME_BASE_REF" \
DALEC_HOMEBREW_LIVE_MATERIALIZER_REF="$MATERIALIZER_REF" \
DALEC_HOMEBREW_LIVE_FRONTEND_REF="$FRONTEND_REF" \
DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN="$DALEC_FRONTEND_PIN" \
DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
DALEC_HOMEBREW_LIVE_IMAGE="$FINAL_IMAGE" \
DALEC_HOMEBREW_LIVE_OUTPUT=load \
DALEC_HOMEBREW_LIVE_PROGRESS="$PROGRESS" \
  ./scripts/live-test.sh

docker run --rm --network none --entrypoint /bin/bash "$FINAL_IMAGE" -lc '
  set -euo pipefail
  hello | grep -F "Hello, world!"
  test -s /home/linuxbrew/.linuxbrew/opt/libdf/lib/libdf.so
  a365 --version | grep -Eq '[0-9]+[.][0-9]+[.][0-9]+'
'

for name in resolution.json materialization-v2.json runtime-inventory.json; do
  docker run --rm --network none --entrypoint /bin/cat "$FINAL_IMAGE" \
    "/usr/share/dalec-homebrew/$name" > "$WORK/$name"
done

jq -e '
  (.requested | map(.requested)) == [
    "hello",
    "sozercan/repo/a365",
    "svt/avtools/libdf"
  ]
' "$WORK/resolution.json" >/dev/null

LIBDF_ID=svt/avtools/libdf
LIBDF_TAP=svt/avtools
jq -e --arg id "$LIBDF_ID" --arg tap "$LIBDF_TAP" '
  .components.catalog_extractor_ref as $extractor |
  ([.metadata_sources[] | select(
    .tap == $tap and
    .catalog_policy_version == "tap-catalog-v1" and
    .extraction.policy_version == "build-local-tap-extraction-v1" and
    .extraction.extractor_ref == $extractor
  )] | if length == 1 then .[0] else empty end) as $source |
  .schema_version == "dalec-homebrew-resolution/v2" and
  ($source.commit | test("^[0-9a-f]{40}$")) and
  (([.requested[] | select(.requested == $id and .id == $id)] | length) == 1) and
  (([.nodes[] | select(
    .id == $id and
    .tap == $tap and
    .name == "libdf" and
    .homebrew_full_name == $id and
    .bottle.transport.oci == null and
    .bottle.transport.https.fetch_policy_version == "homebrew-bottle-fetch-v1"
  )] | length) == 1) and
  ((.install_order | index($id)) != null)
' "$WORK/resolution.json" >/dev/null

jq -e --arg id "$LIBDF_ID" --arg tap "$LIBDF_TAP" '
  .schema_version == "dalec-homebrew-materialization/v2" and
  .preparation.schema_version == "dalec-homebrew-preparation/v2" and
  (([.preparation.verified_bottles[] | select(.id == $id)] | length) == 1) and
  (([.preparation.fetch_evidence[] | select(
    .artifact_id == $id and
    .schema_version == "bottle-fetch-evidence/v1" and
    .fetch_policy_version == "homebrew-bottle-fetch-v1"
  )] | length) == 1) and
  (([.preparation.staged_formulae[] | select(.id == $id and .tap == $tap and .name == "libdf")] | length) == 1) and
  (([.install_deltas[] | select(.schema_version == "dalec-homebrew-install-delta/v2" and .id == $id)] | length) == 1)
' "$WORK/materialization-v2.json" >/dev/null

jq -e --arg id "$LIBDF_ID" '
  .schema_version == "dalec-homebrew-runtime-inventory/v2" and
  (([.entries[] | select(
    .formula_id == $id and
    .type == "file" and
    (.path | endswith("/libdf.so")) and
    .size > 0
  )] | length) >= 1)
' "$WORK/runtime-inventory.json" >/dev/null

A365_ID=sozercan/repo/a365
A365_TAP=sozercan/repo
jq -e --arg id "$A365_ID" --arg tap "$A365_TAP" '
  .components.catalog_extractor_ref as $extractor |
  ([.metadata_sources[] | select(
    .tap == $tap and
    .catalog_policy_version == "tap-catalog-v1" and
    .extraction.policy_version == "build-local-tap-extraction-v1" and
    .extraction.extractor_ref == $extractor
  )] | if length == 1 then .[0] else empty end) as $source |
  .schema_version == "dalec-homebrew-resolution/v2" and
  ($source.commit | test("^[0-9a-f]{40}$")) and
  (([.requested[] | select(.requested == $id and .id == $id)] | length) == 1) and
  (([.nodes[] | select(
    .id == $id and
    .tap == $tap and
    .name == "a365" and
    .homebrew_full_name == $id and
    (.formula_version | test("^[0-9]+[.][0-9]+[.][0-9]+([+._-][0-9A-Za-z.-]+)?$")) and
    (.pkg_version | test("^[0-9]+[.][0-9]+[.][0-9]+([+._-][0-9A-Za-z.-]+)?$")) and
    .bottle.transport.oci == null and
    .bottle.transport.https == null and
    .bottle.transport.local.policy_version == "build-local-artifact-v1" and
    .bottle.transport.local.sha256 == .bottle.sha256 and
    .bottle.transport.local.size == .bottle.size and
    .bottle.prebuilt_derivation.policy_version == "prebuilt-derived-bottle-v1" and
    .bottle.prebuilt_derivation.source.format == "tar+gzip" and
    .bottle.prebuilt_derivation.source.transport.https.fetch_policy_version == "homebrew-bottle-fetch-v1" and
    .bottle.prebuilt_derivation.source.sha256 == "sha256:71461c31e350cabf4e718a5e1331b39a395a6dc9183bb3ea5922f0fac67404ce" and
    .bottle.prebuilt_derivation.payload.source_path == "a365" and
    .bottle.prebuilt_derivation.payload.destination_path == "bin/a365" and
    .bottle.prebuilt_derivation.formula_source.transport.tap.id == $tap and
    .bottle.prebuilt_derivation.formula_source.transport.tap.repository == "https://github.com/sozercan/homebrew-repo" and
    .bottle.prebuilt_derivation.formula_source.transport.tap.commit == $source.commit and
    .bottle.prebuilt_derivation.elf.machine == "x86_64" and
    .provenance.waiver.policy == "prebuilt-archive-buildkit-and-verified-checksum-v1"
  )] | length) == 1) and
  ((.install_order | index($id)) != null)
' "$WORK/resolution.json" >/dev/null

jq -e --arg id "$A365_ID" --arg tap "$A365_TAP" '
  .schema_version == "dalec-homebrew-materialization/v2" and
  (([.preparation.verified_bottles[] | select(.id == $id and .receipt == null)] | length) == 1) and
  (([.preparation.fetch_evidence[] | select(.artifact_id == $id)] | length) == 0) and
  (([.preparation.staged_formulae[] | select(.id == $id and .tap == $tap and .name == "a365")] | length) == 1) and
  (([.install_deltas[] | select(.schema_version == "dalec-homebrew-install-delta/v2" and .id == $id)] | length) == 1)
' "$WORK/materialization-v2.json" >/dev/null

jq -e --arg id "$A365_ID" '
  .schema_version == "dalec-homebrew-runtime-inventory/v2" and
  (([.entries[] | select(
    .formula_id == $id and
    .type == "file" and
    (.path | endswith("/bin/a365")) and
    .size > 0
  )] | length) == 1)
' "$WORK/runtime-inventory.json" >/dev/null

cat <<RESULT
DALEC_HOMEBREW_E2E_RUNTIME_BASE_REF=$RUNTIME_BASE_REF
DALEC_HOMEBREW_E2E_BOTTLE_FETCHER_REF=$BOTTLE_FETCHER_REF
DALEC_HOMEBREW_E2E_CATALOG_EXTRACTOR_REF=$EXTRACTOR_REF
DALEC_HOMEBREW_E2E_MATERIALIZER_REF=$MATERIALIZER_REF
DALEC_HOMEBREW_E2E_FRONTEND_REF=$FRONTEND_REF
DALEC_HOMEBREW_E2E_DALEC_FRONTEND_REF=$DALEC_FRONTEND_REF
DALEC_HOMEBREW_E2E_DALEC_ROUTE=$DALEC_ROUTE
DALEC_HOMEBREW_E2E_FINAL_IMAGE=$FINAL_IMAGE
RESULT
