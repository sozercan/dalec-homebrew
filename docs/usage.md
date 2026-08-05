# Usage reference

`dalec-homebrew` accepts a deliberately small Dalec contract and produces a Linux runtime image from verified Homebrew bottles or exact release-policy-authorized prebuilt executable archives.

## Requirements

- A Linux `amd64` or `arm64` target
- Docker Buildx or `buildctl` backed by BuildKit 0.31.2 or newer
- A `dalec-homebrew` frontend reference pinned by digest
- Network access from the BuildKit daemon to the frontend and its bound components, `formulae.brew.sh`, and `ghcr.io`; V2 non-core builds also require the release-bound catalog-service origin and selected public bottle or prebuilt-archive hosts

The frontend, runtime base, materializer, and—when V2 non-core support is compiled—bottle fetcher, catalog-service origin, ingestion key policy, tap policy, and executable runtime policy are treated as one release component tuple. Mutable image tags are not accepted as trusted inputs.

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

The complete example is available at [`../examples/hello.yaml`](../examples/hello.yaml). Use `linux/arm64` for an Arm target. BuildKit-normalized default-variant spellings such as `linux/amd64/v1` and `linux/arm64/v8` are equivalent; non-default variants and other operating systems or architectures are unsupported.

## Declare runtime dependencies

The map form supports the V1 per-Formula options:

```yaml
dependencies:
  runtime:
    hello: {}                 # homebrew/core/hello
    homebrew/core/jq: {}       # identical canonical core form
    acme/tools/widget: {}      # V2: github.com/acme/homebrew-tools
    ripgrep:
      arch: [amd64, arm64]
```

The list shorthand is also accepted:

```yaml
dependencies:
  runtime: [hello, jq]
```

Dependency rules:

- An omitted or empty `version` list selects the current stable Formula in the authenticated metadata. Any non-empty version constraint is rejected; historical versions and version ranges are not supported.
- Explicit canonical versioned Formula names such as `python@3.14` are supported. Version-looking requests must be exact canonical names; they do not select arbitrary historical releases.
- A bare name canonicalizes to `homebrew/core/<name>`; explicit `homebrew/core/<name>` is identical, and duplicate canonical roots such as both forms of `hello` are rejected.
- A qualified V2 name has exactly `owner/tap/formula`. URL-like input, arbitrary remotes, casks, credentials, uppercase or non-ASCII components, colons, backslashes, control characters, and malformed or overlong components are rejected before metadata access.
- Qualified roots are rejected before network access unless the frontend binary has the complete release-bound V2 capability tuple.
- Authenticated `homebrew/core` aliases, old names, and in-core migrations may resolve to the current canonical Formula. V2 additionally supports signed same-tap aliases and renames plus fully qualified cross-tap migrations; dependency lookup never searches unrelated taps.
- A Formula without a supported bottle fails unless its exact Formula ID is authorized by the embedded prebuilt-archive policy. Initial prebuilt support is root-only, invokes neither the Formula `install` method nor source-build fallback, and accepts only the policy-fixed archive inventory, executable mapping, platform, and static-binary properties.
- Formula short names must start with a lowercase letter or digit and contain only lowercase letters, digits, `+`, `_`, `.`, `@`, or `-`. Malformed `@` syntax is rejected before metadata access.
- `arch` may contain `amd64`, `arm64`, or both. Duplicate or unsupported entries are rejected. A root omitted by `arch` is not part of that platform's closure.
- A non-empty selected-target `dependencies.runtime` map replaces the global runtime map as a group; it is not merged per Formula. If the target omits runtime dependencies, the global map is inherited. Both scopes are still validated fail-closed.
- Every selected platform must have at least one applicable runtime root.
- Root declaration order is preserved for resolution records and the default generated `PATH`. Requested Formulae that expose the same executable basename fail instead of silently shadowing one another.
- A multi-platform build fails if the same canonical requested root resolves to different package versions on different platforms. Architecture-filtered roots that appear on only one platform are independent.

A target-specific declaration is selected with Buildx `--target`:

```yaml
targets:
  production:
    dependencies:
      runtime:
        hello: {}
```

```console
docker buildx build \
  --target production \
  --platform linux/amd64 \
  --file spec.yaml \
  --tag hello-runtime:production \
  --load \
  .
```

## Configure the image

The supported global and selected-target Dalec image fields are:

- `entrypoint`
- `cmd`
- `env`
- `labels`
- `volumes`
- `working_dir`
- `stop_signal`
- `user`

Selected-target image settings overlay the global image configuration: non-empty scalar fields override, `env` entries are appended and resolved by variable name, and `labels` and `volumes` are merged by key.

`entrypoint` and `cmd` are strings split with shell-style quoting into OCI argument arrays; they are not implicitly wrapped in a shell. `env` entries use `NAME=value` form. The frontend generates a deterministic Homebrew-aware `PATH`; an explicit image `PATH` overrides it and must not be empty.

The default runtime user is `linuxbrew` (`1000:1000`) and the default working directory is `/home/linuxbrew`. The accepted named identities are `linuxbrew` and `linuxbrew:linuxbrew`. The frontend rejects `root`, UID or GID zero, malformed identities, and other named users. An explicit numeric non-root identity must include both UID and GID, for example `1234:1235`.

Volume paths must be absolute and clean. A volume may not equal, contain, or be contained by any protected runtime path:

- `/home/linuxbrew/.linuxbrew`
- `/usr/share/dalec-homebrew`
- `/etc/passwd`
- `/etc/group`

Runtime code and configuration are normalized to root ownership and non-writable modes. For every Formula in the verified closure, the policy creates one Homebrew-prefix writable state subtree for the final runtime identity: `/home/linuxbrew/.linuxbrew/var/<canonical-formula>`. Broader writable Homebrew-prefix paths are not supported.

## Add runtime tests

Global tests and selected-target tests are appended and run during the image build. Each Dalec test supports:

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

Supported test fields are `name`, `dir`, `env`, `steps`, and `files`. Each step supports `command`, `env`, `stdin`, `stdout`, and `stderr`. Output and file-content checks support `equals`, `contains`, `matches`, `starts_with`, `ends_with`, and `empty`; every configured assertion is evaluated. File checks additionally support `permissions`, `is_dir`, `not_exist`, `no_follow`, and `link_target`. Use `empty: true` to assert empty output. `not_exist` cannot be combined with positive file assertions.

Tests have these execution semantics:

- Each test starts from its own isolated copy of the final pruned filesystem. Steps within one test run sequentially and share their mutations; test mutations never enter the exported image or another test.
- Commands run as `/bin/sh -c`, must exit zero, and stop that test on the first failure. File checks run after all steps.
- Commands use the final image user and image environment. Test-level environment entries override image values, and step-level entries override test values. `dir` overrides the final image working directory for commands.
- Networking is disabled. Test mounts and source fetching are rejected rather than ignored.
- Each step has a 10-minute timeout, command output is bounded to 16 MiB per stream, and file-content assertions are bounded to 16 MiB per file. The frontend requests a 2 GiB memory limit and two-CPU quota for each test execution.

See the files under [`../examples/`](../examples/) for command output, filesystem, plugin, locale, generated-data, and stateful workload checks.

## Supported Dalec contract

Package metadata fields such as `name`, `description`, `website`, `version`, `revision`, and `license` may be supplied, but they are optional and do not drive dependency resolution for this runtime-only frontend.

V1 behavior is limited to core-only global and selected-target `dependencies.runtime`, the image fields listed above, and tests without mounts. V2 retains that contract and adds public default GitHub taps addressed only as `owner/tap/formula`; runtime input still cannot provide repository URLs, keys, or endpoint configuration. Unknown non-extension Dalec fields are rejected by the strict decoder. The following known Dalec features are also rejected:

- build, recommended, test, or sysext dependencies
- extra package repositories
- sources and patches
- build steps, build environment, build mounts, caches, or build network configuration
- package artifacts or package configuration
- `provides`, `replaces`, or `conflicts`
- target frontend forwarding
- image base overrides or post-install image steps
- casks, private or authenticated taps, arbitrary Git remotes, general source builds, user-defined archive recipes, historical versions, version ranges, and bottles whose embedded Formula requires unstaged tap-local Ruby helper files
- test mounts or networked tests

Unsupported or malformed Dalec document fields are rejected before Homebrew metadata or bottle registry access.

## Runtime contents and evidence

The output contains the selected Formulae, their verified runtime closure, a conservative Ubuntu Noble runtime base, and these machine-readable evidence files under `/usr/share/dalec-homebrew`:

| File | Purpose |
| --- | --- |
| `manifest.json` | Final runtime manifest and resolution binding |
| `resolution.json` | Authenticated metadata, exact bottle or prebuilt/derived artifact identities, dependency closure, and component identities |
| `runtime-inventory.json` | Selected runtime paths, ownership, modes, digests, and package attribution |
| `prune-manifest.json` | Versioned record of the runtime pruning decision |
| `sbom.spdx.json` | SPDX 2.3 software bill of materials |
| `materialization.json` / `materialization-v2.json` | Offline installation and per-artifact V2 preparation/install evidence, including prebuilt derivation when selected |
| `runtime-base-packages.tsv` | Chisel package versions, architectures, selected regular payload bytes, and verified source `.deb` SHA-256 values |
| `runtime-base-artifacts.tsv` | Deliberate non-package files included in the runtime base |
| `runtime-base-chisel.manifest.wall` | Chisel's authoritative path and slice manifest |

These nine files are embedded in each platform image; they are not signed OCI attestations. The release workflow signs the reusable component tuple, publishes component SBOM, provenance, and vulnerability evidence, and preserves each integration image's resolution, inventory, prune, materialization, base, and embedded SBOM files in checksum-authenticated runtime-evidence archives.

The base retains common runtime facilities such as CA trust, Bash and Dash, core command-line utilities, NSS and DNS support, timezone data, glibc conversion data, and common C and C++ libraries.

The final image does **not** contain `apt`, `dpkg`, Chisel, `brew`, the Homebrew repository or download cache, installer logs, receipts, embedded Formula source, or materializer and test tooling.

For the verification and assembly flow, see [`architecture.md`](architecture.md). For release trust and digest binding, see [`release.md`](release.md) and the repository [`SECURITY.md`](../SECURITY.md).
