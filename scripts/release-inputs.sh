#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

for tool in docker jq go python3; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "required tool is unavailable: $tool" >&2
    exit 127
  }
done

targets=(
  runtime-base-amd64
  runtime-base-arm64
  materializer-amd64
  materializer-arm64
  frontend
)

bake=$(docker buildx bake --print "${targets[@]}")

for target in "${targets[@]}"; do
  jq -e --arg target "$target" '.target[$target] != null' <<<"$bake" >/dev/null || {
    echo "release bake target is missing: $target" >&2
    exit 1
  }
done

source_date_epoch=$(jq -er --argjson targets "$(printf '%s\n' "${targets[@]}" | jq -R . | jq -s .)" '
  [$targets[] as $target | .target[$target].args.SOURCE_DATE_EPOCH]
  | if any(. == null or . == "") then error("SOURCE_DATE_EPOCH is missing") else . end
  | unique
  | if length == 1 then .[0] else error("release targets use conflicting SOURCE_DATE_EPOCH values") end
' <<<"$bake")

case "$source_date_epoch" in
  ''|*[!0-9]*)
    echo "invalid SOURCE_DATE_EPOCH in docker-bake.hcl: $source_date_epoch" >&2
    exit 1
    ;;
esac

dockerfile_arg() {
  local name=$1 values
  values=$(sed -n "s/^ARG ${name}=//p" Dockerfile | sort -u)
  if [[ -z "$values" ]]; then
    echo "Dockerfile ARG $name has no default" >&2
    exit 1
  fi
  if [[ $(wc -l <<<"$values" | tr -d '[:space:]') != 1 ]]; then
    echo "Dockerfile ARG $name has conflicting defaults" >&2
    exit 1
  fi
  printf '%s\n' "$values"
}

dockerfile_epoch=$(dockerfile_arg SOURCE_DATE_EPOCH)
if [[ "$dockerfile_epoch" != "$source_date_epoch" ]]; then
  echo "Dockerfile SOURCE_DATE_EPOCH ($dockerfile_epoch) differs from docker-bake.hcl ($source_date_epoch)" >&2
  exit 1
fi

ubuntu_snapshot=$(dockerfile_arg UBUNTU_SNAPSHOT)
python3 - "$source_date_epoch" "$ubuntu_snapshot" <<'PY'
from datetime import datetime, timezone
import sys

epoch = int(sys.argv[1])
snapshot = sys.argv[2]
try:
    parsed = datetime.strptime(snapshot, "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
except ValueError as exc:
    raise SystemExit(f"invalid UBUNTU_SNAPSHOT {snapshot!r}: {exc}")
actual = datetime.fromtimestamp(epoch, timezone.utc)
if actual != parsed:
    raise SystemExit(
        f"SOURCE_DATE_EPOCH {epoch} resolves to {actual.isoformat()} instead of UBUNTU_SNAPSHOT {snapshot}"
    )
PY

if jq -e --argjson targets "$(printf '%s\n' "${targets[@]}" | jq -R . | jq -s .)" '
  [$targets[] as $target | (.target[$target].args.DALEC_SKIP_TESTS // "")] | any(. != "")
' <<<"$bake" >/dev/null; then
  echo "release bake targets must not set DALEC_SKIP_TESTS" >&2
  exit 1
fi

frontend_ref=$(jq -r '.target.frontend.args.FRONTEND_REF // ""' <<<"$bake")
if [[ -n "$frontend_ref" ]]; then
  echo "release frontend must derive its own identity from the digest-pinned gateway source" >&2
  exit 1
fi

for name in RUNTIME_BASE_REF MATERIALIZER_REF; do
  value=$(jq -r --arg name "$name" '.target.frontend.args[$name] // ""' <<<"$bake")
  if [[ -n "$value" ]]; then
    echo "default frontend $name must be empty; release CI supplies the immutable index reference" >&2
    exit 1
  fi
done

for target in runtime-base-amd64 materializer-amd64; do
  expected='linux/amd64'
  actual=$(jq -er --arg target "$target" '.target[$target].platforms | if length == 1 then .[0] else error("expected one platform") end' <<<"$bake")
  [[ "$actual" == "$expected" ]] || {
    echo "$target builds $actual, expected $expected" >&2
    exit 1
  }
done
for target in runtime-base-arm64 materializer-arm64; do
  expected='linux/arm64'
  actual=$(jq -er --arg target "$target" '.target[$target].platforms | if length == 1 then .[0] else error("expected one platform") end' <<<"$bake")
  [[ "$actual" == "$expected" ]] || {
    echo "$target builds $actual, expected $expected" >&2
    exit 1
  }
done

frontend_platforms=$(jq -c '.target.frontend.platforms | sort' <<<"$bake")
[[ "$frontend_platforms" == '["linux/amd64","linux/arm64"]' ]] || {
  echo "frontend platforms are $frontend_platforms, expected linux/amd64 and linux/arm64" >&2
  exit 1
}

runtime_base_amd64=$(jq -er '.target["runtime-base-amd64"].args.RUNTIME_BASE' <<<"$bake")
runtime_base_arm64=$(jq -er '.target["runtime-base-arm64"].args.RUNTIME_BASE' <<<"$bake")
for ref in "$runtime_base_amd64" "$runtime_base_arm64"; do
  [[ "$ref" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] || {
    echo "runtime base is not digest pinned: $ref" >&2
    exit 1
  }
done
[[ "$runtime_base_amd64" != "$runtime_base_arm64" ]] || {
  echo "amd64 and arm64 runtime bases unexpectedly use the same manifest" >&2
  exit 1
}

homebrew_commit=$(jq -er '.target.frontend.args.HOMEBREW_COMMIT' <<<"$bake")
portable_ruby_version=$(jq -er '.target.frontend.args.HOMEBREW_RUBY_VERSION' <<<"$bake")
verification_keys_digest=$(jq -er '.target.frontend.args.HOMEBREW_KEYS_DIGEST' <<<"$bake")

[[ "$homebrew_commit" =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid Homebrew commit: $homebrew_commit" >&2
  exit 1
}
[[ -n "$portable_ruby_version" ]] || {
  echo "portable Ruby version is empty" >&2
  exit 1
}
[[ "$verification_keys_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "invalid verification key digest: $verification_keys_digest" >&2
  exit 1
}

dockerfile_frontend=$(sed -n '1s/^# syntax=//p' Dockerfile)
[[ "$dockerfile_frontend" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] || {
  echo "Dockerfile frontend is not digest pinned: $dockerfile_frontend" >&2
  exit 1
}

go_image=$(dockerfile_arg GO_IMAGE)
chisel_version=$(dockerfile_arg CHISEL_VERSION)
chisel_releases_commit=$(dockerfile_arg CHISEL_RELEASES_COMMIT)
chisel_releases_sha256=$(dockerfile_arg CHISEL_RELEASES_SHA256)
chisel_amd64_sha256=$(dockerfile_arg CHISEL_AMD64_SHA256)
chisel_arm64_sha256=$(dockerfile_arg CHISEL_ARM64_SHA256)
homebrew_archive_sha256=$(dockerfile_arg HOMEBREW_ARCHIVE_SHA256)
dockerfile_homebrew_commit=$(dockerfile_arg HOMEBREW_COMMIT)
dockerfile_ruby_version=$(dockerfile_arg HOMEBREW_RUBY_VERSION)
dockerfile_keys_digest=$(dockerfile_arg HOMEBREW_KEYS_DIGEST)

[[ "$go_image" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] || {
  echo "GO_IMAGE is not digest pinned: $go_image" >&2
  exit 1
}
[[ -n "$chisel_version" ]] || { echo "CHISEL_VERSION is empty" >&2; exit 1; }
[[ "$chisel_releases_commit" =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid chisel-releases commit: $chisel_releases_commit" >&2
  exit 1
}
for name in chisel_releases_sha256 chisel_amd64_sha256 chisel_arm64_sha256 homebrew_archive_sha256; do
  value=${!name}
  [[ "$value" =~ ^[0-9a-f]{64}$ ]] || {
    echo "invalid $name: $value" >&2
    exit 1
  }
done
[[ "$dockerfile_homebrew_commit" == "$homebrew_commit" ]] || {
  echo "Dockerfile Homebrew commit differs from docker-bake.hcl" >&2
  exit 1
}
[[ "$dockerfile_ruby_version" == "$portable_ruby_version" ]] || {
  echo "Dockerfile portable Ruby version differs from docker-bake.hcl" >&2
  exit 1
}
[[ "$dockerfile_keys_digest" == "$verification_keys_digest" ]] || {
  echo "Dockerfile verification key digest differs from docker-bake.hcl" >&2
  exit 1
}

for target in materializer-amd64 materializer-arm64; do
  target_commit=$(jq -er --arg target "$target" '.target[$target].args.HOMEBREW_COMMIT' <<<"$bake")
  target_ruby=$(jq -er --arg target "$target" '.target[$target].args.HOMEBREW_RUBY_VERSION' <<<"$bake")
  [[ "$target_commit" == "$homebrew_commit" ]] || {
    echo "$target Homebrew commit differs from frontend" >&2
    exit 1
  }
  [[ "$target_ruby" == "$portable_ruby_version" ]] || {
    echo "$target portable Ruby version differs from frontend" >&2
    exit 1
  }
done

dalec_module=$(go list -m -f '{{.Version}}' github.com/project-dalec/dalec)
buildkit_module=$(go list -m -f '{{.Version}}' github.com/moby/buildkit)
[[ -n "$dalec_module" && -n "$buildkit_module" ]] || {
  echo "failed to resolve release module versions" >&2
  exit 1
}

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
  --arg dalec_module "$dalec_module" \
  --arg buildkit_module "$buildkit_module" \
  '{
    schema_version: "dalec-homebrew-release-inputs/v1",
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
    dalec_module: $dalec_module,
    buildkit_module: $buildkit_module
  }'
