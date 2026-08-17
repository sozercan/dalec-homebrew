<div align="center">
  <h1>dalec-homebrew</h1>
  <p><strong>Turn Homebrew packages into small, non-root Linux container images.</strong></p>
</div>

## What is this?

`dalec-homebrew` is a [Dalec](https://github.com/project-dalec/dalec) extension
for Docker Buildx. You describe the tools you want in YAML instead of writing a
Dockerfile:

```yaml
dependencies:
  runtime:
    curl: {}
    jq: {}
```

BuildKit resolves those Formulae and their runtime dependencies, verifies the
selected artifacts, installs them without network access, and copies an
allowlisted runtime onto a clean Ubuntu base. The final image contains neither
Homebrew nor a package manager.

New to Dalec, frontends, Formulae, or bottles? You can follow the quickstart
without knowing those terms; the [glossary](CONTEXT.md) explains them.

## Why use it?

- **Minimal runtime:** only explicitly selected runtime files and evidence are
  copied from the build environment.
- **Non-root by default:** processes run as `linuxbrew` (`1000:1000`), while
  runtime code remains root-owned and non-writable.
- **Verified inputs:** metadata, component identities, package digests, and
  archive contents are checked before installation.
- **Offline installation and tests:** Homebrew materialization and declared
  runtime tests run without network access.
- **Auditable output:** every image includes an SPDX SBOM plus resolution,
  inventory, minimization, and materialization evidence.

## Quickstart

The steps below build GNU Hello. Run every block in the same Bash session.

> [!IMPORTANT]
> A released metadata snapshot is accepted for seven days. If no published
> release has fresh metadata, there is temporarily no supported published build
> path. The setup below detects that condition before the large download; do not
> bypass or extend the limit.

### 1. Install the prerequisites

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

### 2. Prepare a verified release

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

### 3. Create the spec

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

### 4. Build and run

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

## Versions, in plain English

- `DALEC_HOMEBREW_VERSION` selects one published component, policy, and metadata
  tuple. The quickstart finds the newest release automatically.
- `DALEC_FRONTEND_VERSION` is the compatible upstream Dalec version recorded by
  that release. The image itself is still selected by digest.
- Top-level `version: 1.0.0` is descriptive spec metadata. The image has that
  tag only because the build command explicitly uses `--tag ...:1.0.0`.
- `hello: {}` selects the stable Formula in the release's metadata snapshot.
  Exact Formula names such as `python@3.14` work; version ranges and historical
  versions do not.

## What you built

| Property | Value |
| --- | --- |
| Runtime user | `linuxbrew` (`1000:1000`) |
| Working directory | `/home/linuxbrew` |
| Homebrew prefix | `/home/linuxbrew/.linuxbrew` |
| Evidence and SBOM | `/usr/share/dalec-homebrew` |
| Package managers in the image | None |

To add packages, add keys under `dependencies.runtime`. To configure commands,
environment, labels, volumes, or tests, see the [usage guide](docs/usage.md).

## Supported scope

| Supported | Not supported |
| --- | --- |
| Linux `amd64` and `arm64` | Other platforms |
| Stable Formulae from the release snapshot | Historical versions or ranges |
| `homebrew/core` and public default GitHub taps | Casks, private taps, arbitrary Git remotes |
| Bottles and exact policy-authorized prebuilt executables | General source builds |
| Built-in non-root runtime base | Custom runtime bases |
| Offline build-time runtime tests | Networked tests or test mounts |

## Common failures

| Message or symptom | Action |
| --- | --- |
| Metadata is stale | Wait for a fresh release; never extend the release limit |
| Child or index is not digest-pinned | Read both values from verified `components.json` |
| Target or route is rejected | Build exactly `--target homebrew/image` through upstream Dalec |
| A Formula or version is unavailable | Use a stable bottled Formula or exact published versioned name |
| A runtime test cannot reach the network | Replace it with an offline assertion |

## Documentation

- [Usage](docs/usage.md) — packages, image settings, tests, evidence, and errors
- [Examples](examples/README.md) — templates versus integration fixtures
- [Glossary](CONTEXT.md) — project terminology
- [Security](SECURITY.md) — guarantees, trust boundaries, and limitations
- [Architecture](docs/architecture.md) — build and verification flow
- [Release and rollback](docs/release.md) — operator procedures
- [Contributing](CONTRIBUTING.md) — local development and validation

Licensed under the [Apache License 2.0](LICENSE).
