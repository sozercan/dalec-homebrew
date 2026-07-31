<div align="center">
  <h1>dalec-homebrew</h1>
  <p><strong>Verified Homebrew bottles. Clean Dalec runtime images.</strong></p>
  <p>Declare packages once, verify the complete closure, and ship a purpose-built runtime.</p>
  <p><code>Dalec spec</code> → <code>authenticated metadata</code> → <code>verified bottles</code> → <code>runtime image</code></p>
  <p>
    <a href="https://github.com/sozercan/dalec-homebrew/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/sozercan/dalec-homebrew/actions/workflows/ci.yml/badge.svg"></a>
    <a href="LICENSE"><img alt="Apache-2.0 license" src="https://img.shields.io/badge/license-Apache--2.0-4c6ef5.svg"></a>
  </p>
</div>

---

`dalec-homebrew` is an out-of-tree [Dalec](https://github.com/project-dalec/dalec) BuildKit gateway frontend. It turns packages declared in `dependencies.runtime` into a non-root Linux image built from authenticated `homebrew/core` metadata and exact bottle layers.

## What you get

- **Straightforward specs** — add Homebrew Formulae through Dalec's normal runtime dependency field.
- **Verified inputs** — bind signed Formula metadata, checksums, and OCI descriptors into a replayable resolution record.
- **Offline installation** — materialize the resolved bottle closure with networking disabled.
- **A clean runtime** — start from a snapshot-pinned Ubuntu Chisel base without OS package managers, Chisel, the Homebrew repository or download cache, or materializer tooling.
- **Build evidence** — include an SPDX SBOM, runtime inventory, resolution record, pruning data, and base-image provenance.

## Build an image

You need Docker Buildx backed by BuildKit 0.31.2 or newer. The builder must be able to reach the frontend and its bound components, `formulae.brew.sh`, and `ghcr.io`.

The checked-in examples are templates; this repository does not currently publish a ready-to-use frontend digest. Build the components from source or use a digest supplied by your release pipeline.

After replacing `<frontend-digest>` in [`examples/hello.yaml`](examples/hello.yaml) with that immutable digest, run:

```console
docker buildx build \
  --platform linux/amd64 \
  --file examples/hello.yaml \
  --tag hello-runtime:local \
  --load \
  .

docker run --rm hello-runtime:local
```

```text
Hello, world!
```

> [!IMPORTANT]
> The frontend reference must use `@sha256:...`. Mutable tags are rejected. To build the frontend and its components from source, follow [`CONTRIBUTING.md`](CONTRIBUTING.md).

Adding packages is just a spec change:

```yaml
dependencies:
  runtime:
    hello: {}
    jq: {}
    ripgrep:
      arch: [amd64, arm64]
```

See the [usage reference](docs/usage.md) for image settings, tests, dependency rules, and the complete supported contract.

## Scope

| Supported | Not currently supported |
| --- | --- |
| Linux `amd64` and `arm64` | Other platforms |
| Current `homebrew/core` Formulae with compatible Linux bottles | Casks, third-party taps, or source builds |
| Canonical versioned Formulae such as `python@3.14` | Historical versions or version ranges |
| Non-root runtime identities | Root users or custom image bases |
| Dalec command and file tests without mounts | Networked tests or test mounts |

## Examples

| Example | Demonstrates |
| --- | --- |
| [`hello.yaml`](examples/hello.yaml) | Smallest complete runtime |
| [`live-python.yaml`](examples/live-python.yaml) | Extensions, TLS, SQLite, compression, and time zones |
| [`live-redis.yaml`](examples/live-redis.yaml) | Stateful non-root execution |
| [`live-graphviz.yaml`](examples/live-graphviz.yaml) | Plugins and generated runtime indexes |
| [`live-glibc.yaml`](examples/live-glibc.yaml) | Brewed loader, locales, and conversion data |

## Documentation

- [Usage reference](docs/usage.md) — specs, builds, supported fields, and image contents
- [Security policy](SECURITY.md) — trust boundaries, verification properties, and reporting
- [Architecture](docs/architecture.md) — resolution, materialization, and runtime assembly
- [Release and rollback](docs/release.md) — component tuples, immutable releases, and promotion
- [Contributing](CONTRIBUTING.md) — local development, validation, and live tests

## License

Licensed under the [Apache License 2.0](LICENSE).
