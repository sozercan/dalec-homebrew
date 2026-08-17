<div align="center">
  <h1>🍺 dalec-homebrew</h1>
  <p><strong>Turn a list of Homebrew packages into a small, non-root Linux container image.</strong></p>
</div>

---

## What is this?

`dalec-homebrew` is a build plugin for Docker — a
[BuildKit](https://github.com/moby/buildkit) frontend, used through
[Dalec](https://github.com/project-dalec/dalec). Instead of writing a
Dockerfile, you list the packages you want:

```yaml
dependencies:
  runtime:
    curl: {}
    jq: {}
```

`docker buildx build` then produces an image with those packages, everything
they need at runtime, and almost nothing else. You never install `brew`
anywhere: the build downloads Homebrew's prebuilt packages (**bottles**),
checks their signatures and digests, installs them with the network switched
off, and copies the result onto a minimal Ubuntu base.

The image runs as a non-root user, has no package manager inside, and ships an
SPDX SBOM plus a record of everything that went into it.

## Requirements

- **Docker** with Buildx, backed by BuildKit `0.31.2` or newer.
- A **`linux/amd64` or `linux/arm64`** target.
- **Network access from the builder** to `ghcr.io`, GitHub, and Homebrew's
  bottle hosts.
- **`curl`**, **`jq`**, **`cosign`**, and `sha256sum` (macOS: `shasum -a 256`).

## Quickstart

### 1. Get the release inputs

A release publishes the frontend images and the Homebrew package index they were
tested with. These commands pick the newest release, download what the build
needs, and verify it against the release's signed checksums. Run them in a fresh
shell — `set -euo pipefail` stops at the first failure.

```console
set -euo pipefail

DALEC_HOMEBREW_VERSION=$(curl -fsSL https://api.github.com/repos/sozercan/dalec-homebrew/releases |
  jq -er 'map(select(.draft == false and .prerelease == false)) | max_by(.published_at).tag_name')
RELEASE=https://github.com/sozercan/dalec-homebrew/releases/download/$DALEC_HOMEBREW_VERSION

mkdir -p release homebrew-metadata
for asset in components.json SHA256SUMS SHA256SUMS.bundle \
  metadata-bundle-manifest.json metadata-formula.jws.json metadata-migrations.jws.json; do
  curl -fsSL -o "release/$asset" "$RELEASE/$asset"
done

cosign verify-blob \
  --bundle release/SHA256SUMS.bundle \
  --certificate-identity-regexp '^https://github\.com/sozercan/dalec-homebrew/\.github/workflows/release\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  release/SHA256SUMS
(cd release && sha256sum -c --ignore-missing SHA256SUMS)

cp release/metadata-bundle-manifest.json homebrew-metadata/manifest.json
cp release/metadata-formula.jws.json homebrew-metadata/formula.jws.json
cp release/metadata-migrations.jws.json homebrew-metadata/formula_tap_migrations.jws.json

ARCH=$(docker info --format '{{.Architecture}}' | sed 's/x86_64/amd64/; s/aarch64/arm64/')
DALEC_HOMEBREW_INDEX=$(jq -er .frontend.index release/components.json)
DALEC_HOMEBREW_CHILD=$(jq -er --arg arch "$ARCH" \
  '.frontend.platforms[] | select(.platform.architecture == $arch).ref' release/components.json)
DALEC_HOMEBREW_METADATA_BUNDLE=$PWD/homebrew-metadata
DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$(jq -er .metadata_bundle_digest release/components.json)

jq -e '[.formula, .migrations] | map(.generated_at | fromdateiso8601) | min > now - 604800' \
  release/metadata-bundle-manifest.json >/dev/null &&
  echo "Using dalec-homebrew $DALEC_HOMEBREW_VERSION for linux/$ARCH" ||
  echo "WARNING: this release is past the 7-day Homebrew metadata window; wait for a newer release"
```

Two things are worth knowing about what just happened:

- **Everything is pinned by digest.** `components.json` is signed release data,
  so the frontend images it names cannot be swapped out later. Tags are rejected
  by the build.
- **Releases expire.** The Homebrew package index in a release is accepted for
  seven days, and that limit cannot be raised. Always use the newest release.

### 2. Write a spec

```console
cat > hello.yaml <<EOF
# syntax=ghcr.io/project-dalec/dalec/frontend:latest

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

The first line tells Docker to build this file with Dalec instead of treating it
as a Dockerfile, and `targets.homebrew.frontend.image` tells Dalec to hand the
work to `dalec-homebrew`. To reproduce a build later, replace `latest` with the
Dalec digest your release was tested against: download `inputs.json` from the
same release and read `.dalec_frontend.index`.

### 3. Build and run it

```console
docker buildx build \
  --build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
  --build-arg "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" \
  --build-context "dalec-homebrew-metadata=$DALEC_HOMEBREW_METADATA_BUNDLE" \
  --target homebrew/image \
  --platform "linux/$ARCH" \
  --file hello.yaml \
  --tag hello-runtime:quickstart \
  --load \
  .

docker run --rm hello-runtime:quickstart
```

Output:

```text
Hello, world!
```

The first build takes a few minutes while BuildKit downloads and verifies the
frontend, the runtime base, and the packages. Later builds reuse its cache.

## What you built

| Property | Value |
| --- | --- |
| User | `linuxbrew` (`1000:1000`), non-root |
| Working directory | `/home/linuxbrew` |
| Packages | `/home/linuxbrew/.linuxbrew` |
| Evidence and SBOM | `/usr/share/dalec-homebrew` |

The image does **not** contain `brew`, `apt`, `dpkg`, package caches, install
receipts, Formula source, or build and test tooling. The evidence files record
the exact package versions, bottle digests, file inventory, what was removed,
and an SPDX SBOM — see
[runtime contents and evidence](docs/usage.md#runtime-contents-and-evidence).

## Choose your packages

```yaml
dependencies:
  runtime:
    jq: {}                    # a package from homebrew/core
    python@3.14: {}           # a versioned Formula name
    acme/tools/widget: {}     # a package from a public GitHub tap
    ripgrep:
      arch: [amd64, arm64]    # only for these architectures
```

- You always get the **current stable** version. Version ranges and historical
  versions are rejected; pin a `dalec-homebrew` release instead.
- A plain name means `homebrew/core`; `owner/tap/formula` means the public
  GitHub repository `github.com/<owner>/homebrew-<tap>`.
- Casks, private taps, arbitrary Git URLs, and source builds are not supported.

## Configure and test the image

`image` accepts `entrypoint`, `cmd`, `env`, `labels`, `volumes`, `working_dir`,
`stop_signal`, and `user`. Tests run inside the finished image, as the final
user, with networking disabled; a failing test fails the build.

```yaml
image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/hello

tests:
  - name: hello-works
    steps:
      - command: hello
        stdout:
          contains: ["Hello, world!"]
```

The full field list is in the [usage reference](docs/usage.md#configure-the-image).

## What is supported

| Supported | Not supported |
| --- | --- |
| `linux/amd64` and `linux/arm64` | Any other platform |
| Current stable Formulae from `homebrew/core` and public GitHub taps | Historical versions, version ranges, casks, private taps |
| Official bottles, plus a few release-approved prebuilt archives | Source builds |
| Non-root images on the built-in runtime base | Custom base images |
| Offline tests in the final image | Networked tests or test mounts |

## Troubleshooting

| Message | Fix |
| --- | --- |
| `metadata is stale: ... maximum age 168h0m0s` | The release is more than 7 days old. Use a newer one. |
| `frontend: reference "..." is not digest-pinned` | A tag was used instead of a digest. Re-run step 1. |
| `required named context "dalec-homebrew-metadata" is missing` | Add `--build-context dalec-homebrew-metadata=...`. |
| `frontend platform child and index must use the same repository` | `DALEC_HOMEBREW_FRONTEND_INDEX_REF` is missing or mismatched. |
| `no such handler for target "": available targets: image` | Use `--target homebrew/image`, not `--target homebrew`. |

More messages and their causes are in the
[usage reference](docs/usage.md#troubleshooting).

## Learn more

- [Usage reference](docs/usage.md) — every field, setting, test assertion, and
  evidence file, plus ready-made [examples](examples/).
- [Architecture](docs/architecture.md) — how verification and assembly work.
- [Security](SECURITY.md) — what the build guarantees, and its limits.
- [Releases](docs/release.md) · [Contributing](CONTRIBUTING.md)

Questions and bug reports are welcome in
[GitHub issues](https://github.com/sozercan/dalec-homebrew/issues); report
suspected vulnerabilities through
[SECURITY.md](SECURITY.md#reporting-a-vulnerability) instead.
