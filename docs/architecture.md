# Architecture

`dalec-homebrew` separates networked resolution from offline execution. A build first turns a Dalec dependency declaration into a canonical, digest-bound resolution record, then materializes that record without network access and copies an allowlisted runtime into an independent Ubuntu Chisel base.

## Build pipeline

```text
raw Dalec preflight
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

- `internal/spec`: raw dependency-order extraction and typed V1 Dalec validation.
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
- `internal/policy`: V1 runtime allowlist, writable-state rules, and pruning-policy binding.
- `internal/runtime`: final image environment, PATH, working-directory, and non-root identity construction.
- `internal/runtimebase`: build-only Ubuntu snapshot transport and Chisel manifest and package evidence conversion.
- `internal/runtimefs`: allowlist assembly, ownership and mode normalization, inventory, pruning evidence, runtime manifest, and SPDX output.
- `internal/runtimecheck`: static ELF, loader, library, shebang, and link checks.
- `internal/testplan` and `internal/testrunner`: conversion and execution for the supported public Dalec test subset.
- `internal/frontend`: DockerUI fan-out, shared snapshot orchestration, image configuration, test dependencies, and exporter epoch.
- `internal/release`: canonical component-manifest and platform-reference validation; online registry, signing, and promotion checks remain release-workflow responsibilities.
- `internal/buildfiles`: source-level contract tests for Dockerfile, pin
  inventory, Bake, and release workflow definitions.

Command entrypoints live under `cmd/`; component image recipes live in [`../Dockerfile`](../Dockerfile) and [`../docker-bake.hcl`](../docker-bake.hcl).

## Resolution and replay

A resolution record binds:

- the effective Dalec input digest and target platform
- Formula and migration payload and envelope digests, freshness sources, timestamps, URLs, and recorded signature-verification evidence
- exact OCI index, manifest, config, and layer descriptor identities plus selected annotations
- requested roots and the resolved dependency closure
- deterministic installation order
- frontend, runtime-base, and materializer component identities
- runtime, attestation-waiver, and pruning-policy inputs

Formula and migration documents are fetched and verified separately because upstream does not provide a signed common snapshot identifier. The combined snapshot digest commits to the exact accepted payload pair, but does not prove that Homebrew published the pair atomically. An authenticated `generated_date` takes precedence over HTTP metadata for each document; otherwise freshness uses `Last-Modified`. The record's `generated_at` and `source_date_epoch` use the earlier accepted document timestamp.

A record is immutable once passed to the materializer. Replaying the same authenticated record and component tuple never reads a mutable Formula tag; BuildKit fetches the exact recorded layer digest.

The record contains digests and verification evidence, not the source JWS envelopes or OCI index, manifest, and config bodies. The independent materializer verifier therefore rechecks schema, graph reachability and order, descriptor and checksum relationships, component references, runtime identity, policy bindings, and the presence of recorded verified signatures; it does not re-run JWS or registry verification. A persisted record is a trust artifact and must be authenticated with its release evidence before replay.

`dockerui.Client.Build` may run platform callbacks concurrently. The verified metadata snapshot is immutable and shared, registry clients synchronize token state, and resolution and materialization records remain invocation-local. Root Formula `PkgVersion` values are compared after all platform callbacks and before exporter finalization.

## Bottle verification and installation

The compressed bottle digest must match both the selected OCI layer and the checksum authenticated by the Formula JWS. Before installation, the archive verifier rejects traversal, special files, setid bits, ACL and sparse metadata, security, trusted, and capability xattrs, collisions, and unbounded expansion. Bounded `user.*` xattrs are retained in verification inventory. Hardlinks remain keg-local. Relative symlinks under the source keg's `libexec/` tree may leave the keg only for `opt/<signed-direct-dependency>/...` targets using only canonical leading traversal to the prefix and no dot segments after the dependency root inside the Homebrew prefix. The installed link must retain the exact verified archive target text, and the materializer verifies that it resolves inside the exact dependency keg before pouring the owner and again after installation. All other escaping links remain rejected.

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

Bottle paths and types remain authoritative after pouring. Content or link-target changes are accepted only for paths listed in the recorded OCI tab's `changed_files`, entries in which the verifier detected Homebrew relocation placeholders, the keg-root receipt and SPDX document, and the exact validated Node npm link rewrite. The OCI tab is bound into the resolution record but is not authenticated by the Formula JWS. The permitted external dependency-link exception never allows its verified target text to change. Mode changes are limited to adding the owner-write bit on declared changed files and authenticated Python virtual-environment templates; setid, group or other write, execute, and sticky-bit changes remain rejected.

The only extra keg subtree currently accepted is `glibc/lib/locale`, which the verified Homebrew `glibc` Formula generates with its brewed `localedef`. Reconciliation bounds its entry count, per-file and aggregate size, requires known ownership and non-writable, non-executable ordinary files or directories, and rejects links and special modes. Every other unexpected keg path still fails closed.

Generated global runtime indexes have explicit, versioned handling:

- The `gdk-pixbuf` loader cache is accepted only at `lib/gdk-pixbuf-2.0/2.10.0/loaders.cache`. Its grammar, ownership, mode, size, module count, and exact module set are checked, and every module must resolve to a verified loader in the bottle closure.
- The `shared-mime-info` database under `share/mime` is accepted only from its verified generator, with fixed required outputs, bounded file, count, and size totals, safe path grammar, and parsed generated XML.
- Fontconfig's verified shared `etc/fonts` configuration is retained explicitly.
- Node's post-install npm runtime is accepted only at `lib/node_modules/npm`. Every entry must match the verified private tree under the Node keg, except for an exact generated `npmrc`; `npm`, `npx`, and manpage links must terminate in that validated runtime.

All generated files are re-hashed in runtime inventory and normalized to root-owned, non-writable output. Ordinary global copies retain their original bottle attribution.

Runtime ELF verification treats non-exposed inventory files with object-data extensions `.a`, `.o`, `.lo`, and `.syso` containing `ET_REL` as linker or object data, including Homebrew glibc's raw `libmcheck.a` and foreign-architecture Go objects. Such objects remain invalid when exposed as commands or stored as shared objects, plugins, or arbitrary helpers. Executable scripts require a usable interpreter unless an exact record-derived package, path, and shebang exception classifies the file as auxiliary data; exposed and requested scripts never receive that exception.

## Runtime assembly and tests

The installed prefix is scanned into an inventory. Only allowlist-selected prefix entries are copied into the materialized overlay; the materializer then adds the explicit resolution, inventory, prune, manifest, SPDX, base, and installation evidence files under `/usr/share/dalec-homebrew`. Ownership and modes are normalized before static runtime checks and Dalec tests run.

Each declared Dalec test runs on an independent branch derived from the final pruned state. The frontend injects an ephemeral test runner and plan under `/__dalec_homebrew`; those files are not exported. Commands inherit the final image user, environment, and working directory, after which the supported test-level directory, test environment, and step environment overrides apply. BuildKit disables networking for each branch. Development frontends may set `DALEC_SKIP_TESTS`; release-bound frontends reject that bypass.

The image contents and evidence files are listed in the [usage reference](usage.md).
