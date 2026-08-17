# Documentation

Choose the shortest guide that matches what you are trying to do.

## Build and use images

1. **[README quickstart](../README.md#quickstart)** — start from no Dalec or
   BuildKit knowledge and build GNU Hello.
2. **[Usage guide](usage.md)** — select packages, configure an image, add
   offline runtime tests, verify release assets, and troubleshoot failures.
3. **[Examples](../examples/README.md)** — templates and integration fixtures,
   clearly categorized.
4. **[Glossary](../CONTEXT.md)** — definitions for Dalec, frontend, Formula,
   bottle, resolution, materialization, and release terminology.

## Evaluate security and design

- **[Security model](../SECURITY.md)** — enforced properties, trust boundaries,
  upstream limitations, and private vulnerability reporting.
- **[Architecture](architecture.md)** — frontend routing, network boundaries,
  resolution, archive verification, offline installation, runtime assembly, and
  public-tap ingestion.

## Maintain or release the project

- **[Contributing](../CONTRIBUTING.md)** — tool versions, local checks, live
  integration tests, component builds, and pull-request expectations.
- **[Release and rollback](release.md)** — the immutable component tuple,
  pinned inputs, signing evidence, promotion, and rollback.

## Architecture decisions

Architecture Decision Records (ADRs) explain why a significant design was
chosen and which alternatives were rejected:

- [ADR 0001: Policy-authorized prebuilt executable archives](adr/0001-policy-authorized-prebuilt-archives.md)
- [ADR 0002: Build-local public tap ingestion](adr/0002-build-local-tap-ingestion.md)

## About numeric schema suffixes

Some JSON `schema_version` values, evidence filenames, source directories, and
compatibility commands contain numeric suffixes. These identify machine-format
generations retained by the implementation. They are not separate product
editions or user-selectable build modes; current users follow one documented
workflow.
