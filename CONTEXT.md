# Glossary

This glossary explains the terms used in the README and technical guides. You
do not need to memorize them before completing the quickstart.

## BuildKit

The build engine used by Docker. BuildKit pulls frontend images, executes build
graphs, caches results, and exports the final OCI image.

## Docker Buildx

The Docker command-line plugin that sends builds to BuildKit. In this project,
`docker buildx build` reads a Dalec YAML file instead of a Dockerfile.

## Frontend

A program that teaches BuildKit how to interpret a build file. The upstream
**Dalec frontend** reads the YAML spec and routes the `homebrew` target. The
**dalec-homebrew frontend** resolves, verifies, installs, and assembles the
Homebrew runtime. Frontends run inside BuildKit; users do not install them as
local executables.

## Dalec

[Dalec](https://github.com/project-dalec/dalec) is a declarative build system
for packages and container images. A Dalec spec describes metadata,
dependencies, image configuration, tests, and targets in YAML.

## Dalec spec

The YAML file supplied to BuildKit. For example, `dependencies.runtime` selects
Homebrew Formulae, `image` configures the final container, and `tests` checks
the completed runtime. The top-level spec `version` is optional descriptive
metadata recorded in input evidence; it neither tags the image nor chooses a
Homebrew Formula version.

## Formula

A Homebrew package definition. `hello` is a short name for
`homebrew/core/hello`; `owner/tap/formula` identifies a Formula in a public tap.
An exact versioned name such as `python@3.14` is part of the Formula's name.

## `homebrew/core`

Homebrew's primary public Formula repository. Bare Formula names in a Dalec
spec resolve here.

## Tap

A Homebrew Formula repository outside `homebrew/core`. This project accepts
only the `owner/tap/formula` form and derives the public default GitHub
repository `https://github.com/<owner>/homebrew-<tap>`. Users cannot supply an
arbitrary Git URL or credentials.

## Bottle

A prebuilt Homebrew package archive. A bottle has a Cellar **keg** layout and
embedded Formula metadata that can be verified and poured without compiling
source.

## Keg

The version-specific directory in which Homebrew installs one Formula, usually
under `/home/linuxbrew/.linuxbrew/Cellar/<formula>/<version>`.

## Prebuilt executable archive

A checksummed upstream release archive containing an already-built executable
rather than a Homebrew bottle. It is not a source build and is accepted only
when the release policy authorizes that exact Formula and archive shape.

## Derived bottle

A canonical bottle-shaped artifact produced deterministically from an
authorized prebuilt executable archive. Evidence keeps the upstream archive and
the derived artifact as separate identities; the derived artifact is never
presented as an upstream-published bottle.

## Runtime root and dependency closure

A **root** is a Formula named directly in `dependencies.runtime`. The
**closure** is that root plus every transitive runtime dependency needed for the
selected platform.

## Resolution

The network-enabled phase that authenticates metadata, computes the dependency
closure, selects exact package descriptors, and writes `resolution.json`.
Resolution records digests and ordering so later phases do not consult mutable
package names.

## Materialization

The network-disabled phase that verifies every selected artifact again and
installs the exact resolved closure. The materializer receives read-only inputs
and a private output; its full filesystem is never copied into the final image.

## Runtime base

The clean, minimal Ubuntu Noble filesystem underneath the Homebrew runtime. It
is built with Chisel from snapshot-pinned inputs and does not contain Chisel,
`apt`, or `dpkg` in the final image.

## Runtime minimization

The automatic release-policy step that removes narrowly classified,
development-only files from transitive core dependencies after the complete
closure has been installed. Requested Formulae form the retention boundary.
Users cannot disable or broaden the policy.

## Component tuple

The exact set of frontend, runtime-base, materializer, bottle-fetcher, and
catalog-extractor image digests plus the Homebrew, Ubuntu, toolchain, key, and
policy inputs that make one release. Components from different releases must
not be mixed.

## OCI index and platform child

A multi-platform OCI **index** points to one immutable **child** manifest per
platform. A build uses the `dalec-homebrew` child matching `linux/amd64` or
`linux/arm64`, while separately recording the parent index from the same
release.

## Digest-pinned reference

An OCI image reference ending in an immutable SHA-256 identity, such as
`registry.example/image@sha256:<64 hexadecimal characters>`. A tag such as
`:latest` can move; a digest identifies exact content.

## JWS

JSON Web Signature. Homebrew publishes signed Formula and migration documents;
`dalec-homebrew` verifies their signatures with its pinned public-key set before
using the metadata.

## SBOM

A software bill of materials. Every output image contains an SPDX 2.3 SBOM at
`/usr/share/dalec-homebrew/sbom.spdx.json`.

## Evidence

Machine-readable records describing resolution, installation, runtime files,
minimization, the runtime base, and the SBOM. Embedded evidence helps audit an
image but is not itself an OCI signature; release signatures and attestations
are published separately.

## Artifact policy

Immutable release-bound rules that authorize exact non-standard artifacts and
define their accepted archive shape, payload mapping, executable properties,
and derivation behavior. Build input cannot add or modify artifact policy.
