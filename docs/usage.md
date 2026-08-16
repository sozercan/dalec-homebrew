# Usage guide

This guide explains how to build and customize runtime images with
`dalec-homebrew`. If this is your first build, complete the
[README quickstart](../README.md#quickstart) first. Definitions for **Dalec
spec**, **frontend**, **Formula**, **bottle**, and other project terms are in the
[glossary](../CONTEXT.md).

## Requirements

- A target of `linux/amd64` or `linux/arm64`
- Docker Buildx backed by BuildKit 0.31.2 or newer, or an equivalent `buildctl`
  setup
- Bash, `curl`, `jq`, `install`, `grep`, `awk`, `tr`, `wc`, and either
  `sha256sum` or `shasum` to prepare release inputs
- [`cosign`](https://docs.sigstore.dev/cosign/system_config/installation/)
  3.1.2 or a compatible newer release to authenticate release assets
- Network access from the BuildKit daemon to the frontend and component images,
  `ghcr.io`, selected public GitHub taps, and the package hosts selected by the
  release policy

Docker Desktop includes Docker Buildx. Confirm the client and builder are
available with:

```console
set -euo pipefail
docker info >/dev/null
docker buildx version
docker buildx inspect --bootstrap
```

The frontend images run inside BuildKit. You do not install Dalec, Homebrew, or
`dalec-homebrew` as local commands.

## Understand the versions

A build mentions several different versions. They have different purposes:

| Value | Meaning |
| --- | --- |
| `DALEC_HOMEBREW_VERSION` | The published `dalec-homebrew` release whose components, policy, and Homebrew metadata are used together |
| `DALEC_FRONTEND_VERSION` | The newest compatible upstream Dalec version recorded by the selected `dalec-homebrew` release; its image is still used by exact digest |
| Top-level spec `version` | Optional descriptive Dalec metadata, recorded in input evidence; it does not set an image tag or choose a Formula version |
| A Formula name such as `python@3.14` | An exact versioned name currently published by Homebrew |

An empty dependency value such as `hello: {}` selects the stable Formula in
the release's authenticated metadata snapshot. Historical releases and version
ranges are not supported. In the examples, the output gets tag `1.0.0` only
because `--tag hello-homebrew:1.0.0` is passed explicitly.

## Prepare release inputs

Released frontends require the exact component tuple and authenticated Homebrew
metadata bundle from the same `dalec-homebrew` release. The
[quickstart download commands](../README.md#2-download-and-verify-the-current-release-inputs)
automatically discover the newest release, require fresh metadata, verify its
required files, and create this local layout:

```text
.dalec-homebrew/
├── release/
│   ├── components.json
│   ├── inputs.json
│   ├── metadata-bundle.digest
│   ├── metadata-bundle-manifest.json
│   ├── metadata-formula.jws.json
│   ├── metadata-migrations.jws.json
│   ├── SHA256SUMS
│   └── SHA256SUMS.bundle
└── metadata/
    ├── manifest.json
    ├── formula.jws.json
    └── formula_tap_migrations.jws.json
```

`components.json` supplies the exact frontend index and per-platform child
references. Do not guess these digests, mix files from different releases, or
substitute a mutable `dalec-homebrew` tag.

### Verify release assets

The README already performs the checks below. They are repeated here as a
standalone reference: authenticate the release checksum set with Cosign, verify
every downloaded input against it, and reject stale metadata before building.

Choose a version explicitly, or leave it unset to discover the newest
published release, then download the complete input set:

```console
set -euo pipefail
mkdir -p .dalec-homebrew/release .dalec-homebrew/metadata
if [ -z "${DALEC_HOMEBREW_VERSION:-}" ]; then
  DALEC_HOMEBREW_VERSION="$(
    curl -fsSL https://api.github.com/repos/sozercan/dalec-homebrew/releases/latest \
      | jq -er .tag_name
  )"
fi
DALEC_HOMEBREW_RELEASE_URL="https://github.com/sozercan/dalec-homebrew/releases/download/$DALEC_HOMEBREW_VERSION"

for file in \
  components.json \
  inputs.json \
  metadata-bundle.digest \
  metadata-bundle-manifest.json \
  metadata-formula.jws.json \
  metadata-migrations.jws.json \
  SHA256SUMS \
  SHA256SUMS.bundle
do
  curl -fL "$DALEC_HOMEBREW_RELEASE_URL/$file" \
    -o ".dalec-homebrew/release/$file"
done
```

Verify the keyless signature. The certificate identity restricts acceptance to
the release workflow on this repository's `main` branch:

```console
set -euo pipefail
cosign verify-blob \
  --bundle .dalec-homebrew/release/SHA256SUMS.bundle \
  --certificate-identity-regexp '^https://github\.com/sozercan/dalec-homebrew/\.github/workflows/release\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  .dalec-homebrew/release/SHA256SUMS
```

Then verify the inputs used by the build. The line-count check prevents a
misspelled filename from silently dropping out of the filtered checksum list:

```console
set -euo pipefail
grep -E '  \./(components\.json|inputs\.json|metadata-bundle\.digest|metadata-bundle-manifest\.json|metadata-formula\.jws\.json|metadata-migrations\.jws\.json)$' \
  .dalec-homebrew/release/SHA256SUMS \
  > .dalec-homebrew/release/REQUIRED_SHA256SUMS
test "$(wc -l < .dalec-homebrew/release/REQUIRED_SHA256SUMS | tr -d ' ')" -eq 6

if command -v sha256sum >/dev/null 2>&1; then
  (cd .dalec-homebrew/release && sha256sum --check REQUIRED_SHA256SUMS)
else
  (cd .dalec-homebrew/release && shasum -a 256 --check REQUIRED_SHA256SUMS)
fi

jq -e '
  [.formula.generated_at, .migrations.generated_at]
  | map(fromdateiso8601)
  | min >= (now - 7 * 24 * 60 * 60)
' .dalec-homebrew/release/metadata-bundle-manifest.json >/dev/null || {
  echo "The release metadata is older than seven days; use a fresh release." >&2
  exit 1
}
```

The seven-day limit is enforced again by the released frontend. If no fresh
release is available, there is temporarily no supported published build path;
do not raise or bypass the limit.

Finally, reconstruct the fixed three-file metadata context and check the
manifest digest recorded by the release:

```console
set -euo pipefail
DALEC_HOMEBREW_METADATA_BUNDLE="$PWD/.dalec-homebrew/metadata"
install -m 0444 .dalec-homebrew/release/metadata-bundle-manifest.json "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json"
install -m 0444 .dalec-homebrew/release/metadata-formula.jws.json "$DALEC_HOMEBREW_METADATA_BUNDLE/formula.jws.json"
install -m 0444 .dalec-homebrew/release/metadata-migrations.jws.json "$DALEC_HOMEBREW_METADATA_BUNDLE/formula_tap_migrations.jws.json"
DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST="$(tr -d '\r\n' < .dalec-homebrew/release/metadata-bundle.digest)"

if command -v sha256sum >/dev/null 2>&1; then
  MANIFEST_SHA256="$(sha256sum "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json" | awk '{print $1}')"
else
  MANIFEST_SHA256="$(shasum -a 256 "$DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json" | awk '{print $1}')"
fi
test "$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" = "sha256:$MANIFEST_SHA256"
```

The frontend independently verifies the Homebrew JWS documents during the
build. The checks above additionally authenticate which release files you chose
to supply.

## Build an image

Choose a platform and read its exact child reference from the verified
component manifest:

```console
set -euo pipefail
DALEC_HOMEBREW_PLATFORM=linux/amd64
DALEC_HOMEBREW_ARCH="${DALEC_HOMEBREW_PLATFORM#linux/}"
DALEC_HOMEBREW_INDEX="$(jq -er '.frontend.index' .dalec-homebrew/release/components.json)"
DALEC_HOMEBREW_CHILD="$(
  jq -er --arg arch "$DALEC_HOMEBREW_ARCH" '
    .frontend.platforms[]
    | select(.platform.os == "linux" and .platform.architecture == $arch)
    | .ref
  ' .dalec-homebrew/release/components.json
)"
```

Create `spec.yaml`. Read the release-approved upstream Dalec version and exact
image digest from the verified inputs, then use a shell heredoc to insert the
immutable references without copying them by hand:

```console
set -euo pipefail
DALEC_SYNTAX="$(jq -er '.dalec_frontend.index' .dalec-homebrew/release/inputs.json)"
DALEC_FRONTEND_VERSION="$(jq -er '.dalec_frontend.module.version' .dalec-homebrew/release/inputs.json)"
printf 'Using upstream Dalec %s\n' "$DALEC_FRONTEND_VERSION"

cat > spec.yaml <<EOF
# syntax=$DALEC_SYNTAX

name: hello-homebrew
version: 1.0.0
revision: 1
description: GNU Hello in a minimal runtime image
website: https://www.gnu.org/software/hello/
license: GPL-3.0-or-later

dependencies:
  runtime:
    hello: {}

image:
  entrypoint: /home/linuxbrew/.linuxbrew/bin/hello

targets:
  homebrew:
    frontend:
      image: $DALEC_HOMEBREW_CHILD
EOF
```

Build and load the image into Docker:

```console
set -euo pipefail
docker buildx build \
  --build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
  --build-arg "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" \
  --build-context "dalec-homebrew-metadata=$DALEC_HOMEBREW_METADATA_BUNDLE" \
  --target homebrew/image \
  --platform "$DALEC_HOMEBREW_PLATFORM" \
  --file spec.yaml \
  --tag hello-homebrew:1.0.0 \
  --load \
  .
```

Use `linux/arm64` for an Arm target. The release-reference lookup above expects
exactly `linux/amd64` or `linux/arm64`; other operating systems, architectures,
and explicit variant spellings are unsupported by this workflow.

The quickstart discovers the newest project release automatically, while the
spec uses `.dalec_frontend.index` from verified `inputs.json`. Do not substitute
the mutable upstream `:latest` tag: it may not be compatible with, or covered by
the evidence for, the selected release tuple.

### Use `buildctl` instead of Docker Buildx

If you operate BuildKit directly, use the upstream Dalec image as a gateway and
map the metadata directory as the required named context:

```console
set -euo pipefail
buildctl --addr "$BUILDKIT_HOST" build \
  --frontend gateway.v0 \
  --opt "source=$DALEC_SYNTAX" \
  --opt filename=spec.yaml \
  --opt target=homebrew/image \
  --opt "platform=$DALEC_HOMEBREW_PLATFORM" \
  --opt "build-arg:DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX" \
  --opt "build-arg:DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" \
  --opt context:dalec-homebrew-metadata=local:dalec-homebrew-metadata \
  --local context=. \
  --local dockerfile=. \
  --local "dalec-homebrew-metadata=$DALEC_HOMEBREW_METADATA_BUNDLE" \
  --output type=oci,dest=hello-homebrew.tar
```

`buildctl` has no generic equivalent of Docker's `--load`. The command above
writes an OCI archive; use an image output with `name=...` and `push=true` to
publish directly to a registry. The BuildKit daemon—not the `buildctl` client—
needs registry and package-host access.

### Why the target is `homebrew/image`

There are two BuildKit frontends in the supported route:

```text
spec.yaml
  -> upstream Dalec reads and validates the spec
  -> Dalec selects the homebrew target
  -> dalec-homebrew builds the child image route
```

The complete target is therefore `homebrew/image`. Directly using
`dalec-homebrew` as the syntax frontend, selecting bare `homebrew`, changing the
child route, or adding frontend command-line values is rejected. These checks
run before Homebrew metadata or registries are accessed.

## Declare runtime dependencies

`dependencies.runtime` is a non-empty YAML mapping:

```yaml
dependencies:
  runtime:
    hello: {}                  # homebrew/core/hello
    homebrew/core/jq: {}       # the same explicit core form
    acme/tools/widget: {}      # illustrative public-tap identity
    ripgrep:
      arch: [amd64, arm64]
```

`acme/tools/widget` is a shape example, not a promise that this Formula exists.
Use an identity published by the tap you intend to build.

Dependency rules:

- Omit a global or selected-target `dependencies` section when it is not used.
  If present, it must contain a non-empty `runtime` map. To inherit global
  dependencies in the `homebrew` target, omit the target's `dependencies`
  section entirely.
- An omitted or empty `version` list selects the stable Formula in the
  authenticated release snapshot. Any non-empty version constraint is
  rejected.
- Exact versioned Formula names published by Homebrew, such as `python@3.14`,
  are supported. They are names, not requests for arbitrary historical
  versions.
- A bare name canonicalizes to `homebrew/core/<name>`. Declaring both bare and
  explicit forms of the same Formula is a duplicate and fails.
- A public-tap name has exactly `owner/tap/formula`. It maps only to that tap's
  default public GitHub repository. URLs, credentials, arbitrary remotes,
  uppercase or non-ASCII components, casks, and malformed names are rejected
  before network access.
- Authenticated aliases, old names, and migrations may resolve to a current
  canonical Formula. Dependency lookup never searches unrelated taps.
- A Formula without a supported bottle fails unless its exact Formula ID is
  authorized by the release's prebuilt-executable policy. Users cannot add an
  archive URL or recipe through the spec.
- Formula short names start with a lowercase letter or digit and may contain
  lowercase letters, digits, `+`, `_`, `.`, `@`, or `-`.
- `arch` may contain `amd64`, `arm64`, or both. Duplicate or unsupported values
  are rejected. Every selected platform must retain at least one root.
- A non-empty selected-target runtime map replaces the global runtime map as a
  group; maps are not merged Formula by Formula.
- Declaration order has no precedence meaning. Roots are canonicalized and
  sorted for evidence and the generated `PATH`; installation uses a separate
  deterministic dependency-first order. Requested Formulae exposing the same
  command name fail instead of silently shadowing each other.
- When producing both architectures, a requested root must resolve to the same
  package version on each applicable platform.

## Build for both architectures

The spec contains one exact platform-child frontend reference, so build each
platform with its matching child reference. Push the two resulting manifests
and assemble an OCI index from those tested digests. Do not use one platform's
child reference for the other platform, and do not re-resolve packages during
index assembly.

This is an operator workflow rather than a one-command `--platform` shortcut.
The repository's release workflow follows the same pattern: per-platform build
and test first, controlled index assembly second. See
[Release requirements](release.md#release-requirements) for the complete
promotion contract.

## Automatic runtime minimization

This section describes the policy in the current source tree and the next
release built from it. The policy is immutable within each published version,
so an older release retains its own documented behavior instead of silently
adopting new rules.

The policy in the current source tree resolves, verifies, and installs the
complete runtime closure before applying conservative minimization. No spec field or
build argument disables, selects, or broadens it.

Requested Formulae are the retention boundary: their package payload is not
subject to this added minimization. Only transitive `homebrew/core` Formulae are
eligible, and only these policy-enumerated classes may be removed:

- headers under `include/`;
- manual and Info trees under `share/man/` and `share/info/`;
- exact pkg-config, CMake, and aclocal build-metadata locations;
- exact Formula- and path-specific Python standard-library test subtrees;
- shell completions in the known Bash, Fish, and Zsh locations; and
- static `.a` archives below bounded `lib/` locations, excluding protected
  runtime-data trees.

The minimizer keeps shared libraries, plugins, `libexec`, configuration,
locales, Python site-packages, `ensurepip`, `venv`, `node_modules`, Formula
`share/doc` content, legal and license text, and static archives in protected
runtime-data locations.

An exact release-policy capability may preserve compiler or MPI development
payload across its verified dependency closure. Unsigned OCI annotations cannot
activate that retention. If a path cannot be classified safely, or pruning
would break a retained link, the build fails or keeps the content rather than
guessing.

## Configure the image

A complete image configuration looks like this:

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

The supported global and selected-target image fields are:

- `entrypoint`
- `cmd`
- `env`
- `labels`
- `volumes`
- `working_dir`
- `stop_signal`
- `user`

Selected-target settings overlay global settings. Non-empty scalar fields
override; `env` entries are appended and resolved by variable name; labels and
volumes are merged by key.

`entrypoint` and `cmd` are strings split with shell-style quoting into OCI
argument arrays. They are not implicitly wrapped in a shell. Environment values
use `NAME=value` form. The frontend generates a deterministic Homebrew-aware
`PATH`; an explicit `PATH` overrides it and must not be empty.

The default runtime user is `linuxbrew` (`1000:1000`) and the default working
directory is `/home/linuxbrew`. Accepted named values are `linuxbrew` and
`linuxbrew:linuxbrew`. `root`, UID or GID zero, malformed identities, and other
named users are rejected. A numeric non-root identity must include UID and GID,
for example `1234:1235`.

Volume paths must be absolute and clean. A volume cannot overlap these protected
runtime paths:

- `/home/linuxbrew/.linuxbrew`
- `/usr/share/dalec-homebrew`
- `/etc/passwd`
- `/etc/group`

Runtime code and configuration are root-owned and non-writable. Each Formula in
the verified closure receives one writable state directory for the runtime
identity: `/home/linuxbrew/.linuxbrew/var/<canonical-formula>`.

## Add runtime tests

Tests run while the image is built. This example checks command output and the
final filesystem:

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
`ends_with`, and `empty`; all configured assertions run. File checks also
support `permissions`, `is_dir`, `not_exist`, `no_follow`, and `link_target`.
`not_exist` cannot be combined with a positive assertion.

Execution rules:

- Every test starts from an isolated copy of the final, minimized filesystem.
  Steps in one test share mutations, but those mutations never enter the
  exported image or another test.
- Commands run through `/bin/sh -c`, use the final non-root user, environment,
  and working directory, and must exit zero.
- Test-level environment values override image values; step-level values
  override test values. `dir` overrides the image working directory.
- Networking, source fetching, and test mounts are disabled.
- Each step has a 10-minute timeout. Command output and file-content assertions
  are bounded to 16 MiB per stream or file. The frontend requests a 2 GiB
  memory limit and two-CPU quota for each test execution.

See [`../examples/`](../examples/) for command-output, filesystem, plugin,
locale, generated-data, and stateful workload examples. `live-*.yaml` files are
base fixtures for `scripts/live-test.sh`; the helper injects release routing,
so those files are not standalone copy-and-paste builds. `hello.yaml` and
`forwarded-hello.yaml` are templates whose digest placeholders must be filled
from verified release data.

## Supported Dalec fields

Package metadata such as `name`, `description`, `website`, `version`,
`revision`, and `license` may be supplied. This runtime-only frontend accepts it
and commits it to the effective-input digest, but it does not project those
fields into OCI image configuration or Homebrew dependency resolution.

The supported contract is limited to runtime dependencies, the image fields
listed above, the test subset above, public default GitHub taps, and automatic
runtime minimization. Unknown non-extension fields are rejected. These known
Dalec features are also unsupported:

- build, recommended, test, or sysext dependencies;
- extra package repositories;
- sources, patches, build steps, build mounts, caches, or build-network
  configuration;
- package artifacts or package configuration;
- `provides`, `replaces`, or `conflicts`;
- frontend forwarding outside the exact `homebrew/image` route;
- custom image bases or post-install image steps;
- casks, private taps, arbitrary Git remotes, general source builds,
  user-defined archive recipes, historical versions, or version ranges; and
- test mounts or networked tests.

Malformed or unsupported input fails before Homebrew metadata or package
registry access.

## Runtime contents and evidence

The output contains the selected Formulae, their verified runtime closure, and
a conservative Ubuntu Noble runtime base. Machine-readable evidence is stored
under `/usr/share/dalec-homebrew`:

| File | Purpose |
| --- | --- |
| `manifest.json` | Final runtime manifest and resolution binding |
| `resolution.json` | Authenticated metadata, exact package identities, dependency closure, and component identities |
| `runtime-inventory.json` | Runtime paths, ownership, modes, digests, and package attribution |
| `prune-manifest.json` | The runtime minimization decision |
| `sbom.spdx.json` | SPDX 2.3 software bill of materials |
| `materialization-v2.json` | Offline installation and per-artifact verification evidence; the suffix is an internal schema generation |
| `runtime-base-packages.tsv` | Chisel package versions, architectures, selected bytes, and source package digests |
| `runtime-base-artifacts.tsv` | Deliberate non-package files in the runtime base |
| `runtime-base-chisel.manifest.wall` | Chisel's path and slice manifest |

Some evidence filenames and `schema_version` values contain numeric suffixes
for machine-format compatibility. They are not user-selectable product modes;
the current frontend writes the coherent set it expects.

These files are embedded evidence, not signed OCI attestations. The release
workflow separately publishes signed component provenance, SBOMs, vulnerability
reports, and checksum-authenticated runtime-evidence archives.

Copy evidence out without assuming the minimal image contains `jq`:

```console
set -euo pipefail
cid="$(docker create hello-homebrew:1.0.0)"
docker cp "$cid:/usr/share/dalec-homebrew/manifest.json" ./manifest.json
docker cp "$cid:/usr/share/dalec-homebrew/sbom.spdx.json" ./sbom.spdx.json
docker rm "$cid"
```

The base includes CA trust, Bash and Dash, common command-line utilities, NSS
and DNS support, timezone data, glibc conversion data, and common C/C++ runtime
libraries. It does **not** include `apt`, `dpkg`, Chisel, `brew`, Homebrew's
repository or cache, receipts, Formula source, materializer tools, or test
runners.

## Troubleshooting

### The frontend reports a metadata bundle error

Use all metadata files from the same release, preserve the exact three context
filenames, pass the recorded manifest digest, and use a release with a fresh
snapshot. If none exists, wait for a new release rather than bypassing the
limit. Release-bound frontends do not accept an omitted,
partial, substituted, or stale bundle.

### The child frontend or index does not match

Read both references from one verified `components.json`. Select the child for
the requested architecture and pass the manifest's frontend index through
`DALEC_HOMEBREW_FRONTEND_INDEX_REF`. A tag, the index used as a child, a child
from another platform, or a mixed release tuple fails closed.

### The build says the target or source is invalid

Use upstream Dalec as the syntax frontend, keep the exact
`targets.homebrew.frontend.image` mapping, and build `--target homebrew/image`.
Do not invoke the child directly or add frontend `cmdline` values.

### A package or version is unavailable

Check the current Formula name on
[formulae.brew.sh](https://formulae.brew.sh/). Use `owner/tap/formula` for a
public GitHub tap. Only stable Formulae in the authenticated snapshot, exact versioned Formula
names present in that snapshot, supported bottles, and policy-authorized prebuilt executables work.
There is no source-build or historical-version fallback.

### A runtime test fails on networking

This is expected if the test contacts an external service. Tests run on the
final filesystem with networking disabled. Replace the check with an offline
command or filesystem assertion.

### A command is missing from `PATH`

Confirm the requested Formula actually publishes that executable. If multiple
requested Formulae expose the same basename, the build fails rather than
choosing precedence. An explicit image `PATH` replaces the generated one, so it
must include every directory you need.

For the trust boundaries behind these errors, read the
[security model](../SECURITY.md). For the implementation flow, read the
[architecture guide](architecture.md).
