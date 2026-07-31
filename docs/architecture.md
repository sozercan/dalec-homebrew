# Architecture

`dalec-homebrew` separates networked resolution from offline execution. A build first turns a Dalec dependency declaration into a canonical, digest-bound resolution record, then materializes that record without network access and copies an allowlisted runtime into an independent Ubuntu Chisel base.

## Build pipeline

```text
raw Dalec preflight
  -> signed Formula and migration metadata
  -> per-platform OCI descriptor resolution
  -> canonical resolution.json and independent verification
  -> exact bottle layers through llb.ImageBlob
  -> static archive verification
  -> offline Homebrew install in deterministic topological order
  -> per-install containment and complete-keg verification
  -> verified reconciliation of approved generated runtime data
  -> allowlist overlay onto a snapshot-pinned Chisel runtime base
  -> static ELF, shebang, and link verification
  -> final-user, final-environment Dalec tests
  -> OCI image configuration with resolution source_date_epoch
```

The network boundary sits between resolution and materialization:

- Resolution authenticates Homebrew metadata, discovers registry objects, and records exact descriptors.
- Materialization receives only the immutable record and exact bottle blobs.
- BuildKit disables networking for bottle installation and runtime tests.
- The full materializer filesystem is never exported into the final image.

See [`../SECURITY.md`](../SECURITY.md) for the properties enforced at each boundary.

## Package layout

- `internal/spec`: raw dependency-order extraction and typed V1 Dalec validation.
- `internal/homebrew/metadata`: bounded HTTP fetch, RFC 7797/PS512 JWS verification, freshness and rollback policy, and canonical alias, rename, and migration lookup.
- `internal/homebrew/oci`: Distribution authentication and exact descriptor-chain validation.
- `internal/resolver`: fixed-point current-Formula closure resolution and deterministic topological ordering.
- `internal/resolution`: canonical replay record and independent verifier.
- `internal/bottle`: pre-install compressed-byte and tar security verification.
- `internal/materializer`: deterministic offline installs, prefix snapshots, containment, generated-receipt, and closure checks.
- `internal/runtimebase`: build-only Ubuntu snapshot transport and Chisel manifest and package evidence conversion.
- `internal/runtimefs`: allowlist assembly, ownership and mode normalization, inventory, pruning evidence, runtime manifest, and SPDX output.
- `internal/runtimecheck`: static ELF, loader, library, shebang, and link checks.
- `internal/testplan` and `internal/testrunner`: conversion and execution for the supported public Dalec test subset.
- `internal/frontend`: DockerUI fan-out, shared snapshot orchestration, image configuration, test dependencies, and exporter epoch.

Command entrypoints live under `cmd/`; component image recipes live in [`../Dockerfile`](../Dockerfile) and [`../docker-bake.hcl`](../docker-bake.hcl).

## Resolution and replay

A resolution record binds:

- the effective Dalec input and target platform
- authenticated Formula and migration metadata
- exact OCI index, manifest, config, and layer descriptors
- requested roots and the resolved dependency closure
- deterministic installation order
- frontend, runtime-base, and materializer component identities
- runtime, attestation, and pruning policy inputs

A record is immutable once passed to the materializer. Replaying the same record and component tuple never reads a mutable Formula tag; BuildKit fetches the exact recorded layer digest.

`dockerui.Client.Build` may run platform callbacks concurrently. The verified metadata snapshot is immutable and shared, registry clients synchronize token state, and resolution and materialization records remain invocation-local. Root Formula `PkgVersion` values are compared after all platform callbacks and before exporter finalization.

## Bottle verification and installation

The compressed bottle digest must match both the selected OCI layer and the checksum authenticated by the Formula JWS. Before installation, the archive verifier rejects traversal, escaping links, special files, setid bits, security capabilities and xattrs, collisions, and unbounded expansion.

The materializer uses a small read-only Ruby adapter around the pinned Homebrew `FormulaInstaller`. This avoids the public `brew install` command's mutable tap and writable-repository preflight while preserving Homebrew's normal bottle pour, relocation, linking, and post-install behavior.

The Go materializer verifies each local bottle first, passes one immutable bottle at a time, disables networking in the enclosing LLB exec, and validates every resulting prefix mutation. Dependency selection and source fallback remain outside the installer.

## Runtime base and materializer separation

The final runtime base is a conservative Ubuntu Chisel Noble root filesystem copied into `scratch`. Chisel, its download cache, `apt`, `dpkg`, PAM and account-management tools, and the build-only snapshot proxy are absent.

The selected slices preserve release identity, passwd and group data, NSS state, glibc loaders and complete conversion data, C.UTF-8, timezone data, CA trust, Bash and Dash, core command-line tools, Perl base, procps, selected util-linux commands, `libgcc_s`, and `libstdc++`.

The materializer deliberately derives from the pinned full Ubuntu child image instead of the Chisel runtime. It installs pouring and relocation tools from the same Ubuntu snapshot, creates the matching `linuxbrew` identity and loader link, and receives only the Chisel base's compact package and artifact evidence through a read-only build mount.

The generated Homebrew runtime is overlaid onto the independent Chisel base, so materializer tooling cannot leak into the final image.

## Snapshot-pinned Chisel input

Chisel package requests pass through a build-only allowlisted proxy that maps only the standard Ubuntu archive hosts to the configured immutable `snapshot.ubuntu.com` timestamp. Chisel remains responsible for verifying signed archive metadata and package hashes.

The runtime carries three base-evidence files:

- `runtime-base-packages.tsv` binds package versions, architectures, selected regular payload bytes, and verified source `.deb` SHA-256 values. The materializer also accepts the legacy three-column package, version, and architecture format.
- `runtime-base-artifacts.tsv` records non-package artifacts deliberately copied into the base.
- `runtime-base-chisel.manifest.wall` retains Chisel's complete compressed path and slice manifest.

The proxy and evidence converter are build-only tools and are not copied into any component image.

## Verified post-install data

Bottle inventory remains authoritative after pouring. The only extra keg subtree currently accepted is `glibc/lib/locale`, which the authenticated Homebrew `glibc` Formula deterministically generates with its brewed `localedef`.

Reconciliation bounds its entry count, per-file and aggregate size, requires known ownership and non-writable, non-executable ordinary files or directories, and rejects links and special modes. Every other unexpected keg path still fails closed.

Generated global runtime indexes have explicit, versioned handling:

- The `gdk-pixbuf` loader cache is accepted only at `lib/gdk-pixbuf-2.0/2.10.0/loaders.cache`. Its grammar, ownership, mode, size, module count, and exact module set are checked, and every module must resolve to a verified loader in the bottle closure.
- The `shared-mime-info` database under `share/mime` is accepted only from its verified generator, with fixed required outputs, bounded file, count, and size totals, safe path grammar, and parsed generated XML.
- Fontconfig's verified shared `etc/fonts` configuration is retained explicitly.

All generated files are re-hashed in runtime inventory and normalized to root-owned, non-writable output. Ordinary global copies retain their original bottle attribution.

Runtime ELF verification treats a private `.a` file containing a raw `ET_REL` object as linker data, matching Homebrew glibc's `libmcheck.a`. Such an object is still rejected if exposed as a command; shared-object, plugin, and arbitrary executable relocatable-object paths remain invalid.

## Runtime assembly and tests

The installed prefix is scanned into an inventory. Only inventory-selected files are copied into the final state, where ownership and modes are normalized before static runtime checks and Dalec tests run.

Tests execute against the final pruned filesystem with the configured user, environment, and working directory. The image contents and evidence files are listed in the [usage reference](usage.md).
