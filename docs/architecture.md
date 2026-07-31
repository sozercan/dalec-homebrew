# Architecture

The frontend separates networked resolution from offline execution.

- `internal/spec`: raw dependency-order extraction and typed V1 Dalec validation.
- `internal/homebrew/metadata`: bounded HTTP fetch, RFC 7797/PS512 JWS verification, freshness/rollback policy, and canonical alias/rename/migration lookup.
- `internal/homebrew/oci`: Distribution authentication and exact descriptor-chain validation.
- `internal/resolver`: fixed-point current-stable closure resolution and deterministic topological ordering.
- `internal/resolution`: canonical replay record and independent verifier.
- `internal/bottle`: pre-install compressed-byte and tar security verification.
- `internal/materializer`: deterministic offline installs, prefix snapshots, containment, generated-receipt and closure checks.
- `internal/runtimebase`: build-only Ubuntu snapshot transport and Chisel manifest/package evidence conversion.
- `internal/runtimefs`: clean-base allowlist assembly, ownership/mode normalization, inventory, prune evidence, runtime manifest, and SPDX.
- `internal/runtimecheck`: static ELF, loader, library, shebang, and link checks.
- `internal/testrunner`: the supported public Dalec test subset.
- `internal/frontend`: DockerUI fan-out, shared snapshot orchestration, image config, test dependencies, and exporter epoch.

`dockerui.Client.Build` may run platform callbacks concurrently. The verified metadata snapshot is immutable and shared; registry clients synchronize token state; resolution/materialization records are invocation-local. Root Formula `PkgVersion` values are compared after platform callbacks and before exporter finalization.

A resolution record is immutable once passed to the materializer. Replaying the same record and component tuple never reads a mutable Formula tag; BuildKit fetches the exact recorded layer digest.

The materializer uses a small read-only Ruby adapter around the pinned
Homebrew `FormulaInstaller`. This avoids the public `brew install` command's
mutable tap and writable-repository preflight while preserving Homebrew's
normal bottle pour, relocation, linking, and post-install behavior. The Go
materializer verifies each local bottle first, passes one immutable bottle at
a time, disables networking in the enclosing LLB exec, and validates every
resulting prefix mutation.

## Runtime base and materializer separation

The final runtime base is a conservative Ubuntu Chisel Noble rootfs copied into `scratch`. Chisel, its download cache, `apt`, `dpkg`, PAM/account-management tools, and the build-only snapshot proxy are absent. The selected slices preserve release identity, passwd/group and NSS state, glibc loaders and full gconv data, C.UTF-8, timezone data, CA trust, Bash/Dash, coreutils, grep, sed, findutils, awk, tar/gzip, Perl base, procps, selected util-linux commands, `libgcc_s`, and `libstdc++`.

The materializer deliberately derives from the pinned full Ubuntu child image instead of the Chisel runtime. It installs pouring/relocation tools from the same Ubuntu snapshot, creates the matching `linuxbrew` identity and loader link, and receives only the Chisel base's compact package/artifact evidence through a read-only build mount. The generated runtime is still assembled by overlaying the verified materializer output onto the independent Chisel base, so materializer tooling cannot leak into the final image.

Chisel package requests pass through a build-only allowlisted proxy that maps only the standard Ubuntu archive hosts to the configured immutable `snapshot.ubuntu.com` timestamp. Chisel remains responsible for verifying signed archive metadata and package hashes. The retained `runtime-base-chisel.manifest.wall` and five-column `runtime-base-packages.tsv` bind package versions, architectures, selected bytes, and source package SHA-256 values into final evidence and SPDX.

## Verified post-install data

Bottle inventory remains authoritative after pouring. The only extra keg subtree currently accepted is `glibc/lib/locale`, which the authenticated Homebrew `glibc` Formula deterministically generates with its brewed `localedef`. Reconciliation bounds its entry count, per-file and aggregate size, requires known ownership and non-writable/non-executable ordinary files or directories, and rejects links and special modes. Every other unexpected keg path still fails closed.

Runtime ELF verification treats a private `.a` file containing a raw `ET_REL` object as linker data, matching Homebrew glibc's `libmcheck.a`. Such an object is still rejected if exposed as a command; `.so`/plugin paths and arbitrary executable relocatable objects remain invalid.
