# Usage reference

`dalec-homebrew` accepts a deliberately small Dalec contract and produces a Linux runtime image from verified Homebrew bottles.

## Requirements

- A Linux `amd64` or `arm64` target
- Docker Buildx or `buildctl` backed by BuildKit 0.31.2 or newer
- A `dalec-homebrew` frontend reference pinned by digest
- Network access from the BuildKit daemon to the frontend and its bound components, `formulae.brew.sh`, and `ghcr.io`

The frontend, runtime base, and materializer are treated as one release component tuple. Mutable image tags are not accepted as trusted inputs.

## Build an image

A minimal Dalec spec looks like this:

```yaml
# syntax=ghcr.io/sozercan/dalec-homebrew@sha256:<frontend-digest>

dependencies:
  runtime:
    hello: {}

image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/hello
```

Replace `<frontend-digest>` with the immutable digest supplied by a trusted release or local component build, then build it:

```console
docker buildx build \
  --platform linux/amd64 \
  --file examples/hello.yaml \
  --tag hello-runtime:local \
  --load \
  .
```

The complete example is available at [`../examples/hello.yaml`](../examples/hello.yaml). Use `linux/arm64` for an Arm target.

## Declare runtime dependencies

The map form supports per-Formula options:

```yaml
dependencies:
  runtime:
    hello: {}
    jq: {}
    ripgrep:
      arch: [amd64, arm64]
```

The list shorthand is also accepted:

```yaml
dependencies:
  runtime: [hello, jq]
```

Dependency rules:

- Empty version constraints select the current Formula in the authenticated Homebrew snapshot.
- Canonical versioned Formula names such as `python@3.14` are supported.
- `arch` may contain `amd64`, `arm64`, or both.
- Formula names containing tap or path syntax are rejected before metadata access.
- Historical versions and version ranges are not supported.
- Dependencies may be declared globally and on the selected Dalec target.
- Every selected platform must have at least one applicable runtime dependency.
- A multi-platform build fails if a requested root resolves to different package versions across platforms.

## Configure the image

The supported Dalec image fields are:

- `entrypoint`
- `cmd`
- `env`
- `labels`
- `volumes`
- `working_dir`
- `stop_signal`
- `user`

The default runtime user is `linuxbrew` (`1000:1000`) and the default working directory is `/home/linuxbrew`. The frontend rejects `root`, UID or GID zero, malformed identities, and unknown named users. Explicit numeric non-root identities must include both UID and GID.

Volumes may not overlap protected runtime paths.

## Add runtime tests

Global and selected-target Dalec command and file tests are supported when they do not use mounts. Tests run during the image build against the final pruned filesystem with the final image user, environment, and working directory. Test execution has no network access.

See the files under [`../examples/`](../examples/) for command output, filesystem, plugin, locale, and stateful workload checks.

## Supported Dalec contract

Package metadata fields such as `name`, `description`, `website`, `version`, `revision`, and `license` may be supplied, but they are optional for this dependency-only runtime frontend.

V1 accepts global and selected-target `dependencies.runtime`, the image fields listed above, and tests without mounts.

The following are rejected:

- build, recommended, test, or sysext dependencies
- extra package repositories
- sources and patches
- build steps, build mounts, caches, or build network configuration
- package artifacts or package configuration
- `provides`, `replaces`, or `conflicts`
- image base overrides or post-install image steps
- casks, third-party taps, source builds, historical versions, and version ranges
- test mounts or networked tests

## Runtime contents and evidence

The output contains the selected Formulae, their verified runtime closure, a conservative Ubuntu Noble runtime base, and machine-readable evidence under `/usr/share/dalec-homebrew`:

| File | Purpose |
| --- | --- |
| `manifest.json` | Final runtime manifest and resolution binding |
| `resolution.json` | Authenticated metadata, exact OCI descriptors, dependency closure, and component identities |
| `runtime-inventory.json` | Selected runtime paths, ownership, modes, digests, and package attribution |
| `prune-manifest.json` | Versioned record of the runtime pruning decision |
| `sbom.spdx.json` | SPDX 2.3 software bill of materials |
| `materialization.json` | Offline installation and verified-bottle results |
| `runtime-base-packages.tsv` | Chisel package versions, architectures, selected bytes, and source package digests |
| `runtime-base-artifacts.tsv` | Deliberate non-package files included in the runtime base |
| `runtime-base-chisel.manifest.wall` | Chisel's authoritative path and slice manifest |

The base retains common runtime facilities such as CA trust, Bash and Dash, core command-line utilities, NSS and DNS support, timezone data, glibc conversion data, and common C and C++ libraries.

The final image does **not** contain `apt`, `dpkg`, Chisel, `brew`, the Homebrew repository or download cache, installer logs, receipts, embedded Formula source, or materializer and test tooling.

For the verification and assembly flow, see [`architecture.md`](architecture.md). For release trust and digest binding, see [`release.md`](release.md) and the repository [`SECURITY.md`](../SECURITY.md).
