# dalec-homebrew

An out-of-tree [Dalec](https://github.com/project-dalec/dalec) BuildKit gateway frontend that turns `dependencies.runtime` into a minimal Linux image containing verified `homebrew/core` bottles.

The implementation is intentionally narrow:

- Linux `amd64` and `arm64`
- current stable `homebrew/core` Formulae
- bottle-only installation
- authenticated Homebrew JWS metadata
- exact OCI index, manifest, config, and layer descriptors
- offline materialization with `llb.NetModeNone`
- clean-runtime-base assembly (the materializer filesystem is never exported)
- non-root runtime users and root-owned, non-writable code
- global and selected-target Dalec command/file tests

Historical versions, version ranges, casks, third-party taps, source builds, custom runtime bases, and networked tests are not supported in V1.

## Dalec interface

```yaml
# syntax=ghcr.io/sozercan/dalec-homebrew@sha256:<frontend-digest>

name: homebrew-runtime
description: Runtime image with Homebrew packages
website: https://brew.sh/
version: 0.1.0
revision: 1
license: Apache-2.0

dependencies:
  runtime:
    hello: {}
    jq: {}
    ripgrep:
      arch: [amd64, arm64]

image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/hello
```

The legacy list shorthand is also accepted:

```yaml
dependencies:
  runtime: [hello, jq]
```

Empty version constraints select the stable Formula in the authenticated snapshot. Any non-empty `version` list is rejected as a V2 feature. Formula names containing tap/path syntax are rejected before metadata access. Explicit canonical versioned Formulae such as `python@3.13` are supported.

## Build pipeline

```text
raw Dalec preflight
  -> signed Formula + migration metadata
  -> per-platform OCI descriptor resolution
  -> canonical resolution.json + independent verification
  -> exact layer blobs through llb.ImageBlob
  -> static archive verification
  -> offline Homebrew install in deterministic topological order
  -> per-install containment and complete-keg verification
  -> allowlist copy into a clean runtime base
  -> static ELF/shebang/link verification
  -> final-user, final-environment Dalec tests
  -> OCI image configuration with resolution source_date_epoch
```

The output embeds:

- `/usr/share/dalec-homebrew/manifest.json`
- `/usr/share/dalec-homebrew/resolution.json`
- `/usr/share/dalec-homebrew/runtime-inventory.json`
- `/usr/share/dalec-homebrew/prune-manifest.json`
- `/usr/share/dalec-homebrew/sbom.spdx.json`
- `/usr/share/dalec-homebrew/materialization.json`
- `/usr/share/dalec-homebrew/runtime-base-packages.tsv`
- `/usr/share/dalec-homebrew/runtime-base-artifacts.tsv`

The final image does **not** contain `brew`, the Homebrew repository, caches, installer logs, receipts, embedded Formula source, or materializer/test tooling.

## Component tuple

A release frontend must bind digest-pinned runtime-base and materializer images. The frontend source itself must also be invoked by digest. Values may be compiled in with the Dockerfile or supplied as build args for local development:

```text
DALEC_HOMEBREW_RUNTIME_BASE
DALEC_HOMEBREW_MATERIALIZER
DALEC_HOMEBREW_FRONTEND_REF
DALEC_HOMEBREW_COMMIT
DALEC_HOMEBREW_KEYS_DIGEST
```

`DALEC_HOMEBREW_FRONTEND_REF` defaults to BuildKit's digest-pinned gateway `source` option. Mutable tags are rejected. The platform child digests returned by BuildKit are recorded in each resolution record.

See [`release/components.example.json`](release/components.example.json) and [`docs/release.md`](docs/release.md).

## Development

Requirements:

- Go 1.25.9 or newer
- BuildKit 0.31.2 or newer for `llb.ImageBlob`, `State.Requires`, and current exporter epoch behavior
- Docker Buildx or `buildctl` for image integration tests
- `jq` for the live component-build helper

Run validation:

```console
./scripts/check.sh
```

Generate a repeatable JSON image-size report. Registry references include exact
manifest, config, and compressed layer sizes; local images also include rootfs,
package, evidence, largest-path, and history data:

```console
./scripts/image-size-report.sh --platform linux/amd64 IMAGE@sha256:<digest>
./scripts/image-size-report.sh --insecure --platform linux/arm64 localhost:5000/IMAGE:tag
```

Run a real single-platform frontend build against a registry reachable from the
selected Buildx builder:

```console
DALEC_HOMEBREW_LIVE_BUILDER=dalec-homebrew-live-builder \
DALEC_HOMEBREW_LIVE_REGISTRY=dalec-homebrew-live-registry:5000 \
DALEC_HOMEBREW_LIVE_PLATFORM=linux/arm64 \
DALEC_HOMEBREW_LIVE_IMAGE=dalec-homebrew-live:arm64 \
./scripts/live-test.sh
```

The helper builds and publishes digest-addressed runtime-base, materializer,
and frontend components, renders [`examples/live-test.yaml`](examples/live-test.yaml)
with the resulting frontend digest, runs its Dalec command/file tests, and
loads the final image. The supplied registry may use HTTP for local testing,
but it must be configured in the selected BuildKit daemon.

Resolve current Formulae without materializing an image:

```console
go run ./cmd/resolve --roots hello,jq,ripgrep --arch amd64 --output resolution.json
go run ./cmd/record-verify resolution.json # requires a release-bound component tuple
```

Build component images in release order:

1. `runtime-base-amd64` and `runtime-base-arm64`
2. platform materializer children
3. immutable multi-platform base/materializer indexes
4. frontend, with those index/child identities bound into its release manifest

The included `docker-bake.hcl` exposes the build targets. Signing, provenance, vulnerability scanning, OCI referrer publication, mirror retention, and promotion are release-pipeline responsibilities and are deliberately not performed by the frontend.

## V1 contract checks

Supported Dalec fields:

- required package metadata
- global and selected-target `dependencies.runtime`
- global and selected-target tests without mounts
- image `entrypoint`, `cmd`, `env`, `labels`, `volumes`, `working_dir`, `stop_signal`, and `user`

Rejected fields include build/recommended/test/sysext dependencies, extra repositories, sources, patches, build steps/mounts, package artifacts/configuration, provides/replaces/conflicts, image bases/post steps, and volumes overlapping protected runtime paths.

The default user is `linuxbrew` (`1000:1000`). `root`, UID/GID zero, malformed identities, and unknown named users are rejected. Numeric non-root identities are supported.

## Security and upstream trust notes

Homebrew's public `formula.jws.json` authenticates Formula metadata and bottle checksums but currently has no signed timestamp. The implementation records whether freshness came from signed payload data or HTTP `Last-Modified`; production release jobs should supply a monotonic rollback floor and persist accepted snapshot identities.

The Formula JWS authenticates the compressed bottle checksum, but it does not bind the OCI index annotations containing `sh.brew.tab`. This implementation:

1. treats signed Formula declarations as package identity authority,
2. verifies the entire fetched OCI descriptor chain by digest and size,
3. requires the selected layer digest to equal the signed Homebrew checksum,
4. treats bottle-tab dependencies as minimum/consistency evidence, and
5. records an explicit upstream-attestation waiver unless release infrastructure provides a stronger attestation policy.

Current Homebrew bottle tarballs generally do not contain `INSTALL_RECEIPT.json`; Homebrew creates it while pouring the bottle. The archive verifier can require a pre-install receipt for fixtures/alternate producers, while production verifies the generated receipt after offline installation.

Modern Homebrew also forbids local bottle paths by default and its public
`brew install` command performs mutable tap/prefix preflight. The materializer
therefore invokes the pinned Homebrew `FormulaInstaller` through a minimal
read-only Ruby adapter after independent bottle verification. Resolution,
dependency selection, network access, and source fallback remain disabled;
normal Homebrew extraction, relocation, linking, `etc`/`var`, global
post-install, and Formula post-install still run.

See [`SECURITY.md`](SECURITY.md) for the enforced trust boundaries.

## Module pins

The project pins:

- `github.com/project-dalec/dalec v0.21.5-0.20260728234020-5fa2c46d716b` (includes the list-form dependency decoder fix)
- `github.com/moby/buildkit v0.31.2`
- Homebrew commit `77d90328ca2f63ff4ec1f67de0ade5632f5d2335` in the materializer recipe
- Ubuntu Noble platform-child digests in `docker-bake.hcl`

These are release inputs, not update channels. A release should update and sign the complete tuple together.
