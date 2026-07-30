# Architecture

The frontend separates networked resolution from offline execution.

- `internal/spec`: raw dependency-order extraction and typed V1 Dalec validation.
- `internal/homebrew/metadata`: bounded HTTP fetch, RFC 7797/PS512 JWS verification, freshness/rollback policy, and canonical alias/rename/migration lookup.
- `internal/homebrew/oci`: Distribution authentication and exact descriptor-chain validation.
- `internal/resolver`: fixed-point current-stable closure resolution and deterministic topological ordering.
- `internal/resolution`: canonical replay record and independent verifier.
- `internal/bottle`: pre-install compressed-byte and tar security verification.
- `internal/materializer`: deterministic offline installs, prefix snapshots, containment, generated-receipt and closure checks.
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
