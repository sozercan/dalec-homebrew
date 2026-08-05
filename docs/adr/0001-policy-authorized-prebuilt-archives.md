# ADR 0001: Policy-authorized prebuilt executable archives

- Status: Accepted
- Date: 2026-08-05

## Context

Some public Homebrew Formulae distribute platform-specific, checksummed executable archives but do not publish Homebrew bottles. `sozercan/repo/a365` is the initial case: its Linux release archive contains an already-built `a365` executable, while the Formula installs that executable into `bin`.

Treating such an archive as a bottle would weaken the bottle layout and embedded-Formula checks. Running the Formula through Homebrew's source-install path would broaden the product to arbitrary source builds and execute unrestricted installation logic.

## Decision

Support prebuilt executable archives only through an exact, release-bound Formula-ID policy.

For an authorized Formula, the catalog service:

1. Authenticates the exact tap commit and platform-specific stable archive metadata.
2. Fetches the archive with the bounded HTTPS policy.
3. Verifies the complete archive inventory and executable properties against the embedded policy.
4. Produces a deterministic receiptless derived bottle containing only the policy-selected payload and the exact authenticated Formula source.
5. Records the upstream archive, verification, derivation recipe, and derived-bottle identities separately in signed evidence.

The frontend fetches only the signed derived bottle, and the materializer uses the existing network-disabled bottle installation and containment path. The Formula's `install` and `post_install` methods are not executed for derived bottles.

Native Homebrew bottles always take precedence. Missing policy, changed Formula source, changed archive identity, unsupported archive shape, or failed executable inspection fails closed. Users cannot provide URLs, mappings, keys, or policy overrides.

## Rejected alternatives

### Relax bottle verification

Rejected because a flat release archive has no Cellar layout, embedded Formula, or bottle receipt semantics.

### Run Homebrew source installation

Rejected because it invokes arbitrary Formula installation logic, build tooling, and source fallback behavior outside the supported contract.

### Install files directly into a synthetic keg

Rejected for the first implementation because it would duplicate Homebrew linking and receipt behavior and add parallel runtime inventory, pruning, and SBOM paths.

### Treat the derived bottle as upstream provenance

Rejected. Evidence must distinguish the original upstream archive from the service-derived bottle and bind both digests.

## Consequences

- Support remains limited to exact Formula IDs embedded in a signed release policy.
- Formula or release changes require a new reviewed policy binding.
- Catalog and resolution evidence gains an explicit prebuilt-derivation record.
- The existing hostile-bottle verifier and offline per-package materializer remain authoritative for installation.
- General Homebrew source builds and user-defined archive recipes remain unsupported.
