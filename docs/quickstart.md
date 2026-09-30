# Quickstart

Build and run GNU Hello from a verified Homebrew bottle. No prior Dalec,
BuildKit, or Homebrew knowledge is needed; the [glossary](../CONTEXT.md) defines
the terms. Run every block in the same Bash session.

The steps below build GNU Hello. Run every block in the same Bash session.

> [!IMPORTANT]
> A released metadata snapshot is accepted for seven days. If no published
> release has fresh metadata, there is temporarily no supported published build
> path. The setup below detects that condition before the large download; do not
> bypass or extend the limit.

## 1. Install the prerequisites

You need:

- [Docker](https://docs.docker.com/get-docker/) with `docker buildx`;
- Bash, `curl`, [`jq`](https://jqlang.github.io/jq/download/), `install`,
  `grep`, `awk`, `tr`, `wc`, and either `sha256sum` or `shasum`; and
- [`cosign`](https://docs.sigstore.dev/cosign/system_config/installation/)
  3.1.2 or a compatible newer release.

On Windows, use WSL or another compatible Bash environment. Confirm Docker and
Buildx are ready:

```console
set -euo pipefail
docker info >/dev/null
docker buildx version
```

## 2. Prepare a verified release

This block discovers the newest release unless you set
`DALEC_HOMEBREW_VERSION` first. It authenticates the release assets, rejects
stale metadata, and selects the exact component and Dalec frontend digests for
your platform.

```console
set -euo pipefail

RELEASE_DIR="$PWD/.dalec-homebrew/release"
DALEC_HOMEBREW_METADATA_BUNDLE="$PWD/.dalec-homebrew/metadata"
mkdir -p "$RELEASE_DIR" "$DALEC_HOMEBREW_METADATA_BUNDLE"

if [ -z "${DALEC_HOMEBREW_VERSION:-}" ]; then
  DALEC_HOMEBREW_VERSION="$(
    curl -fsSL https://api.github.com/repos/sozercan/dalec-homebrew/releases/latest \
      | jq -er .tag_name
  )"
fi
RELEASE_URL="https://github.com/sozercan/dalec-homebrew/releases/download/$DALEC_HOMEBREW_VERSION"

curl -fsSL "$RELEASE_URL/metadata-bundle-manifest.json" \
  -o "$RELEASE_DIR/metadata-bundle-manifest.json"
jq -e '
  [.formula.generated_at, .migrations.generated_at]
  | map(fromdateiso8601)
  | min >= (now - 7 * 24 * 60 * 60)
' "$RELEASE_DIR/metadata-bundle-manifest.json" >/dev/null || {
  echo "No published dalec-homebrew release currently has fresh metadata." >&2
  exit 1
}

for file in \
  components.json inputs.json metadata-bundle.digest \
  metadata-formula.jws.json metadata-migrations.jws.json \
  SHA256SUMS SHA256SUMS.bundle
do
  curl -fsSL "$RELEASE_URL/$file" -o "$RELEASE_DIR/$file"
done

cosign verify-blob \
  --bundle "$RELEASE_DIR/SHA256SUMS.bundle" \
  --certificate-identity-regexp '^https://github\.com/sozercan/dalec-homebrew/\.github/workflows/release\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$RELEASE_DIR/SHA256SUMS"

grep -E '  \./(components\.json|inputs\.json|metadata-bundle\.digest|metadata-bundle-manifest\.json|metadata-formula\.jws\.json|metadata-migrations\.jws\.json)$' \
  "$RELEASE_DIR/SHA256SUMS" > "$RELEASE_DIR/REQUIRED_SHA256SUMS"
test "$(wc -l < "$RELEASE_DIR/REQUIRED_SHA256SUMS" | tr -d ' ')" -eq 6
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$RELEASE_DIR" && sha256sum --check REQUIRED_SHA256SUMS)
  manifest_sha256="$(sha256sum "$RELEASE_DIR/metadata-bundle-manifest.json" | awk '{print $1}')"
else
  (cd "$RELEASE_DIR" && shasum -a 256 --check REQUIRED_SHA256SUMS)
  manifest_sha256="$(shasum -a 256 "$RELEASE_DIR/metadata-bundle-manifest.json" | awk '{print $1}')"
fi

install -m 0444 "$RELEASE_DIR/metadata-bundle-manifest.json" \
  "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json"
install -m 0444 "$RELEASE_DIR/metadata-formula.jws.json" \
  "$DALEC_HOMEBREW_METADATA_BUNDLE/formula.jws.json"
install -m 0444 "$RELEASE_DIR/metadata-migrations.jws.json" \
  "$DALEC_HOMEBREW_METADATA_BUNDLE/formula_tap_migrations.jws.json"
DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST="$(tr -d '\r\n' < "$RELEASE_DIR/metadata-bundle.digest")"
test "$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" = "sha256:$manifest_sha256"

if [ -z "${DALEC_HOMEBREW_PLATFORM:-}" ]; then
  case "$(docker info --format '{{.Architecture}}')" in
    x86_64|amd64) DALEC_HOMEBREW_PLATFORM=linux/amd64 ;;
    arm64|aarch64) DALEC_HOMEBREW_PLATFORM=linux/arm64 ;;
    *) echo "Set DALEC_HOMEBREW_PLATFORM to linux/amd64 or linux/arm64" >&2; exit 1 ;;
  esac
fi
case "$DALEC_HOMEBREW_PLATFORM" in
  linux/amd64|linux/arm64) ;;
  *) echo "Unsupported platform: $DALEC_HOMEBREW_PLATFORM" >&2; exit 1 ;;
esac
arch="${DALEC_HOMEBREW_PLATFORM#linux/}"

DALEC_HOMEBREW_INDEX="$(jq -er '.frontend.index' "$RELEASE_DIR/components.json")"
DALEC_HOMEBREW_CHILD="$(
  jq -er --arg arch "$arch" '
    .frontend.platforms[]
    | select(.platform.os == "linux" and .platform.architecture == $arch)
    | .ref
  ' "$RELEASE_DIR/components.json"
)"
DALEC_SYNTAX="$(jq -er '.dalec_frontend.index' "$RELEASE_DIR/inputs.json")"
DALEC_FRONTEND_VERSION="$(jq -er '.dalec_frontend.module.version' "$RELEASE_DIR/inputs.json")"
printf 'Using dalec-homebrew %s with Dalec %s for %s\n' \
  "$DALEC_HOMEBREW_VERSION" "$DALEC_FRONTEND_VERSION" "$DALEC_HOMEBREW_PLATFORM"
```

The upstream `frontend:latest` tag is deliberately not used: it can move to a
version that the selected release has not tested.

## 3. Create the spec

```console
set -euo pipefail
cat > hello.yaml <<EOF
# syntax=$DALEC_SYNTAX

name: hello-homebrew
version: 1.0.0
revision: 1
description: GNU Hello in a minimal runtime image
website: https://www.gnu.org/software/hello/
license: GPL-3.0-or-later

dependencies:
  runtime:
    hello: {}

image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/hello

tests:
  - name: hello-runs
    steps:
      - command: hello
        stdout:
          contains: ["Hello, world!"]

targets:
  homebrew:
    frontend:
      image: $DALEC_HOMEBREW_CHILD
EOF
```

## 4. Build and run

```console
set -euo pipefail
docker buildx build \
  --build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
  --build-arg "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" \
  --build-context "dalec-homebrew-metadata=$DALEC_HOMEBREW_METADATA_BUNDLE" \
  --target homebrew/image \
  --platform "$DALEC_HOMEBREW_PLATFORM" \
  --file hello.yaml \
  --tag hello-homebrew:1.0.0 \
  --load \
  .

docker run --rm hello-homebrew:1.0.0
```

Expected output:

```text
Hello, world!
```

## What you built

| Property | Value |
| --- | --- |
| Runtime user | `linuxbrew` (`1000:1000`) |
| Working directory | `/home/linuxbrew` |
| Homebrew prefix | `/home/linuxbrew/.linuxbrew` |
| Evidence and SBOM | `/usr/share/dalec-homebrew` |
| Package managers in the image | None |

To add packages, add keys under `dependencies.runtime`. To configure commands,
environment, labels, volumes, or tests, see the [usage guide](usage.md).

