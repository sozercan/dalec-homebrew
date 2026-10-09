# Quickstart

Turn a short YAML spec into a container image. You need
[Docker](https://docs.docker.com/get-docker/) with Buildx, Bash, `curl`, and
[`jq`](https://jqlang.github.io/jq/download/). No Dalec, BuildKit, or Homebrew
knowledge is needed; the [glossary](../CONTEXT.md) defines the terms.

> [!NOTE]
> This path downloads release inputs from GitHub over TLS and does not check
> their signatures. For CI, production, or anything you publish, use the
> [verified release build](verified-release.md) instead. The build itself still
> verifies all Homebrew metadata and packages either way.

## 1. Get the release inputs

```console
set -euo pipefail
VERSION="$(curl -fsSL https://api.github.com/repos/sozercan/dalec-homebrew/releases/latest | jq -er .tag_name)"
URL="https://github.com/sozercan/dalec-homebrew/releases/download/$VERSION"
DALEC_HOMEBREW_METADATA_BUNDLE="$PWD/.dalec-homebrew/metadata"
mkdir -p "$DALEC_HOMEBREW_METADATA_BUNDLE"

curl -fsSL "$URL/metadata-bundle-manifest.json" -o "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json"
curl -fsSL "$URL/metadata-formula.jws.json" -o "$DALEC_HOMEBREW_METADATA_BUNDLE/formula.jws.json"
curl -fsSL "$URL/metadata-migrations.jws.json" -o "$DALEC_HOMEBREW_METADATA_BUNDLE/formula_tap_migrations.jws.json"
jq -e '[.formula.generated_at, .migrations.generated_at] | map(fromdateiso8601) | min >= (now - 7 * 24 * 60 * 60)' \
  "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json" >/dev/null || {
  echo "The newest release has stale metadata; wait for a new release." >&2
  exit 1
}

DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST="$(curl -fsSL "$URL/metadata-bundle.digest" | tr -d '\r\n')"
COMPONENTS="$(curl -fsSL "$URL/components.json")"
DALEC_SYNTAX="$(curl -fsSL "$URL/inputs.json" | jq -er '.dalec_frontend.index')"
DALEC_HOMEBREW_INDEX="$(jq -er '.frontend.index' <<<"$COMPONENTS")"
ARCH="$(docker info --format '{{.Architecture}}' | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
DALEC_HOMEBREW_CHILD="$(jq -er --arg arch "$ARCH" '.frontend.platforms[] | select(.platform.architecture == $arch) | .ref' <<<"$COMPONENTS")"
```

A release's metadata is accepted for seven days. If the check above fails, there
is temporarily no fresh release; the limit is never extended.

## 2. Write a spec

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

targets:
  homebrew:
    frontend:
      image: $DALEC_HOMEBREW_CHILD
EOF
```

## 3. Build and run

```console
set -euo pipefail
docker buildx build \
  --build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
  --build-arg "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" \
  --build-context "dalec-homebrew-metadata=$DALEC_HOMEBREW_METADATA_BUNDLE" \
  --target homebrew/image \
  --platform "linux/$ARCH" \
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

To add packages, add keys under `dependencies.runtime`. The
[usage guide](usage.md) covers image settings and tests.
