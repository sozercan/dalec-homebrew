# Release and rollback

A release is an immutable tuple of frontend, runtime-base, materializer, Homebrew, verification-key, module, snapshot, Chisel, and policy inputs. Promotion adds references to already tested digests; it never rebuilds or re-resolves them.

## Component tuple

A release frontend must bind digest-pinned runtime-base and materializer images. The frontend itself must also be invoked by digest.

The corresponding frontend build options are:

```text
DALEC_HOMEBREW_RUNTIME_BASE
DALEC_HOMEBREW_MATERIALIZER
DALEC_HOMEBREW_FRONTEND_REF
DALEC_HOMEBREW_COMMIT
DALEC_HOMEBREW_KEYS_DIGEST
DALEC_HOMEBREW_RUBY_VERSION
```

The runtime-base, materializer, Homebrew, key-set, and Ruby values are normally compiled into the frontend image. They may be supplied as build options for local development, but a supplied value cannot override a release-bound value.

The frontend cannot bind its own final digest before it is published. `DALEC_HOMEBREW_FRONTEND_REF` therefore normally comes from BuildKit's digest-pinned gateway `source` option and must match it when explicitly supplied. Mutable tags are rejected. The platform child digests returned by BuildKit are recorded in every resolution record.

See [`../release/components.example.json`](../release/components.example.json) for the canonical manifest shape. The checked-in file contains placeholders and is illustrative. Validate a populated manifest with:

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

## Release procedure

A production release pipeline must:

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

## Rollback

Rollback selects an earlier signed component, index, and resolution tuple together with its mirrored blobs. It must not reconstruct an old release from current Homebrew metadata.
