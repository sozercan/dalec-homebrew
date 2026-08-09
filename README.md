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

The only supported production path uses two immutable gateway images:

1. the upstream Dalec syntax frontend, which selects the `homebrew` target; and
2. the digest-pinned `dalec-homebrew` child frontend, selected through
   `targets.homebrew.frontend.image` and invoked at child route `image`.

Release-bound child frontends also require the matching digest-pinned
`dalec-homebrew` parent index through the
`DALEC_HOMEBREW_FRONTEND_INDEX_REF` build argument. Upstream Dalec forwards
that argument to the child; the child keeps the executing platform child and
its separately trusted parent index as distinct identities.

The child advertises `image` only so upstream Dalec can discover and forward to
that route. Direct use of `dalec-homebrew` as the syntax frontend is unsupported:
an `image` solve without the forwarded `homebrew` target context is rejected.

Use the exact digests from trusted release evidence. The repository binding in
[`release/dalec-frontend.json`](release/dalec-frontend.json) records the upstream
Dalec index, its Linux platform children, and the fixed `homebrew/image` route.

### Build from the command line through upstream Dalec

Build from stdin through upstream Dalec with `jq`:

```console
DALEC_FRONTEND=ghcr.io/project-dalec/dalec/frontend@sha256:<dalec-frontend-digest>
DALEC_HOMEBREW_INDEX=ghcr.io/sozercan/dalec-homebrew@sha256:<dalec-homebrew-index-digest>
DALEC_HOMEBREW_CHILD=ghcr.io/sozercan/dalec-homebrew@sha256:<dalec-homebrew-child-digest>

jq -nc --arg child_frontend "$DALEC_HOMEBREW_CHILD" '{
  dependencies: {runtime: {hello: {}}},
  image: {entrypoint: "/home/linuxbrew/.linuxbrew/bin/hello"},
  targets: {homebrew: {frontend: {image: $child_frontend}}}
}' |
  docker buildx build \
    --build-arg "BUILDKIT_SYNTAX=$DALEC_FRONTEND" \
    --build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
    --target homebrew/image \
    --platform linux/amd64 \
    --tag hello-runtime:inline \
    --load \
    -

docker run --rm hello-runtime:inline
```

### Build from YAML

Save this as `hello.yaml`, replacing the syntax and child placeholders with
immutable digests:

```yaml
# syntax=ghcr.io/project-dalec/dalec/frontend@sha256:<dalec-frontend-digest>

dependencies:
  runtime:
    hello: {}

image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/hello

targets:
  homebrew:
    frontend:
      image: ghcr.io/sozercan/dalec-homebrew@sha256:<dalec-homebrew-child-digest>
```

Build and run it:

```console
DALEC_HOMEBREW_INDEX=ghcr.io/sozercan/dalec-homebrew@sha256:<dalec-homebrew-index-digest>

docker buildx build \
  --build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
  --target homebrew/image \
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

The upstream frontend forwards the effective Dalec spec to the exact
`targets.homebrew.frontend.image`. The child requires the selected spec target
to be `homebrew`, the child route to be `image`, target and invocation `cmdline`
values to be empty, and the target frontend image to equal the child gateway
`source`. Inside the child solve, `source` identifies `dalec-homebrew`, so the
child can authenticate its own digest but cannot prove which parent frontend
invoked it. Trusted releases bind the upstream Dalec index and children
externally through the release pin and signed provenance.

The `dependencies.runtime` mapping is unordered. For each platform, applicable
roots are sorted lexicographically by canonical requested Formula ID. This
canonical order is recorded in resolution evidence and drives the default
generated `PATH`; installation uses a separate deterministic topological order
so dependencies precede dependents. Each global or selected-target
`dependencies` scope must either be omitted or contain a non-empty runtime map;
omit the selected scope to inherit global roots.

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

Start with the standalone [forwarded hello](examples/forwarded-hello.yaml). The
[multi-package toolchain](examples/live-toolchain.yaml),
[curl](examples/live-curl.yaml), [Python plus curl](examples/live-python-curl.yaml),
[Hugging Face plus curl](examples/live-hf-curl.yaml), [Python](examples/live-python.yaml),
[Redis](examples/live-redis.yaml), [Graphviz](examples/live-graphviz.yaml), and
[glibc](examples/live-glibc.yaml) files are base fixtures for `scripts/live-test.sh`; the helper validates them,
injects the release-bound `targets.homebrew.frontend.image` child mapping, and
builds through upstream Dalec's `homebrew/image` target.

## Learn more

[Usage](docs/usage.md) · [Security](SECURITY.md) · [Architecture](docs/architecture.md) · [Releases](docs/release.md) · [Contributing](CONTRIBUTING.md)
