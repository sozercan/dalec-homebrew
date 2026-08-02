# Release and rollback

A release is an immutable tuple of frontend, runtime-base, materializer, Homebrew, verification-key, module, snapshot, Chisel, and policy inputs. Promotion adds references to already tested digests; it never rebuilds or re-resolves them.

## Component tuple

A release frontend must bind digest-pinned runtime-base and materializer images. The frontend itself must also be invoked by digest.

At invocation, the frontend accepts these gateway build options as bindings:

```text
DALEC_HOMEBREW_RUNTIME_BASE
DALEC_HOMEBREW_MATERIALIZER
DALEC_HOMEBREW_FRONTEND_REF
DALEC_HOMEBREW_COMMIT
DALEC_HOMEBREW_KEYS_DIGEST
DALEC_HOMEBREW_RUBY_VERSION
```

These options are not general overrides. Runtime-base, materializer, and Homebrew commit values may fill a binding only when that value was not compiled into the frontend; otherwise a supplied value must match the compiled value. Ruby falls back to `4.0.6` even when no value was compiled, and the key-set digest must match the embedded keys. To test different Ruby or key-set pins, rebuild the frontend with the Dockerfile arguments `HOMEBREW_RUBY_VERSION` and `HOMEBREW_KEYS_DIGEST`; a different key set also requires updating the embedded keys to match that digest.

The frontend cannot bind its own final digest before it is published. `DALEC_HOMEBREW_FRONTEND_REF` therefore normally comes from BuildKit's digest-pinned gateway `source` option and must match it when explicitly supplied. Mutable tags are rejected. The platform child digests returned by BuildKit are recorded in every resolution record.

See [`../release/components.example.json`](../release/components.example.json)
for the canonical manifest shape. The checked-in file contains placeholders
and is illustrative. Release automation generates the manifest with
`cmd/release-manifest`. Validate a populated manifest with:

```console
go run ./cmd/release-verify path/to/components.json
```

## Component build order

Build and publish components in this order:

1. `runtime-base-amd64` and `runtime-base-arm64`
2. platform materializer children derived from the corresponding full Ubuntu children
3. immutable multi-platform runtime-base and materializer indexes
4. the frontend, bound to the released base and materializer identities
5. the completed component manifest, after the frontend index and child digests are known

[`../docker-bake.hcl`](../docker-bake.hcl) exposes the repository build targets. Signing, provenance, vulnerability scanning, OCI referrer publication, mirror retention, and promotion are release-pipeline responsibilities and are deliberately not performed by the frontend.

## Current pinned inputs

The repository currently pins:

- `github.com/project-dalec/dalec v0.21.5-0.20260728234020-5fa2c46d716b`
- `github.com/moby/buildkit v0.31.2`
- Homebrew commit `77d90328ca2f63ff4ec1f67de0ade5632f5d2335`
- portable Ruby `4.0.6`
- Ubuntu Noble platform-child digests in [`../docker-bake.hcl`](../docker-bake.hcl)
- Ubuntu snapshot `20260610T000000Z`
- `SOURCE_DATE_EPOCH=1781049600`
- Chisel `1.4.2` with platform-specific binary SHA-256 values
- `chisel-releases` commit `f42d76490045602d83de8afef5126987179a6693` and its archive SHA-256
- the embedded Homebrew verification-key set and policy version

The exact image and archive digests live beside the build instructions in [`../Dockerfile`](../Dockerfile). These values are release inputs, not update channels. Update, test, review, and sign the complete tuple together.

## Release requirements

An end-to-end production release must satisfy the following requirements. The
repository workflow automates steps 1-5 for a reusable component tuple;
downstream product releases consume that signed tuple and complete steps 6-9.
The example runtimes are integration fixtures and are not promoted.

1. Build `amd64` and `arm64` runtime-base children from the pinned Chisel binary, immutable `chisel-releases` commit, and Ubuntu snapshot with `SOURCE_DATE_EPOCH` fixed.
2. Build materializer children from the corresponding pinned full Ubuntu child images and copy only the matching Chisel runtime-base package and artifact evidence into them.
3. Test and sign the runtime-base and materializer children and immutable multi-platform indexes.
4. Build the frontend with the base, materializer, Homebrew, key-set, module, and policy inputs bound, then publish its platform children and index by digest.
5. Test and sign the frontend children and index, then produce and sign the completed component manifest binding every frontend, base, and materializer digest.
6. Resolve once, retain the signed metadata envelopes and resolution records, and mirror every selected layer by digest.
7. Build platform runtime images, test and scan them by manifest digest, then assemble the final index from those exact manifests.
8. Attach signed SPDX, provenance, resolution, inventory, prune, materialization, base evidence, and vulnerability or VEX evidence.
9. Promote by adding references to existing digests; never rebuild or re-resolve during promotion.

Release CI must reject:

- `DALEC_SKIP_TESTS`
- mutable component references
- conflicting `SOURCE_DATE_EPOCH` values
- changed Chisel, release-definition, snapshot, or Ubuntu pins without review
- unsigned or mixed component tuples
- exporter settings that differ between test and promotion

## Automated component release

[`.github/workflows/release.yml`](../.github/workflows/release.yml) publishes an
existing v-prefixed SemVer tag, including supported pre-releases. Dispatch it
from `main` with a tag whose commit is reachable from `main`; this keeps write
and signing privileges in the trusted workflow rather than the tagged source.

Runs are serialized repository-wide, and an existing published release is
rejected. With no target release, the workflow builds a new tuple and stages a
draft. With an existing draft, it skips rebuilding and resumes only after the
draft's complete signed bundle has been verified. An incomplete or mismatched
draft fails closed and must be removed explicitly before a new build; the
workflow never deletes a draft or replaces release assets.

For a new tuple, the workflow:

1. Runs normal CI and validates the tagged release inputs.
2. Builds and smoke-tests the `amd64` and `arm64` component children and indexes,
   then builds the frontend against the exact runtime-base and materializer
   indexes.
3. Runs every focused integration spec on native `amd64` and `arm64` workers,
   requires one authenticated Homebrew metadata snapshot across every spec and
   platform, and produces deterministic runtime evidence, SPDX SBOMs, and
   vulnerability reports.
4. Generates the component manifest and provenance, signs every component child
   and index with keyless Cosign, attaches the exact SLSA and SPDX predicates,
   and signs the component manifest, metadata snapshot, and checksum set.

Promotion revalidates the tag and signed bundle, creates or resumes the draft,
and verifies its exact asset inventory. Each component version tag must be
absent or already resolve to the tested index digest; the workflow creates only
missing tags, verifies them, rechecks the draft, and then publishes it. It never
rebuilds during promotion or publishes a `latest` tag.

The workflow records the accepted metadata snapshot but does not compare it
with earlier releases. See [`../SECURITY.md`](../SECURITY.md) for the resulting
cross-release anti-rollback limitation.

The GitHub `release` environment gates signing and promotion. Configure required
reviewers, protect release-critical paths with branch rules, and prevent updates
or deletion of `v*.*.*` tags. GHCR and GitHub Release writes use the scoped
`GITHUB_TOKEN`; keyless Cosign uses GitHub Actions OIDC.

Release assets include:

- `components.json`, `components.digest`, and `components.json.bundle`
- `digests.json`, `inputs.json`, `metadata-snapshot.json`, its Sigstore bundle,
  and the SLSA provenance predicate
- per-platform SPDX SBOMs and vulnerability reports
- deterministic runtime evidence archives for `amd64` and `arm64`
- `SHA256SUMS` and `SHA256SUMS.bundle`

The signed component images and their SLSA and SPDX attestations remain in the
registry. The GitHub release contains the records needed to identify and verify
that tuple; it does not publish the example runtime images.

## Rollback

Rollback selects an earlier signed component, index, and resolution tuple together with its mirrored blobs. It must not reconstruct an old release from current Homebrew metadata.
