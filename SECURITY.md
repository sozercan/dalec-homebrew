# Security policy

## Security properties

The V1 implementation is expected to preserve these properties:

1. Unsupported Dalec inputs and malformed Formula names fail before metadata or registry access.
2. Formula identity and bottle checksums come from a verified Homebrew JWS payload and a pinned public-key set.
3. Registry tags are discovery inputs only. Resolution records bind raw index, manifest, config, and layer descriptors.
4. The compressed bottle digest must equal both the selected OCI layer digest and the authenticated Homebrew checksum.
5. Bottle archives are scanned before installation and cannot contain traversal, special files, setid bits, security capabilities or xattrs, collisions, or unbounded expansion. Hardlinks remain keg-local. Symlinks remain keg-local except for relative links under the owner keg's `libexec/` tree that use prefix-contained `opt/<signed-direct-dependency>/...` targets using only canonical leading traversal to the prefix and no dot segments after the dependency root that are verified against the exact resolved dependency keg before and after installation; every other escaping link is rejected.
6. The independent resolution verifier and every bottle verifier complete before Homebrew executes.
7. Materialization has no network, secrets, SSH agents, sockets, devices, or shared writable caches.
8. Homebrew is invoked only with local bottles, dependency selection and source fallback disabled, and every resulting prefix mutation checked. Unexpected kegs and source-built receipts fail.
9. Only an inventory-selected filesystem subset is copied into the final runtime state.
10. Runtime code, links, libraries, plugins, and ancestors are root-owned and non-writable. Only explicitly versioned `var/<formula>` paths are writable by the runtime identity.
11. Runtime tests use the final pruned state, final user, final environment and working directory, and no network.
12. The Noble runtime base is cut from a fixed Ubuntu snapshot with a SHA-256-pinned Chisel binary and immutable, checksummed slice definitions. The build-only proxy accepts only Ubuntu archive hosts, while Chisel remains responsible for signed Release and package-digest verification; neither Chisel nor the proxy reaches the final image.
13. Generated shared runtime indexes are accepted only at versioned paths with package and capability checks, bounded structure and size, authenticated runtime ownership, and explicit evidence attribution. Node's global `lib/node_modules/npm` runtime is accepted only as a bounded, exact copy of the verified private npm tree plus one exact prefix-bound `npmrc`, with command and manpage links bound back to that validated tree. Unrelated global `lib` or shared-data mutations continue to fail closed.

## Upstream trust limitations

Homebrew's public JWS documents authenticate metadata and bottle checksums,
but do not always include an authenticated generation timestamp. When absent,
freshness relies on the unsigned HTTP `Last-Modified` value.

Callers may supply a metadata rollback floor, but the repository release
workflow does not persist one across releases. For release-bound frontends, it
limits metadata age to seven days and future skew to 15 minutes, requires every
release integration to use the same authenticated snapshot, and records that
snapshot in signed evidence. This is not cross-release anti-rollback: a
previously superseded but still-fresh signed snapshot may be accepted by a
later release.

The Formula JWS authenticates the compressed bottle checksum, but it does not bind the OCI index annotations containing `sh.brew.tab`. This implementation:

1. treats signed Formula declarations as package identity authority,
2. verifies the complete fetched OCI descriptor chain by digest and size,
3. requires the selected layer digest to equal the signed Homebrew checksum,
4. treats bottle-tab dependencies as minimum and consistency evidence, and
5. records an explicit upstream-attestation waiver unless release infrastructure supplies a stronger attestation policy.

Current Homebrew bottle tarballs generally do not contain `INSTALL_RECEIPT.json`; Homebrew creates it while pouring the bottle. The archive verifier can require a pre-install receipt for fixtures or alternate producers, while production verifies the generated receipt after offline installation.

Modern Homebrew forbids local bottle paths by default, and its public `brew install` command performs mutable tap and prefix preflight. The materializer therefore invokes the pinned Homebrew `FormulaInstaller` through a minimal read-only Ruby adapter after independent bottle verification. Resolution, dependency selection, network access, and source fallback remain disabled; normal Homebrew extraction, relocation, linking, `etc` and `var` handling, and Formula post-install hooks still run.

See [`docs/architecture.md`](docs/architecture.md) for the complete resolution, materialization, and runtime-base flow.

## Out of scope and external controls

The frontend does not hold signing credentials and cannot itself guarantee registry retention, CI builder identity, vulnerability database freshness, VEX approval, or immutable rollback mirrors. Release automation must sign the frontend, base, and materializer tuple; exact platform images; resolution and evidence artifacts; and provenance, then promote by digest without rebuilding.

Reports that demonstrate a practical violation of the properties above are in scope even when the root cause is an upstream metadata-format limitation.

## Reporting

Please report vulnerabilities privately to the repository maintainers. Include a minimal Dalec spec or resolution record, the affected platform and component digests, and a reproduction where possible. Do not include credentials or private registry tokens.
