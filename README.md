<div align="center">
  <h1>dalec-homebrew</h1>
  <p><strong>Turn Homebrew packages into small, non-root Linux container images.</strong></p>
</div>

`dalec-homebrew` is a [Dalec](https://github.com/project-dalec/dalec) extension
for Docker Buildx. List the tools you want in YAML instead of writing a
Dockerfile:

```yaml
dependencies:
  runtime:
    curl: {}
    jq: {}
```

BuildKit resolves those Homebrew Formulae and their runtime dependencies,
verifies every artifact, installs them offline, and copies an allowlisted
runtime onto a clean Ubuntu base. The final image contains neither Homebrew nor
a package manager.

- **Minimal and non-root:** runs as `linuxbrew` (`1000:1000`) with root-owned,
  non-writable runtime code.
- **Verified:** metadata, components, package digests, and archives are checked
  before installation.
- **Offline:** installation and runtime tests have no network access.
- **Auditable:** every image carries an SPDX SBOM and resolution, inventory, and
  materialization evidence.

## Get started

The [quickstart](docs/quickstart.md) turns a short spec into a GNU Hello image in
three steps: get the release inputs, write the spec, and build. It needs Docker
with Buildx, Bash, `curl`, and `jq`. For CI and production, use the
[verified release build](docs/verified-release.md), which authenticates the
release with Cosign.

> [!IMPORTANT]
> Released metadata is accepted for seven days. If no release is that fresh,
> there is temporarily no supported published build path. Wait for a new release;
> never extend the limit.

## Scope

| Supported | Not supported |
| --- | --- |
| Linux `amd64` and `arm64` | Other platforms |
| Stable Formulae from the release snapshot | Historical versions or ranges |
| `homebrew/core` and public default GitHub taps | Casks, private taps, arbitrary Git remotes |
| Bottles and policy-authorized prebuilt executables | General source builds |
| Built-in non-root runtime base | Custom runtime bases |
| Offline runtime tests | Networked tests |

## Documentation

| Guide | Contents |
| --- | --- |
| [Quickstart](docs/quickstart.md) | Build and run your first image |
| [Verified release build](docs/verified-release.md) | Cosign-authenticated inputs for CI and production |
| [Usage](docs/usage.md) | Packages, image settings, tests, evidence, troubleshooting |
| [Examples](examples/README.md) | Templates and integration fixtures |
| [Glossary](CONTEXT.md) | Dalec, Formula, bottle, and release terms |
| [Security](SECURITY.md) | Guarantees, trust boundaries, limitations |
| [Architecture](docs/architecture.md) | Build and verification flow |
| [Release and rollback](docs/release.md) | Maintainer procedures |
| [Contributing](CONTRIBUTING.md) | Development and validation |

Licensed under the [Apache License 2.0](LICENSE).
