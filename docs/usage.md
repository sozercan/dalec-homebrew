# Usage guide

Start with the [README quickstart](../README.md#quickstart). This page explains
how to change the package list, image configuration, and tests after the release
inputs have been prepared. Unfamiliar terms are defined in the
[glossary](../CONTEXT.md).

## Build inputs

Every released build uses four inputs from one authenticated release:

| Input | Used as |
| --- | --- |
| Release-approved upstream Dalec digest | The spec's `# syntax=` frontend |
| Exact `dalec-homebrew` platform child | `targets.homebrew.frontend.image` |
| Matching `dalec-homebrew` parent index | `DALEC_HOMEBREW_FRONTEND_INDEX_REF` |
| Three-file Homebrew metadata bundle and its manifest digest | Named context `dalec-homebrew-metadata` and `DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST` |

The [release-preparation step](../README.md#2-prepare-a-verified-release)
authenticates the signed checksum set, verifies all consumed assets, enforces
the seven-day metadata limit, and exports the shell variables used below. Do
not mix releases, derive trusted identities from mutable tags, or skip that
step.

The selected BuildKit daemon—not only the local terminal—needs access to the
frontend and component registries, selected public GitHub taps, and package
hosts.

## Build an image

With `hello.yaml` and the variables from the quickstart:

```console
set -euo pipefail
docker buildx build \
  --build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
  --build-arg "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" \
  --build-context "dalec-homebrew-metadata=$DALEC_HOMEBREW_METADATA_BUNDLE" \
  --target homebrew/image \
  --platform "$DALEC_HOMEBREW_PLATFORM" \
  --file hello.yaml \
  --tag hello-homebrew:1.0.0 \
  --load \
  .
```

Use exactly `homebrew/image`: upstream Dalec selects the `homebrew` spec target
and forwards the child route `image`. Direct child invocation, bare `homebrew`,
other routes, nested forwarding, and non-empty frontend `cmdline` values are
rejected before package access.

`--load` exports one platform to the local Docker daemon. For registry output,
replace it with `--push` and use a registry tag.

## Understand the versions

| Value | Meaning |
| --- | --- |
| `DALEC_HOMEBREW_VERSION` | One immutable release tuple selected from GitHub Releases |
| `DALEC_FRONTEND_VERSION` | The compatible upstream Dalec version recorded by that tuple; execution remains digest-pinned |
| Top-level spec `version` | Optional descriptive input recorded in evidence; it does not tag the image or select a Formula version |
| `python@3.14` | An exact Formula name present in the release snapshot |

An omitted dependency version selects the stable Formula in the authenticated
release snapshot. Non-empty version constraints, version ranges, and arbitrary
historical releases are rejected.

## Choose packages

`dependencies.runtime` is a non-empty mapping:

```yaml
dependencies:
  runtime:
    hello: {}                  # homebrew/core/hello
    homebrew/core/jq: {}       # the same explicit core form
    python@3.14: {}            # an exact Formula name
    owner/tap/formula: {}      # a public default GitHub tap
    ripgrep:
      arch: [amd64, arm64]
```

### Names and sources

- A bare name means `homebrew/core/<name>`. Declaring both forms is a duplicate.
- `owner/tap/formula` maps only to
  `https://github.com/<owner>/homebrew-<tap>`. It is a shape example; use a
  Formula that actually exists in your selected tap.
- Casks, credentials, private taps, arbitrary URLs or remotes, uppercase or
  non-ASCII names, malformed `@` syntax, and unsupported characters are
  rejected before package access.
- Authenticated aliases, renames, and migrations may resolve to the current
  canonical Formula; lookup never searches unrelated taps.

### Platforms and ordering

- `arch` accepts `amd64`, `arm64`, or both. Every selected platform must retain
  at least one requested root.
- A target-specific runtime map replaces the global map as a group. Omit the
  target's entire `dependencies` section to inherit the global map.
- Each present dependency scope must contain a non-empty runtime map.
- Declaration order has no precedence. Roots are canonicalized and sorted for
  evidence and the generated `PATH`; installation uses a separate
  dependency-first order.
- Requested Formulae exposing the same command name fail rather than silently
  shadowing one another.

A Formula without a supported bottle fails unless its exact identity is in the
release's prebuilt-executable policy. Users cannot add archive URLs or recipes,
and there is no source-build fallback.

## Configure the image

```yaml
image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/curl
  cmd: --version
  env:
    - NO_COLOR=1
  labels:
    org.opencontainers.image.title: curl runtime
  volumes:
    /data: {}
  working_dir: /home/linuxbrew
  stop_signal: SIGTERM
  user: linuxbrew
```

Supported fields are `entrypoint`, `cmd`, `env`, `labels`, `volumes`,
`working_dir`, `stop_signal`, and `user`.

- `entrypoint` and `cmd` are shell-style-split strings, not shell-wrapped
  commands.
- Environment entries use `NAME=value`. An explicit `PATH` replaces the
  generated Homebrew-aware value and cannot be empty.
- The default user is `linuxbrew` (`1000:1000`) and the default working
  directory is `/home/linuxbrew`. `root`, UID or GID zero, malformed identities,
  and other named users are rejected. Numeric identities use `UID:GID`.
- Volumes cannot overlap `/home/linuxbrew/.linuxbrew`,
  `/usr/share/dalec-homebrew`, `/etc/passwd`, or `/etc/group`.
- Runtime code is root-owned and non-writable. Each Formula receives one
  writable state directory at
  `/home/linuxbrew/.linuxbrew/var/<canonical-formula>`.

Target image settings overlay global settings: scalars override, environment
entries are resolved by name, and labels and volumes merge by key.

## Add runtime tests

Tests run on isolated copies of the final filesystem, as the final non-root
user, with networking disabled:

```yaml
tests:
  - name: hello-runs
    steps:
      - command: hello
        stdout:
          contains: ["Hello, world!"]
    files:
      /home/linuxbrew/.linuxbrew/bin/hello:
        permissions: 0o555
      /home/linuxbrew/.linuxbrew/bin/brew:
        not_exist: true
```

Supported test fields are `name`, `dir`, `env`, `steps`, and `files`. A step
supports `command`, `env`, `stdin`, `stdout`, and `stderr`. Output and content
checks support `equals`, `contains`, `matches`, `starts_with`, `ends_with`, and
`empty`. File checks also support `permissions`, `is_dir`, `not_exist`,
`no_follow`, and `link_target`.

Steps within one test share mutations; separate tests do not, and no mutation
enters the exported image. Commands run through `/bin/sh -c` and must exit zero.
Each step has a 10-minute timeout; output and file-content checks are bounded to
16 MiB. Test mounts, source fetching, and network access are rejected.

## Build both architectures

A published child is platform-specific. Build `linux/amd64` and `linux/arm64`
separately with each platform's child, push the tested manifests, and assemble
an OCI index from those digests. Do not reuse one platform's child for the
other, and do not re-resolve packages while assembling the index. See the
[release requirements](release.md#release-requirements) for the operator flow.

## Runtime minimization

The policy in the current source tree resolves, verifies, and installs the full
closure before removing narrowly classified development files from eligible
transitive `homebrew/core` Formulae. Requested Formulae are the retention
boundary. Each published release keeps its own immutable policy.

The six eligible classes are headers, manual and Info trees, exact build
metadata locations, policy-authorized Python standard-library tests, shell
completions, and bounded static archives. Shared libraries, plugins, `libexec`,
configuration, locales, site-packages, `ensurepip`, `venv`, `node_modules`,
Formula `share/doc` content, legal text, and protected runtime-data archives
remain.

An exact release-policy capability may retain compiler or MPI development
payload across its verified dependency closure. Caller input and unsigned OCI
annotations cannot disable, widen, or activate policy behavior.

## Supported Dalec contract

Optional top-level metadata—`name`, `description`, `website`, `version`,
`revision`, and `license`—is committed to the effective-input digest but does
not alter OCI configuration or dependency resolution.

The supported behavior is limited to runtime dependencies, the image fields and
test subset above, public default GitHub taps, and release-bound runtime policy.
The following are rejected:

- build, recommended, test, or sysext dependencies;
- extra repositories, sources, patches, build steps, build mounts, caches, or
  build-network configuration;
- package artifacts or package configuration;
- `provides`, `replaces`, or `conflicts`;
- custom image bases or post-install image steps;
- casks, private taps, arbitrary Git remotes, general source builds,
  user-defined archive recipes, historical versions, or version ranges; and
- test mounts or networked tests.

Unknown non-extension fields and malformed input fail before package metadata or
registry access.

## Runtime contents and evidence

The final image contains the selected Formulae, their verified runtime closure,
and a conservative Ubuntu Noble base. It does not contain `brew`, `apt`, `dpkg`,
Chisel, package repositories or caches, receipts, Formula source, or build/test
tooling.

Evidence lives under `/usr/share/dalec-homebrew`:

| File | Purpose |
| --- | --- |
| `manifest.json` | Final runtime manifest and resolution binding |
| `resolution.json` | Package closure, authenticated metadata, artifact, and component identities |
| `runtime-inventory.json` | Paths, ownership, modes, digests, and package attribution |
| `prune-manifest.json` | Runtime minimization decisions |
| `sbom.spdx.json` | SPDX 2.3 SBOM |
| `materialization-v2.json` | Offline installation and per-artifact verification evidence |
| `runtime-base-packages.tsv` | Base package identities and verified source package digests |
| `runtime-base-artifacts.tsv` | Deliberate non-package base files |
| `runtime-base-chisel.manifest.wall` | Chisel path and slice manifest |

Numeric suffixes in evidence filenames and `schema_version` values identify
machine-format generations, not user-selectable product modes. Embedded evidence
is not itself a signed OCI attestation; release attestations are published
separately.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| Metadata is stale or missing | Use all three files from one fresh release; never extend the seven-day limit |
| Child/index mismatch | Read both exact references from the same verified `components.json` |
| Target or source rejected | Use upstream Dalec and exactly `--target homebrew/image` |
| Package or version unavailable | Use a stable bottled Formula or exact versioned name present in the snapshot |
| Test cannot reach the network | Replace it with an offline command or file assertion |
| Command missing from `PATH` | Confirm the Formula publishes it and that an explicit `PATH` did not replace the generated value |

For guarantees and limitations, read the [security model](../SECURITY.md). For
pipeline details, read the [architecture guide](architecture.md).
