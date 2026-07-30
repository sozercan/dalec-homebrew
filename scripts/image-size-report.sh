#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  scripts/image-size-report.sh [OPTIONS] IMAGE [PLATFORM]

Emit a machine-readable JSON size report for an OCI image.

Arguments:
  IMAGE                 Image reference or local Docker image name.
  PLATFORM              Optional platform in os/arch[/variant] form.

Options:
  --platform PLATFORM   Explicit platform (same as positional PLATFORM).
  --top N               Number of largest entries to report (default: 25).
  --insecure            Allow an insecure HTTP registry for crane/skopeo.
  -h, --help            Show this help.

The report includes registry manifest/config/layer sizes when the reference can
be resolved, a local Docker inspect cross-check when present, flattened rootfs
statistics, dpkg package inventory, evidence files, and image history.

Required tools: docker, jq, python3.
Optional tools: crane, skopeo. When neither is installed, Docker Buildx
imagetools is used for registry metadata where possible. Crane is preferred for
registry rootfs export; otherwise a local Docker image is exported. The script
may pull or temporarily copy a registry image into Docker when no exact registry
export path is available.

Diagnostics are written to stderr. JSON is the only stdout output.
USAGE
}

fail() {
  echo "image-size-report: $*" >&2
  exit 1
}

warn() {
  echo "image-size-report: warning: $*" >&2
  printf '%s\n' "$*" >>"$WARNINGS_FILE"
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || fail "required tool not found: $1"
}

normalize_arch() {
  case "$1" in
    x86_64) printf '%s\n' amd64 ;;
    aarch64) printf '%s\n' arm64 ;;
    *) printf '%s\n' "$1" ;;
  esac
}

parse_platform() {
  local value=$1
  local os arch variant extra
  IFS=/ read -r os arch variant extra <<<"$value"
  [[ -n "${os:-}" && -n "${arch:-}" && -z "${extra:-}" ]] || return 1
  arch=$(normalize_arch "$arch")
  PLATFORM_OS=$os
  PLATFORM_ARCH=$arch
  PLATFORM_VARIANT=${variant:-}
  if [[ -n "$PLATFORM_VARIANT" ]]; then
    EFFECTIVE_PLATFORM="$PLATFORM_OS/$PLATFORM_ARCH/$PLATFORM_VARIANT"
  else
    EFFECTIVE_PLATFORM="$PLATFORM_OS/$PLATFORM_ARCH"
  fi
}

repository_from_ref() {
  local ref=$1 last
  if [[ "$ref" == *@* ]]; then
    printf '%s\n' "${ref%@*}"
    return
  fi
  last=${ref##*/}
  if [[ "$last" == *:* ]]; then
    printf '%s\n' "${ref%:*}"
  else
    printf '%s\n' "$ref"
  fi
}

sha256_file() {
  python3 - "$1" <<'PY'
import hashlib
import sys

h = hashlib.sha256()
with open(sys.argv[1], "rb") as f:
    for chunk in iter(lambda: f.read(1024 * 1024), b""):
        h.update(chunk)
print("sha256:" + h.hexdigest())
PY
}

file_size() {
  wc -c <"$1" | tr -d '[:space:]'
}

IMAGE=
REQUESTED_PLATFORM=
TOP_N=25
INSECURE=false
POSITIONAL=()

while (($#)); do
  case "$1" in
    --platform)
      (($# >= 2)) || fail "--platform requires a value"
      [[ -z "$REQUESTED_PLATFORM" ]] || fail "platform specified more than once"
      REQUESTED_PLATFORM=$2
      shift 2
      ;;
    --top)
      (($# >= 2)) || fail "--top requires a value"
      TOP_N=$2
      shift 2
      ;;
    --insecure)
      INSECURE=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      while (($#)); do POSITIONAL+=("$1"); shift; done
      ;;
    -*)
      fail "unknown option: $1"
      ;;
    *)
      POSITIONAL+=("$1")
      shift
      ;;
  esac
done

((${#POSITIONAL[@]} >= 1 && ${#POSITIONAL[@]} <= 2)) || {
  usage >&2
  exit 64
}
IMAGE=${POSITIONAL[0]}
if ((${#POSITIONAL[@]} == 2)); then
  [[ -z "$REQUESTED_PLATFORM" ]] || fail "platform specified both positionally and with --platform"
  REQUESTED_PLATFORM=${POSITIONAL[1]}
fi
[[ "$TOP_N" =~ ^[1-9][0-9]*$ ]] || fail "--top must be a positive integer"

require_tool docker
require_tool jq
require_tool python3

TMPDIR_ROOT=${TMPDIR:-/tmp}
WORK=$(mktemp -d "$TMPDIR_ROOT/dalec-homebrew-image-size.XXXXXX")
WARNINGS_FILE="$WORK/warnings.txt"
: >"$WARNINGS_FILE"
CONTAINER_ID=
TEMP_IMAGE=
cleanup() {
  if [[ -n "$CONTAINER_ID" ]]; then
    docker rm -v "$CONTAINER_ID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$TEMP_IMAGE" ]]; then
    docker image rm "$TEMP_IMAGE" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

DOCKER_DAEMON=false
if docker info >/dev/null 2>"$WORK/docker-info.err"; then
  DOCKER_DAEMON=true
else
  warn "Docker daemon is unavailable; local inspect/history/export cross-checks will be skipped"
fi

LOCAL_INSPECT_REF=
if $DOCKER_DAEMON && docker image inspect "$IMAGE" >"$WORK/docker-inspect.json" 2>"$WORK/docker-inspect.err"; then
  LOCAL_INSPECT_REF=$IMAGE
fi

PLATFORM_OS=
PLATFORM_ARCH=
PLATFORM_VARIANT=
EFFECTIVE_PLATFORM=
if [[ -n "$REQUESTED_PLATFORM" ]]; then
  parse_platform "$REQUESTED_PLATFORM" || fail "invalid platform '$REQUESTED_PLATFORM' (expected os/arch[/variant])"
elif [[ -n "$LOCAL_INSPECT_REF" ]]; then
  PLATFORM_OS=$(jq -r '.[0].Os // "linux"' "$WORK/docker-inspect.json")
  PLATFORM_ARCH=$(normalize_arch "$(jq -r '.[0].Architecture // empty' "$WORK/docker-inspect.json")")
  PLATFORM_VARIANT=$(jq -r '.[0].Variant // empty' "$WORK/docker-inspect.json")
  [[ -n "$PLATFORM_ARCH" ]] || fail "could not determine architecture from local Docker image"
  if [[ -n "$PLATFORM_VARIANT" ]]; then
    EFFECTIVE_PLATFORM="$PLATFORM_OS/$PLATFORM_ARCH/$PLATFORM_VARIANT"
  else
    EFFECTIVE_PLATFORM="$PLATFORM_OS/$PLATFORM_ARCH"
  fi
elif $DOCKER_DAEMON; then
  PLATFORM_OS=$(docker info --format '{{.OSType}}' 2>/dev/null || true)
  PLATFORM_ARCH=$(normalize_arch "$(docker info --format '{{.Architecture}}' 2>/dev/null || true)")
  if [[ -n "$PLATFORM_OS" && -n "$PLATFORM_ARCH" ]]; then
    EFFECTIVE_PLATFORM="$PLATFORM_OS/$PLATFORM_ARCH"
  fi
fi

CRANE=false
SKOPEO=false
command -v crane >/dev/null 2>&1 && CRANE=true
command -v skopeo >/dev/null 2>&1 && SKOPEO=true

crane_manifest() {
  if $INSECURE; then crane manifest --insecure "$1"; else crane manifest "$1"; fi
}

crane_config() {
  if $INSECURE; then crane config --insecure "$1"; else crane config "$1"; fi
}

crane_export() {
  if $INSECURE; then crane export --insecure "$1" -; else crane export "$1" -; fi
}

skopeo_manifest() {
  if $INSECURE; then
    skopeo inspect --raw --tls-verify=false "docker://$1"
  else
    skopeo inspect --raw "docker://$1"
  fi
}

skopeo_config() {
  if $INSECURE; then
    skopeo inspect --config --raw --tls-verify=false "docker://$1"
  else
    skopeo inspect --config --raw "docker://$1"
  fi
}

REGISTRY_AVAILABLE=false
REGISTRY_RESOLVER=
TOP_MANIFEST="$WORK/top-manifest.json"
SELECTED_MANIFEST="$WORK/selected-manifest.json"
SELECTED_CONFIG="$WORK/selected-config.json"
CONFIG_FETCH_EXACT=false
TOP_DIGEST=
SELECTED_DIGEST=
SELECTED_REF=$IMAGE
SELECTED_PLATFORM=
INDEX_DESCRIPTOR="$WORK/index-descriptor.json"
: >"$INDEX_DESCRIPTOR"

fetch_top_manifest() {
  if $CRANE && crane_manifest "$IMAGE" >"$TOP_MANIFEST" 2>"$WORK/crane-manifest.err"; then
    REGISTRY_RESOLVER=crane
    return 0
  fi
  if $SKOPEO && skopeo_manifest "$IMAGE" >"$TOP_MANIFEST" 2>"$WORK/skopeo-manifest.err"; then
    REGISTRY_RESOLVER=skopeo
    return 0
  fi
  if docker buildx imagetools inspect --raw "$IMAGE" >"$TOP_MANIFEST" 2>"$WORK/buildx-manifest.err"; then
    REGISTRY_RESOLVER=docker-buildx
    return 0
  fi
  return 1
}

fetch_manifest_ref() {
  local ref=$1 output=$2
  case "$REGISTRY_RESOLVER" in
    crane)
      crane_manifest "$ref" >"$output"
      ;;
    skopeo)
      skopeo_manifest "$ref" >"$output"
      ;;
    docker-buildx)
      docker buildx imagetools inspect --raw "$ref" >"$output"
      ;;
    *) return 1 ;;
  esac
}

fetch_config_ref() {
  local ref=$1 output=$2
  case "$REGISTRY_RESOLVER" in
    crane)
      crane_config "$ref" >"$output"
      CONFIG_FETCH_EXACT=true
      ;;
    skopeo)
      skopeo_config "$ref" >"$output"
      CONFIG_FETCH_EXACT=true
      ;;
    docker-buildx)
      docker buildx imagetools inspect --format '{{json .Image}}' "$ref" >"$output"
      CONFIG_FETCH_EXACT=false
      ;;
    *) return 1 ;;
  esac
}

if fetch_top_manifest && jq -e 'type == "object"' "$TOP_MANIFEST" >/dev/null 2>&1; then
  REGISTRY_AVAILABLE=true
  TOP_DIGEST=$(sha256_file "$TOP_MANIFEST")
  IS_INDEX=false
  if jq -e '.manifests | type == "array"' "$TOP_MANIFEST" >/dev/null 2>&1; then
    IS_INDEX=true
    [[ -n "$EFFECTIVE_PLATFORM" ]] || fail "reference resolves to an image index; specify --platform os/arch[/variant]"
    jq -e '
      all(.manifests[];
        (.digest | type == "string")
        and (.size | type == "number" and . >= 0 and floor == .))
    ' "$TOP_MANIFEST" >/dev/null 2>&1 || fail "image index contains invalid descriptors"

    CANDIDATES=$(jq -c \
      --arg os "$PLATFORM_OS" \
      --arg arch "$PLATFORM_ARCH" \
      --arg variant "$PLATFORM_VARIANT" '
        [.manifests[]
          | select(.platform | type == "object")
          | select(.platform.os | type == "string")
          | select(.platform.architecture | type == "string")
          | select(.platform.os == $os and .platform.architecture == $arch)
          | select(($variant == "") or (.platform.variant // "") == $variant)]
      ' "$TOP_MANIFEST")
    CANDIDATE_COUNT=$(jq 'length' <<<"$CANDIDATES")
    ((CANDIDATE_COUNT > 0)) || fail "image index has no child for $EFFECTIVE_PLATFORM"
    if ((CANDIDATE_COUNT > 1)); then
      fail "image index has multiple children for $EFFECTIVE_PLATFORM; specify a platform variant"
    fi
    jq '.[0]' <<<"$CANDIDATES" >"$INDEX_DESCRIPTOR"
    SELECTED_DIGEST=$(jq -r '.digest' "$INDEX_DESCRIPTOR")
    SELECTED_PLATFORM=$(jq -r '
      .platform.os + "/" + .platform.architecture
      + (if (.platform.variant // "") == "" then "" else "/" + .platform.variant end)
    ' "$INDEX_DESCRIPTOR")
    REPOSITORY=$(repository_from_ref "$IMAGE")
    SELECTED_REF="$REPOSITORY@$SELECTED_DIGEST"
    fetch_manifest_ref "$SELECTED_REF" "$SELECTED_MANIFEST" 2>"$WORK/selected-manifest.err" \
      || fail "resolved index child $SELECTED_REF but could not fetch its manifest"
    [[ "$(sha256_file "$SELECTED_MANIFEST")" == "$SELECTED_DIGEST" ]] \
      || fail "resolved index child manifest does not match descriptor digest $SELECTED_DIGEST"
  else
    cp "$TOP_MANIFEST" "$SELECTED_MANIFEST"
    SELECTED_DIGEST=$TOP_DIGEST
    REPOSITORY=$(repository_from_ref "$IMAGE")
    SELECTED_REF="$REPOSITORY@$SELECTED_DIGEST"
  fi

  jq -e '
    (.config | type == "object")
    and (.config.digest | type == "string")
    and (.config.size | type == "number" and . >= 0 and floor == .)
    and (.layers | type == "array")
    and all(.layers[];
      (.digest | type == "string")
      and (.size | type == "number" and . >= 0 and floor == .))
  ' "$SELECTED_MANIFEST" >/dev/null 2>&1 \
    || fail "registry response is not an OCI/Docker image manifest"
  fetch_config_ref "$SELECTED_REF" "$SELECTED_CONFIG" 2>"$WORK/selected-config.err" \
    || fail "could not fetch image config for $SELECTED_REF"
  jq -e 'type == "object"' "$SELECTED_CONFIG" >/dev/null 2>&1 \
    || fail "image config is not valid JSON"
  CONFIG_DIGEST=$(jq -r '.config.digest' "$SELECTED_MANIFEST")
  if $CONFIG_FETCH_EXACT; then
    [[ "$CONFIG_DIGEST" == sha256:* ]] || fail "unsupported image config digest algorithm: $CONFIG_DIGEST"
    [[ "$(sha256_file "$SELECTED_CONFIG")" == "$CONFIG_DIGEST" ]] \
      || fail "fetched image config does not match descriptor digest $CONFIG_DIGEST"
  fi

  CONFIG_OS=$(jq -r '.os // empty' "$SELECTED_CONFIG")
  CONFIG_ARCH=$(normalize_arch "$(jq -r '.architecture // empty' "$SELECTED_CONFIG")")
  CONFIG_VARIANT=$(jq -r '.variant // empty' "$SELECTED_CONFIG")
  if [[ -z "$SELECTED_PLATFORM" && -n "$CONFIG_OS" && -n "$CONFIG_ARCH" ]]; then
    SELECTED_PLATFORM="$CONFIG_OS/$CONFIG_ARCH"
    [[ -n "$CONFIG_VARIANT" ]] && SELECTED_PLATFORM="$SELECTED_PLATFORM/$CONFIG_VARIANT"
  fi
  if [[ -n "$SELECTED_PLATFORM" ]]; then
    SELECTED_OS=${SELECTED_PLATFORM%%/*}
    SELECTED_REST=${SELECTED_PLATFORM#*/}
    SELECTED_ARCH=${SELECTED_REST%%/*}
    if [[ "$SELECTED_REST" == */* ]]; then
      SELECTED_VARIANT=${SELECTED_REST#*/}
    else
      SELECTED_VARIANT=
    fi
    if [[ -n "$REQUESTED_PLATFORM" ]]; then
      [[ "$PLATFORM_OS" == "$SELECTED_OS" && "$PLATFORM_ARCH" == "$SELECTED_ARCH" ]] \
        || fail "requested platform $REQUESTED_PLATFORM does not match selected image platform $SELECTED_PLATFORM"
      if [[ -n "$PLATFORM_VARIANT" && "$PLATFORM_VARIANT" != "$SELECTED_VARIANT" ]]; then
        fail "requested platform $REQUESTED_PLATFORM does not match selected image platform $SELECTED_PLATFORM"
      fi
    fi
    PLATFORM_OS=$SELECTED_OS
    PLATFORM_ARCH=$SELECTED_ARCH
    PLATFORM_VARIANT=$SELECTED_VARIANT
    EFFECTIVE_PLATFORM=$SELECTED_PLATFORM
  fi
else
  REGISTRY_AVAILABLE=false
  REGISTRY_RESOLVER=
  warn "registry metadata could not be resolved; canonical OCI sizes will be unavailable"
fi

local_inspect_matches_selection() {
  local local_os local_arch local_variant
  local_os=$(jq -r '.[0].Os // empty' "$WORK/docker-inspect.json")
  local_arch=$(normalize_arch "$(jq -r '.[0].Architecture // empty' "$WORK/docker-inspect.json")")
  local_variant=$(jq -r '.[0].Variant // empty' "$WORK/docker-inspect.json")

  if [[ -n "$EFFECTIVE_PLATFORM" ]]; then
    [[ "$local_os" == "$PLATFORM_OS" && "$local_arch" == "$PLATFORM_ARCH" ]] || return 1
    if [[ -n "$PLATFORM_VARIANT" && "$local_variant" != "$PLATFORM_VARIANT" ]]; then
      return 1
    fi
  fi
  $REGISTRY_AVAILABLE || return 0

  local local_id config_digest manifest_digest local_diff_ids selected_diff_ids
  local_id=$(jq -r '.[0].Id // empty' "$WORK/docker-inspect.json")
  config_digest=$(jq -r '.config.digest' "$SELECTED_MANIFEST")
  manifest_digest=$SELECTED_DIGEST
  local_diff_ids=$(jq -c '.[0].RootFS.Layers // []' "$WORK/docker-inspect.json")
  selected_diff_ids=$(jq -c '.rootfs.diff_ids // []' "$SELECTED_CONFIG")
  [[ "$local_diff_ids" == "$selected_diff_ids" ]] || return 1

  if [[ "$local_id" == "$config_digest" || "$local_id" == "$manifest_digest" ]]; then
    return 0
  fi
  jq -e --arg digest "$manifest_digest" '
    any((.[0].RepoDigests // [])[]; endswith("@" + $digest))
  ' "$WORK/docker-inspect.json" >/dev/null 2>&1
}

if [[ -n "$LOCAL_INSPECT_REF" ]] && ! local_inspect_matches_selection; then
  warn "local Docker image does not match the selected registry image/platform; ignoring the local copy"
  LOCAL_INSPECT_REF=
fi

if $REGISTRY_AVAILABLE; then
  TOP_MANIFEST_BYTES=$(file_size "$TOP_MANIFEST")
  SELECTED_MANIFEST_BYTES=$(file_size "$SELECTED_MANIFEST")
  CONFIG_FETCHED_BYTES=$(file_size "$SELECTED_CONFIG")
  SIZE_SUMMARY=$(jq -n \
    --argjson selected_manifest_bytes "$SELECTED_MANIFEST_BYTES" \
    --argjson top_manifest_bytes "$TOP_MANIFEST_BYTES" \
    --argjson is_index "$IS_INDEX" \
    --slurpfile manifest "$SELECTED_MANIFEST" '
      ($manifest[0]) as $m
      | ($m.config.size) as $config
      | ([$m.layers[].size] | add // 0) as $layers
      | (if $is_index then $top_manifest_bytes else 0 end) as $index
      | {
          config_descriptor_bytes: $config,
          compressed_layer_bytes: $layers,
          oci_bytes: ($selected_manifest_bytes + $config + $layers),
          index_bytes: $index,
          total_with_index_bytes: ($selected_manifest_bytes + $config + $layers + $index)
        }
    ')
  CONFIG_DESCRIPTOR_BYTES=$(jq -r '.config_descriptor_bytes' <<<"$SIZE_SUMMARY")
  COMPRESSED_LAYER_BYTES=$(jq -r '.compressed_layer_bytes' <<<"$SIZE_SUMMARY")
  OCI_BYTES=$(jq -r '.oci_bytes' <<<"$SIZE_SUMMARY")
  INDEX_BYTES=$(jq -r '.index_bytes' <<<"$SIZE_SUMMARY")
  TOTAL_WITH_INDEX_BYTES=$(jq -r '.total_with_index_bytes' <<<"$SIZE_SUMMARY")

  jq -n \
    --arg resolver "$REGISTRY_RESOLVER" \
    --arg reference "$IMAGE" \
    --arg selected_ref "$SELECTED_REF" \
    --arg top_digest "$TOP_DIGEST" \
    --arg selected_digest "$SELECTED_DIGEST" \
    --arg selected_platform "$SELECTED_PLATFORM" \
    --argjson top_manifest_bytes "$TOP_MANIFEST_BYTES" \
    --argjson selected_manifest_bytes "$SELECTED_MANIFEST_BYTES" \
    --argjson config_descriptor_bytes "$CONFIG_DESCRIPTOR_BYTES" \
    --argjson config_fetched_bytes "$CONFIG_FETCHED_BYTES" \
    --argjson config_fetch_exact "$CONFIG_FETCH_EXACT" \
    --argjson compressed_layer_bytes "$COMPRESSED_LAYER_BYTES" \
    --argjson oci_bytes "$OCI_BYTES" \
    --argjson index_bytes "$INDEX_BYTES" \
    --argjson total_with_index_bytes "$TOTAL_WITH_INDEX_BYTES" \
    --slurpfile top "$TOP_MANIFEST" \
    --slurpfile manifest "$SELECTED_MANIFEST" \
    --slurpfile config "$SELECTED_CONFIG" '
      ($top[0]) as $t
      | ($manifest[0]) as $m
      | ($config[0]) as $c
      | {
          available: true,
          resolver: $resolver,
          reference: $reference,
          selected_reference: $selected_ref,
          selected_platform: (if $selected_platform == "" then null else $selected_platform end),
          index: (if ($t.manifests | type) == "array" then {
            digest: $top_digest,
            media_type: ($t.mediaType // null),
            size_bytes: $top_manifest_bytes,
            manifest_count: ($t.manifests | length)
          } else null end),
          manifest: {
            digest: $selected_digest,
            media_type: ($m.mediaType // null),
            size_bytes: $selected_manifest_bytes
          },
          config: {
            digest: $m.config.digest,
            media_type: ($m.config.mediaType // null),
            descriptor_size_bytes: $config_descriptor_bytes,
            fetched_size_bytes: $config_fetched_bytes,
            fetched_size_is_exact: $config_fetch_exact,
            descriptor_matches_fetched_size:
              (if $config_fetch_exact then $config_descriptor_bytes == $config_fetched_bytes else null end),
            architecture: ($c.architecture // null),
            os: ($c.os // null),
            variant: ($c.variant // null)
          },
          layers: [$m.layers | to_entries[] | {
            index: .key,
            digest: .value.digest,
            media_type: (.value.mediaType // null),
            compressed_size_bytes: .value.size
          }],
          compressed_layer_size_bytes: $compressed_layer_bytes,
          oci_size_bytes: $oci_bytes,
          index_size_bytes: $index_bytes,
          oci_size_with_index_bytes: $total_with_index_bytes
        }
    ' >"$WORK/registry.json"

  jq -n --slurpfile manifest "$SELECTED_MANIFEST" --slurpfile config "$SELECTED_CONFIG" '
    ($manifest[0]) as $m
    | ($config[0]) as $c
    | reduce (($c.history // [])[]) as $h
        ({layer_index: 0, entries: []};
          if ($h.empty_layer // false) then
            .entries += [$h + {layer: null}]
          else
            . as $state
            | .entries += [$h + {layer: (
                if $state.layer_index < ($m.layers | length) then {
                  index: $state.layer_index,
                  digest: $m.layers[$state.layer_index].digest,
                  media_type: ($m.layers[$state.layer_index].mediaType // null),
                  compressed_size_bytes: $m.layers[$state.layer_index].size,
                  diff_id: ($c.rootfs.diff_ids[$state.layer_index] // null)
                } else null end
              )}]
            | .layer_index += 1
          end)
    | .entries
  ' >"$WORK/registry-history.json"
else
  jq -n --arg error "registry metadata unavailable; install crane or skopeo, or use a registry supported by docker buildx imagetools" \
    '{available:false,error:$error}' >"$WORK/registry.json"
  printf '[]\n' >"$WORK/registry-history.json"
fi

# Retry local Docker inspection by config digest when the input reference itself
# is not tagged in the daemon.
if $DOCKER_DAEMON && [[ -z "$LOCAL_INSPECT_REF" ]] && $REGISTRY_AVAILABLE; then
  CONFIG_DIGEST=$(jq -r '.config.digest' "$SELECTED_MANIFEST")
  if docker image inspect "$CONFIG_DIGEST" >"$WORK/docker-inspect.json" 2>"$WORK/docker-inspect.err"; then
    if local_inspect_matches_selection; then
      LOCAL_INSPECT_REF=$CONFIG_DIGEST
    fi
  fi
fi

if [[ -n "$LOCAL_INSPECT_REF" ]]; then
  docker history --no-trunc --human=false --format '{{json .}}' "$LOCAL_INSPECT_REF" \
    2>"$WORK/docker-history.err" \
    | jq -s 'map({
        id: .ID,
        created_at: .CreatedAt,
        created_since: .CreatedSince,
        created_by: .CreatedBy,
        size_bytes: (.Size | tonumber),
        comment: .Comment
      })' >"$WORK/docker-history.json" || printf '[]\n' >"$WORK/docker-history.json"

  jq --arg ref "$LOCAL_INSPECT_REF" '.[0] | {
    available: true,
    inspected_reference: $ref,
    id: .Id,
    repo_tags: (.RepoTags // []),
    repo_digests: (.RepoDigests // []),
    created: .Created,
    architecture: .Architecture,
    os: .Os,
    variant: (.Variant // null),
    inspect_size_bytes: .Size,
    rootfs_diff_ids: (.RootFS.Layers // [])
  }' "$WORK/docker-inspect.json" >"$WORK/docker.json"
else
  printf '[]\n' >"$WORK/docker-history.json"
  jq -n --arg error "image is not present in the local Docker daemon" \
    '{available:false,error:$error}' >"$WORK/docker.json"
fi

cat >"$WORK/analyze-rootfs.py" <<'PY'
import json
import posixpath
import sys
import tarfile

TOP_N = int(sys.argv[1])
SOURCE = sys.argv[2]

RUNTIME_INJECTED = {
    "/.dockerenv",
    "/etc/hostname",
    "/etc/hosts",
    "/etc/mtab",
    "/etc/resolv.conf",
}
RUNTIME_PREFIXES = ("/dev", "/proc", "/sys")


def normalize(name):
    while name.startswith("./"):
        name = name[2:]
    name = name.lstrip("/")
    value = posixpath.normpath("/" + name)
    return value if value != "/." else "/"


def excluded(path):
    if path in RUNTIME_INJECTED:
        return True
    return any(path == prefix or path.startswith(prefix + "/") for prefix in RUNTIME_PREFIXES)


def parents(path):
    current = posixpath.dirname(path)
    while True:
        yield current
        if current == "/":
            break
        current = posixpath.dirname(current)


def parse_dpkg_status(data):
    packages = []
    text = data.decode("utf-8", "replace")
    for paragraph in text.split("\n\n"):
        fields = {}
        current = None
        for line in paragraph.splitlines():
            if line.startswith((" ", "\t")) and current:
                fields[current] += "\n" + line[1:]
                continue
            if ":" not in line:
                current = None
                continue
            current, value = line.split(":", 1)
            fields[current] = value.strip()
        if fields.get("Status") != "install ok installed" or "Package" not in fields:
            continue
        try:
            installed_size = int(fields.get("Installed-Size", "0"))
        except ValueError:
            installed_size = 0
        name = fields["Package"]
        architecture = fields.get("Architecture")
        if fields.get("Multi-Arch") == "same" and architecture:
            name += ":" + architecture
        packages.append({
            "name": name,
            "version": fields.get("Version"),
            "architecture": architecture,
            "installed_size_kib": installed_size,
        })
    packages.sort(key=lambda p: (-p["installed_size_kib"], p["name"]))
    return packages


directories = {"/"}
regular_sizes = {}
hardlinks = {}
symlinks = set()
other = set()
directory_bytes = {}
excluded_paths = []
dpkg_status = None

archive = tarfile.open(fileobj=sys.stdin.buffer, mode="r|*")
for member in archive:
    path = normalize(member.name)
    if path == "/":
        directories.add(path)
        continue
    if excluded(path):
        excluded_paths.append(path)
        continue
    for parent in parents(path):
        directories.add(parent)
    if member.isdir():
        directories.add(path)
    elif member.isreg():
        regular_sizes[path] = member.size
        for parent in parents(path):
            directory_bytes[parent] = directory_bytes.get(parent, 0) + member.size
        if path == "/var/lib/dpkg/status":
            f = archive.extractfile(member)
            if f is not None:
                dpkg_status = f.read()
    elif member.islnk():
        hardlinks[path] = normalize(member.linkname)
    elif member.issym():
        symlinks.add(path)
    else:
        other.add(path)


def hardlink_size(path, seen=None):
    if path in regular_sizes:
        return regular_sizes[path]
    if path not in hardlinks:
        return 0
    if seen is None:
        seen = set()
    if path in seen:
        return 0
    seen.add(path)
    return hardlink_size(hardlinks[path], seen)

files = [
    {"path": path, "size_bytes": size, "type": "regular"}
    for path, size in regular_sizes.items()
]
files.extend(
    {"path": path, "size_bytes": hardlink_size(path), "type": "hardlink"}
    for path in hardlinks
)
files.sort(key=lambda entry: (-entry["size_bytes"], entry["path"]))

largest_directories = [
    {"path": path, "size_bytes": size}
    for path, size in directory_bytes.items()
]
largest_directories.sort(key=lambda entry: (-entry["size_bytes"], entry["path"]))

packages = parse_dpkg_status(dpkg_status) if dpkg_status is not None else []
evidence_files = sorted(
    path for path in list(regular_sizes) + list(hardlinks)
    if posixpath.dirname(path) == "/usr/share/dalec-homebrew"
)
rootfs_size = sum(regular_sizes.values())

json.dump({
    "available": True,
    "source": SOURCE,
    "measurement": "flattened tar regular payload bytes; hardlinked payloads counted once",
    "size_bytes": rootfs_size,
    "size_kib_rounded_up": (rootfs_size + 1023) // 1024,
    "counts": {
        "files": len(regular_sizes) + len(hardlinks),
        "directories": len(directories),
        "symlinks": len(symlinks),
        "hardlinks": len(hardlinks),
        "other_entries": len(other),
        "packages": len(packages),
        "evidence_files": len(evidence_files),
    },
    "runtime_injected_entries_excluded": {
        "count": len(set(excluded_paths)),
        "paths": sorted(set(excluded_paths)),
        "policy": [
            "/.dockerenv",
            "/dev/**",
            "/proc/**",
            "/sys/**",
            "/etc/hostname",
            "/etc/hosts",
            "/etc/mtab",
            "/etc/resolv.conf",
        ],
    },
    "evidence": {
        "directory": "/usr/share/dalec-homebrew",
        "files": evidence_files,
    },
    "packages": {
        "database": "dpkg" if dpkg_status is not None else None,
        "installed_size_kib_total": sum(p["installed_size_kib"] for p in packages),
        "largest": packages[:TOP_N],
    },
    "largest_directories": largest_directories[:TOP_N],
    "largest_files": files[:TOP_N],
}, sys.stdout, sort_keys=True)
sys.stdout.write("\n")
PY

ROOTFS_READY=false
if $REGISTRY_AVAILABLE && $CRANE; then
  if crane_export "$SELECTED_REF" 2>"$WORK/crane-export.err" \
      | python3 "$WORK/analyze-rootfs.py" "$TOP_N" "registry-export:$SELECTED_REF" >"$WORK/rootfs.json"; then
    ROOTFS_READY=true
  else
    warn "crane rootfs export failed; trying a local Docker export"
  fi
fi

# If no exact registry export is available, use an existing local image. As a
# last resort, pull/copy a selected child into a temporary Docker tag.
if ! $ROOTFS_READY && [[ -z "$LOCAL_INSPECT_REF" ]] && $DOCKER_DAEMON; then
  PULL_REF=$IMAGE
  $REGISTRY_AVAILABLE && PULL_REF=$SELECTED_REF
  if [[ -n "$EFFECTIVE_PLATFORM" ]] && docker pull --platform "$EFFECTIVE_PLATFORM" "$PULL_REF" >"$WORK/docker-pull.out" 2>"$WORK/docker-pull.err"; then
    if docker image inspect "$PULL_REF" >"$WORK/docker-inspect.json" 2>"$WORK/docker-inspect.err" && local_inspect_matches_selection; then
      LOCAL_INSPECT_REF=$PULL_REF
    fi
  elif docker pull "$PULL_REF" >"$WORK/docker-pull.out" 2>"$WORK/docker-pull.err"; then
    if docker image inspect "$PULL_REF" >"$WORK/docker-inspect.json" 2>"$WORK/docker-inspect.err" && local_inspect_matches_selection; then
      LOCAL_INSPECT_REF=$PULL_REF
    fi
  fi
fi

if ! $ROOTFS_READY && [[ -z "$LOCAL_INSPECT_REF" ]] && $REGISTRY_AVAILABLE && $SKOPEO && $DOCKER_DAEMON; then
  TEMP_IMAGE="dalec-homebrew-image-size-report:tmp-$$-$RANDOM"
  SKOPEO_COPY=(skopeo --insecure-policy)
  [[ -n "$PLATFORM_OS" ]] && SKOPEO_COPY+=(--override-os "$PLATFORM_OS")
  [[ -n "$PLATFORM_ARCH" ]] && SKOPEO_COPY+=(--override-arch "$PLATFORM_ARCH")
  [[ -n "$PLATFORM_VARIANT" ]] && SKOPEO_COPY+=(--override-variant "$PLATFORM_VARIANT")
  SKOPEO_COPY+=(copy)
  $INSECURE && SKOPEO_COPY+=(--src-tls-verify=false)
  SKOPEO_COPY+=("docker://$SELECTED_REF" "docker-daemon:$TEMP_IMAGE")
  if "${SKOPEO_COPY[@]}" >"$WORK/skopeo-copy.out" 2>"$WORK/skopeo-copy.err"; then
    docker image inspect "$TEMP_IMAGE" >"$WORK/docker-inspect.json"
    if local_inspect_matches_selection; then
      LOCAL_INSPECT_REF=$TEMP_IMAGE
    fi
  fi
fi

if ! $ROOTFS_READY && [[ -n "$LOCAL_INSPECT_REF" ]]; then
  DECLARED_VOLUME_COUNT=$(jq '.[0].Config.Volumes // {} | length' "$WORK/docker-inspect.json")
  if ((DECLARED_VOLUME_COUNT > 0)); then
    fail "local Docker export would mask declared volume paths; install crane and use a registry-resolvable reference"
  fi
  CREATE_ARGS=(docker create)
  [[ -n "$EFFECTIVE_PLATFORM" ]] && CREATE_ARGS+=(--platform "$EFFECTIVE_PLATFORM")
  CREATE_ARGS+=("$LOCAL_INSPECT_REF")
  if CONTAINER_ID=$("${CREATE_ARGS[@]}" 2>"$WORK/docker-create.err"); then
    if docker export "$CONTAINER_ID" 2>"$WORK/docker-export.err" \
        | python3 "$WORK/analyze-rootfs.py" "$TOP_N" "docker-export:$LOCAL_INSPECT_REF" >"$WORK/rootfs.json"; then
      ROOTFS_READY=true
    fi
    docker rm -v "$CONTAINER_ID" >/dev/null 2>&1 || true
    CONTAINER_ID=
  fi
fi

$ROOTFS_READY || fail "could not export the image rootfs; install crane, make the image available to Docker, or configure registry access"

# A pull or skopeo copy may have made a local cross-check available after the
# first Docker section was generated. Refresh it and its history.
if [[ -n "$LOCAL_INSPECT_REF" ]] && $DOCKER_DAEMON; then
  docker image inspect "$LOCAL_INSPECT_REF" >"$WORK/docker-inspect.json"
  docker history --no-trunc --human=false --format '{{json .}}' "$LOCAL_INSPECT_REF" \
    2>"$WORK/docker-history.err" \
    | jq -s 'map({
        id: .ID,
        created_at: .CreatedAt,
        created_since: .CreatedSince,
        created_by: .CreatedBy,
        size_bytes: (.Size | tonumber),
        comment: .Comment
      })' >"$WORK/docker-history.json" || printf '[]\n' >"$WORK/docker-history.json"
  jq --arg ref "$LOCAL_INSPECT_REF" '.[0] | {
    available: true,
    inspected_reference: $ref,
    id: .Id,
    repo_tags: (.RepoTags // []),
    repo_digests: (.RepoDigests // []),
    created: .Created,
    architecture: .Architecture,
    os: .Os,
    variant: (.Variant // null),
    inspect_size_bytes: .Size,
    rootfs_diff_ids: (.RootFS.Layers // [])
  }' "$WORK/docker-inspect.json" >"$WORK/docker.json"
fi

jq -Rsc 'split("\n") | map(select(length > 0))' "$WARNINGS_FILE" >"$WORK/warnings.json"

GENERATED_AT=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
DOCKER_VERSION=$(docker version --format '{{.Client.Version}}' 2>/dev/null || docker --version 2>/dev/null || true)
JQ_VERSION=$(jq --version 2>/dev/null || true)
CRANE_VERSION=
SKOPEO_VERSION=
$CRANE && CRANE_VERSION=$(crane version 2>/dev/null | head -1 || true)
$SKOPEO && SKOPEO_VERSION=$(skopeo --version 2>/dev/null || true)

jq -n \
  --arg schema_version "dalec-homebrew-image-size-report/v1" \
  --arg generated_at "$GENERATED_AT" \
  --arg image "$IMAGE" \
  --arg requested_platform "$REQUESTED_PLATFORM" \
  --arg effective_platform "$EFFECTIVE_PLATFORM" \
  --arg docker_version "$DOCKER_VERSION" \
  --arg jq_version "$JQ_VERSION" \
  --arg crane_version "$CRANE_VERSION" \
  --arg skopeo_version "$SKOPEO_VERSION" \
  --slurpfile registry "$WORK/registry.json" \
  --slurpfile docker "$WORK/docker.json" \
  --slurpfile rootfs "$WORK/rootfs.json" \
  --slurpfile registry_history "$WORK/registry-history.json" \
  --slurpfile docker_history "$WORK/docker-history.json" \
  --slurpfile warnings "$WORK/warnings.json" '
    {
      schema_version: $schema_version,
      generated_at: $generated_at,
      image: $image,
      platform: {
        requested: (if $requested_platform == "" then null else $requested_platform end),
        effective: (if $effective_platform == "" then null else $effective_platform end)
      },
      tools: {
        docker: $docker_version,
        jq: $jq_version,
        crane: (if $crane_version == "" then null else $crane_version end),
        skopeo: (if $skopeo_version == "" then null else $skopeo_version end)
      },
      registry: $registry[0],
      docker: $docker[0],
      cross_checks: {
        registry_oci_and_docker_inspect:
          (if ($registry[0].available and $docker[0].available) then
            {
              registry_oci_size_bytes: $registry[0].oci_size_bytes,
              docker_inspect_size_bytes: $docker[0].inspect_size_bytes,
              comparison: "reported separately; Docker size semantics vary by daemon storage backend"
            }
          else null end)
      },
      rootfs: $rootfs[0],
      history: {
        registry_order: "oldest_to_newest",
        registry: $registry_history[0],
        docker_order: "newest_to_oldest",
        docker: $docker_history[0]
      },
      warnings: $warnings[0]
    }
  '
