<div align="center">
  <h1>🍺 dalec-homebrew</h1>
  <p><strong>Turn a short list of Homebrew packages into a small, non-root Linux container image.</strong></p>
</div>

---

## What is this?

`dalec-homebrew` is a build plugin for Docker — a
[BuildKit](https://github.com/moby/buildkit) *frontend*, used through
[Dalec](https://github.com/project-dalec/dalec). Instead of writing a
Dockerfile, you write a few lines of YAML that name the packages you want:

```yaml
dependencies:
  runtime:
    jq: {}
    curl: {}
```

`docker buildx build` then produces a container image that contains those
packages, everything they need at runtime, and almost nothing else.

You do not need to know Homebrew, and you never install `brew` anywhere. The
build downloads Homebrew's pre-built packages (called **bottles**), checks their
signatures and digests, installs them with the network switched off, and copies
the finished files onto a minimal Ubuntu base.

## Why use it

- **Small images.** Only runtime files are kept. Headers, manual pages, build
  metadata, shell completions, and static archives from dependencies are left
  out automatically.
- **Non-root by default.** The image runs as `linuxbrew` (`1000:1000`), and all
  program code is root-owned and read-only.
- **No package manager inside.** No `brew`, `apt`, `dpkg`, caches, receipts, or
  build tooling ships in the final image.
- **Verified, not just downloaded.** Package metadata is signature-checked,
  every bottle is bound to an exact digest, archives are scanned before they are
  unpacked, and installation runs offline.
- **Auditable.** Every image carries an SPDX SBOM plus JSON evidence describing
  exactly what went in, at `/usr/share/dalec-homebrew`.
- **Reproducible.** Components are digest-pinned, ordering is deterministic, and
  timestamps are fixed.

## Before you start

You need:

- **Docker** with Buildx, backed by BuildKit `0.31.2` or newer
  (`docker buildx version`).
- A build target of **`linux/amd64` or `linux/arm64`**. Nothing else is
  supported.
- **Network access from the builder** to `ghcr.io`, GitHub, and the Homebrew
  bottle hosts.
- **`curl`, `jq`, and `sha256sum`** (on macOS, `shasum -a 256`) for the setup
  steps below.

Everything else is downloaded during the build.

## Quickstart

The four steps below build a `hello` image. Copy them into the same shell
session; each step reuses variables from the previous one.

### Step 1 — Pick a release

`dalec-homebrew` is published as a container image on GitHub Container Registry.
Releases are tagged with a version, and
[the latest release](https://github.com/sozercan/dalec-homebrew/releases/latest)
is the one you want.

> **Use the newest release.** Each release carries the Homebrew package index it
> was tested against, and that snapshot is only accepted for **7 days**. Older
> releases fail the build with a clear "metadata is stale" error.

```console
DALEC_HOMEBREW_VERSION=v0.2.9
DALEC_HOMEBREW_REPO=ghcr.io/sozercan/dalec-homebrew
ARCH=amd64   # or arm64 — your builder's architecture; Apple Silicon is arm64,
             # most CI runners are amd64 (`docker buildx inspect` lists it)
```

The build has to name the frontend by **digest**, not by tag, so that the image
doing the work is exactly the one that was published and signed. Turn the
version tag into the two digests the build needs:

```console
DALEC_HOMEBREW_INDEX=$DALEC_HOMEBREW_REPO@$(
  docker buildx imagetools inspect "$DALEC_HOMEBREW_REPO:$DALEC_HOMEBREW_VERSION" \
    --format '{{.Manifest.Digest}}')

DALEC_HOMEBREW_CHILD=$DALEC_HOMEBREW_REPO@$(
  docker buildx imagetools inspect --raw "$DALEC_HOMEBREW_REPO:$DALEC_HOMEBREW_VERSION" |
    jq -r --arg arch "$ARCH" \
      '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest')

echo "$DALEC_HOMEBREW_INDEX"
echo "$DALEC_HOMEBREW_CHILD"
```

- `DALEC_HOMEBREW_INDEX` is the multi-platform release image.
- `DALEC_HOMEBREW_CHILD` is the single-architecture image that actually runs on
  your builder. The build keeps both identities so the release evidence can be
  checked independently.

To build for the *other* architecture, keep the child that matches your builder
and pass that platform in step 4; BuildKit then needs emulation (for example
`docker run --privileged --rm tonistiigi/binfmt --install all`) to run the
target-architecture steps.

### Step 2 — Download the release's Homebrew package index

Homebrew publishes a signed index of every Formula. Each `dalec-homebrew`
release captures and verifies one snapshot of it, so that everyone building with
that release resolves the same package versions. You supply that snapshot to
the build as a folder of three files.

```console
DALEC_HOMEBREW_ASSETS=https://github.com/sozercan/dalec-homebrew/releases/download/$DALEC_HOMEBREW_VERSION

for asset in SHA256SUMS metadata-bundle.digest metadata-bundle-manifest.json \
  metadata-formula.jws.json metadata-migrations.jws.json; do
  curl -fsSLO "$DALEC_HOMEBREW_ASSETS/$asset"
done

# Check what you downloaded (on macOS, replace sha256sum with shasum -a 256).
grep -E 'metadata-(bundle|formula|migrations)' SHA256SUMS | sha256sum -c -

DALEC_HOMEBREW_METADATA_BUNDLE=$PWD/homebrew-metadata
mkdir -p "$DALEC_HOMEBREW_METADATA_BUNDLE"
install -m 0444 metadata-bundle-manifest.json "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json"
install -m 0444 metadata-formula.jws.json "$DALEC_HOMEBREW_METADATA_BUNDLE/formula.jws.json"
install -m 0444 metadata-migrations.jws.json "$DALEC_HOMEBREW_METADATA_BUNDLE/formula_tap_migrations.jws.json"

DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$(tr -d '\n' < metadata-bundle.digest)
test "$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" = "sha256:$(sha256sum "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json" | awk '{print $1}')"
```

The three file names inside the bundle folder matter; the build rejects anything
else. The last line is a sanity check that the digest you will pass to the build
really is the digest of the manifest you just assembled.

The release also signs `SHA256SUMS` with keyless
[Cosign](https://github.com/sigstore/cosign). If you have `cosign` installed,
check that signature before trusting the checksums:

```console
curl -fsSLO "$DALEC_HOMEBREW_ASSETS/SHA256SUMS.bundle"
cosign verify-blob \
  --bundle SHA256SUMS.bundle \
  --certificate-identity-regexp '^https://github.com/sozercan/dalec-homebrew/\.github/workflows/release\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
```

### Step 3 — Write a spec

Create `hello.yaml`. The first line tells Docker to build this file with Dalec
instead of treating it as a Dockerfile, and `targets.homebrew.frontend.image`
tells Dalec to hand the work to `dalec-homebrew`:

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

`:latest` keeps the quickstart simple. For builds you want to reproduce or
verify later, pin the Dalec frontend to the exact digest recorded in
[`release/dalec-frontend.json`](release/dalec-frontend.json) — that is the
version every release is tested against.

### Step 4 — Build and run it

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

`--target homebrew/image` is required: `homebrew` selects the target in your
spec, and `image` is the only thing `dalec-homebrew` knows how to build.

The first build pulls the frontend images and the packages you asked for, so it
takes a few minutes. Later builds reuse BuildKit's cache.

## What you just built

The image contains the packages you asked for, their verified runtime
dependencies, and a small Ubuntu Noble base with CA certificates, a shell, and
common C libraries. It does **not** contain `brew`, `apt`, `dpkg`, package
caches, install receipts, Formula source, or build and test tooling.

| Property | Value |
| --- | --- |
| User | `linuxbrew` (`1000:1000`), non-root |
| Working directory | `/home/linuxbrew` |
| Packages installed under | `/home/linuxbrew/.linuxbrew` |
| `PATH` | generated for the packages you requested |
| Evidence and SBOM | `/usr/share/dalec-homebrew` |

Have a look inside:

```console
docker run --rm --entrypoint ls hello-runtime:quickstart /usr/share/dalec-homebrew
```

Those files record the exact package versions, bottle digests, file inventory,
what was pruned, and an SPDX SBOM. See
[Runtime contents and evidence](docs/usage.md#runtime-contents-and-evidence).

## Choose your packages

`dependencies.runtime` is a map of package names. The value is usually `{}`
(no options).

```yaml
dependencies:
  runtime:
    jq: {}                    # a package from homebrew/core
    homebrew/core/curl: {}    # the same thing, written out in full
    python@3.14: {}           # a versioned Formula name
    ripgrep:
      arch: [amd64, arm64]    # only build this package for these architectures
    acme/tools/widget: {}     # a package from a public GitHub tap
```

Rules worth knowing up front:

- You always get the **current stable** version of a package. Version ranges and
  historical versions are rejected — pin a `dalec-homebrew` release instead if
  you need a fixed set of package versions.
- Names are Homebrew Formula names. A plain name means `homebrew/core`, and
  `owner/tap/formula` means the public GitHub repository
  `github.com/<owner>/homebrew-<tap>`.
- Casks, private taps, arbitrary Git URLs, and source builds are not supported.

## Configure the image

Anything you would normally set in a Dockerfile has a field here:

```yaml
image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/redis-server
  cmd: --port 6379
  env:
    - LANG=C.UTF-8
  labels:
    org.opencontainers.image.title: redis-runtime
  volumes:
    /data: {}
  working_dir: /home/linuxbrew
  stop_signal: SIGTERM
  user: linuxbrew
```

The full list of supported fields, plus the rules for volumes, `PATH`, and
non-root users, is in the [usage reference](docs/usage.md#configure-the-image).

## Test the image during the build

Tests run inside the finished image, as the final user, with networking
disabled. If a test fails, no image is produced.

```yaml
tests:
  - name: hello-works
    steps:
      - command: hello
        stdout:
          contains: ["Hello, world!"]
    files:
      /home/linuxbrew/.linuxbrew/bin/hello: {}
```

See [Add runtime tests](docs/usage.md#add-runtime-tests) for every available
assertion.

## What is supported

| Supported | Not supported |
| --- | --- |
| `linux/amd64` and `linux/arm64` | Any other platform |
| Current stable `homebrew/core` Formulae | Historical versions, version ranges |
| Public default GitHub taps (`owner/tap/formula`) | Casks, private taps, arbitrary Git remotes |
| Official bottles, plus a few release-approved prebuilt archives | Source builds |
| Non-root images on the built-in runtime base | Custom base images |
| Tests that run offline in the final image | Tests that need a network or mounts |

## Troubleshooting

| Message | What it means |
| --- | --- |
| `metadata is stale: generated ..., maximum age 168h0m0s` | The release you picked is more than 7 days old. Use the [latest release](https://github.com/sozercan/dalec-homebrew/releases/latest). |
| `frontend: reference "...:v0.2.9" is not digest-pinned` | `targets.homebrew.frontend.image` (or `DALEC_HOMEBREW_FRONTEND_INDEX_REF`) uses a tag. Use the digest from step 1. |
| `required named context "dalec-homebrew-metadata" is missing` | The `--build-context dalec-homebrew-metadata=...` flag is missing or points at the wrong folder. |
| `frontend platform child and index must use the same repository` | `DALEC_HOMEBREW_FRONTEND_INDEX_REF` is missing, or the index and child come from different repositories. |
| `no such handler for target "": available targets: image` | You used `--target homebrew`. Use `--target homebrew/image`. |
| `... has version constraints; historical versions and version ranges are not supported` | Remove `version:` from the dependency; only the current stable release is supported. |
| `... does not match invoking gateway source ...` | The digest in `targets.homebrew.frontend.image` is not the one BuildKit loaded. Re-run step 1. |

More errors and their causes are listed in the
[usage reference](docs/usage.md#troubleshooting).

## Examples

Ready-made specs live in [`examples/`](examples/):

| Example | Shows |
| --- | --- |
| [`hello.yaml`](examples/hello.yaml) | A complete spec with a runtime test |
| [`forwarded-hello.yaml`](examples/forwarded-hello.yaml) | The smallest spec that builds an image |
| [`live-curl.yaml`](examples/live-curl.yaml) | curl and its transitive dependencies |
| [`live-python.yaml`](examples/live-python.yaml) | Python with TLS, SQLite, compression, and time zones |
| [`live-redis.yaml`](examples/live-redis.yaml) | A stateful, long-running, non-root service |
| [`live-toolchain.yaml`](examples/live-toolchain.yaml) | Many CLI tools in one image |
| [`live-graphviz.yaml`](examples/live-graphviz.yaml) | Plugins and generated runtime indexes |
| [`ci-noncore-multi-package.yaml`](examples/ci-noncore-multi-package.yaml) | Packages from public GitHub taps alongside `homebrew/core` |

Except for `hello.yaml` and `forwarded-hello.yaml`, these files are the
fixtures used by the project's own live tests, so their first line and
`targets.homebrew.frontend.image` are filled in by the test helper. Copy the
`dependencies`, `image`, and `tests` sections into a spec of your own.

## Learn more

- [Usage reference](docs/usage.md) — every supported field, image setting, test
  assertion, and evidence file.
- [Architecture](docs/architecture.md) — how resolution, verification, offline
  installation, and image assembly fit together.
- [Security](SECURITY.md) — the properties the build guarantees, and its limits.
- [Releases](docs/release.md) — what a release contains and how to verify it.
- [Contributing](CONTRIBUTING.md) — how to build and test `dalec-homebrew`
  itself.

Questions, bug reports, and feature requests are welcome in
[GitHub issues](https://github.com/sozercan/dalec-homebrew/issues). For
suspected vulnerabilities, follow [SECURITY.md](SECURITY.md#reporting-a-vulnerability)
instead of opening a public issue.
