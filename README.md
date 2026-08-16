<div align="center">
  <h1>dalec-homebrew</h1>
  <p><strong>Turn Homebrew packages into small, secure Linux container images.</strong></p>
</div>

`dalec-homebrew` is for people who want a command-line tool or language runtime
from Homebrew, but do not want to ship Homebrew—and all of its build
machinery—in the final container.

You describe the result in a small YAML file. Docker Buildx sends that file to
[Dalec](https://github.com/project-dalec/dalec), and `dalec-homebrew` builds a
ready-to-run image from verified, prebuilt Homebrew packages.

If Dalec, BuildKit frontends, or Homebrew bottles are new to you, that is okay:
the quickstart below explains everything you need. The
[glossary](CONTEXT.md) defines the project-specific terms used elsewhere.

## What it does

Given a package such as `hello`, `curl`, or `python@3.14`, the builder:

1. finds the current stable Linux package and all of its runtime dependencies;
2. verifies the authenticated Homebrew metadata, package digests, and archive
   contents;
3. installs everything without network access;
4. copies only the runtime files onto a clean Ubuntu base; and
5. runs your tests as a non-root user before exporting the image.

The result includes an SPDX SBOM and machine-readable build evidence, but does
**not** include `brew`, `apt`, `dpkg`, Chisel, package caches, or build tools.

## Quickstart

### 1. Install the prerequisites

You need:

- [Docker](https://docs.docker.com/get-docker/) with the `docker buildx`
  command;
- Bash plus standard Unix tools (`install`, `grep`, `awk`, `tr`, `wc`, and
  either `sha256sum` or `shasum`); on Windows, run the commands in WSL or a
  compatible Bash environment;
- `curl` and [`jq`](https://jqlang.github.io/jq/download/); and
- [`cosign`](https://docs.sigstore.dev/cosign/system_config/installation/)
  3.1.2 or newer to authenticate the release files.

Check that Docker is running and Buildx is available:

```console
set -euo pipefail
docker info >/dev/null
docker buildx version
```

The image you build is Linux-only. Your computer may run Linux, macOS, or
Windows with a Linux-backed Docker engine. The selected BuildKit daemon—not
just your terminal—must be able to pull the referenced images and packages.

### 2. Download and verify the current release inputs

> [!IMPORTANT]
> Release metadata expires after seven days. The quickstart requires a release
> published with a still-fresh metadata bundle. If the check below reports that
> the current release is stale, there is temporarily no supported published
> quickstart: wait for a fresh release or use the contributor rebuild workflow.
> Do not increase or bypass the freshness limit.

A `dalec-homebrew` release contains exact component references plus the
Homebrew metadata accepted by those components. The following commands discover
the current release version, reject an expired bundle before the large download,
authenticate the release checksum signature, verify every required file, and
arrange the metadata in the names expected by the builder:

```console
set -euo pipefail
mkdir -p .dalec-homebrew/release .dalec-homebrew/metadata

if [ -z "${DALEC_HOMEBREW_VERSION:-}" ]; then
  DALEC_HOMEBREW_VERSION="$(
    curl -fsSL https://api.github.com/repos/sozercan/dalec-homebrew/releases/latest \
      | jq -er .tag_name
  )"
fi
DALEC_HOMEBREW_RELEASE_URL="https://github.com/sozercan/dalec-homebrew/releases/download/$DALEC_HOMEBREW_VERSION"

for file in components.json metadata-bundle.digest metadata-bundle-manifest.json
do
  curl -fL "$DALEC_HOMEBREW_RELEASE_URL/$file" \
    -o ".dalec-homebrew/release/$file"
done

if ! jq -e '
  [.formula.generated_at, .migrations.generated_at]
  | map(fromdateiso8601)
  | min >= (now - 7 * 24 * 60 * 60)
' .dalec-homebrew/release/metadata-bundle-manifest.json >/dev/null
then
  echo "The current release metadata is older than seven days." >&2
  echo "A fresh dalec-homebrew release is required; do not bypass the freshness check." >&2
  exit 1
fi

for file in \
  inputs.json \
  metadata-formula.jws.json \
  metadata-migrations.jws.json \
  SHA256SUMS \
  SHA256SUMS.bundle
do
  curl -fL "$DALEC_HOMEBREW_RELEASE_URL/$file" \
    -o ".dalec-homebrew/release/$file"
done

cosign verify-blob \
  --bundle .dalec-homebrew/release/SHA256SUMS.bundle \
  --certificate-identity-regexp '^https://github\.com/sozercan/dalec-homebrew/\.github/workflows/release\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  .dalec-homebrew/release/SHA256SUMS

grep -E '  \./(components\.json|inputs\.json|metadata-bundle\.digest|metadata-bundle-manifest\.json|metadata-formula\.jws\.json|metadata-migrations\.jws\.json)$' \
  .dalec-homebrew/release/SHA256SUMS \
  > .dalec-homebrew/release/REQUIRED_SHA256SUMS
test "$(wc -l < .dalec-homebrew/release/REQUIRED_SHA256SUMS | tr -d ' ')" -eq 6

if command -v sha256sum >/dev/null 2>&1; then
  (cd .dalec-homebrew/release && sha256sum --check REQUIRED_SHA256SUMS)
else
  (cd .dalec-homebrew/release && shasum -a 256 --check REQUIRED_SHA256SUMS)
fi

DALEC_HOMEBREW_METADATA_BUNDLE="$PWD/.dalec-homebrew/metadata"
install -m 0444 .dalec-homebrew/release/metadata-bundle-manifest.json \
  "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json"
install -m 0444 .dalec-homebrew/release/metadata-formula.jws.json \
  "$DALEC_HOMEBREW_METADATA_BUNDLE/formula.jws.json"
install -m 0444 .dalec-homebrew/release/metadata-migrations.jws.json \
  "$DALEC_HOMEBREW_METADATA_BUNDLE/formula_tap_migrations.jws.json"
DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST="$(
  tr -d '\r\n' < .dalec-homebrew/release/metadata-bundle.digest
)"

if command -v sha256sum >/dev/null 2>&1; then
  MANIFEST_SHA256="$(sha256sum "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json" | awk '{print $1}')"
else
  MANIFEST_SHA256="$(shasum -a 256 "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json" | awk '{print $1}')"
fi
test "$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" = "sha256:$MANIFEST_SHA256"

printf 'Using dalec-homebrew %s\n' "$DALEC_HOMEBREW_VERSION"
```

The signed checksum set authenticates every downloaded release input. The final
manifest check confirms that the named metadata context has the exact digest
compiled into the released frontend. The [usage guide](docs/usage.md#verify-release-assets)
explains the trust chain in more detail.

### 3. Choose the target architecture

Choose the architecture that your selected BuildKit worker can execute. The
common local-Docker case can be detected automatically; set
`DALEC_HOMEBREW_PLATFORM=linux/amd64` or `linux/arm64` first to override it:

```console
set -euo pipefail
if [ -z "${DALEC_HOMEBREW_PLATFORM:-}" ]; then
  case "$(docker info --format '{{.Architecture}}')" in
    x86_64|amd64) DALEC_HOMEBREW_PLATFORM=linux/amd64 ;;
    arm64|aarch64) DALEC_HOMEBREW_PLATFORM=linux/arm64 ;;
    *) echo "Set DALEC_HOMEBREW_PLATFORM to linux/amd64 or linux/arm64" >&2; exit 1 ;;
  esac
fi

DALEC_HOMEBREW_ARCH="${DALEC_HOMEBREW_PLATFORM#linux/}"
DALEC_HOMEBREW_INDEX="$(
  jq -er '.frontend.index' .dalec-homebrew/release/components.json
)"
DALEC_HOMEBREW_CHILD="$(
  jq -er --arg arch "$DALEC_HOMEBREW_ARCH" '
    .frontend.platforms[]
    | select(.platform.os == "linux" and .platform.architecture == $arch)
    | .ref
  ' .dalec-homebrew/release/components.json
)"
```

A remote Buildx builder may have a different architecture from the local Docker
daemon. Set the platform explicitly in that case. Runtime tests execute target
binaries, so the worker needs native execution or correctly configured
emulation.

### 4. Create a Dalec spec

The newest `dalec-homebrew` release selected above records the upstream Dalec
version it was tested with and that frontend's exact digest. Read both values
from verified `inputs.json`; you do not need to look up a Dalec version:

```console
set -euo pipefail
DALEC_SYNTAX="$(jq -er '.dalec_frontend.index' .dalec-homebrew/release/inputs.json)"
DALEC_FRONTEND_VERSION="$(jq -er '.dalec_frontend.module.version' .dalec-homebrew/release/inputs.json)"
printf 'Using upstream Dalec %s\n' "$DALEC_FRONTEND_VERSION"
```

Create `hello.yaml`:

```console
set -euo pipefail
cat > hello.yaml <<EOF
# syntax=$DALEC_SYNTAX

name: hello-homebrew
version: 1.0.0
revision: 1
description: GNU Hello in a minimal Homebrew-based runtime image
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

A **Dalec spec** is the YAML build description. The first line tells BuildKit
to use the newest release-approved upstream Dalec frontend at its exact digest.
Dalec reads the document and sends the `homebrew` target to the exact
`dalec-homebrew` release component selected above. You do not install either frontend locally;
BuildKit pulls them.

The top-level `version: 1.0.0` is optional descriptive Dalec metadata. This
runtime-only frontend records it in the effective-input evidence, but it does
not set the image tag or select a Homebrew package version. The image is tagged
`1.0.0` only because the later `docker buildx` command says so. `hello: {}`
selects the stable `hello` Formula captured by the release metadata. Historical versions and version ranges are intentionally unsupported;
versioned Formula names published by Homebrew, such as `python@3.14`, work as
written.

### 5. Build and run the image

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

The build may take a few minutes the first time while BuildKit downloads and
verifies the frontend, runtime base, metadata, and Homebrew packages. Later
builds reuse BuildKit's cache.

To confirm the default runtime identity and copy evidence out without assuming
that the image contains `jq`:

```console
set -euo pipefail
docker image inspect --format '{{.Config.User}}' hello-homebrew:1.0.0
cid="$(docker create hello-homebrew:1.0.0)"
docker cp "$cid:/usr/share/dalec-homebrew/manifest.json" ./manifest.json
docker cp "$cid:/usr/share/dalec-homebrew/sbom.spdx.json" ./sbom.spdx.json
docker rm "$cid"
```

The first command prints `linuxbrew`. The copied files are embedded evidence,
not signed OCI attestations; release attestations are published separately.

## Use another Homebrew package

Change the key under `dependencies.runtime` and update the entrypoint. For
example, a two-package image can declare:

```yaml
dependencies:
  runtime:
    curl: {}
    jq: {}

image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/curl
```

Use a bare name for `homebrew/core`, or `owner/tap/formula` for a Formula in a
public GitHub tap whose default repository is
`https://github.com/<owner>/homebrew-<tap>`. Private taps, credentials, and
arbitrary repository URLs are not accepted.

See the [usage guide](docs/usage.md) for image configuration, runtime tests,
multi-platform builds, dependency rules, and troubleshooting.

## Supported scope

| Supported | Not supported |
| --- | --- |
| Linux `amd64` and `arm64` | Other operating systems or architectures |
| Stable Formulae in the release metadata from `homebrew/core` and public default GitHub taps | Historical versions, version ranges, casks, private taps, or arbitrary Git remotes |
| Published bottles and exact release-policy-authorized prebuilt executables | General source builds or user-provided archive recipes |
| A fixed, minimal Ubuntu runtime base | Custom runtime base images |
| Non-root, network-disabled runtime tests | Root images or networked tests |

The policy in the current source tree makes runtime minimization automatic. Requested
Formulae keep their package payload, while safe development-only files from
transitive core dependencies may be removed. Policy is bound to the selected
release version: an older published tuple does not silently acquire newer
rules, and no input can weaken or broaden its rules.

## Version and reproducibility notes

Unless you set `DALEC_HOMEBREW_VERSION` first, the quickstart automatically
selects the newest published `dalec-homebrew` release and then uses the exact
upstream Dalec version approved by that release. This gives users the latest
compatible frontend without requiring them to know or copy a version number.

The mutable upstream `ghcr.io/project-dalec/dalec/frontend:latest` tag is not a
trusted release input: it may move to a version the selected tuple has never
tested. Record `DALEC_HOMEBREW_VERSION` and retain the verified release assets to
repeatably identify and authenticate the tuple. A full replay also needs the
original resolution, public-tap commits and catalogs, selected package or
archive bytes, and mirrored OCI blobs. Normal new solves still enforce metadata
freshness. Promotion and rollback reuse retained exact inputs instead of
resolving current tags or Homebrew metadata again. See the
[release guide](docs/release.md).

## Troubleshooting

- **`docker buildx` is missing:** install or update Docker Desktop, or install
  the Buildx plugin for Docker Engine.
- **No component exists for the platform:** choose exactly `linux/amd64` or
  `linux/arm64` and select the matching child reference.
- **Metadata is too old:** use a release with a fresh authenticated bundle. If
  none is available, the supported published build path is temporarily
  unavailable. Never extend or bypass the release freshness limit.
- **A Formula has no supported bottle:** choose a bottled Formula or one that is
  explicitly authorized by the current release policy. Source fallback is
  disabled.
- **A test tries to access the network:** runtime tests intentionally run with
  networking disabled. Test only behavior available in the completed image.

More failure explanations are in the
[usage troubleshooting guide](docs/usage.md#troubleshooting).

## Documentation

- [Usage guide](docs/usage.md) — build, customize, test, and troubleshoot images
- [Examples](examples/README.md) — distinguish templates from integration fixtures
- [Glossary](CONTEXT.md) — Dalec, BuildKit, Homebrew, and release terminology
- [Security model](SECURITY.md) — guarantees, trust boundaries, and limitations
- [Architecture](docs/architecture.md) — how resolution and assembly work
- [Release and rollback](docs/release.md) — verify, publish, promote, or restore a release
- [Contributing](CONTRIBUTING.md) — local development and validation
- [Documentation map](docs/README.md) — choose a guide by task or audience

Licensed under the [Apache License 2.0](LICENSE).
