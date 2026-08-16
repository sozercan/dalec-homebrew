# Examples

The files in this directory serve two different purposes. Choose the right
category before copying one.

## Start here

For a first build, use the [README quickstart](../README.md#quickstart). It
authenticates the newest fresh release, selects the correct platform child, and
generates a complete `hello.yaml` without digest placeholders.

These two files show the completed Dalec shape but still contain
`dalec-homebrew` digest placeholders:

- [`hello.yaml`](hello.yaml) includes a small runtime test.
- [`forwarded-hello.yaml`](forwarded-hello.yaml) is the smallest forwarded
  runtime without tests.

Fill every placeholder from the same verified release's `components.json` and
metadata assets. Do not replace a child digest with a tag or invoke the child
frontend directly.

## Integration fixtures

The following files are inputs to [`scripts/live-test.sh`](../scripts/live-test.sh).
They intentionally omit `targets.homebrew.frontend.image`; the helper validates
the fixture, pins upstream Dalec, injects the matching child, and supplies the
component tuple:

| File | Runtime behavior exercised |
| --- | --- |
| [`live-test.yaml`](live-test.yaml) | Basic `hello` and `jq` commands |
| [`live-curl.yaml`](live-curl.yaml) | curl and its transitive libpsl helper |
| [`live-python-curl.yaml`](live-python-curl.yaml) | Python plus curl |
| [`live-hf-curl.yaml`](live-hf-curl.yaml) | Hugging Face CLI, curl, CA links, and runtime minimization |
| [`live-python.yaml`](live-python.yaml) | Python extensions, TLS, SQLite, compression, and time zones |
| [`live-glibc.yaml`](live-glibc.yaml) | Brewed loader, locales, and conversion modules |
| [`live-redis.yaml`](live-redis.yaml) | Writable state and a non-root Redis lifecycle |
| [`live-graphviz.yaml`](live-graphviz.yaml) | Plugins, fonts, and generated indexes |
| [`live-toolchain.yaml`](live-toolchain.yaml) | A multi-tool CLI and language runtime closure |

[`ci-noncore-multi-package.yaml`](ci-noncore-multi-package.yaml) is a CI fixture
for the build-local public-tap path and is not a general-purpose sample.

## About `version` and `license`

Top-level metadata belongs to the Dalec fixture. This runtime-only frontend
commits it to input evidence but does not use it to select Homebrew versions,
tag the output image, or replace the per-package license data in the generated
SBOM. A dependency such as `python@3.14` is an exact Formula name; arbitrary
historical versions and version ranges remain unsupported.
