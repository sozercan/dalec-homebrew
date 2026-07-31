# Release and rollback

The repository provides image recipes, canonical records, and verification tools. A production release pipeline must:

1. Build amd64/arm64 runtime-base children from the pinned Chisel binary, immutable `chisel-releases` commit, and Ubuntu snapshot, with `SOURCE_DATE_EPOCH` fixed.
2. Build materializer children from the corresponding pinned full Ubuntu child images and copy only the matching Chisel runtime-base package/artifact evidence into them.
3. Test and sign every child and immutable multi-platform index.
4. Produce and sign a component manifest binding frontend, base, materializer, Homebrew commit, key-set digest, module versions, and policy version.
5. Build the frontend with the component tuple available and publish it by digest.
6. Resolve once, retain the signed metadata envelopes and resolution records, and mirror every selected layer by digest.
7. Build platform runtime images, test/scan them by manifest digest, then assemble the final index from those exact manifests.
8. Attach signed SPDX, provenance, resolution, inventory, prune, materialization, and vulnerability/VEX evidence.
9. Promote by adding references to existing digests; never rebuild or re-resolve during promotion.

Rollback selects an earlier signed component/index/resolution tuple and its mirrored blobs. It must not reconstruct an old release from current Homebrew metadata.

Release CI must reject `DALEC_SKIP_TESTS`, mutable component references, conflicting `SOURCE_DATE_EPOCH`, changed Chisel/release/snapshot pins without review, unsigned or mixed component tuples, and exporter settings that differ between test and promotion.
