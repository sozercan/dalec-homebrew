# Usage reference

This page documents everything `dalec-homebrew` accepts and produces. If you
have not built an image yet, start with the
[quickstart](../README.md#quickstart) and come back here for the details.

`dalec-homebrew` takes a small [Dalec](https://github.com/project-dalec/dalec)
spec that lists Homebrew packages and produces a Linux runtime image built from
verified Homebrew bottles, plus a few release-approved prebuilt executable
archives.

## Requirements

- A build target of `linux/amd64` or `linux/arm64`.
- Docker Buildx or `buildctl` backed by BuildKit `0.31.2` or newer.
- Network access from the BuildKit daemon to `ghcr.io`, GitHub, and the bottle
  hosts used by the packages you request.
- `curl`, `jq`, `cosign`, and `sha256sum` (macOS: `shasum -a 256`) to prepare
  the release inputs.

## Set up the release inputs

Every build needs four things, and all of them come from one `dalec-homebrew`
release:

| Input | Where it goes |
| --- | --- |
| Upstream Dalec frontend | the `# syntax=` line of your spec |
| `dalec-homebrew` platform child | `targets.homebrew.frontend.image` |
| `dalec-homebrew` release index | `--build-arg DALEC_HOMEBREW_FRONTEND_INDEX_REF` |
| Homebrew package index | `--build-context dalec-homebrew-metadata` and `--build-arg DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST` |

The frontend child and index must be digest-pinned; a tag is rejected before
anything is downloaded. Take both from the release's signed `components.json`
rather than resolving a tag yourself:

```console
set -euo pipefail

DALEC_HOMEBREW_VERSION=$(curl -fsSL https://api.github.com/repos/sozercan/dalec-homebrew/releases |
  jq -er 'map(select(.draft == false and .prerelease == false)) | max_by(.published_at).tag_name')
RELEASE=https://github.com/sozercan/dalec-homebrew/releases/download/$DALEC_HOMEBREW_VERSION

mkdir -p release homebrew-metadata
for asset in components.json SHA256SUMS SHA256SUMS.bundle \
  metadata-bundle-manifest.json metadata-formula.jws.json metadata-migrations.jws.json; do
  curl -fsSL -o "release/$asset" "$RELEASE/$asset"
done

cosign verify-blob \
  --bundle release/SHA256SUMS.bundle \
  --certificate-identity-regexp '^https://github\.com/sozercan/dalec-homebrew/\.github/workflows/release\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  release/SHA256SUMS
(cd release && sha256sum -c --ignore-missing SHA256SUMS)

cp release/metadata-bundle-manifest.json homebrew-metadata/manifest.json
cp release/metadata-formula.jws.json homebrew-metadata/formula.jws.json
cp release/metadata-migrations.jws.json homebrew-metadata/formula_tap_migrations.jws.json

ARCH=$(docker info --format '{{.Architecture}}' | sed 's/x86_64/amd64/; s/aarch64/arm64/')
DALEC_HOMEBREW_INDEX=$(jq -er .frontend.index release/components.json)
DALEC_HOMEBREW_CHILD=$(jq -er --arg arch "$ARCH" \
  '.frontend.platforms[] | select(.platform.architecture == $arch).ref' release/components.json)
DALEC_HOMEBREW_METADATA_BUNDLE=$PWD/homebrew-metadata
DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$(jq -er .metadata_bundle_digest release/components.json)
```

Notes on the pieces:

- The metadata folder must contain exactly `manifest.json`, `formula.jws.json`,
  and `formula_tap_migrations.jws.json`. Any other layout is rejected.
- `ARCH` is the architecture of the machine running BuildKit, which is what
  executes the frontend. Building for the other architecture works if that
  builder has emulation configured.
- Use `ghcr.io/project-dalec/dalec/frontend:latest` on the `# syntax=` line
  while getting started. For a build you want to reproduce or verify later, use
  the digest your release was tested against: `jq -er .dalec_frontend.index`
  from the release's `inputs.json`, which matches
  [`../release/dalec-frontend.json`](../release/dalec-frontend.json).

> **Releases expire.** Homebrew metadata older than 168 hours (7 days) is
> rejected and the limit cannot be raised, so a published release only builds
> for a week after its snapshot was captured. Check before building:
>
> ```console
> jq -e '[.formula, .migrations] | map(.generated_at | fromdateiso8601) | min > now - 604800' \
>   release/metadata-bundle-manifest.json
> ```

## Build an image

A complete spec selects `dalec-homebrew` through the `homebrew` target:

```yaml
# syntax=ghcr.io/project-dalec/dalec/frontend:latest

dependencies:
  runtime:
    hello: {}

image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/hello

targets:
  homebrew:
    frontend:
      image: ghcr.io/sozercan/dalec-homebrew@sha256:<child-digest>   # $DALEC_HOMEBREW_CHILD
```

Build it with the index and metadata inputs attached:

```console
docker buildx build \
  --build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
  --build-arg "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" \
  --build-context "dalec-homebrew-metadata=$DALEC_HOMEBREW_METADATA_BUNDLE" \
  --target homebrew/image \
  --platform "linux/$ARCH" \
  --file spec.yaml \
  --tag hello-runtime:local \
  --load \
  .
```

Ready-to-copy specs are in [`../examples/`](../examples/).

### Platforms

Use `linux/amd64` or `linux/arm64`. BuildKit's normalized default-variant
spellings (`linux/amd64/v1`, `linux/arm64/v8`) mean the same thing. Non-default
variants, other architectures, and other operating systems are unsupported.

### Targets and routing

`homebrew` is the Dalec target you select, and `image` is the only route
`dalec-homebrew` serves, so the full target is `homebrew/image`. Upstream Dalec
forwards your spec to the exact image named in
`targets.homebrew.frontend.image`.

Before any Homebrew metadata or registry access, the build requires that:

- the selected Dalec target is exactly `homebrew`;
- the child route is exactly `image`;
- `targets.homebrew.frontend.image` exactly equals the digest-pinned gateway
  image BuildKit loaded; and
- both the target and invocation `cmdline` values are omitted or empty.

Bare `--target homebrew`, unknown or nested routes, and mutable or mismatched
child references fail closed. `dalec-homebrew` advertises the `image` route only
so upstream Dalec can discover and forward to it; using `dalec-homebrew`
directly as the `# syntax=` frontend is not supported and is rejected.

## Choose your packages

`dependencies.runtime` is a map of Formula names to per-package options:

```yaml
dependencies:
  runtime:
    hello: {}                 # homebrew/core/hello
    homebrew/core/jq: {}      # the same identity, written out in full
    python@3.14: {}           # a versioned Formula name
    acme/tools/widget: {}     # github.com/acme/homebrew-tools
    ripgrep:
      arch: [amd64, arm64]
```

### Naming rules

- A bare name means `homebrew/core/<name>`. Writing `homebrew/core/<name>`
  explicitly is identical, and requesting both forms of the same package is
  rejected as a duplicate.
- A tap-qualified name has exactly three parts: `owner/tap/formula`. It resolves
  to the public GitHub repository `https://github.com/<owner>/homebrew-<tap>`
  and nothing else. URL-like input, arbitrary remotes, casks, credentials,
  uppercase or non-ASCII components, colons, backslashes, control characters,
  and malformed or overlong components are rejected before any metadata access.
- Formula short names must start with a lowercase letter or digit and may
  contain only lowercase letters, digits, `+`, `_`, `.`, `@`, or `-`. Malformed
  `@` syntax is rejected.
- Authenticated `homebrew/core` aliases, old names, and in-core migrations
  resolve to the current canonical Formula. Signed same-tap aliases and renames
  plus fully qualified cross-tap migrations are also honored; dependency lookup
  never searches unrelated taps.

### Version rules

- Omit `version` (or leave it empty) to get the current stable Formula in the
  authenticated metadata. Any non-empty version constraint is rejected:
  historical versions and version ranges are not supported. To fix your package
  versions, pin a `dalec-homebrew` release.
- Explicit canonical versioned Formula names such as `python@3.14` are
  supported. They are exact names, not version selectors.

### Architecture rules

- `arch` may contain `amd64`, `arm64`, or both. Duplicate or unsupported
  entries are rejected.
- A package excluded by `arch` is simply not part of that platform's closure.
- Every platform you build must end up with at least one package.
- A multi-platform build fails if the same requested package resolves to
  different versions on different platforms. Packages that appear on only one
  platform are independent.

### Scope and ordering rules

- Each `dependencies` scope — the global one and the one under
  `targets.homebrew` — must either be omitted entirely or contain a non-empty
  `runtime` map. Explicit empty scopes are invalid.
- A non-empty `targets.homebrew.dependencies.runtime` map replaces the global
  map as a group; the two are not merged package by package. Omit the target's
  whole `dependencies` scope to inherit the global map.
- Declaration order carries no meaning. For each platform, packages are sorted
  lexicographically by canonical Formula name; that order is recorded in the
  build evidence and drives the generated `PATH`. Installation uses a separate
  deterministic order so dependencies are installed before dependents.
- Two requested packages that provide the same command name fail the build
  instead of silently shadowing each other.

### Packages without a bottle

A Formula with no supported bottle fails, unless its exact identity is listed in
the release's prebuilt-archive policy. Those entries are root-only, never run a
Formula's `install` method or a source-build fallback, and accept only the
policy-fixed archive inventory, executable mapping, platform, and static-binary
properties. Your spec cannot add archive recipes or enable another Formula.

## Configure the image

These Dalec image fields are supported, globally and under `targets.homebrew`:

- `entrypoint`
- `cmd`
- `env`
- `labels`
- `volumes`
- `working_dir`
- `stop_signal`
- `user`

Target settings overlay the global ones: non-empty scalar fields override,
`env` entries are appended and resolved by variable name, and `labels` and
`volumes` are merged by key.

```yaml
targets:
  homebrew:
    frontend:
      image: ghcr.io/sozercan/dalec-homebrew@sha256:<child-digest>
    dependencies:
      runtime:
        redis: {}
    image:
      entrypoint: /home/linuxbrew/.linuxbrew/bin/redis-server
```

Details worth knowing:

- `entrypoint` and `cmd` are strings split with shell-style quoting into OCI
  argument arrays. They are not wrapped in a shell.
- `env` entries use `NAME=value` form. The build generates a deterministic
  Homebrew-aware `PATH`; setting `PATH` yourself overrides it and must not be
  empty.
- The default user is `linuxbrew` (`1000:1000`) and the default working
  directory is `/home/linuxbrew`. Accepted named identities are `linuxbrew` and
  `linuxbrew:linuxbrew`. `root`, UID or GID zero, other named users, and
  malformed identities are rejected. An explicit numeric identity must include
  both UID and GID, for example `1234:1235`.
- Volume paths must be absolute and clean, and may not equal, contain, or be
  contained by `/home/linuxbrew/.linuxbrew`, `/usr/share/dalec-homebrew`,
  `/etc/passwd`, or `/etc/group`.
- Program code and configuration are root-owned and non-writable. For every
  package in the closure, one writable state directory is created for the
  runtime user: `/home/linuxbrew/.linuxbrew/var/<formula>`. Broader writable
  paths inside the Homebrew prefix are not supported.

## Add runtime tests

Global tests and `targets.homebrew` tests are appended and run during the build.
A failing test fails the build.

```yaml
tests:
  - name: example
    dir: /home/linuxbrew
    env:
      TEST_SCOPE: test
    steps:
      - command: printf '%s' "$TEST_SCOPE"
        env:
          TEST_SCOPE: step
        stdin: ""
        stdout:
          equals: step
          contains: [te]
          matches: ['^step$']
          starts_with: st
          ends_with: ep
        stderr:
          empty: true
    files:
      /home/linuxbrew/.linuxbrew/bin/hello:
        permissions: 0o555
      /home/linuxbrew:
        is_dir: true
      /path/that/must/not/exist:
        not_exist: true
```

Supported test fields are `name`, `dir`, `env`, `steps`, and `files`. Each step
supports `command`, `env`, `stdin`, `stdout`, and `stderr`. Output and file
content checks support `equals`, `contains`, `matches`, `starts_with`,
`ends_with`, and `empty`, and every configured assertion is evaluated. File
checks additionally support `permissions`, `is_dir`, `not_exist`, `no_follow`,
and `link_target`. Use `empty: true` to assert empty output. `not_exist` cannot
be combined with positive file assertions.

How tests run:

- Each test starts from its own copy of the final image filesystem. Steps in one
  test run in order and share their changes; those changes never reach the
  exported image or another test.
- Commands run through `/bin/sh -c`, must exit zero, and stop that test at the
  first failure. File checks run after all steps.
- Commands use the final image user and environment. Test-level `env` overrides
  image values, step-level `env` overrides test values, and `dir` overrides the
  image working directory.
- Networking is disabled. Test mounts and source fetching are rejected rather
  than ignored.
- Each step has a 10-minute timeout, output is bounded to 16 MiB per stream, and
  file-content assertions are bounded to 16 MiB per file. Each test execution
  requests a 2 GiB memory limit and a two-CPU quota.

The files under [`../examples/`](../examples/) show command output, filesystem,
plugin, locale, generated-data, and stateful workload checks.

## What the build removes automatically

After the dependency closure is resolved, verified, and installed offline, the
build removes development-only files from *transitive* `homebrew/core` packages.
Packages you requested are never touched by this step, and no spec field or
build argument can disable, select, or widen it.

Exactly six classes of paths may be removed:

- headers under `include/`;
- manual and Info trees under `share/man/` and `share/info/`;
- build metadata at exact pkg-config, CMake, and aclocal locations;
- an exact, policy-authorized Python standard-library test subtree — a directory
  merely named `test` elsewhere does not qualify;
- shell completions under `share/bash-completion/completions/`,
  `share/fish/vendor_completions.d/`, and `share/zsh/site-functions/`; and
- static `.a` archives below `lib/`, except in protected runtime-data locations
  such as site-packages, `ensurepip`, `venv`, plugins, and `node_modules`.

Everything else stays, including shared libraries, plugins, `libexec`,
configuration, locales, Python site-packages, `ensurepip`, `venv`,
`node_modules`, `share/doc/` content, legal and license text, and static
archives in the protected locations above.

Compiler and MPI packages are a special case: when the release policy authorizes
one, headers, build metadata, and static archives are kept across its verified
dependency closure so it stays usable, while unrelated packages are still pruned
normally. Unsigned OCI executable-path annotations cannot activate that
retention.

Classification is fail-closed: if a path cannot be classified exactly, or if
removing it would break a retained path or link, the build keeps the content or
fails. The policy identity and every decision are recorded in the resolution,
inventory, prune, and runtime-manifest evidence.

## Runtime contents and evidence

The image contains the packages you selected, their verified runtime closure, a
conservative Ubuntu Noble base, and these machine-readable files under
`/usr/share/dalec-homebrew`:

| File | Purpose |
| --- | --- |
| `manifest.json` | Final runtime manifest and resolution binding |
| `resolution.json` | Authenticated metadata, exact bottle or derived-artifact identities, dependency closure, and component identities |
| `runtime-inventory.json` | Selected runtime paths, ownership, modes, digests, and package attribution |
| `prune-manifest.json` | Versioned record of what was removed and why |
| `sbom.spdx.json` | SPDX 2.3 software bill of materials |
| `materialization.json` / `materialization-v2.json` | Offline installation evidence, including per-artifact preparation and any prebuilt derivation |
| `runtime-base-packages.tsv` | Base package versions, architectures, selected payload bytes, and verified source `.deb` SHA-256 values |
| `runtime-base-artifacts.tsv` | Deliberate non-package files included in the runtime base |
| `runtime-base-chisel.manifest.wall` | The base's authoritative path and slice manifest |

These files are embedded in each platform image; they are not signed OCI
attestations. The release pipeline signs the component tuple, publishes
component SBOM, provenance, and vulnerability evidence, and preserves each
integration image's evidence in checksum-authenticated archives.

The base keeps common runtime facilities: CA trust, Bash and Dash, core
command-line utilities, NSS and DNS support, time-zone data, glibc conversion
data, and common C and C++ libraries.

The final image does **not** contain `apt`, `dpkg`, Chisel, `brew`, the Homebrew
repository or download cache, installer logs, receipts, embedded Formula source,
or materializer and test tooling.

## Supported Dalec contract

Package metadata fields such as `name`, `description`, `website`, `version`,
`revision`, and `license` may be supplied. They are optional and do not affect
dependency resolution in this runtime-only frontend.

Unknown fields are rejected by a strict decoder. These Dalec features are
rejected as well:

- build, recommended, test, or sysext dependencies;
- extra package repositories;
- sources and patches;
- build steps, build environment, build mounts, caches, or build network
  configuration;
- package artifacts or package configuration;
- `provides`, `replaces`, or `conflicts`;
- frontend forwarding outside the exact digest-pinned `homebrew/image` route,
  including non-empty `cmdline`, nested forwarding, and unknown child routes;
- image base overrides or post-install image steps;
- casks, private or authenticated taps, arbitrary Git remotes, general source
  builds, user-defined archive recipes, historical versions, version ranges, and
  bottles whose embedded Formula requires unstaged tap-local Ruby helper files;
  and
- test mounts or networked tests.

Unsupported or malformed spec fields and invalid routing metadata are rejected
before any Homebrew metadata or bottle registry access. `dalec-homebrew`
authenticates its own gateway image, not the identity of the upstream Dalec
frontend that dispatched to it; releases bind that parent externally through the
checked-in pin and signed provenance.

## Troubleshooting

| Message | Cause and fix |
| --- | --- |
| `authenticate Homebrew metadata bundle: ... metadata is stale: generated ..., maximum age 168h0m0s` | The release's Homebrew snapshot is older than 7 days. Use the newest release. |
| `release-bound metadata max age 336h0m0s exceeds 168h` | `DALEC_HOMEBREW_METADATA_MAX_AGE` cannot raise the 7-day limit for a released frontend. |
| `frontend: reference "..." is not digest-pinned` | A tag was used for `targets.homebrew.frontend.image` or `DALEC_HOMEBREW_FRONTEND_INDEX_REF`. Resolve the tag to a digest. |
| `frontend platform child and index must use the same repository` | `DALEC_HOMEBREW_FRONTEND_INDEX_REF` is missing, or the index and child come from different repositories. |
| `load Homebrew metadata bundle input: required named context "dalec-homebrew-metadata" is missing` | Add `--build-context dalec-homebrew-metadata=<folder>`. |
| `metadata bundle manifest digest ... does not match release-bound digest ...` | The metadata folder and `DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST` come from different releases. |
| `forwarded target "homebrew" frontend image "..." does not match invoking gateway source "..."` | The digest in the spec differs from the image BuildKit loaded. Re-resolve it. |
| `no such handler for target "": available targets: image` | Build `--target homebrew/image`, not `--target homebrew`. |
| `global runtime dependency "..." has version constraints; historical versions and version ranges are not supported` | Remove `version:`; only the current stable release is available. |
| `... runtime dependency "..." requires release-bound non-core capability bindings` | The frontend you are using was built without public-tap support. Use a published release. |
| `global dependencies.runtime must use map form and contain at least one entry` | A `dependencies` scope is present but empty, or uses list form. Remove the scope entirely to inherit, or list at least one package as a map. |

## Learn more

- [Architecture](architecture.md) — the verification and assembly flow.
- [Security](../SECURITY.md) — the trust boundaries and their limits.
- [Releases](release.md) — release contents, verification, and rollback.
