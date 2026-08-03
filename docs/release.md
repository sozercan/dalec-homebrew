# Release and rollback

A release is an immutable tuple of frontend, runtime-base, materializer,
Homebrew, verification-key, module, builder, snapshot, Chisel, and policy
inputs. Promotion adds references to already tested digests; it never rebuilds
or re-resolves them.

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

`release-verify` decodes and validates the manifest and prints the digest of its
canonical form. It does not require the input bytes to already be canonical,
query a registry, or prove that child descriptors belong to the indexes.
Release CI performs those additional canonical-byte, registry, and index-child
binding checks before signing.

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
- digest-pinned Dockerfile frontend and Go `1.25.9` builder images
- Homebrew commit `77d90328ca2f63ff4ec1f67de0ade5632f5d2335` and source
  archive SHA-256
  `42e3678a8b00d53319f6b88b9384fcc7baa072e44864e41117cc7fd4f78fcb54`
- portable Ruby `4.0.6`
- Ubuntu Noble platform-child digests in [`../docker-bake.hcl`](../docker-bake.hcl)
- Ubuntu snapshot `20260610T000000Z`
- `SOURCE_DATE_EPOCH=1781049600`
- Chisel `1.4.2` with platform-specific binary SHA-256 values
- `chisel-releases` commit `f42d76490045602d83de8afef5126987179a6693` and its archive SHA-256
- verification-key set
  `sha256:ef2d2c9e0219d485df9f07fff7b037feadc36c93085be9ffefb1390f31a3de1d`
- policy version `homebrew-runtime-v1`, summarized in
  [`../policy/v1/policy.json`](../policy/v1/policy.json)

[`../scripts/release-inputs.sh`](../scripts/release-inputs.sh) is the
machine-readable inventory used by CI. It validates the Dockerfile and Bake
defaults and emits the builder images, Ubuntu children, snapshot and epoch,
Chisel and Homebrew source hashes, Ruby and key-set pins, and module versions.
The exact references and hashes live beside the build instructions in
[`../Dockerfile`](../Dockerfile) and
[`../docker-bake.hcl`](../docker-bake.hcl). These values are release inputs, not
update channels. Update, test, review, and sign the complete tuple together.

The BuildKit Go module pin above is distinct from the release executor. The
workflows currently install Buildx `v0.36.0` and start BuildKit `v0.32.0` from a
digest-pinned image. They also pin binfmt, Syft, Trivy, Cosign, and every external
GitHub Action; `provenance.json` records the Buildx, BuildKit, binfmt, Syft, and
Trivy identities used for the release.

## Release requirements

An end-to-end production release must satisfy the following requirements. The
repository workflow automates steps 1-5 for a reusable component tuple;
downstream product releases consume that signed tuple and complete steps 6-9.
The example runtimes are integration fixtures and are not promoted.

1. Build `amd64` and `arm64` runtime-base children from the pinned Chisel binary, immutable `chisel-releases` commit, and Ubuntu snapshot with `SOURCE_DATE_EPOCH` fixed.
2. Build materializer children from the corresponding pinned full Ubuntu child images and copy only the matching Chisel runtime-base package and artifact evidence into them.
3. Smoke-test the runtime-base and materializer children, then assemble and verify their immutable multi-platform indexes.
4. Build the frontend with the base, materializer, Homebrew, key-set, module, and policy inputs bound; publish its platform children and index by digest; generate and verify the completed component manifest; and run every focused runtime spec on native `amd64` and `arm64` workers while producing runtime evidence, component-child SPDX SBOMs, and vulnerability reports.
5. Sign all six component children and three indexes, attach the exact SLSA predicate to every subject and the matching SPDX predicate to each child, then blob-sign the component manifest, accepted metadata snapshot, and checksum set.
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
- any fixed `CRITICAL` vulnerability in a component child; signing and promotion
  both revalidate the digest-bound Trivy reports before proceeding
- exporter settings that differ between test and promotion

## Automated component release

[`.github/workflows/release.yml`](../.github/workflows/release.yml) publishes an
existing v-prefixed, OCI-compatible SemVer tag, including supported
pre-releases. Tags are limited to 128 characters, and numeric pre-release
identifiers cannot have leading zeroes. Build metadata (`+...`) is not accepted
because OCI tags cannot represent it without an additional mapping.

Push a release tag on the current `main` commit to start the pipeline:

```console
git tag -a v1.2.3 -m v1.2.3
git push origin v1.2.3
```

Creating a release tag is a privileged release-operator action. GitHub loads a
tag-push workflow from the tagged commit, so repository access must restrict
creation of `v*.*.*` tags to trusted release operators. The checked-in
[`.github/workflows/release-tag.yml`](../.github/workflows/release-tag.yml) does
not check out source and requests only `actions: write`; it dispatches
`release.yml` explicitly on `main` with both the pushed tag and pushed commit.
Tag protection, rather than the tag-scoped workflow itself, is the authorization
boundary.

A new tuple is built only when the tag names the exact commit that contains the
trusted `main` workflow, so the checked-in path receiving registry write access
remains the reviewed default-branch workflow source. Tags on older or unrelated
commits start a validation run but fail before registry or signing access. A tag
that moves after its push also fails because the workflow binds the original
push commit. A later descendant of `main` may verify and resume an existing
signed draft, provided the current verifier and exact asset contract still
accept the bundle, because recovery never rebuilds the tuple. For an explicit
retry or draft recovery, dispatch the trusted workflow on `main`:

```console
gh workflow run release.yml --ref main -f tag=v1.2.3
```

Existing `repository_dispatch` integrations remain supported with the same
`client_payload.tag` contract.

Runs are serialized repository-wide, and an existing published release is
rejected. With no target release, the workflow builds a new tuple and stages a
draft. With an existing draft, it skips rebuilding and resumes only after the
draft's complete signed bundle has been verified. An incomplete or mismatched
draft fails closed and must be removed explicitly before a new build; the
workflow never deletes a draft or replaces release assets.

A failure before draft creation has no cross-run resume path: a later dispatch
rebuilds the tuple and may leave the unique `build-<sha>-<run>-<attempt>` staging
tags from the failed run. Once the exact signed draft exists, rerunning the
dispatch can resume after a partial promotion; matching component version tags
are accepted and only missing tags are created. The workflow does not clean up
staging tags. Registry retention and cleanup must preserve the digest subjects
and OCI referrers needed to verify or resume the release.

For a new tuple, the workflow:

1. Runs normal CI and validates the tagged release inputs.
2. Builds and smoke-tests the `amd64` and `arm64` runtime-base and materializer
   children, assembles and verifies their indexes, builds the frontend against
   those exact indexes, and generates and verifies the completed component
   manifest.
3. Runs every focused integration spec on native `amd64` and `arm64` workers,
   requires one authenticated Homebrew metadata snapshot across every spec and
   platform, produces deterministic runtime evidence, and separately generates
   SPDX SBOMs and vulnerability reports for each component child.
4. Generates provenance, signs every component child and index with keyless
   Cosign, attaches the exact SLSA predicate to all nine subjects and the
   matching SPDX predicate to the six children, and signs the component
   manifest, metadata snapshot, and checksum set.

Promotion revalidates the tag and signed bundle, creates or resumes the draft,
and verifies its exact asset inventory. Each component version tag must be
absent or already resolve to the tested index digest; the workflow creates only
missing tags, verifies them, rechecks the draft, and then publishes it. It never
rebuilds during promotion or publishes a `latest` tag.

The workflow records the accepted metadata snapshot but does not compare it
with earlier releases. See [`../SECURITY.md`](../SECURITY.md) for the resulting
cross-release anti-rollback limitation.

The GitHub `release` environment gates signing and promotion. Configure required
reviewers, protect release-critical paths with branch rules, restrict release-tag
creation to release operators, and use tag rules to prevent update or deletion.
GHCR and GitHub Release writes use the scoped `GITHUB_TOKEN`; keyless Cosign
uses GitHub Actions OIDC. Push release tags individually.

Release assets include:

- `components.json`, `components.digest`, and `components.json.bundle`
- `digests.json`, `inputs.json`, `metadata-snapshot.json`,
  `metadata-snapshot.json.bundle`, and `provenance.json`
- six per-component, per-platform SPDX SBOMs and six vulnerability reports
- one deterministic `runtime-evidence-<platform>-<spec>.tar.gz` archive for each
  focused spec and platform
- `SHA256SUMS` and `SHA256SUMS.bundle`

The signed checksum set authenticates every non-bundle release asset. The
component manifest and metadata snapshot also have direct Sigstore bundles.
SLSA attestations are attached to all six component children and three indexes;
SPDX attestations are attached to the six children. Vulnerability reports and
runtime evidence archives are checksum-authenticated release assets, not OCI
attestations.

The signed component images and their SLSA and SPDX attestations remain in the
registry. The GitHub release contains the records needed to identify and verify
that tuple; it does not publish the example runtime images.

## Rollback

The repository workflow has no rollback trigger or rollback job. Rollback is an
operator or downstream release-system action that selects an earlier signed
component, index, and resolution tuple together with its mirrored blobs. It
must not reconstruct an old release from current Homebrew metadata.
