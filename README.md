<div align="center">
  <h1>dalec-homebrew</h1>
  <p><strong>Build minimal Linux container images with Homebrew packages.</strong></p>
  <p>Choose the packages you need. <code>dalec-homebrew</code> builds a ready-to-run image for you.</p>
</div>

---

`dalec-homebrew` uses [Dalec](https://github.com/project-dalec/dalec) to turn Homebrew packages into minimal, non-root Linux container images.

## What you get

- Choose packages from `homebrew/core` or public default GitHub taps in a release-bound V2 frontend, including exact policy-authorized prebuilt executable archives.
- Get a minimal, non-root image without Homebrew or package managers.
- Keep an SBOM and a record of everything included in the image.

## Build an image

You need Docker Buildx and a digest-pinned `dalec-homebrew` frontend. Use a digest supplied by a trusted release pipeline, or see [Contributing](CONTRIBUTING.md) to build the component tuple yourself.

### Build from the command line

Build directly from the command line with `jq`:

```console
jq -nc '{
  dependencies: {runtime: {hello: {}}},
  image: {entrypoint: "/home/linuxbrew/.linuxbrew/bin/hello"}
}' |
  docker buildx build \
    --build-arg "BUILDKIT_SYNTAX=ghcr.io/sozercan/dalec-homebrew@sha256:<frontend-digest>" \
    --platform linux/amd64 \
    --tag hello-runtime:inline \
    --load \
    -

docker run --rm hello-runtime:inline
```

### Build from YAML

Save this as `hello.yaml`, replacing `<frontend-digest>` with the same immutable frontend digest:

```yaml
# syntax=ghcr.io/sozercan/dalec-homebrew@sha256:<frontend-digest>

dependencies:
  runtime:
    hello: {}

image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/hello
```

Build and run it:

```console
docker buildx build \
  --platform linux/amd64 \
  --file hello.yaml \
  --tag hello-runtime:spec \
  --load \
  .

docker run --rm hello-runtime:spec
```

Both forms print:

```text
Hello, world!
```

See the [usage reference](docs/usage.md) for image settings, tests, dependency rules, and the complete supported contract.

## Scope

| Supported | Not supported |
| --- | --- |
| Linux `amd64` and `arm64` | Other platforms |
| Current stable `homebrew/core` bottles, public default GitHub tap bottles, and exact release-policy-authorized prebuilt executable archives in V2-capable releases | Casks, private/authenticated taps, arbitrary Git remotes, general source builds, and non-self-contained bottle Formulae |
| Non-root images | Custom base images and networked tests |

## Public taps (V2)

A V2-capable frontend accepts `owner/tap/formula` and derives only the public default GitHub repository `https://github.com/<owner>/homebrew-<tap>`. Bare names and explicit `homebrew/core/formula` canonicalize to the same core identity. The capability is compiled into the signed component tuple; build arguments cannot enable it on a core-only frontend.

Non-core builds run the release-bound catalog extractor directly on the caller's BuildKit worker. BuildKit fetches the derived public GitHub tap, records the exact observed commit/tree/archive identity, and evaluates Formula metadata in a network-disabled read-only exec. Core-only builds continue to use the official Homebrew JWS and GHCR path and never run the extractor.

The frontend verifies bottle checksums and sizes, hostile-archive structure, embedded Formula bytes, and any digest-advertised Sigstore/in-toto bundle covered by the release tap policy. Missing provenance is recorded as an explicit per-artifact waiver; invalid advertised provenance fails the build. No catalog server, signing key, database, or public service origin is required.

A Formula without a bottle remains unsupported unless its exact Formula ID is present in the embedded tap policy as a prebuilt executable archive. For those entries, build-local ingestion verifies the complete upstream archive and executable properties, creates a deterministic receiptless derived bottle containing only the policy-selected payload, and passes those content-addressed bytes directly into offline materialization. Build input cannot add archive recipes or enable another Formula.

```yaml
dependencies:
  runtime:
    acme/tools/widget: {}
```

The example identity above is illustrative; use a Formula present in the public tap selected by your release/test environment.

## Examples

More examples: [multi-package toolchain](examples/live-toolchain.yaml), [Python](examples/live-python.yaml), [Redis](examples/live-redis.yaml), [Graphviz](examples/live-graphviz.yaml), and [glibc](examples/live-glibc.yaml).

## Learn more

[Usage](docs/usage.md) · [Security](SECURITY.md) · [Architecture](docs/architecture.md) · [Releases](docs/release.md) · [Contributing](CONTRIBUTING.md)
