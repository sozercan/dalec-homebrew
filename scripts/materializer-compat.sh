#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

IMAGE=
PLATFORM=
CURRENT=0
MAX_BOTTLE_BYTES=$((256 << 20))

fail_usage() {
  echo "$*" >&2
  exit 64
}

while (( $# > 0 )); do
  case "$1" in
    --image)
      (( $# >= 2 )) || fail_usage "--image requires a value"
      IMAGE=$2
      shift 2
      ;;
    --platform)
      (( $# >= 2 )) || fail_usage "--platform requires a value"
      PLATFORM=$2
      shift 2
      ;;
    --current)
      CURRENT=1
      shift
      ;;
    *) fail_usage "unknown argument: $1" ;;
  esac
done

[[ -n "$IMAGE" ]] || fail_usage "--image is required"
[[ "$IMAGE" != -* && "$IMAGE" != *[[:space:]]* ]] || fail_usage "invalid image reference: $IMAGE"

case "$PLATFORM" in
  linux/amd64)
    ARCH=amd64
    FIXTURE_FILENAME=glibc--2.39_1.x86_64_linux.bottle.2.tar.gz
    FIXTURE_DIGEST=sha256:2fe24afa0fd4034a66340b61fbe952ec7628d5eb30358191730c9ba2b6a1d421
    FIXTURE_SIZE=14716331
    ;;
  linux/arm64)
    ARCH=arm64
    FIXTURE_FILENAME=glibc--2.39_1.arm64_linux.bottle.2.tar.gz
    FIXTURE_DIGEST=sha256:01a46dbd217ab6379da50f9285da81ebaf0928c3457fbb04542b745bc80cb27f
    FIXTURE_SIZE=13869188
    ;;
  *) fail_usage "unsupported --platform: $PLATFORM" ;;
esac

for tool in curl docker jq sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || fail_usage "required tool is unavailable: $tool"
done
if (( CURRENT == 1 )); then
  command -v go >/dev/null 2>&1 || fail_usage "go is required with --current"
fi

TMPDIR_ROOT=${TMPDIR:-/tmp}
WORK=$(mktemp -d "$TMPDIR_ROOT/dalec-homebrew-materializer-compat.XXXXXX")
cleanup() {
  if [[ -n "${WORK:-}" && -d "$WORK" && "$WORK" == "$TMPDIR_ROOT"/dalec-homebrew-materializer-compat.* ]]; then
    rm -rf -- "$WORK"
  fi
}
trap cleanup EXIT

REPOSITORY=ghcr.io/homebrew/core/glibc
PKG_VERSION=2.39_1
FORMULA_SHA256=b97b976075f287a235262dd8d0b8ca3eecb6a6c6ddeff3d5ebb7c3123b20bc56
VERIFY_RECORD="$ROOT/scripts/testdata/materializer-compat/glibc-2.39_1-$ARCH-resolution.json"

if (( CURRENT == 1 )); then
  RECORD="$WORK/resolution.json"
  GOWORK=off GOFLAGS='' go run ./cmd/resolve \
    --roots glibc \
    --arch "$ARCH" \
    --output "$RECORD"

  [[ $(jq -er '[.nodes[] | select(.name == "glibc")] | length' "$RECORD") == 1 ]] || {
    echo "current resolution must contain exactly one glibc node" >&2
    exit 1
  }
  REPOSITORY=$(jq -er '.nodes[] | select(.name == "glibc") | .bottle.repository' "$RECORD")
  FILENAME=$(jq -er '.nodes[] | select(.name == "glibc") | .bottle.filename' "$RECORD")
  DIGEST=$(jq -er '.nodes[] | select(.name == "glibc") | .bottle.layer.digest' "$RECORD")
  SIZE=$(jq -er '.nodes[] | select(.name == "glibc") | .bottle.layer.size' "$RECORD")
  MEDIA_TYPE=$(jq -er '.nodes[] | select(.name == "glibc") | .bottle.layer.media_type' "$RECORD")
  HOMEBREW_SHA256=$(jq -er '.nodes[] | select(.name == "glibc") | .bottle.homebrew_sha256' "$RECORD")
  PKG_VERSION=$(jq -er '.nodes[] | select(.name == "glibc") | .pkg_version' "$RECORD")
  FORMULA_SHA256=
  VERIFY_RECORD=$RECORD

  [[ "$MEDIA_TYPE" == application/vnd.oci.image.layer.v1.tar+gzip ]] || {
    echo "current glibc layer has unsupported media type: $MEDIA_TYPE" >&2
    exit 1
  }
  [[ "$HOMEBREW_SHA256" == "${DIGEST#sha256:}" ]] || {
    echo "current glibc layer digest does not match its authenticated Homebrew checksum" >&2
    exit 1
  }
else
  FILENAME=$FIXTURE_FILENAME
  DIGEST=$FIXTURE_DIGEST
  SIZE=$FIXTURE_SIZE
fi

[[ "$REPOSITORY" == ghcr.io/homebrew/core/glibc ]] || {
  echo "unexpected glibc bottle repository: $REPOSITORY" >&2
  exit 1
}
[[ "$FILENAME" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]*\.tar\.gz$ ]] || {
  echo "invalid glibc bottle filename: $FILENAME" >&2
  exit 1
}
[[ "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "invalid glibc bottle digest: $DIGEST" >&2
  exit 1
}
[[ "$SIZE" =~ ^[1-9][0-9]*$ && ${#SIZE} -le 9 ]] || {
  echo "invalid glibc bottle size: $SIZE" >&2
  exit 1
}
(( SIZE <= MAX_BOTTLE_BYTES )) || {
  echo "glibc bottle exceeds the compatibility limit: $SIZE" >&2
  exit 1
}
[[ "$PKG_VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]*$ ]] || {
  echo "invalid glibc package version: $PKG_VERSION" >&2
  exit 1
}

TOKEN=$(curl --fail --location --silent --show-error \
  --retry 3 --retry-all-errors \
  --proto '=https' --tlsv1.2 \
  --max-filesize 1048576 \
  --get \
  --data-urlencode 'scope=repository:homebrew/core/glibc:pull' \
  --data-urlencode 'service=ghcr.io' \
  https://ghcr.io/token | jq -er '.token | select(type == "string" and length > 0)')

BOTTLE="$WORK/$FILENAME"
curl --fail --location --silent --show-error \
  --retry 3 --retry-all-errors \
  --proto '=https' --tlsv1.2 \
  --max-filesize "$MAX_BOTTLE_BYTES" \
  --header "Authorization: Bearer $TOKEN" \
  "https://ghcr.io/v2/homebrew/core/glibc/blobs/$DIGEST" \
  --output "$BOTTLE"

ACTUAL_SIZE=$(wc -c < "$BOTTLE" | tr -d '[:space:]')
[[ "$ACTUAL_SIZE" == "$SIZE" ]] || {
  echo "glibc bottle size is $ACTUAL_SIZE, expected $SIZE" >&2
  exit 1
}
printf '%s  %s\n' "${DIGEST#sha256:}" "$BOTTLE" | sha256sum -c -

FORMULA_PATH="glibc/$PKG_VERSION/.brew/glibc.rb"
CONTAINER_BOTTLE="/run/dalec-homebrew/$FILENAME"
CONTAINER_RECORD=/run/dalec-homebrew/resolution.json
VERIFIED="$WORK/verified.json"

# The fixed records are reviewed security fixtures, not replay inputs. They
# project the authenticated bottle facts needed by the production archive
# verifier while keeping this compatibility check independent of moving
# metadata. The --current path uses its freshly authenticated resolver record.
docker run --rm \
  --network none \
  --platform "$PLATFORM" \
  --user linuxbrew \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --pids-limit 512 \
  --memory 2g \
  --cpus 2 \
  --mount "type=bind,src=$VERIFY_RECORD,dst=$CONTAINER_RECORD,readonly" \
  --mount "type=bind,src=$BOTTLE,dst=$CONTAINER_BOTTLE,readonly" \
  --entrypoint /usr/local/bin/dalec-homebrew-materializer \
  "$IMAGE" \
  verify-bottle \
  --resolution "$CONTAINER_RECORD" \
  --name glibc \
  --file "$CONTAINER_BOTTLE" \
  > "$VERIFIED"

jq -e \
  --arg digest "$DIGEST" \
  --arg formula_path "$FORMULA_PATH" \
  --arg formula_sha256 "$FORMULA_SHA256" \
  --arg pkg_version "$PKG_VERSION" \
  --arg size "$SIZE" '
    .name == "glibc" and
    .pkg_version == $pkg_version and
    .compressed_sha256 == $digest and
    .compressed_size == ($size | tonumber) and
    .homebrew_sha256 == ($digest | ltrimstr("sha256:")) and
    .formula.path == $formula_path and
    .formula.class_name == "Glibc" and
    .formula.size > 0 and
    ($formula_sha256 == "" or .formula.sha256 == ("sha256:" + $formula_sha256))
  ' "$VERIFIED" >/dev/null

docker run --rm \
  --network none \
  --platform "$PLATFORM" \
  --user linuxbrew \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --pids-limit 512 \
  --memory 2g \
  --cpus 2 \
  --env HOME=/home/linuxbrew \
  --env USER=linuxbrew \
  --env LOGNAME=linuxbrew \
  --env HOMEBREW_NO_AUTO_UPDATE=1 \
  --env HOMEBREW_NO_INSTALL_FROM_API=1 \
  --env HOMEBREW_NO_ANALYTICS=1 \
  --env HOMEBREW_NO_INSTALL_CLEANUP=1 \
  --env HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1 \
  --mount "type=bind,src=$BOTTLE,dst=$CONTAINER_BOTTLE,readonly" \
  --entrypoint /bin/bash \
  "$IMAGE" \
  -ceu '
    bottle=$1
    pkg_version=$2
    prefix=/home/linuxbrew/.linuxbrew
    keg="$prefix/Cellar/glibc/$pkg_version"
    receipt="$keg/INSTALL_RECEIPT.json"
    brew="$prefix/Homebrew/bin/brew"

    test -r "$bottle"
    test ! -w "$bottle"
    "$brew" ruby /usr/local/libexec/dalec-homebrew-pour.rb "$bottle"
    test -x "$keg/bin/locale"
    test -s "$keg/lib/locale/locale-archive"
    test -f "$receipt"
    "$brew" ruby -rjson -e \
      "receipt = JSON.parse(File.binread(ARGV.fetch(0))); abort %q{invalid bottle receipt} unless receipt[%q{built_as_bottle}] == true && receipt[%q{poured_from_bottle}] == true" \
      "$receipt"
  ' -- "$CONTAINER_BOTTLE" "$PKG_VERSION"
