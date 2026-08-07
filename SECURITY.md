# Security policy

## Security properties

The V1 implementation is expected to preserve these properties:

1. Unsupported Dalec inputs, malformed Formula names, and invalid frontend-routing metadata fail before metadata or registry access.
2. Formula identity and bottle checksums come from a verified Homebrew JWS payload and a pinned public-key set.
3. Registry tags are discovery inputs only. Resolution records bind the fetched index descriptor and the selected manifest, config, and layer descriptor identities, sizes, media types, platforms, and selected annotations.
4. The compressed bottle digest must equal both the selected OCI layer digest and the authenticated Homebrew checksum.
5. Bottle archives are scanned before installation and cannot contain traversal, special files, setid bits, ACL or sparse metadata, security, trusted, or capability xattrs, collisions, or unbounded expansion. Bounded `user.*` xattrs may be recorded in verification inventory. Hardlinks remain keg-local. Symlinks remain keg-local except for relative links under the owner keg's `libexec/` tree that use prefix-contained `opt/<signed-direct-dependency>/...` targets using only canonical leading traversal to the prefix and no dot segments after the dependency root. The installed link must retain the exact verified archive target text and resolve inside the exact dependency keg before and after installation; every other escaping link is rejected.
6. Within a frontend build, the internally generated resolution record is mounted read-only, its independent structural verifier completes, and every bottle in the closure verifies before Homebrew executes. Persisted records are not self-authenticating: the verifier does not contain the source JWS envelopes or OCI document bodies, so replayed records must be authenticated by signed release evidence.
7. The materialization LLB exec uses `NetModeNone`, read-only resolution and bottle mounts, and a private scratch output. It declares no secret, SSH, shared-cache, socket, or device mounts.
8. Homebrew is invoked only with private verified local bottles and the exact verified Formula closure staged in an empty, root-owned, read-only tap. Dependency selection and source fallback are disabled, and every resulting prefix mutation is checked. Unexpected kegs and source-built receipts fail.
9. Only allowlist-selected installed-prefix content and explicit generated evidence files are copied from materializer output into the clean runtime base.
10. The Homebrew runtime overlay, code, links, libraries, plugins, and protected prefix ancestors are root-owned and non-writable. Within the Homebrew prefix, only policy-listed `var/<canonical-formula>` subtrees are made writable by the runtime identity.
11. Each declared runtime test runs on an independent branch derived from the final pruned state, with only an ephemeral runner and plan added for the test. Commands use the final non-root user and inherit the final environment and working directory before supported test-level overrides; networking is disabled. Release-bound frontends cannot set `DALEC_SKIP_TESTS`, although development frontends can.
12. The Noble runtime base is cut from a fixed Ubuntu snapshot with a SHA-256-pinned Chisel binary and a checksummed, commit-pinned `chisel-releases` archive. Repository-local slice overrides are bound by source and component digests rather than separate per-file checksums. The build-only proxy accepts only Ubuntu archive hosts, while Chisel remains responsible for signed Release and package-digest verification; neither Chisel nor the proxy reaches the final image.
13. Generated shared runtime data is accepted only at versioned paths with package and capability checks, bounded structure and size, authenticated runtime ownership, and explicit evidence attribution. Node's global `lib/node_modules/npm` runtime is accepted only as a bounded, exact copy of the verified private npm tree plus one exact prefix-bound `npmrc`, with command and manpage links bound back to that validated tree. Unrelated global `lib` or shared-data mutations continue to fail closed.
14. Creating a `v*.*.*` release tag is a trusted release-operator action. Repository access must restrict tag creation to those operators, and tag rules should prevent update or deletion. The checked-in tag workflow rejects tag updates and deletions, dispatches only the privileged release workflow on `main`, and binds the initially pushed commit; a new build requires that commit to equal the trusted workflow commit before any registry or signing job runs.

Production builds require the upstream Dalec target to be
exactly `homebrew`, the child route to be exactly `image`, an empty frontend
`cmdline`, and exact equality between `targets.homebrew.frontend.image` and the
child gateway `source`. The source and all release/provider references must be
SHA-256 digest-pinned. Target frontend metadata is accepted only as routing
metadata for this exact chain; nested forwarding remains rejected.

The forwarded child does **not** receive an authenticated identity for the
upstream Dalec parent. In the child solve, `source` identifies
`dalec-homebrew`; `dalec.target` and any custom parent-reference option are
caller-controlled routing data, not provenance. Releases therefore bind the
upstream index, exact platform children, module identity, and fixed route in
[`release/dalec-frontend.json`](release/dalec-frontend.json), validate the
index-to-child chain externally, and include the upstream index in signed
top-level provenance. Resolution records must not represent a claimed parent as
child-authenticated evidence.

V2 public-tap releases additionally preserve these properties:

15. Prebuilt executable archives are accepted only for exact Formula IDs in the embedded tap policy. The policy binds the Formula source digest, version, platform URLs and checksums, complete archive inventory, payload mapping, archive limits, static ELF properties, Go module identity, and CGO setting; Dalec input cannot provide or override any of those values.
16. Build-local ingestion never runs a prebuilt Formula's `install` or `post_install` method. It verifies the upstream archive, derives a canonical receiptless bottle containing only the selected executable and authenticated Formula source, and binds the upstream and derived identities separately. Native bottles take precedence.
17. Derived bottles pass the same hostile-bottle verification, offline per-package installation, receipt normalization, prefix-delta containment, runtime allowlisting, pruning, and SBOM attribution as upstream bottles. The explicit prebuilt derivation evidence prevents the build-locally generated artifact from being represented as an upstream-published bottle.

## Upstream trust limitations

Homebrew's Formula and migration JWS documents are fetched and authenticated separately; upstream does not sign a common snapshot identifier for the pair. The combined snapshot digest commits to the exact accepted payload pair, but does not prove atomic upstream publication.

The documents do not always include an authenticated generation timestamp. When a signed `generated_date` is absent, freshness relies on the unsigned HTTP `Last-Modified` value. Both documents are freshness-checked independently, and the resolution `generated_at` and `source_date_epoch` use the earlier accepted timestamp.

Callers may supply a metadata rollback floor, but the repository release workflow does not persist one across releases. For release-bound frontends, it limits metadata age to seven days and future skew to 15 minutes, requires every release integration to use the same authenticated snapshot, and records that snapshot in signed evidence. This is not cross-release anti-rollback: a previously superseded but still-fresh signed snapshot may be accepted by a later release.

The Formula JWS authenticates the compressed bottle checksum, but it does not bind OCI index annotations such as `sh.brew.tab` or `sh.brew.path_exec_files`. This implementation:

1. treats signed Formula declarations as package identity authority,
2. verifies the complete fetched OCI descriptor chain by digest and size,
3. requires the selected layer digest to equal the signed Homebrew checksum,
4. treats bottle-tab dependencies as minimum and consistency evidence and uses `changed_files` and executable-path metadata only as bounded reconciliation and runtime-scope hints, and
5. records the fixed V1 upstream-attestation waiver. A stronger upstream attestation policy is not currently configured and would require an explicit policy and component change.

Current Homebrew bottle tarballs generally do not contain `INSTALL_RECEIPT.json`; Homebrew creates it while pouring the bottle. The archive verifier can require a pre-install receipt for fixtures or alternate producers, while production verifies the generated receipt after offline installation. For legacy receipt dependency entries, only an omitted `pkg_version` is derived from `version` and `revision`; explicit empty, null, non-string, or inconsistent dependency values fail. A top-level receipt `pkg_version` may be absent, but when present it must match the resolved node, and source version, version scheme, and closure membership are checked independently.

Modern Homebrew forbids local bottle paths by default, and its public `brew install` command performs mutable tap and prefix preflight. After all bottles verify, the materializer stages their exact embedded Formula sources into a sealed local tap and invokes the pinned Homebrew `FormulaInstaller` through a minimal read-only Ruby adapter. Resolution, dependency selection, network access, and source fallback remain disabled; normal Homebrew extraction, relocation, linking, `etc` and `var` handling, and Formula post-install hooks still run.

See [`docs/architecture.md`](docs/architecture.md) for the complete resolution, materialization, and runtime-base flow.

## Out of scope and external controls

The frontend does not hold signing credentials and cannot itself guarantee registry retention, CI builder identity, parent-frontend identity, vulnerability database freshness, VEX approval, or immutable rollback mirrors. Release automation must authenticate the external upstream Dalec binding, sign the repository-owned frontend, base, and materializer tuple; exact platform images; resolution and evidence artifacts; and provenance, then promote by digest without rebuilding.

Reports that demonstrate a practical violation of the properties above are in scope even when the root cause is an upstream metadata-format limitation.

## Reporting

Please report vulnerabilities privately to the repository maintainers. Include a minimal Dalec spec or resolution record, the affected platform and component digests, and a reproduction where possible. Do not include credentials or private registry tokens.

## V2 non-core tap properties

A release may accept `owner/tap/formula` only when its frontend binary contains the complete V2 capability tuple: bottle-fetcher and catalog-extractor references, tap-policy digest, executable runtime-policy digest, and the exact supported catalog/fetch/provenance policy versions. Invocation build arguments cannot upgrade a core-only frontend.

V2 additionally enforces:

1. Formula graph identity is always `owner/tap/formula`; Cellar rack names are separate and collisions fail closed.
2. BuildKit fetches only the derived public default-GitHub repository. Formula evaluation runs in a release-pinned extractor exec with networking disabled, read-only source mounts, no secrets or SSH, and disposable writable state.
3. Extraction evidence binds the extractor reference, repository, exact commit, tree/archive digests, and canonical catalog digest. Catalog documents remain bounded to 64 MiB each and 256 MiB aggregate with duplicate-member rejection.
4. Build-local ingestion has no centralized monotonic rollback database. Records explicitly use `build-local-exact-commit-no-cross-build-rollback-v1`; retained releases must reuse exact catalog and bottle bytes rather than re-resolve mutable branches.
5. Non-core GHCR bottles repeat the full descriptor and annotation validation. Other bottles pass through the release-bound HTTPS fetcher; private, authenticated, IP-literal, non-public, downgraded, oversized, or unapproved-redirect sources are rejected.
6. Policy-authorized prebuilt archives are verified and transformed into deterministic receiptless bottles during the gateway build. Their bytes use `build-local-artifact-v1` transport and are passed directly to materialization; replay requires retained bytes matching the recorded size and digest.
7. Preparation re-verifies every bottle before Homebrew executes. Bottle-embedded Formula source is staged under its exact synthetic Tap identity; current catalog source and embedded bottle source remain distinct evidence.
8. One network-disabled exec installs each bottle. Protected tap trees and the exact Formula trust store cannot be replaced or modified by the runtime user.
9. Core-only generated-runtime and post-install capabilities are keyed by full Formula ID. A non-core Formula that reuses a core rack name receives no core-specific exception.
10. V1 records remain immutable and V1-only materializers continue to reject V2. Verification tooling decodes both schemas explicitly.
