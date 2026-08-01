# Repository instructions

## First-use context

`dalec-homebrew` is an out-of-tree Dalec BuildKit gateway frontend. It converts
Dalec `dependencies.runtime` entries into minimal Linux OCI images assembled
from verified `homebrew/core` bottles.

The V1 contract is intentionally narrow:

- Linux `amd64` and `arm64` only.
- Current stable `homebrew/core` Formulae, installed from bottles only.
- Authenticated Formula metadata and exact digest-bound OCI descriptors.
- Offline materialization onto a snapshot-pinned Ubuntu Chisel runtime base.
- Non-root final images with root-owned, non-writable runtime code.
- No version ranges, casks, third-party taps, source builds, custom runtime
  bases, or networked runtime tests.

Security, reproducibility, and fail-closed behavior are product requirements.
Do not trade them for convenience or silently broaden the supported contract.

## Before editing

1. Read `README.md` for the public contract and development workflow.
2. Read `docs/architecture.md` before changing package boundaries or BuildKit
   orchestration.
3. Read `SECURITY.md` before changing validation, resolution, archive handling,
   materialization, runtime assembly, policy, or release inputs.
4. Read `docs/release.md` before changing pins, component identities, evidence,
   promotion, or rollback behavior.
5. Inspect the nearest implementation, tests, fixtures, and types before
   introducing a new abstraction.

Establish the task's goal, relevant files or failures, constraints, and
observable completion criteria. For complex, ambiguous, or high-risk work,
gather evidence and write a short plan at the level of meaningful deliverables.

Inspect `git status` before editing. Preserve unrelated user changes; never
reset, discard, or rewrite them.

## Architecture map

- `internal/spec`: dependency-order extraction and the supported Dalec contract.
- `internal/homebrew/metadata`: bounded fetches, JWS verification, freshness,
  rollback policy, aliases, renames, and migrations.
- `internal/homebrew/oci`: registry authentication and exact OCI descriptor
  validation.
- `internal/resolver` and `internal/resolution`: deterministic closure
  resolution, canonical replay records, and independent verification.
- `internal/bottle`: compressed bottle and hostile archive verification.
- `internal/materializer`: deterministic offline installation and containment.
- `internal/runtimebase`: pinned Ubuntu snapshot transport and Chisel evidence.
- `internal/runtimefs` and `internal/runtimecheck`: allowlisted runtime assembly,
  ownership/mode normalization, evidence, and static runtime checks.
- `internal/testplan` and `internal/testrunner`: supported Dalec test conversion
  and execution.
- `internal/frontend`: DockerUI orchestration, platform fan-out, image config,
  tests, and exporter epoch handling.
- `internal/policy`, `policy/v1`, `internal/release`, and `release/`: versioned
  runtime policy and release tuple validation.
- `cmd/*`: thin executable entry points; reusable behavior belongs in
  `internal/*`.

`dockerui.Client.Build` may run platform callbacks concurrently. Shared state
must be immutable after publication or synchronized; invocation-local records
must not leak across platforms.

## Hard constraints

`SECURITY.md` is canonical. In particular:

- Reject unsupported or malformed Dalec input before metadata or registry
  access.
- Preserve JWS, freshness, rollback, descriptor-chain, and digest verification.
  The authenticated bottle checksum and selected OCI layer digest must agree.
- Treat bottles as hostile archives. Keep traversal, link escape, special-file,
  setid/capability, collision, and expansion checks fail-closed and bounded.
- The independent resolution verifier and per-bottle verifier complete before
  Homebrew executes.
- Materialization remains offline and isolated: no network, secrets, SSH
  agents, sockets, devices, or shared writable caches.
- Assemble the final image from an explicit inventory onto the clean runtime
  base; never copy the materializer filesystem wholesale.
- Runtime code and its ancestors remain root-owned and non-writable. Only
  explicit, versioned policy exceptions may be writable by the runtime user.
- Do not leak `brew`, Chisel, package managers, repositories, caches, receipts,
  Formula source, or build/test tooling into the final image.
- Runtime tests use the final pruned filesystem, final user, environment, and
  working directory, with networking disabled.
- Deterministic ordering, canonical serialization, immutable digests, and
  controlled timestamps are correctness properties.
- Treat Dockerfile pins, Ubuntu child digests, Homebrew commit, key-set digest,
  policy version, source epoch, and component references as one release tuple.
  Promotion and rollback reuse verified digests; they never re-resolve or
  rebuild an old release.

If a request conflicts with an invariant, identify the conflict and propose the
smallest explicit contract or policy change instead of bypassing a check.

## Implementation conventions

- Use Go `1.25.9`-compatible code and run `gofmt` on changed Go files.
- Match surrounding naming, structure, comments, and idioms. Prefer existing
  helpers and types over parallel implementations.
- Keep command packages thin; place validation, policy, resolution, and build
  behavior in focused `internal/*` packages.
- Wrap errors with useful operation and input context, preserving causes with
  `%w` where callers may inspect them.
- Bound external input, downloads, decompression, archive traversal, generated
  metadata, and in-memory collections.
- Preserve explicit sorting and install order; never rely on Go map iteration.
- Keep OS-specific implementations, fallbacks, build tags, and cross-build
  behavior aligned.
- Shell scripts use Bash strict mode (`set -euo pipefail`) and remain
  non-interactive in CI.
- Add focused regression tests beside changed code. Security-sensitive changes
  need malformed, adversarial, and boundary cases.
- Do not weaken assertions or remove negative tests merely to make a patch pass.
- Update docs and examples when public behavior, supported inputs, evidence
  formats, security properties, or release procedures change.

## Verification

Run the narrowest relevant test while iterating, then expand validation.

```bash
# Focused tests; substitute the affected package path as needed
go test ./internal/resolver

# Canonical repository check: shell syntax, all tests, vet, selected race tests,
# and Linux static builds for amd64 and arm64
./scripts/check.sh

# Local binaries
make build

# Dependency consistency, when imports or modules change
go mod tidy
git diff --exit-code -- go.mod go.sum

# Dockerfile or Bake changes
go test ./internal/buildfiles
docker buildx bake --print release-children frontend

# Final hygiene
git diff --check
git status --short
```

Additional expectations:

- Metadata, OCI, bottle, resolution, materializer, runtime filesystem, and test
  runner changes: run affected packages with `go test -race` where supported.
- Dockerfile or image-composition changes: build the affected Bake target.
- End-to-end materializer or runtime changes: run `./scripts/live-test.sh` with
  a configured BuildKit builder. Component rebuild mode also needs a writable
  registry; published-tuple mode instead needs pull access to all three
  digest-pinned component references. Use the focused specs under `examples/`
  for the behavior changed.
- Published-image hardening changes: run `./scripts/vm-live-validate.sh` against
  the exact image digest when the VM target is available.

Live checks require Docker/BuildKit and registry access. Component rebuild
mode also requires networked component builds; published-tuple mode can reuse
an existing immutable tuple. If live checks cannot be run, report the closest
verification performed and why the live check was skipped. Never claim a check
passed unless it was run.

## Change-specific review

- **Dalec contract:** update `internal/spec`, README contract text, examples, and
  positive and negative tests together.
- **Metadata, registry, or bottles:** review trust boundaries, limits, digest
  binding, rollback behavior, and adversarial fixtures.
- **Resolution:** preserve canonical records, deterministic closure/order,
  immutable replay, and independent verification.
- **Materialization:** preserve offline execution, per-bottle verification,
  prefix containment, expected-keg checks, and receipt normalization.
- **Runtime policy/filesystem:** review ownership, modes, links, writable paths,
  generated-data attribution, inventory, prune evidence, SPDX, and static checks.
- **Pins or releases:** review `Dockerfile`, `docker-bake.hcl`,
  `policy/v1/policy.json`, `release/components.example.json`, and
  `docs/release.md` as a coherent set.
- **Concurrency:** test both platforms and use race detection for shared caches,
  clients, snapshot state, and callback coordination.

## Skills

| Skill | Use for |
| --- | --- |
| `$autoreview` | Review before commit/land on non-trivial code changes. Repeat until no accepted/actionable findings remain. Skip for trivial/docs-only work, equivalent manual review, or when the human opts out. |

## Git and completion

- Keep patches focused; avoid unrelated refactors, formatting churn, or
  dependency updates.
- Do not commit, push, or open a pull request unless requested.
- Commits use Conventional Commit subjects and `git commit -s`.
- Pull request titles use Conventional Commit format. Do not add `[codex]`, and
  do not open a draft pull request unless explicitly requested.
- Completion reports summarize behavior changed, verification actually run,
  and skipped checks, assumptions, or remaining risks.

Keep this root file limited to stable repository-wide knowledge. Put genuinely
specialized rules in a nested `AGENTS.md`; do not accumulate task history,
temporary debugging notes, duplicated documentation, or stale facts.
