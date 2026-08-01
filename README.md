<div align="center">
  <h1>dalec-homebrew</h1>
  <p><strong>Build minimal Linux container images with Homebrew packages.</strong></p>
  <p>Choose the packages you need. <code>dalec-homebrew</code> builds a ready-to-run image for you.</p>
</div>

---

`dalec-homebrew` uses [Dalec](https://github.com/project-dalec/dalec) to turn Homebrew packages into minimal, non-root Linux container images.

## What you get

- Choose packages from `homebrew/core`.
- Get a minimal, non-root image without Homebrew or package managers.
- Keep an SBOM and a record of everything included in the image.

## Build an image

You need Docker Buildx and a digest-pinned `dalec-homebrew` frontend. Use the digest from a published release; if none is available, see [Contributing](CONTRIBUTING.md) to build the frontend yourself.

### Build from the command line

Build directly from the command line with `jq`:

```console
docker buildx build \
  --build-arg "BUILDKIT_SYNTAX=ghcr.io/sozercan/dalec-homebrew@sha256:<frontend-digest>" \
  --platform linux/amd64 \
  --tag hello-runtime:inline \
  --load \
  - <<<"$(jq -c '.dependencies.runtime = {"hello": {}} | .image.entrypoint = "/home/linuxbrew/.linuxbrew/bin/hello"' <<<"{}")"

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
| Current `homebrew/core` bottles | Casks, third-party taps, and source builds |
| Non-root images | Custom base images and networked tests |

## Examples

More examples: [multi-package toolchain](examples/live-toolchain.yaml), [Python](examples/live-python.yaml), [Redis](examples/live-redis.yaml), [Graphviz](examples/live-graphviz.yaml), and [glibc](examples/live-glibc.yaml).

## Releases

Maintainers publish an existing v-prefixed SemVer tag, including supported pre-releases, by running the release workflow from the trusted `main` branch. The workflow builds, tests, scans, and signs the complete `amd64`/`arm64` component tuple before promoting the exact tested digests. See the [release guide](docs/release.md).

## Learn more

[Usage](docs/usage.md) · [Security](SECURITY.md) · [Architecture](docs/architecture.md) · [Releases](docs/release.md) · [Contributing](CONTRIBUTING.md)
