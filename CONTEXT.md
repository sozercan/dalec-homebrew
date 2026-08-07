# Domain glossary

## Bottle

A Homebrew package archive with a Cellar keg layout and embedded Formula metadata that can be verified and poured without building source.

## Prebuilt executable archive

A checksummed upstream release archive containing already-built executable payloads rather than a Homebrew bottle. It is not a source build and is unsupported unless an exact Formula ID is authorized by the release policy.

## Derived bottle

A canonical bottle-shaped artifact deterministically produced from a policy-authorized prebuilt executable archive. Its evidence retains the identity and digest of both the upstream archive and the derived artifact; it is never represented as an upstream-published bottle.

## Artifact policy

Release-bound rules that authorize an exact Formula ID and define the only accepted archive shape, payload mapping, executable properties, and derivation behavior. Build input cannot add or modify artifact policy.
