# Contributing

Contributions are welcome. This guide covers the local workflow; the [architecture](docs/architecture.md), [security policy](SECURITY.md), and [release guide](docs/release.md) describe the design constraints that changes must preserve.

## Prerequisites

For Go development:

- Go 1.25.9 or newer
- Bash, Make, `jq`, and Python 3

For component and integration work:

- Docker Buildx backed by BuildKit 0.31.2 or newer
- For component rebuild mode, a writable OCI registry reachable from the selected BuildKit daemon
- For published-tuple mode, pull access from the selected BuildKit daemon to each digest-pinned component reference
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

Use focused commands while iterating, then run the complete suite before opening a pull request.

| Command | Runs |
| --- | --- |
| `make test` | All Go tests |
| `make vet` | `go vet ./...` |
| `make build` | Host builds for the primary command binaries, including `dalec-homebrew-release-manifest` |
| `make check` | Shell syntax checks, tests, vet, selected race tests, and Linux cross-builds for every command on `amd64` and `arm64` |

The full validation entrypoint is also available directly:

```console
./scripts/check.sh
```

Cross-compiled binaries are written to `/tmp`; validation should not modify the repository.

## Run the live BuildKit test

The live helper builds and tests one final, single-platform runtime image in either mode:

| Mode | Required variables | Behavior |
| --- | --- | --- |
| Component rebuild | `DALEC_HOMEBREW_LIVE_BUILDER`, `DALEC_HOMEBREW_LIVE_REGISTRY`, and `DALEC_HOMEBREW_LIVE_PLATFORM` | Builds and pushes the runtime base, materializer, and frontend with one fixed source-date epoch, then consumes their reported digests. |
| Published tuple | Builder and platform plus all three `DALEC_HOMEBREW_LIVE_*_REF` variables | Skips component builds and passes the immutable tuple to the release-bound frontend. |

Leave all three component-reference variables unset to rebuild, or set all three to replay a published tuple. Partial sets and mutable or malformed references are rejected before the builder is inspected. The input spec must start with a `# syntax=` directive; the helper replaces that first line with the selected frontend digest before the final build.

Example component rebuild:

```console
DALEC_HOMEBREW_LIVE_BUILDER=dalec-homebrew-live-builder \
DALEC_HOMEBREW_LIVE_REGISTRY=dalec-homebrew-live-registry:5000 \
DALEC_HOMEBREW_LIVE_PLATFORM=linux/arm64 \
DALEC_HOMEBREW_LIVE_IMAGE=dalec-homebrew-live:arm64 \
./scripts/live-test.sh
```

The repository does not provision the builder or registry. Rebuild mode needs a writable staging registry reachable from the builder; an HTTP registry is acceptable only when that daemon is explicitly configured for it. Published mode needs pull access to the supplied component references. `DALEC_HOMEBREW_LIVE_OUTPUT=push` additionally requires write access for the final image.

All helper builds disable provenance. The helper does not verify release signatures or attestations, sign, scan, promote, or clean up registry artifacts; use the release workflow for those guarantees.

Common options:

| Variable | Default | Purpose |
| --- | --- | --- |
| `DALEC_HOMEBREW_LIVE_SPEC` | `examples/live-test.yaml` | Dalec spec used for the final image |
| `DALEC_HOMEBREW_LIVE_IMAGE` | `dalec-homebrew-live:dev` | Final local or registry tag |
| `DALEC_HOMEBREW_LIVE_OUTPUT` | `load` | `load` the final image or `push` it |
| `DALEC_HOMEBREW_LIVE_PROGRESS` | `plain` | Buildx progress output |
| `DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE` | unset | RFC3339 lower bound for authenticated Homebrew metadata |

Rebuild-only options are `DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH` (default `1781049600`), `DALEC_HOMEBREW_LIVE_RUN_ID` (timestamp and architecture), and `DALEC_HOMEBREW_LIVE_UBUNTU_BASE` (the pinned platform child by default).

Published mode requires digest-pinned `DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF`, `DALEC_HOMEBREW_LIVE_MATERIALIZER_REF`, and `DALEC_HOMEBREW_LIVE_FRONTEND_REF`. Runtime-base and materializer inputs identify release indexes; the frontend input identifies the selected platform child. References printed after a rebuild identify that run's single-platform staging outputs, not release indexes.

The helper always prints the final image digest and immutable `DALEC_HOMEBREW_LIVE_FINAL_REF`; after `push`, that reference is pullable from the final image repository.

## Exercise focused runtime closures

Use `DALEC_HOMEBREW_LIVE_SPEC` to run the same helper with a focused example:

- [`examples/live-toolchain.yaml`](examples/live-toolchain.yaml) — Azure CLI, OpenTofu, Go, Node/npm, jq, ripgrep, kubectl, and Helm in one closure
- [`examples/live-python.yaml`](examples/live-python.yaml) — extensions, TLS and CA data, SQLite, compression, and time zones
- [`examples/live-glibc.yaml`](examples/live-glibc.yaml) — the brewed loader, locale archive, and conversion modules
- [`examples/live-redis.yaml`](examples/live-redis.yaml) — a stateful non-root lifecycle
- [`examples/live-graphviz.yaml`](examples/live-graphviz.yaml) — plugins and generated shared runtime indexes

## Validate a published image on a VM

For a pushed image using the default `linuxbrew` identity (`1000:1000`), the VM helper pulls it over SSH and runs the image with networking disabled, a read-only root filesystem, all capabilities dropped, and `no-new-privileges`:

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
docker buildx bake runtime-base-amd64 materializer-amd64
```

Local tags are staging references. Release builds must follow the immutable component order, pin review, signing, and promotion requirements in [`docs/release.md`](docs/release.md).

## Pull request checklist

- Add or update tests for behavior changes.
- Update user documentation when the supported Dalec contract changes.
- Run `make check`.
- Keep component and snapshot inputs digest-pinned; update the complete release tuple together.
- Avoid committing generated binaries, local resolution records, or size reports.
