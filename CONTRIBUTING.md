# Contributing

Contributions are welcome. This guide covers the local workflow; the [architecture](docs/architecture.md), [security policy](SECURITY.md), and [release guide](docs/release.md) describe the design constraints that changes must preserve.

## Prerequisites

For Go development:

- Go 1.25.9 or newer
- Bash and Make

For component and integration work:

- Docker Buildx or `buildctl` backed by BuildKit 0.31.2 or newer
- `jq`
- A writable OCI registry reachable from the selected BuildKit daemon
- Outbound access to GitHub and the pinned Ubuntu snapshot while building components

Additional reporting and VM validation tools are listed in their sections below.

## Set up the repository

Download dependencies and build the host tools:

```console
go mod download
make build
```

`make build` writes ignored, host-native binaries to `bin/`. The frontend is a BuildKit gateway process rather than a conventional CLI; the materializer and test runner are pipeline internals.

## Run validation

Use focused commands while iterating, then run the complete suite before opening a pull request.

| Command | Runs |
| --- | --- |
| `make test` | All Go tests |
| `make vet` | `go vet ./...` |
| `make build` | Host builds for the six primary commands |
| `make check` | Shell syntax checks, tests, vet, selected race tests, and Linux cross-builds for every command on `amd64` and `arm64` |

The full validation entrypoint is also available directly:

```console
./scripts/check.sh
```

Cross-compiled binaries are written to `/tmp`; validation should not modify the repository.

## Run the live BuildKit test

The live helper performs one complete, single-platform build:

1. build and push a runtime-base component,
2. build and push a materializer component,
3. build and push a frontend bound to those component digests,
4. replace the example's syntax line with the frontend digest,
5. build and test the final runtime image, and
6. load or push the result and print the immutable component references.

```console
DALEC_HOMEBREW_LIVE_BUILDER=dalec-homebrew-live-builder \
DALEC_HOMEBREW_LIVE_REGISTRY=dalec-homebrew-live-registry:5000 \
DALEC_HOMEBREW_LIVE_PLATFORM=linux/arm64 \
DALEC_HOMEBREW_LIVE_IMAGE=dalec-homebrew-live:arm64 \
./scripts/live-test.sh
```

The repository does not provision the builder or registry. The registry must be reachable from the BuildKit daemon, not only from the host. An HTTP registry is fine for local development when it is configured as insecure in that daemon.

The helper uses temporary mutable tags for publication but consumes every component by digest. It disables provenance and does not sign, scan, promote, or clean up registry artifacts.

Optional variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `DALEC_HOMEBREW_LIVE_SPEC` | `examples/live-test.yaml` | Dalec spec used for the final image |
| `DALEC_HOMEBREW_LIVE_IMAGE` | `dalec-homebrew-live:dev` | Final local or registry tag |
| `DALEC_HOMEBREW_LIVE_OUTPUT` | `load` | `load` the final image or `push` it |
| `DALEC_HOMEBREW_LIVE_PROGRESS` | `plain` | Buildx progress output |
| `DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH` | `1781049600` | Reproducible component timestamp |
| `DALEC_HOMEBREW_LIVE_RUN_ID` | timestamp and architecture | Temporary component tag suffix |
| `DALEC_HOMEBREW_LIVE_UBUNTU_BASE` | pinned platform child | Runtime base override for local testing |

When `DALEC_HOMEBREW_LIVE_OUTPUT=push`, the helper prints `DALEC_HOMEBREW_LIVE_FINAL_REF` with the immutable final manifest digest.

## Exercise focused runtime closures

Use `DALEC_HOMEBREW_LIVE_SPEC` to run the same helper with a focused example:

- [`examples/live-python.yaml`](examples/live-python.yaml) — extensions, TLS and CA data, SQLite, compression, and time zones
- [`examples/live-glibc.yaml`](examples/live-glibc.yaml) — the brewed loader, locale archive, and conversion modules
- [`examples/live-redis.yaml`](examples/live-redis.yaml) — a stateful non-root lifecycle
- [`examples/live-graphviz.yaml`](examples/live-graphviz.yaml) — plugins and generated shared runtime indexes

## Validate a published image on a VM

For a pushed image, the VM helper pulls it over SSH and runs the image with networking disabled, a read-only root filesystem, all capabilities dropped, and `no-new-privileges`:

```console
./scripts/vm-live-validate.sh \
  127.0.0.1:5556/dalec-homebrew-live@sha256:<digest> amd64
```

Requirements:

- `ssh` on the local machine
- Docker on the SSH target
- an SSH target named `vm`, or `DALEC_HOMEBREW_VM_SSH_TARGET` set to another host

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

It requires Docker, `jq`, and Python 3. `crane` or `skopeo` is optional; the script falls back to Docker Buildx metadata and may temporarily pull or copy the image when necessary.

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

Validate and digest a populated component manifest with:

```console
go run ./cmd/release-verify path/to/components.json
```

[`release/components.example.json`](release/components.example.json) documents the shape but contains placeholders and is not directly verifiable.

## Build component images

[`docker-bake.hcl`](docker-bake.hcl) exposes platform-specific runtime-base and materializer targets plus the multi-platform frontend target:

```console
docker buildx bake --print
docker buildx bake runtime-base-amd64 materializer-amd64
```

Local tags are staging references. Release builds must follow the immutable component order, pin review, signing, and promotion requirements in [`docs/release.md`](docs/release.md).

## Pull request checklist

- Add or update tests for behavior changes.
- Update user documentation when the supported Dalec contract changes.
- Run `make check`.
- Keep component and snapshot inputs digest-pinned; update the complete release tuple together.
- Avoid committing generated binaries, local resolution records, or size reports.
