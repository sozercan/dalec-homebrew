#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

fail() {
  echo "$*" >&2
  exit 1
}

for tool in docker jq go python3; do
  command -v "$tool" >/dev/null 2>&1 || fail "required tool is unavailable: $tool"
done

release_registry=${REGISTRY:-ghcr.io/sozercan}
release_version=${VERSION:-dev}
metadata_bundle_digest=${METADATA_BUNDLE_DIGEST:-}
if [[ -n "$metadata_bundle_digest" && ! "$metadata_bundle_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  fail "METADATA_BUNDLE_DIGEST must be empty or one sha256 digest"
fi

dalec_frontend_file=release/dalec-frontend.json
dalec_frontend=$(python3 - "$dalec_frontend_file" <<'PYJSON'
import json
from pathlib import Path
import sys

path = Path(sys.argv[1])
max_bytes = 64 << 10


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON member {key!r}")
        result[key] = value
    return result


def reject_constant(value):
    raise ValueError(f"invalid JSON constant {value}")


try:
    size = path.stat().st_size
    if size <= 0 or size > max_bytes:
        raise ValueError(f"binding size {size} is outside the allowed range")
    raw = path.read_bytes()
    if len(raw) != size:
        raise ValueError("binding size changed while reading")
    document = json.loads(
        raw.decode("utf-8"),
        object_pairs_hook=unique_object,
        parse_constant=reject_constant,
    )
    if not isinstance(document, dict):
        raise ValueError("binding is not an object")
except (OSError, UnicodeError, ValueError) as exc:
    raise SystemExit(f"{path}: invalid Dalec frontend binding: {exc}")

sys.stdout.write(json.dumps(document, ensure_ascii=False, allow_nan=False, separators=(",", ":")))
PYJSON
)
jq -e '
  (keys == ["index", "module", "platforms", "route", "schema_version"]) and
  .schema_version == "dalec-homebrew-dalec-frontend/v1" and
  ((.module | keys) == ["path", "version"]) and
  .module.path == "github.com/project-dalec/dalec" and
  (.module.version | type == "string" and test("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")) and
  .route == "homebrew/image" and
  (.index | type == "string" and length > 0) and
  ((.platforms | keys) == ["linux/amd64", "linux/arm64"]) and
  all(.platforms[]; type == "string" and length > 0)
' <<<"$dalec_frontend" >/dev/null || fail "$dalec_frontend_file has an unsupported schema or value"
dalec_frontend_index=$(jq -er .index <<<"$dalec_frontend")
dalec_frontend_amd64=$(jq -er '.platforms["linux/amd64"]' <<<"$dalec_frontend")
dalec_frontend_arm64=$(jq -er '.platforms["linux/arm64"]' <<<"$dalec_frontend")

targets=(
  runtime-base-amd64
  runtime-base-arm64
  bottle-fetcher-amd64
  bottle-fetcher-arm64
  catalog-extractor-amd64
  catalog-extractor-arm64
  materializer-amd64
  materializer-arm64
  frontend
  frontend-amd64
  frontend-arm64
)
targets_json=$(printf '%s\n' "${targets[@]}" | jq -Rsc 'split("\n") | map(select(length > 0))')
unset BUILDX_BAKE_FILE BUILDX_BAKE_PATH_SEPARATOR
bake=$(docker buildx bake --print \
  --var "REGISTRY=$release_registry" \
  --var "VERSION=$release_version" \
  "${targets[@]}")

missing_target=$(jq -r --argjson targets "$targets_json" '
  . as $bake
  | first($targets[] as $target | select($bake.target[$target] == null) | $target) // ""
' <<<"$bake")
[[ -z "$missing_target" ]] || fail "release bake target is missing: $missing_target"

source_date_epoch=$(jq -er --argjson targets "$targets_json" '
  . as $bake
  | [$targets[] as $target | $bake.target[$target].args.SOURCE_DATE_EPOCH]
  | if any(. == null or . == "") then error("SOURCE_DATE_EPOCH is missing") else . end
  | unique
  | if length == 1 then .[0] else error("release targets use conflicting SOURCE_DATE_EPOCH values") end
' <<<"$bake")
[[ "$source_date_epoch" =~ ^[0-9]+$ ]] || fail "invalid SOURCE_DATE_EPOCH in docker-bake.hcl: $source_date_epoch"

dockerfile_arg() {
  local name=$1 values
  values=$(sed -n "s/^ARG ${name}=//p" Dockerfile)
  [[ -n "$values" ]] || fail "Dockerfile ARG $name has no default"
  [[ $(wc -l <<<"$values" | tr -d '[:space:]') == 1 ]] || fail "Dockerfile ARG $name has multiple defaults"
  printf '%s\n' "$values"
}

dockerfile_epoch=$(dockerfile_arg SOURCE_DATE_EPOCH)
[[ "$dockerfile_epoch" == "$source_date_epoch" ]] || fail \
  "Dockerfile SOURCE_DATE_EPOCH ($dockerfile_epoch) differs from docker-bake.hcl ($source_date_epoch)"

ubuntu_snapshot=$(dockerfile_arg UBUNTU_SNAPSHOT)
python3 - "$source_date_epoch" "$ubuntu_snapshot" <<'PY'
from datetime import datetime, timezone
import sys

epoch = int(sys.argv[1])
snapshot = sys.argv[2]
try:
    parsed = datetime.strptime(snapshot, "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
    actual = datetime.fromtimestamp(epoch, timezone.utc)
except (OSError, OverflowError, ValueError) as exc:
    raise SystemExit(f"invalid release timestamp: {exc}")
if actual != parsed:
    raise SystemExit(
        f"SOURCE_DATE_EPOCH {epoch} resolves to {actual.isoformat()} instead of UBUNTU_SNAPSHOT {snapshot}"
    )
PY

if jq -e --argjson targets "$targets_json" '
  . as $bake
  | [$targets[] as $target | ($bake.target[$target].args.DALEC_SKIP_TESTS // "")]
  | any(. != "")
' <<<"$bake" >/dev/null; then
  fail "release bake targets must not set DALEC_SKIP_TESTS"
fi

for target in frontend frontend-amd64 frontend-arm64; do
  for name in FRONTEND_REF RUNTIME_BASE_REF MATERIALIZER_REF BOTTLE_FETCHER_REF CATALOG_EXTRACTOR_REF METADATA_BUNDLE_DIGEST; do
    value=$(jq -r --arg target "$target" --arg name "$name" '.target[$target].args[$name] // ""' <<<"$bake")
    [[ -z "$value" ]] || fail "default $target $name must be empty; release CI supplies or derives the immutable reference"
  done
done
for target in frontend frontend-amd64 frontend-arm64 materializer-amd64 materializer-arm64; do
  for name in BOTTLE_FETCHER_REF CATALOG_EXTRACTOR_REF TAP_POLICY_DIGEST EXECUTABLE_RUNTIME_POLICY_DIGEST SUPPORTED_CATALOG_POLICY_VERSIONS SUPPORTED_FETCH_POLICY_VERSIONS SUPPORTED_PROVENANCE_POLICY_VERSIONS; do
    value=$(jq -r --arg target "$target" --arg name "$name" '.target[$target].args[$name] // ""' <<<"$bake")
    [[ -z "$value" ]] || fail "default $target $name must be empty; release CI supplies the immutable V2 binding"
  done
done

validate_bake_target() {
  local name=$1 target=$2 platforms=$3 tag=$4 unsafe
  jq -e \
    --arg name "$name" \
    --arg target "$target" \
    --argjson platforms "$platforms" \
    --arg tag "$tag" '
      .target[$name] as $actual
      | $actual != null
        and $actual.context == "."
        and $actual.dockerfile == "Dockerfile"
        and $actual.target == $target
        and (($actual.platforms | sort) == ($platforms | sort))
        and $actual.tags == [$tag]
    ' <<<"$bake" >/dev/null || fail \
    "$name must use context ., Dockerfile, target $target, platforms $platforms, and staging tag $tag"

  unsafe=$(jq -r --arg name "$name" '
    (.target[$name] | keys - ["args", "context", "dockerfile", "platforms", "tags", "target"])
    | first // ""
  ' <<<"$bake")
  [[ -z "$unsafe" ]] || fail "$name must not set $unsafe"
}
validate_bake_target runtime-base-amd64 runtime-base '["linux/amd64"]' \
  "$release_registry/dalec-homebrew-runtime-base:${release_version}-amd64"
validate_bake_target runtime-base-arm64 runtime-base '["linux/arm64"]' \
  "$release_registry/dalec-homebrew-runtime-base:${release_version}-arm64"
validate_bake_target bottle-fetcher-amd64 bottle-fetcher '["linux/amd64"]' \
  "$release_registry/dalec-homebrew-bottle-fetcher:${release_version}-amd64"
validate_bake_target bottle-fetcher-arm64 bottle-fetcher '["linux/arm64"]' \
  "$release_registry/dalec-homebrew-bottle-fetcher:${release_version}-arm64"
validate_bake_target catalog-extractor-amd64 catalog-extractor '["linux/amd64"]' \
  "$release_registry/dalec-homebrew-catalog-extractor:${release_version}-amd64"
validate_bake_target catalog-extractor-arm64 catalog-extractor '["linux/arm64"]' \
  "$release_registry/dalec-homebrew-catalog-extractor:${release_version}-arm64"
validate_bake_target materializer-amd64 materializer '["linux/amd64"]' \
  "$release_registry/dalec-homebrew-materializer:${release_version}-amd64"
validate_bake_target materializer-arm64 materializer '["linux/arm64"]' \
  "$release_registry/dalec-homebrew-materializer:${release_version}-arm64"
validate_bake_target frontend frontend '["linux/amd64","linux/arm64"]' \
  "$release_registry/dalec-homebrew:$release_version"
validate_bake_target frontend-amd64 frontend '["linux/amd64"]' \
  "$release_registry/dalec-homebrew:${release_version}-amd64"
validate_bake_target frontend-arm64 frontend '["linux/arm64"]' \
  "$release_registry/dalec-homebrew:${release_version}-arm64"

runtime_base_amd64=$(jq -er '.target["runtime-base-amd64"].args.RUNTIME_BASE' <<<"$bake")
runtime_base_arm64=$(jq -er '.target["runtime-base-arm64"].args.RUNTIME_BASE' <<<"$bake")
materializer_base_amd64=$(jq -er '.target["materializer-amd64"].args.RUNTIME_BASE' <<<"$bake")
materializer_base_arm64=$(jq -er '.target["materializer-arm64"].args.RUNTIME_BASE' <<<"$bake")
catalog_extractor_base_amd64=$(jq -er '.target["catalog-extractor-amd64"].args.RUNTIME_BASE' <<<"$bake")
catalog_extractor_base_arm64=$(jq -er '.target["catalog-extractor-arm64"].args.RUNTIME_BASE' <<<"$bake")
[[ "$materializer_base_amd64" == "$runtime_base_amd64" ]] || fail \
  "materializer-amd64 Ubuntu base differs from runtime-base-amd64"
[[ "$materializer_base_arm64" == "$runtime_base_arm64" ]] || fail \
  "materializer-arm64 Ubuntu base differs from runtime-base-arm64"
[[ "$catalog_extractor_base_amd64" == "$runtime_base_amd64" ]] || fail \
  "catalog-extractor-amd64 Ubuntu base differs from runtime-base-amd64"
[[ "$catalog_extractor_base_arm64" == "$runtime_base_arm64" ]] || fail \
  "catalog-extractor-arm64 Ubuntu base differs from runtime-base-arm64"
[[ "$runtime_base_amd64" != "$runtime_base_arm64" ]] || fail \
  "amd64 and arm64 runtime bases unexpectedly use the same manifest"

dockerfile_frontend=$(sed -n '1s/^# syntax=//p' Dockerfile)

go_image=$(dockerfile_arg GO_IMAGE)
chisel_version=$(dockerfile_arg CHISEL_VERSION)
chisel_releases_commit=$(dockerfile_arg CHISEL_RELEASES_COMMIT)
chisel_releases_sha256=$(dockerfile_arg CHISEL_RELEASES_SHA256)
chisel_amd64_sha256=$(dockerfile_arg CHISEL_AMD64_SHA256)
chisel_arm64_sha256=$(dockerfile_arg CHISEL_ARM64_SHA256)
homebrew_archive_sha256=$(dockerfile_arg HOMEBREW_ARCHIVE_SHA256)
homebrew_commit=$(dockerfile_arg HOMEBREW_COMMIT)
portable_ruby_version=$(dockerfile_arg HOMEBREW_RUBY_VERSION)
verification_keys_digest=$(dockerfile_arg HOMEBREW_KEYS_DIGEST)

GOWORK=off GOFLAGS='' go run ./cmd/live-input-verify \
  --pinned-ref "runtime-base-amd64=$runtime_base_amd64" \
  --pinned-ref "runtime-base-arm64=$runtime_base_arm64" \
  --pinned-ref "GO_IMAGE=$go_image" \
  --pinned-ref "Dockerfile frontend=$dockerfile_frontend" \
  --pinned-ref "Dalec frontend index=$dalec_frontend_index" \
  --pinned-ref "Dalec frontend linux/amd64=$dalec_frontend_amd64" \
  --pinned-ref "Dalec frontend linux/arm64=$dalec_frontend_arm64" \
  --dalec-frontend-file "$dalec_frontend_file" >/dev/null

dalec_frontend_repository=${dalec_frontend_index%@sha256:*}
[[ "${dalec_frontend_amd64%@sha256:*}" == "$dalec_frontend_repository" ]] || fail \
  "Dalec frontend linux/amd64 child uses a different repository from the index"
[[ "${dalec_frontend_arm64%@sha256:*}" == "$dalec_frontend_repository" ]] || fail \
  "Dalec frontend linux/arm64 child uses a different repository from the index"
[[ "$dalec_frontend_amd64" != "$dalec_frontend_arm64" ]] || fail \
  "Dalec frontend platform children unexpectedly use the same manifest"

[[ -n "$chisel_version" ]] || fail "CHISEL_VERSION is empty"
[[ "$chisel_releases_commit" =~ ^[0-9a-f]{40}$ ]] || fail \
  "invalid chisel-releases commit: $chisel_releases_commit"
for name in chisel_releases_sha256 chisel_amd64_sha256 chisel_arm64_sha256 homebrew_archive_sha256; do
  value=${!name}
  [[ "$value" =~ ^[0-9a-f]{64}$ ]] || fail "invalid $name: $value"
done
[[ "$homebrew_commit" =~ ^[0-9a-f]{40}$ ]] || fail "invalid Homebrew commit: $homebrew_commit"
[[ -n "$portable_ruby_version" ]] || fail "portable Ruby version is empty"
[[ "$verification_keys_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || fail \
  "invalid verification key digest: $verification_keys_digest"

expected_args=$(jq -n \
  --arg GO_IMAGE "$go_image" \
  --arg UBUNTU_SNAPSHOT "$ubuntu_snapshot" \
  --arg CHISEL_VERSION "$chisel_version" \
  --arg CHISEL_RELEASES_COMMIT "$chisel_releases_commit" \
  --arg CHISEL_RELEASES_SHA256 "$chisel_releases_sha256" \
  --arg CHISEL_AMD64_SHA256 "$chisel_amd64_sha256" \
  --arg CHISEL_ARM64_SHA256 "$chisel_arm64_sha256" \
  --arg HOMEBREW_ARCHIVE_SHA256 "$homebrew_archive_sha256" \
  --arg HOMEBREW_COMMIT "$homebrew_commit" \
  --arg HOMEBREW_RUBY_VERSION "$portable_ruby_version" \
  --arg HOMEBREW_KEYS_DIGEST "$verification_keys_digest" \
  '$ARGS.named')
override=$(jq -r --argjson targets "$targets_json" --argjson expected "$expected_args" '
  . as $bake
  | first(
      $targets[] as $target
      | $expected | to_entries[]
      | . as $entry
      | select(($bake.target[$target].args[$entry.key] // $entry.value) != $entry.value)
      | "\($target) overrides \($entry.key) with an unrecorded release value"
    ) // ""
' <<<"$bake")
[[ -z "$override" ]] || fail "$override"

module_version() {
  local module=$1 metadata
  metadata=$(GOWORK=off GOFLAGS='' go list -m -json "$module")
  jq -e --arg module "$module" '
    .Path == $module and ((.Replace // null) == null)
  ' <<<"$metadata" >/dev/null || {
    echo "release module $module must not be replaced" >&2
    return 1
  }
  jq -er '.Version | select(type == "string" and length > 0)' <<<"$metadata"
}

dalec_module=$(module_version github.com/project-dalec/dalec)
buildkit_module=$(module_version github.com/moby/buildkit)

jq -n \
  --arg source_date_epoch "$source_date_epoch" \
  --arg ubuntu_snapshot "$ubuntu_snapshot" \
  --arg runtime_base_amd64 "$runtime_base_amd64" \
  --arg runtime_base_arm64 "$runtime_base_arm64" \
  --arg dockerfile_frontend "$dockerfile_frontend" \
  --arg go_image "$go_image" \
  --arg chisel_version "$chisel_version" \
  --arg chisel_releases_commit "$chisel_releases_commit" \
  --arg chisel_releases_sha256 "$chisel_releases_sha256" \
  --arg chisel_amd64_sha256 "$chisel_amd64_sha256" \
  --arg chisel_arm64_sha256 "$chisel_arm64_sha256" \
  --arg homebrew_archive_sha256 "$homebrew_archive_sha256" \
  --arg homebrew_commit "$homebrew_commit" \
  --arg portable_ruby_version "$portable_ruby_version" \
  --arg verification_keys_digest "$verification_keys_digest" \
  --arg metadata_bundle_digest "$metadata_bundle_digest" \
  --arg dalec_module "$dalec_module" \
  --arg buildkit_module "$buildkit_module" \
  --argjson dalec_frontend "$dalec_frontend" \
  '{
    schema_version: "dalec-homebrew-release-inputs/v2",
    source_date_epoch: $source_date_epoch,
    ubuntu_snapshot: $ubuntu_snapshot,
    ubuntu_base: {
      "linux/amd64": $runtime_base_amd64,
      "linux/arm64": $runtime_base_arm64
    },
    builder: {
      dockerfile_frontend: $dockerfile_frontend,
      go_image: $go_image,
      chisel_version: $chisel_version,
      chisel_releases_commit: $chisel_releases_commit,
      chisel_releases_sha256: $chisel_releases_sha256,
      chisel_sha256: {
        amd64: $chisel_amd64_sha256,
        arm64: $chisel_arm64_sha256
      }
    },
    homebrew_commit: $homebrew_commit,
    homebrew_archive_sha256: $homebrew_archive_sha256,
    portable_ruby_version: $portable_ruby_version,
    verification_keys_digest: $verification_keys_digest,
    metadata_bundle_digest: $metadata_bundle_digest,
    dalec_module: $dalec_module,
    buildkit_module: $buildkit_module,
    dalec_frontend: $dalec_frontend
  }'
