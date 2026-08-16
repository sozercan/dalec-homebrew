# Architecture

This page explains how a build works internally. It is written for
contributors, reviewers, and anyone auditing an image. To *build* images, read
the [usage reference](usage.md) instead.

`dalec-homebrew` separates networked resolution from offline execution. A build
first turns a Dalec dependency declaration into a canonical, digest-bound
resolution record, then materializes that record without network access and
copies an allowlisted runtime into an independent Ubuntu Chisel base.

In order, a build:

1. validates routing metadata and the Dalec spec before touching the network;
2. authenticates Homebrew metadata and resolves the complete dependency
   closure into an immutable record of exact digests;
3. verifies every bottle as a hostile archive;
4. installs the closure offline, with no network, secrets, or shared caches;
5. copies an allowlisted runtime onto a clean, snapshot-pinned Ubuntu base and
   writes the evidence files; and
6. runs your declared tests on the finished filesystem with networking
   disabled.

## Frontend routing

The only supported production invocation uses upstream Dalec with an
out-of-tree child frontend:

```text
BuildKit
  -> digest-pinned upstream Dalec frontend
  -> targets.homebrew.frontend.image
  -> digest-pinned dalec-homebrew child frontend, child route image
  -> resolution and materialization pipeline
```

The child still advertises `image` for upstream Dalec route discovery. That is
not direct-invocation compatibility: the router requires the selected Dalec
target to be `homebrew` and the child target to be `image`, and rejects an
unforwarded `image` solve.

The upstream frontend selects the `homebrew` spec target, discovers the child
frontend's advertised routes, and forwards `homebrew/image` as child target
`image`. It replaces the child solve's gateway `source` with the exact
`targets.homebrew.frontend.image` and forwards the effective typed Dalec spec.
Before any metadata or registry access, `dalec-homebrew` requires the selected
spec target to be exactly `homebrew`, the child route to be exactly `image`, the
target's frontend image to equal its gateway `source`, and both target and
invocation `cmdline` values to be empty. These routing checks complete before
metadata or registry access. Runtime dependency maps carry no declaration-order
semantics. Applicable roots for each platform are sorted lexicographically by
canonical requested Formula ID for resolution evidence and the default
generated `PATH`; installation uses a separate
deterministic topological order. A dependency scope is either omitted or
contains a non-empty runtime map. Explicit empty scopes fail preflight, which
also catches the markers emitted when the exact pinned upstream Dalec v0.21.5
dispatcher drops legacy list syntax during typed-spec forwarding.

The child can authenticate only its own gateway source. BuildKit does not give
the child an authenticated identity for the parent that initiated the nested
solve, and caller-provided parent options would be forgeable. Therefore
[`../release/dalec-frontend.json`](../release/dalec-frontend.json) binds the
upstream index, exact Linux children, module identity, and route externally;
top-level signed release provenance authenticates that parent binding. The
resolution record's frontend fields continue to identify `dalec-homebrew`, not
the upstream dispatcher.

## Build pipeline

```text
upstream Dalec target selection and forwarding
  -> dalec-homebrew route and source validation
  -> raw Dalec preflight
  -> separately verified Formula and migration metadata
  -> per-platform OCI descriptor resolution
  -> canonical resolution.json and independent structural verification
  -> exact bottle layers through llb.ImageBlob
  -> static archive verification
  -> offline Homebrew install in deterministic topological order
  -> per-install containment and complete-keg verification
  -> verified reconciliation of approved generated runtime data
  -> allowlist overlay onto a snapshot-pinned Chisel runtime base
  -> static ELF, shebang, and link verification
  -> declared Dalec tests on final-user runtime branches
  -> OCI image configuration with resolution source_date_epoch
```

The network boundary sits between resolution and materialization:

- Resolution authenticates Homebrew metadata, discovers registry objects, and records exact descriptor identities and selected annotations.
- Materialization receives the internally generated record and exact bottle blobs through read-only mounts, with a private scratch output.
- The materialization and runtime-test LLB execs use `NetModeNone` and declare no secret, SSH, shared-cache, socket, or device mounts.
- The full materializer filesystem is never exported into the final image.

See [`../SECURITY.md`](../SECURITY.md) for the properties enforced at each boundary.

## Package layout

- `internal/spec`: runtime-dependency shape and name preflight plus typed
  Dalec validation.
- `internal/config`: gateway build-option parsing, immutable component bindings,
  and release-bound metadata policy.
- `internal/homebrew/metadata`: bounded HTTP fetch, RFC 7797/PS512 JWS verification, freshness and rollback policy, and canonical alias, rename, and migration lookup.
- `internal/homebrew/oci`: Distribution authentication and exact descriptor-chain validation.
- `internal/homebrew/version`: Homebrew version, revision, and minimum-version
  comparison.
- `internal/resolver`: fixed-point current-Formula closure resolution and deterministic topological ordering.
- `internal/resolution`: canonical replay record and structural self-consistency verifier.
- `internal/bottle`: pre-install compressed-byte and tar security verification.
- `internal/materializer`: verified Formula staging, deterministic offline installs, prefix snapshots, containment, generated-receipt, and closure checks.
- `internal/llbutil`: deterministic LLB state construction and read-only
  resolution, bottle, and evidence transport.
- `internal/policy`: runtime allowlist, writable-state rules, and pruning-policy binding.
- `internal/runtime`: final image environment, PATH, working-directory, and non-root identity construction.
- `internal/runtimebase`: build-only Ubuntu snapshot transport and Chisel manifest and package evidence conversion.
- `internal/runtimefs`: allowlist assembly, ownership and mode normalization, inventory, pruning evidence, runtime manifest, and SPDX output.
- `internal/runtimecheck`: static ELF, loader, library, shebang, and link checks.
- `internal/testplan` and `internal/testrunner`: conversion and execution for the supported public Dalec test subset.
- `internal/frontend`: child gateway routing, target-list
  subrequests, DockerUI fan-out, shared snapshot orchestration, image
  configuration, test dependencies, and exporter epoch.
- `internal/release`: canonical component-manifest, release-bound upstream Dalec
  frontend pin, and platform-reference validation; online registry, signing, and
  promotion checks remain release-workflow responsibilities.
- `internal/buildfiles`: source-level contract tests for Dockerfile, pin
  inventory, Bake, and release workflow definitions.

Command entrypoints live under `cmd/`; component image recipes live in [`../Dockerfile`](../Dockerfile) and [`../docker-bake.hcl`](../docker-bake.hcl).

## Resolution and replay

A resolution record binds:

- the effective Dalec input digest and target platform
- requested roots sorted lexicographically by canonical requested Formula ID,
  independent of YAML map ordering
- Formula and migration payload and envelope digests, timestamp trust source,
  timestamps, and recorded signature-verification evidence
- exact OCI index, manifest, config, and layer descriptor identities plus selected annotations
- the resolved dependency closure
- a separately computed deterministic topological installation order
- frontend, runtime-base, materializer, bottle-fetcher, and catalog-extractor
  component identities plus the complete policy tuple
- runtime, attestation-waiver, and pruning-policy inputs

Requested-root order and installation order are distinct. Canonical root order
is reflected in resolution evidence and the generated default `PATH`, while the
topological installation order ensures dependencies are installed before their
dependents.

Here `frontend` means the executing `dalec-homebrew` child frontend. Upstream
Dalec is an externally authenticated release input and provenance material; it
is not projected into the child-generated resolution as if the child had
authenticated its parent.

Formula and migration documents are fetched and verified separately because upstream does not provide a signed common snapshot identifier. The combined snapshot digest commits to the exact accepted payload pair, but does not prove that Homebrew published the pair atomically. An authenticated `generated_date` takes precedence over HTTP metadata for each document; otherwise freshness uses `Last-Modified`. The record's `generated_at` and `source_date_epoch` use the earlier accepted document timestamp.

Production releases capture and verify those two JWS envelopes once before
building either frontend child. The bundle manifest and its digest are immutable
release inputs: the digest is compiled into both children, and upstream Dalec
must forward the same digest as `DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST` while
the caller supplies the bundle through the fixed
`dalec-homebrew-metadata` named context. A missing context, malformed bundle,
or digest mismatch fails closed. Builds of a frontend without a compiled bundle
digest retain the live-fetch path.

Within one frontend invocation the verified snapshot is shared immutably across
platform callbacks. The release workflow supplies the same captured bundle to
every platform and spec invocation, so those jobs do not independently refetch
metadata. Records mark the aggregate timestamp as fully signed only when
both document timestamps are authenticated; release signing still reconciles
every observation strictly and records each envelope digest in the signed
metadata snapshot.

A record is immutable once passed to the materializer. Replaying the same authenticated record and component tuple never reads a mutable Formula tag; BuildKit fetches the exact recorded layer digest.

The record contains digests and verification evidence, not the source JWS envelopes or OCI index, manifest, and config bodies. The independent materializer verifier therefore rechecks schema, graph reachability and order, descriptor and checksum relationships, component references, runtime identity, policy bindings, and the presence of recorded verified signatures; it does not re-run JWS or registry verification. A persisted record is a trust artifact and must be authenticated with its release evidence before replay.

`dockerui.Client.Build` may run platform callbacks concurrently. The verified metadata snapshot is immutable and shared, registry clients synchronize token state, and resolution and materialization records remain invocation-local. Root Formula `PkgVersion` values are compared after all platform callbacks and before exporter finalization.

## Bottle verification and installation

The compressed bottle digest must match both the selected OCI layer and the checksum authenticated by the Formula JWS. Before installation, the archive verifier rejects traversal, special files, setid bits, ACL and sparse metadata, security, trusted, and capability xattrs, collisions, and unbounded expansion. Bounded `user.*` xattrs are retained in verification inventory. Hardlinks remain keg-local. Relative symlinks under the source keg's `libexec/` tree may leave the keg only for `opt/<signed-direct-dependency>/...` targets using canonical leading traversal to the prefix and no dot segments after the dependency root. One separately versioned rule permits exact core `certifi` paths `lib/python3.<minor>/site-packages/certifi/cacert.pem`, where `<minor>` is one or two decimal digits without a leading zero, to target only `etc/ca-certificates/cert.pem` when exact core `ca-certificates` is a direct dependency; the Python-version aliases must independently match that source grammar. The installed links retain their exact verified archive text. The materializer validates dependency-keg containment for `opt` links and validates the shared CA target as a direct, protected, nonempty regular file of at most 1 MiB before pouring the owner and again after installation. All other escaping links remain rejected.

Historical bottle tabs created before Homebrew emitted `pkg_version` are accepted only when that field is absent. The resolver derives the canonical value from the tab's version and revision while retaining the exact raw annotation; explicit empty, null, non-string, or inconsistent values remain invalid. Installed or pre-install receipt dependency entries receive the same omission-only compatibility derivation. A receipt's top-level `pkg_version` may be absent, but when present it must match the resolved node; receipt source version, version scheme, and dependency closure remain independently checked.

The materializer uses a small read-only Ruby adapter around the pinned Homebrew `FormulaInstaller`. This avoids the public `brew install` command's mutable tap and writable-repository preflight while preserving Homebrew's normal bottle pour, relocation, linking, and post-install behavior.

The Go materializer copies every input bottle through a no-follow descriptor into a private directory and verifies the complete closure before Homebrew executes. It then stages the exact verified embedded Formula sources into an empty, root-owned tap with `0555` directories and `0444` files. Installation passes one immutable bottle at a time, with dependency selection and source fallback outside the installer, and validates every resulting prefix mutation.

## Runtime base and materializer separation

The final runtime base is a conservative Ubuntu Chisel Noble root filesystem copied into `scratch`. Chisel, its download cache, `apt`, `dpkg`, PAM and account-management tools, and the build-only snapshot proxy are absent.

The selected slices preserve release identity, passwd and group data, NSS state, glibc loaders and complete conversion data, C.UTF-8, timezone data, CA trust, Bash and Dash, core command-line tools, Perl base, procps, selected util-linux commands, `libgcc_s`, and `libstdc++`.

The materializer deliberately derives from the pinned full Ubuntu child image instead of the Chisel runtime. It installs pouring and relocation tools from the same Ubuntu snapshot, creates the matching `linuxbrew` identity and loader link, and copies the matching CA bundle, CA copyright, and compact package and artifact evidence from the Chisel root through a read-only build mount.

The generated Homebrew runtime is overlaid onto the independent Chisel base, so materializer tooling cannot leak into the final image.

## Snapshot-pinned Chisel input

Chisel package requests pass through a build-only allowlisted proxy that maps only the standard Ubuntu archive hosts to the configured immutable `snapshot.ubuntu.com` timestamp. Chisel remains responsible for verifying signed archive metadata and package hashes.

The runtime carries three base-evidence files:

- `runtime-base-packages.tsv` binds package versions, architectures, selected regular payload bytes, and verified source `.deb` SHA-256 values. The materializer also accepts the legacy three-column package, version, and architecture format.
- `runtime-base-artifacts.tsv` records non-package artifacts deliberately copied into the base.
- `runtime-base-chisel.manifest.wall` retains Chisel's complete compressed path and slice manifest.

The proxy and evidence converter are build-only tools and are not copied into any component image.

## Verified post-install data

Bottle paths and types remain authoritative after pouring. Content or link-target changes are accepted only for paths listed in the recorded OCI tab's `changed_files`, entries in which the verifier detected Homebrew relocation placeholders, the keg-root receipt and SPDX document, and the exact validated Node npm link rewrite. The OCI tab is bound into the resolution record but is not authenticated by the Formula JWS. Neither external-link exception allows its verified target text to change. Mode changes are limited to adding the owner-write bit on declared changed files and authenticated Python virtual-environment templates; setid, group or other write, execute, and sticky-bit changes remain rejected.

The only extra keg subtree currently accepted is `glibc/lib/locale`, which the verified Homebrew `glibc` Formula generates with its brewed `localedef`. Reconciliation bounds its entry count, per-file and aggregate size, requires known ownership and non-writable, non-executable ordinary files or directories, and rejects links and special modes. Every other unexpected keg path still fails closed.

Generated global runtime indexes have explicit, versioned handling:

- The `gdk-pixbuf` loader cache is accepted only at `lib/gdk-pixbuf-2.0/2.10.0/loaders.cache`. Its grammar, ownership, mode, size, module count, and exact module set are checked, and every module must resolve to a verified loader in the bottle closure.
- The `shared-mime-info` database under `share/mime` is accepted only from its verified generator, with fixed required outputs, bounded file, count, and size totals, safe path grammar, and parsed generated XML.
- Fontconfig's verified shared `etc/fonts` configuration is retained explicitly.
- Node's post-install npm runtime is accepted only at `lib/node_modules/npm`. Every entry must match the verified private tree under the Node keg, except for an exact generated `npmrc`; `npm`, `npx`, and manpage links must terminate in that validated runtime.

All generated files are re-hashed in runtime inventory and normalized to root-owned, non-writable output. Ordinary global copies retain their original bottle attribution.

Runtime ELF verification treats non-exposed inventory files with object-data extensions `.a`, `.o`, `.lo`, and `.syso` containing `ET_REL` as linker or object data, including Homebrew glibc's raw `libmcheck.a` and foreign-architecture Go objects. Such objects remain invalid when exposed as commands or stored as shared objects, plugins, or arbitrary helpers. Executable scripts require a usable interpreter unless an exact record-derived package, path, and shebang exception classifies the file as auxiliary data; exposed and requested scripts never receive that exception. For transitive core `libpsl`, the exact global `bin/psl-make-dafsa` link is pruned while the authenticated keg script remains inventoried as auxiliary data with its exact `/usr/bin/env python` shebang.

## Runtime assembly and tests

The installed prefix is scanned into an inventory. Only allowlist-selected prefix entries are copied into the materialized overlay; the materializer then adds the explicit resolution, inventory, prune, manifest, SPDX, base, and installation evidence files under `/usr/share/dalec-homebrew`. Ownership and modes are normalized before static runtime checks and Dalec tests run.

Runtime minimization is an automatic release-policy step, not dependency
resolution or a caller-selected mode. After the complete closure has been
resolved, every bottle verified, and every Formula installed offline, assembly
omits exact policy-classified paths only from transitive `homebrew/core` kegs.
Requested Formulae remain outside the added pruning pass. The six bounded
classes are headers, man and Info trees, exact build-metadata locations,
policy-authorized Python standard-library test subtrees, shell completions, and
static archives in bounded `lib/` locations.

The classifier is fail-closed by canonical Formula identity and relative path.
It never generalizes from a basename such as `test`, and it preserves shared
libraries, plugins, `libexec`, configuration, locales, site-packages,
`ensurepip`, `venv`, `node_modules`, Formula `share/doc` content, legal or
license text, and static archives inside protected runtime-data locations.

Before classifying paths, the policy derives a development-payload retention
set from exact release-bound Formula policy capabilities and the verified
dependency graph. A capability-authorized compiler or MPI Formula retains
headers, build metadata, and static archives for its node and verified
dependency closure.
Unsigned OCI executable-path annotations cannot activate this rule. This is a
retention rule rather than a seventh prune class; unrelated nodes remain
eligible for all six classes.

Retained links and runtime validation must still succeed on the final state.
The pruning-policy identity and exact decisions are committed into resolution,
inventory, prune, and runtime-manifest evidence; changing the policy requires a
new release tuple. Invocation input cannot disable or broaden it.

Each declared Dalec test runs on an independent branch derived from the final pruned state. The frontend injects an ephemeral test runner and plan under `/__dalec_homebrew`; those files are not exported. Commands inherit the final image user, environment, and working directory, after which the supported test-level directory, test environment, and step environment overrides apply. BuildKit disables networking for each branch. Development frontends may set `DALEC_SKIP_TESTS`; release-bound frontends reject that bypass.

The image contents and evidence files are listed in the [usage reference](usage.md).

## Public tap flow

The core path is unchanged; a release-bound ingestion path is added only when a root uses `owner/tap/formula`:

```text
all-platform raw/typed preflight
  -> official core JWS authentication
  -> one per-invocation catalog-set request
  -> build-local exact-commit tap extraction and canonical catalog validation
  -> independent cross-tap normalization, cycle/order, platform, and rack-collision checks
  -> independent GHCR descriptor resolution or one bounded HTTPS fetch exec per selected artifact
  -> canonical resolution record
  -> prepare (all bottles or policy-derived bottles and fetch evidence verified; synthetic taps and trust store sealed)
  -> one network-disabled install exec per install-order node
  -> finalize onto the clean runtime base with inventory, prune, manifest, and SPDX evidence
```

Core-only builds bypass tap extraction. For non-core roots, the frontend constructs one invocation-wide fixed-point ingestion plan. BuildKit fetches each reached default-GitHub tap once, binds its exact commit/tree/archive identity, and evaluates Formula metadata through the digest-pinned extractor with networking disabled, read-only source mounts, and disposable cache and temporary mounts. The frontend strictly decodes each bounded catalog and independently recomputes dependency normalization, closure, cycles, ordering, and rack collisions.

No catalog server or signing key is involved. Metadata evidence records `build-local-tap-extraction-v1` plus the extractor reference and canonical catalog digest. Because there is no shared writer, build-local ingestion cannot provide a cross-build monotonic sequence floor; the explicit rollback policy records that limitation, and release replay requires retained catalog and artifact bytes.

HTTPS bottles never use `llb.HTTP`. A minimal CA-plus-static-binary fetcher enforces public DNS/IP destinations, HTTPS port 443, redirect allowlists, exact positive size up to 1 GiB, a 15-minute deadline, and SHA-256 before atomically publishing mode-0444 output and redacted evidence.

A Formula without a native bottle remains unsupported unless its exact Formula ID appears in the release-bound prebuilt-archive section of the tap policy. In that case build-local ingestion verifies the complete bounded tar+gzip inventory plus the selected static ELF and Go build properties, then creates a deterministic receiptless **derived bottle** containing only the policy-selected executable and exact authenticated Formula source. The derived bytes remain an invocation-local content-addressed BuildKit input (`build-local-artifact-v1`) and are reverified by the offline materializer. Formula `install` and `post_install` methods are not run.

During ingestion, GHCR source annotations are used to recover the exact historical bottle-source commit and Formula path. The source file is fetched at that immutable revision and compared byte-for-byte with the embedded Formula after the deterministic Homebrew bottle block (which is intentionally omitted from bottle Formula copies) is removed. Generic HTTPS bottles do not expose an equivalent authenticated source revision, so ingestion leaves the historical repository/commit/path fields empty and requires the explicit `https-bottle-embedded-formula-digest-only-v1` bottle-source waiver. The current build-local exact-commit tap source and the separately verified embedded-Formula digest remain distinct evidence.

GHCR artifacts may advertise a Sigstore bundle through an OCI 1.1 referrer or the digest-bound `dev.sigstore.bundle.url` and `dev.sigstore.bundle.digest` annotations; generic HTTPS bottles use the deterministic `<bottle-path>.sigstore.json` sidecar convention. When present, ingestion fetches the bounded bundle, verifies its SHA-256, in-toto subject, transparency evidence, GitHub Actions issuer, and repository-scoped identity against the Sigstore trusted root embedded in the tap policy. If no supported discovery route exposes evidence, the artifact records the explicit build-local catalog/checksum provenance waiver; partial, ambiguous, or invalid advertised evidence fails closed.

Materialization seeds the protected Homebrew prefix once, stages bottle-embedded Formula source under exact synthetic tap paths, and writes a read-only `trust.json` containing only selected non-core Formula IDs. Each install starts from a fresh materializer rootfs with networking disabled; only the cumulative prefix and explicit delta evidence persist. Formula-specific runtime exceptions are granted only to exact `homebrew/core/...` identities.
