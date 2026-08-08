#!/usr/bin/env bash
set -euo pipefail

SSH_TARGET=${DALEC_HOMEBREW_VM_SSH_TARGET:-vm}

usage() {
  cat >&2 <<'USAGE'
usage: scripts/vm-live-validate.sh <image-ref> [amd64|arm64]

Pulls the image on the SSH target (default: vm), resolves the pulled image to
its immutable local image ID, inspects its OCI platform and configured user,
and validates the final runtime under restrictive Docker settings.

Set DALEC_HOMEBREW_VM_SSH_TARGET to override the SSH target.
USAGE
}

die() {
  echo "vm-live-validate: $*" >&2
  exit 1
}

remote_quote() {
  local value=$1
  value=${value//\'/\'\\\'\'}
  printf "'%s'" "$value"
}

remote() {
  local arg command quoted
  command=
  for arg in "$@"; do
    quoted=$(remote_quote "$arg")
    if [[ -n "$command" ]]; then
      command+=" "
    fi
    command+="$quoted"
  done
  # Every remote argument is single-quoted by remote_quote before expansion.
  # shellcheck disable=SC2029
  ssh "$SSH_TARGET" "$command"
}

normalize_arch() {
  case "$1" in
    amd64 | linux/amd64 | x86_64)
      printf '%s\n' amd64
      ;;
    arm64 | linux/arm64 | aarch64)
      printf '%s\n' arm64
      ;;
    *)
      return 1
      ;;
  esac
}

if (( $# < 1 || $# > 2 )); then
  usage
  exit 64
fi

IMAGE_REF=$1
EXPECTED_ARCH=${2:-}

[[ -n "$IMAGE_REF" ]] || die "image reference must not be empty"
command -v ssh >/dev/null 2>&1 || die "ssh is required"

if [[ -n "$EXPECTED_ARCH" ]]; then
  EXPECTED_ARCH=$(normalize_arch "$EXPECTED_ARCH") || {
    usage
    die "unsupported expected architecture: $2"
  }
fi

echo "==> Checking Docker on SSH target $SSH_TARGET"
remote docker version --format '{{.Server.Version}}' >/dev/null

echo "==> Pulling $IMAGE_REF on $SSH_TARGET"
remote docker image pull "$IMAGE_REF"

IMAGE_ID=$(remote docker image inspect --format '{{.Id}}' "$IMAGE_REF")
[[ "$IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  die "docker returned an invalid image ID for $IMAGE_REF: $IMAGE_ID"
}

IMAGE_OS=$(remote docker image inspect --format '{{.Os}}' "$IMAGE_ID")
IMAGE_ARCH=$(remote docker image inspect --format '{{.Architecture}}' "$IMAGE_ID")
CONFIG_USER=$(remote docker image inspect --format '{{.Config.User}}' "$IMAGE_ID")
REPO_DIGESTS=$(remote docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$IMAGE_ID")
RESOLVED_REF=$(printf '%s\n' "$REPO_DIGESTS" | sed -n '1p')
if [[ -z "$RESOLVED_REF" ]]; then
  RESOLVED_REF=$IMAGE_ID
fi

[[ "$IMAGE_OS" == linux ]] || die "image OS is $IMAGE_OS, expected linux"
case "$IMAGE_ARCH" in
  amd64 | arm64) ;;
  *) die "unsupported image architecture: $IMAGE_ARCH" ;;
esac
if [[ -n "$EXPECTED_ARCH" && "$IMAGE_ARCH" != "$EXPECTED_ARCH" ]]; then
  die "image architecture is $IMAGE_ARCH, expected $EXPECTED_ARCH"
fi
case "$CONFIG_USER" in
  '' | root | root:* | 0 | 0:*)
    die "image configured user is root or unset: ${CONFIG_USER:-<unset>}"
    ;;
esac

cat <<REPORT
==> Resolved image
    requested:   $IMAGE_REF
    repo digest: $RESOLVED_REF
    image ID:    $IMAGE_ID
    platform:    $IMAGE_OS/$IMAGE_ARCH
    user:        $CONFIG_USER
REPORT

echo "==> Running restricted runtime validation"
remote docker run \
  --rm \
  --interactive \
  --network none \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
  --entrypoint /bin/sh \
  "$IMAGE_ID" \
  -s <<'CONTAINER_VALIDATOR'
set -eu

fail() {
  echo "runtime validation failed: $*" >&2
  exit 1
}

assert_absent() {
  if [ -e "$1" ] || [ -L "$1" ]; then
    fail "forbidden artifact exists: $1"
  fi
}

assert_root_owned_non_user_writable() {
  checked_path=$1
  if [ ! -e "$checked_path" ] && [ ! -L "$checked_path" ]; then
    return 0
  fi

  bad_owner=$(find "$checked_path" -xdev \( ! -uid 0 -o ! -gid 0 \) -print -quit)
  if [ -n "$bad_owner" ]; then
    fail "runtime code is not root-owned: $bad_owner"
  fi

  bad_mode=$(find "$checked_path" -xdev ! -type l -perm /022 -print -quit)
  if [ -n "$bad_mode" ]; then
    fail "runtime code is group/other-writable: $bad_mode"
  fi
}

PREFIX=${HOMEBREW_PREFIX:-/home/linuxbrew/.linuxbrew}
EVIDENCE_DIR=/usr/share/dalec-homebrew

[ -r /etc/os-release ] || fail "/etc/os-release is missing"
# shellcheck disable=SC1091
. /etc/os-release
[ "${ID:-}" = ubuntu ] || fail "OS ID is ${ID:-<unset>}, expected ubuntu"
[ "${VERSION_ID:-}" = 24.04 ] || fail "VERSION_ID is ${VERSION_ID:-<unset>}, expected 24.04"
case "${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}" in
  noble) ;;
  *) fail "Ubuntu codename is ${VERSION_CODENAME:-${UBUNTU_CODENAME:-<unset>}}, expected noble" ;;
esac

runtime_uid=$(id -u)
runtime_gid=$(id -g)
[ "$runtime_uid:$runtime_gid" = 1000:1000 ] || {
  fail "runtime identity is $runtime_uid:$runtime_gid, expected 1000:1000"
}
case " $(id -G) " in
  *' 0 '*) fail "runtime user unexpectedly belongs to GID 0" ;;
esac

[ -d "$PREFIX" ] || fail "Homebrew prefix is missing: $PREFIX"
[ -d "$PREFIX/Cellar" ] || fail "Homebrew Cellar is missing: $PREFIX/Cellar"
[ "$(stat -c '%u:%g' "$PREFIX")" = 0:0 ] || fail "Homebrew prefix is not root-owned"

for code_path in \
  "$PREFIX/Cellar" \
  "$PREFIX/opt" \
  "$PREFIX/bin" \
  "$PREFIX/sbin" \
  "$PREFIX/lib" \
  "$PREFIX/libexec" \
  "$PREFIX/share" \
  "$PREFIX/etc"
do
  assert_root_owned_non_user_writable "$code_path"
done

for evidence_name in \
  manifest.json \
  resolution.json \
  runtime-inventory.json \
  prune-manifest.json \
  sbom.spdx.json \
  materialization.json \
  runtime-base-packages.tsv \
  runtime-base-artifacts.tsv \
  runtime-base-chisel.manifest.wall
do
  evidence_path=$EVIDENCE_DIR/$evidence_name
  [ -f "$evidence_path" ] || fail "evidence file is missing or not regular: $evidence_path"
  [ ! -L "$evidence_path" ] || fail "evidence file must not be a symlink: $evidence_path"
  [ -s "$evidence_path" ] || fail "evidence file is empty: $evidence_path"
  assert_root_owned_non_user_writable "$evidence_path"
done

grep -Fq '"schema_version":"dalec-homebrew-runtime-manifest/v2"' \
  "$EVIDENCE_DIR/manifest.json" || fail "runtime manifest schema is invalid"
grep -Fq '"schema_version":"dalec-homebrew-resolution/v2"' \
  "$EVIDENCE_DIR/resolution.json" || fail "resolution schema is invalid"
grep -Fq '"schema_version":"dalec-homebrew-runtime-inventory/v2"' \
  "$EVIDENCE_DIR/runtime-inventory.json" || fail "runtime inventory schema is invalid"
grep -Fq '"schema_version":"dalec-homebrew-prune-manifest/v3"' \
  "$EVIDENCE_DIR/prune-manifest.json" || fail "prune manifest schema is invalid"
grep -Fq '"spdxVersion":"SPDX-2.3"' \
  "$EVIDENCE_DIR/sbom.spdx.json" || fail "SBOM is not SPDX 2.3"
grep -Fq '"name":"dalec-homebrew-linux-' \
  "$EVIDENCE_DIR/sbom.spdx.json" || fail "SBOM document identity is invalid"

for forbidden_path in \
  /__dalec_homebrew \
  /home/linuxbrew/.cache \
  "$PREFIX/Homebrew" \
  "$PREFIX/Library" \
  "$PREFIX/.git" \
  "$PREFIX/.cache" \
  "$PREFIX/cache" \
  "$PREFIX/logs" \
  "$PREFIX/Caskroom" \
  "$PREFIX/bin/brew" \
  "$PREFIX/sbin/brew" \
  "$PREFIX/var/cache" \
  "$PREFIX/var/homebrew" \
  "$PREFIX/var/run/homebrew" \
  "$PREFIX/var/locks/homebrew" \
  "$PREFIX/var/log/homebrew" \
  "$PREFIX/share/doc/homebrew" \
  "$PREFIX/share/man/man1/brew.1" \
  "$PREFIX/share/zsh/site-functions/_brew" \
  "$PREFIX/share/fish/vendor_completions.d/brew.fish" \
  "$PREFIX/share/bash-completion/completions/brew" \
  "$PREFIX/etc/bash_completion.d/brew" \
  /usr/bin/apt \
  /usr/bin/apt-get \
  /usr/bin/dpkg \
  /usr/bin/dpkg-query \
  /var/lib/dpkg/status \
  /var/lib/apt/lists \
  /var/cache/apt/archives \
  /usr/bin/chisel \
  /usr/local/bin/chisel \
  /var/lib/chisel \
  /dalec-homebrew-frontend \
  /usr/local/bin/dalec-homebrew-materializer \
  /usr/local/bin/dalec-homebrew-test-runner \
  /usr/local/bin/dalec-homebrew-record-verify \
  /usr/local/bin/dalec-homebrew-release-verify \
  /usr/local/bin/dalec-homebrew-runtime-base-evidence \
  /usr/local/bin/dalec-homebrew-snapshot-proxy \
  /usr/local/libexec/dalec-homebrew-pour.rb \
  "$PREFIX/libexec/dalec-homebrew-materializer" \
  "$PREFIX/libexec/dalec-homebrew-test-runner" \
  "$PREFIX/libexec/dalec-homebrew-record-verify" \
  "$PREFIX/share/dalec-homebrew-tools"
do
  assert_absent "$forbidden_path"
done

printf '%s\n' \
  "runtime validation passed" \
  "os=ubuntu" \
  "version=24.04" \
  "codename=noble" \
  "identity=$runtime_uid:$runtime_gid" \
  "evidence_files=9"
CONTAINER_VALIDATOR

echo "==> VM live validation passed for $RESOLVED_REF ($IMAGE_OS/$IMAGE_ARCH)"
