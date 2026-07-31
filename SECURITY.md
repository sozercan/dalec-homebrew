# Security policy

## Security properties

The V1 implementation is expected to preserve these properties:

1. Unsupported Dalec inputs and malformed Formula names fail before metadata or registry access.
2. Formula identity and bottle checksums come from a verified Homebrew JWS payload and a pinned public-key set.
3. Registry tags are discovery inputs only. Resolution records bind raw index, manifest, config, and layer descriptors.
4. The compressed bottle digest must equal both the selected OCI layer digest and the authenticated Homebrew checksum.
5. Bottle archives are scanned before installation and cannot contain traversal, escaping links, special files, setid bits, security capabilities/xattrs, collisions, or unbounded expansion.
6. The independent resolution verifier and every bottle verifier complete before `brew` executes.
7. Materialization has no network, secrets, SSH agents, sockets, devices, or shared writable caches.
8. Homebrew is invoked only with local bottles, `--ignore-dependencies`, and `--force-bottle`; unexpected kegs and source-built receipts fail.
9. Only an inventory-selected filesystem subset is copied into a fresh runtime base.
10. Runtime code, links, libraries, plugins, and ancestors are root-owned and non-writable. Only explicitly versioned `var/<formula>` paths are writable by the runtime identity.
11. Runtime tests use the final pruned state, final user, final environment/working directory, and no network.
12. The Noble runtime base is cut from a fixed Ubuntu snapshot with a SHA-256-pinned Chisel binary and immutable checksummed slice definitions. The build-only proxy accepts only Ubuntu archive hosts, while Chisel remains responsible for signed Release and package-digest verification; neither Chisel nor the proxy reaches the final image.
13. Generated shared runtime indexes are accepted only at versioned paths with
    package/capability checks, bounded structure and size, authenticated runtime
    ownership, and explicit runtime evidence attribution; unrelated global
    `lib` or shared-data mutations continue to fail closed.

## Out of scope / external controls

The frontend does not hold signing credentials and cannot itself guarantee registry retention, CI builder identity, vulnerability database freshness, VEX approval, or immutable rollback mirrors. Release automation must sign the frontend/base/materializer tuple, exact platform images, resolution/evidence artifacts, and provenance, then promote by digest without rebuilding.

The public Homebrew Formula JWS does not currently provide a signed timestamp or bind the OCI `sh.brew.tab` annotation. See the trust notes in the README. Reports that demonstrate a practical violation of the checks above are in scope even when the root cause is an upstream metadata-format limitation.

## Reporting

Please report vulnerabilities privately to the repository maintainers. Include a minimal Dalec spec or resolution record, the affected platform/component digests, and a reproduction where possible. Do not include credentials or private registry tokens.
