# ADR 0002: Build-local public tap ingestion

- Status: Accepted
- Date: 2026-08-05

## Plain-language summary

Public taps are inspected inside the caller's BuildKit worker for each build. The project does not run a central catalog service. Every build records the exact Git commit and artifact bytes it observed, but a future build can observe a moved default branch; reproducible replay therefore retains the original inputs.

For term definitions, see the [glossary](../../CONTEXT.md).

## Context

The initial non-core design required a hosted catalog service, persistent single-writer state, a PS512 signing key, a dedicated ingestion worker, and an HTTPS origin. That preserved centralized sequencing but imposed an operational service on every capable release.

## Decision

Run public-tap ingestion as one-shot content-addressed solves on the caller's BuildKit worker.

The release binds a digest-pinned catalog extractor. The frontend derives the default public GitHub repository, resolves its exact commit through `llb.Git`, and runs Formula evaluation in a network-disabled, read-only extractor exec without secrets, SSH, or shared writable caches. It reads only bounded canonical metadata, independently recomputes the closure, and verifies selected artifacts before materialization.

Policy-derived bottles remain BuildKit states and use `build-local-artifact-v1`; they are never uploaded to a project-operated service. Core-only builds do not pull or run the extractor.

## Consequences

- No catalog-service deployment, database, signing key, public tunnel, or dedicated remote worker is required.
- The catalog extractor becomes an explicit release component.
- Untrusted Formula evaluation shares the caller's trusted BuildKit worker, although the exec remains network-disabled and isolated.
- There is no centralized cross-build rollback floor. Records bind the exact observed commit and use `build-local-exact-commit-no-cross-build-rollback-v1` explicitly.
- Replaying a retained release requires the exact retained catalog, upstream archive, and locally derived bottle bytes.

## Related documentation

- [Usage: public-tap names](../usage.md#declare-runtime-dependencies)
- [Architecture: public-tap flow](../architecture.md#public-tap-flow)
- [Security: public-tap properties](../../SECURITY.md#public-tap-security-properties)
