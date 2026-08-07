# Contributing

Contributions are welcome. This guide covers the local workflow; the [architecture](docs/architecture.md), [security policy](SECURITY.md), and [release guide](docs/release.md) describe the design constraints that changes must preserve.

## Prerequisites

For Go development:

- Go 1.25.9 for CI and release parity; newer local toolchains are not the
  release-tested toolchain
- Bash, Make, `jq`, and Python 3

For component and integration work:

- Docker Buildx `v0.36.0` backed by BuildKit `v0.32.0` for parity with CI
  and release automation; these are the release-tested executor pins, while
  `github.com/moby/buildkit v0.31.2` is the Go module version
- For component rebuild mode, a writable OCI registry reachable from the selected BuildKit daemon
- For published-tuple mode, pull access from the selected BuildKit daemon to each digest-pinned component reference and the release-bound upstream Dalec frontend
- Builder access to authenticated Homebrew metadata, selected bottle layers, component registries, and the pinned Ubuntu inputs used by the selected mode

Additional reporting and VM validation tools are listed in their sections below.

## Set up the repository

Download dependencies and build the host tools:

```console
go mod download
make build
```

`make build` writes ignored, host-native binaries to `bin/`. The frontend is a BuildKit gateway process rather than a conventional CLI; the materializer and test runner are pipeline internals.

## Run validation

Use focused commands while iterating, then run the relevant CI-parity checks
before opening a pull request.

| Command | Runs |
| --- | --- |
| `make test` | All Go tests |
| `make vet` | `go vet ./...` |
| `make build` | Host builds for the primary command binaries, including `dalec-homebrew-release-manifest` |
| `make check` | Shell syntax checks, tests, vet, selected race tests, and Linux cross-builds for every command on `amd64` and `arm64` |

The canonical repository check is also available directly:

```console
./scripts/check.sh
```

Cross-compiled binaries are written to `/tmp`; validation should not modify the repository.

CI also runs dependency tidiness, host builds, and whitespace checks:

```console
go mod tidy
git diff --exit-code -- go.mod go.sum
make build
git diff --check
```

For workflow, Dockerfile, or Bake changes, CI additionally lints the workflows
with its pinned `actionlint` image, validates `./scripts/release-inputs.sh`,
prints the `release-children` and `frontend` Bake graph, and builds the `amd64`
frontend with the pinned Buildx and BuildKit executor. Run the applicable Docker
checks from [Build component images](#build-component-images) locally.

## Run the live BuildKit test

The live helper builds and tests one final, single-platform runtime image in
either component mode, but both modes use the canonical forwarding chain:

```text
BuildKit -> upstream Dalec -> dalec-homebrew homebrew/image -> runtime image
```

| Mode | Required variables | Behavior |
| --- | --- | --- |
| Component rebuild | `DALEC_HOMEBREW_LIVE_BUILDER`, `DALEC_HOMEBREW_LIVE_REGISTRY`, and `DALEC_HOMEBREW_LIVE_PLATFORM` | Builds and pushes the runtime base, materializer, and provider frontend with one fixed source-date epoch, then consumes their reported digests. |
| Published tuple | Builder and platform plus all three `DALEC_HOMEBREW_LIVE_*_REF` component variables | Skips component builds and passes the immutable tuple through upstream Dalec to the release-bound provider. |

The helper validates [`release/dalec-frontend.json`](release/dalec-frontend.json)
before any Docker command. It uses the pinned upstream index and fixed
`homebrew/image` route by default. Release automation may additionally set
`DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF` and
`DALEC_HOMEBREW_LIVE_TARGET` together; the reference must equal the pinned
index or the selected platform child, and the target must equal the pinned
route. Partial, mutable, mismatched, or malformed inputs fail before the builder
is inspected.

Leave all three provider-component reference variables unset to rebuild, or set
all three to replay a published tuple. The input fixture must start with a
`# syntax=` directive and must not already define a top-level `targets` mapping.
The helper replaces the directive with upstream Dalec, injects the exact
`targets.homebrew.frontend.image`, and builds `--target homebrew/image`.

Example component rebuild:

```console
DALEC_HOMEBREW_LIVE_BUILDER=dalec-homebrew-live-builder \
DALEC_HOMEBREW_LIVE_REGISTRY=dalec-homebrew-live-registry:5000 \
DALEC_HOMEBREW_LIVE_PLATFORM=linux/arm64 \
DALEC_HOMEBREW_LIVE_IMAGE=dalec-homebrew-live:arm64 \
./scripts/live-test.sh
```

The repository does not provision the builder or registry. Rebuild mode needs
a writable staging registry reachable from the builder; an HTTP registry is
acceptable only when that daemon is explicitly configured for it. Published
mode needs pull access to all supplied component references and the pinned
upstream Dalec frontend. `DALEC_HOMEBREW_LIVE_OUTPUT=push` additionally requires
write access for the final image.

All helper builds disable provenance. The helper is integration validation, not
a substitute for release provenance or signed evidence. The child frontend can
validate its own gateway `source`, but BuildKit does not give it an authenticated
identity for the parent dispatcher; the checked-in pin and top-level release
provenance bind upstream Dalec externally.

Common optional variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `DALEC_HOMEBREW_LIVE_SPEC` | `examples/live-test.yaml` | Direct-form Dalec fixture adapted for the forwarded final image |
| `DALEC_HOMEBREW_LIVE_IMAGE` | `dalec-homebrew-live:dev` | Final local or registry tag |
| `DALEC_HOMEBREW_LIVE_OUTPUT` | `load` | `load` the final image or `push` it |
| `DALEC_HOMEBREW_LIVE_PROGRESS` | `plain` | Buildx progress output |
| `DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE` | unset | RFC3339 lower bound for authenticated Homebrew metadata |
| `DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN` | `release/dalec-frontend.json` | External upstream Dalec index/children/route binding |

Rebuild-only options are `DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH` (default
`1781049600`), `DALEC_HOMEBREW_LIVE_RUN_ID` (timestamp and architecture), and
`DALEC_HOMEBREW_LIVE_UBUNTU_BASE` (the pinned platform child by default).

Published mode requires digest-pinned
`DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF`,
`DALEC_HOMEBREW_LIVE_MATERIALIZER_REF`, and
`DALEC_HOMEBREW_LIVE_FRONTEND_REF`. Runtime-base and materializer inputs
identify release indexes; the provider frontend input identifies the selected
platform child. References printed after a rebuild identify that run's
single-platform staging outputs, not release indexes.

The helper prints the selected provider components, upstream Dalec reference
and route, metadata floor, final image name, final digest, and immutable
`DALEC_HOMEBREW_LIVE_FINAL_REF` as shell assignments. After `push`, the final
reference is pullable from the final image repository.

## Exercise focused runtime closures

Use `DALEC_HOMEBREW_LIVE_SPEC` to run the same helper with a focused example:

- [`examples/live-toolchain.yaml`](examples/live-toolchain.yaml) — Azure CLI, OpenTofu, Go, Node/npm, jq, ripgrep, kubectl, and Helm in one closure
- [`examples/live-python.yaml`](examples/live-python.yaml) — extensions, TLS and CA data, SQLite, compression, and time zones
- [`examples/live-glibc.yaml`](examples/live-glibc.yaml) — the brewed loader, locale archive, and conversion modules
- [`examples/live-redis.yaml`](examples/live-redis.yaml) — a stateful non-root lifecycle
- [`examples/live-graphviz.yaml`](examples/live-graphviz.yaml) — plugins and generated shared runtime indexes

## Run the non-core production-path E2E

Pull requests run [`scripts/noncore-e2e.sh`](scripts/noncore-e2e.sh) on
`linux/amd64`. Unlike the core-only fixture set, this test exercises actual upstream-Dalec forwarding and assembles the full V2
path: a local registry, one component BuildKit worker, the release-bound
catalog extractor and bottle fetcher, and V2 materializer/frontend images. Tap
metadata and policy-derived bottles remain content-addressed BuildKit states;
no catalog service, signing key, or public tunnel is started. It then builds
[`examples/ci-noncore-multi-package.yaml`](examples/ci-noncore-multi-package.yaml)
from public-tap Formulae plus a core Formula and reruns runtime checks with
networking disabled. Build-local ingestion resolves each tap's default branch
and records the exact observed commit in extraction evidence.

The script is CI-oriented and requires explicit digest-pinned BuildKit and
registry image inputs. It validates the upstream Dalec binding from
`release/dalec-frontend.json` before starting Docker:

```console
DALEC_HOMEBREW_E2E_BUILDKIT_IMAGE=docker.io/moby/buildkit@sha256:<digest> \
DALEC_HOMEBREW_E2E_REGISTRY_IMAGE=docker.io/library/registry@sha256:<digest> \
DALEC_HOMEBREW_E2E_DALEC_FRONTEND_PIN=release/dalec-frontend.json \
DALEC_HOMEBREW_E2E_RUN_ID=local-1 \
./scripts/noncore-e2e.sh
```

It needs Docker privilege for the BuildKit worker, public network access to
GitHub, Homebrew metadata, GHCR, and the selected tap bottle hosts, and unused
loopback port `5000` for the ephemeral local registry.

## Validate a published image on a VM

For a pushed image using the default `linuxbrew` identity (`1000:1000`), pass
the exact digest-pinned image to the VM helper. It pulls the image over SSH and
runs it with networking disabled, a read-only root filesystem, all capabilities
dropped, and `no-new-privileges`:

```console
./scripts/vm-live-validate.sh \
  127.0.0.1:5556/dalec-homebrew-live@sha256:<digest> amd64
```

Requirements:

- `ssh` on the local machine
- Docker on the SSH target
- registry connectivity and credentials on the SSH target to pull the image
- an SSH target named `vm`, or `DALEC_HOMEBREW_VM_SSH_TARGET` set to another host

The helper also checks Linux `amd64` or `arm64`, Ubuntu 24.04 Noble, the
`1000:1000` runtime identity, required runtime evidence, root-owned non-writable
code, and the absence of build and package-management tooling. It is a runtime
hardening check only: it does not verify Cosign signatures, attestations, or the
signed release bundle.

## Generate an image-size report

The size reporter emits JSON for registry or local images:

```console
./scripts/image-size-report.sh \
  --platform linux/amd64 \
  IMAGE@sha256:<digest>

./scripts/image-size-report.sh \
  --insecure \
  --platform linux/arm64 \
  localhost:5000/IMAGE:tag
```

It requires Docker, `jq`, and Python 3. `crane` or `skopeo` is optional; the
script falls back to Docker Buildx metadata and may temporarily pull or copy the
image when necessary. Private images require credentials for the selected
resolver. `--insecure` permits an HTTP registry for `crane` or `skopeo`; it does
not provide authentication, and Docker or Buildx fallback paths still depend on
the daemon's registry configuration.

## Developer utilities

Resolve current Formulae without materializing an image:

```console
go run ./cmd/resolve \
  --roots hello,jq,ripgrep \
  --arch amd64 \
  --output resolution.json
```

The standalone resolver is a diagnostic tool. It authenticates Homebrew metadata and records GHCR descriptors, but it does not bind a release component tuple. Use the record verifier only with a resolution produced by an actual frontend build or another release-aware workflow:

```console
go run ./cmd/record-verify path/to/release-bound-resolution.json
```

`record-verify` checks schema and internal relationships recorded in the file; it
does not re-fetch or cryptographically reverify the source JWS envelopes or OCI
documents. Authenticate a persisted record through its signed release evidence
before relying on that structural verification.

Generate a canonical component manifest from immutable index and child references with `cmd/release-manifest` (run it with `--help` for the complete flag list), then validate and digest the result:

```console
go run ./cmd/release-manifest --help
go run ./cmd/release-verify path/to/components.json
```

[`release/components.example.json`](release/components.example.json) documents the shape but contains placeholders and is not directly verifiable.

## Build component images

[`docker-bake.hcl`](docker-bake.hcl) exposes platform-specific runtime-base and materializer targets plus the multi-platform frontend target:

```console
./scripts/release-inputs.sh | jq .
docker buildx bake --print release-children frontend
docker buildx bake --load runtime-base-amd64 materializer-amd64
docker buildx build \
  --target frontend \
  --platform linux/amd64 \
  --provenance=false \
  --tag dalec-homebrew:dev \
  --load \
  .

REGISTRY=registry.example VERSION=dev \
  docker buildx bake --push runtime-base-amd64 materializer-amd64
```

After loading a materializer child, exercise its pinned Homebrew checkout with
the digest-bound `glibc` regression bottle:

```console
./scripts/materializer-compat.sh \
  --image ghcr.io/sozercan/dalec-homebrew-materializer:dev-amd64 \
  --platform linux/amd64
```

Add `--current` to authenticate the current Homebrew Formula metadata, resolve
the exact `glibc` OCI layer, and pour that bottle with networking disabled. The
fixed check uses minimized, reviewed resolver records under `scripts/testdata/`
only to drive the production hostile-archive verifier; they are test fixtures,
not replayable release records. The fixed check is deterministic regression
coverage, while the moving check detects new Formula DSL requirements. Both are
focused materializer probes and do not replace `examples/live-glibc.yaml`
through the complete offline frontend path.

The two `--load` commands are local single-platform smoke builds. `--load`
exports their results to the local Docker daemon; the final command demonstrates
staging Bake output with explicit `REGISTRY`, `VERSION`, and `--push` values.
With a `docker-container` builder and no export option, results may remain only
in BuildKit's cache.
Local and staging tags are mutable discovery references, not released
identities. Release builds must follow the immutable component order, pin
review, signing, and promotion requirements in
[`docs/release.md`](docs/release.md).

## Pull request checklist

- Add or update tests for behavior changes.
- Update user documentation when the supported Dalec contract changes.
- Run `make check` and the relevant CI-parity checks above.
- Keep component and snapshot inputs digest-pinned; update the complete release tuple together.
- Avoid committing generated binaries, local resolution records, or size reports.
