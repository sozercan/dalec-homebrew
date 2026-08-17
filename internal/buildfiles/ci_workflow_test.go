package buildfiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/project-dalec/dalec"
)

func TestCIExercisesProductionPathNonCoreV2(t *testing.T) {
	root := repositoryRoot(t)
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, want := range []string{
		"Non-core multi-package container E2E",
		"Validate upstream Dalec forwarding pin",
		"--dalec-frontend-file release/dalec-frontend.json",
		"Build and test upstream-forwarded build-local non-core V2",
		"DALEC_HOMEBREW_E2E_DALEC_FRONTEND_PIN: release/dalec-frontend.json",
		"DALEC_HOMEBREW_E2E_SPEC: examples/ci-noncore-multi-package.yaml",
		"docker buildx bake --print release-children release-frontend frontend",
		"run: ./scripts/noncore-e2e.sh",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("CI workflow is missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"examples/ci-multi-package.yaml",
		"Multi-package container E2E\n",
		"A365_TAP_COMMIT",
		"AVTOOLS_TAP_COMMIT",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("CI workflow retains obsolete core-only E2E marker %q", forbidden)
		}
	}
}

func TestNonCoreE2EUsesProductionCatalogIngestionAndOfflineRuntime(t *testing.T) {
	root := repositoryRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "scripts", "noncore-e2e.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, want := range []string{
		`--dalec-frontend-file "$DALEC_FRONTEND_PIN"`,
		`--base-spec-file "$SPEC"`,
		`--pinned-ref "DALEC_HOMEBREW_E2E_BUILDKIT_IMAGE=$BUILDKIT_IMAGE"`,
		`--pinned-ref "DALEC_HOMEBREW_E2E_REGISTRY_IMAGE=$REGISTRY_IMAGE"`,
		`DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN="$DALEC_FRONTEND_PIN"`,
		"DALEC_HOMEBREW_E2E_DALEC_ROUTE=$DALEC_ROUTE",
		"expect_forwarding_rejection list-only",
		"expect_forwarding_rejection global-map-target-list",
		"expect_forwarding_rejection global-list-target-map",
		"upstream Dalec accepted unsupported dependency shape",
		"global dependencies.runtime must use map form and contain at least one entry",
		"target homebrew dependencies.runtime must use map form and contain at least one entry",
		`--catalog-extractor-ref "$EXTRACTOR_REF"`,
		`--build-arg "CATALOG_EXTRACTOR_REF=$EXTRACTOR_REF"`,
		`--build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$FRONTEND_INDEX_REF"`,
		`go run ./cmd/metadata-bundle`,
		`--digest-output "$WORK/metadata-bundle.digest"`,
		`--build-arg "METADATA_BUNDLE_DIGEST=$METADATA_BUNDLE_DIGEST"`,
		`--build-arg "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$METADATA_BUNDLE_DIGEST"`,
		`--build-context "dalec-homebrew-metadata=$WORK/metadata-bundle"`,
		`DALEC_HOMEBREW_LIVE_FRONTEND_INDEX_REF="$FRONTEND_INDEX_REF"`,
		`DALEC_HOMEBREW_LIVE_METADATA_BUNDLE="$WORK/metadata-bundle"`,
		`.components.frontend_index_ref == $frontend_index`,
		`.components.frontend_ref == $frontend`,
		"docker run --rm --network none",
		".components.catalog_extractor_ref as $extractor",
		".extraction.policy_version == \"build-local-tap-extraction-v1\"",
		".extraction.extractor_ref == $extractor",
		`(.requested | map(.requested)) == [
    "hello",
    "sozercan/repo/a365",
    "svt/avtools/libdf"
  ]`,
		".requested == $id and .id == $id",
		".tap == $tap",
		".bottle.transport.https.fetch_policy_version == \"homebrew-bottle-fetch-v1\"",
		".bottle.transport.local.policy_version == \"build-local-artifact-v1\"",
		".artifact_id == $id",
		".formula_id == $id",
		"A365_ID=sozercan/repo/a365",
		".formula_version | test(\"^[0-9]+[.][0-9]+[.][0-9]+",
		".bottle.prebuilt_derivation.policy_version == \"prebuilt-derived-bottle-v1\"",
		".bottle.prebuilt_derivation.formula_source.transport.tap.commit == $source.commit",
		"$source.commit | test(\"^[0-9a-f]{40}$\")",
		"endswith(\"/bin/a365\")",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("non-core E2E script is missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"--fixture",
		"--tap-commit",
		"DALEC_HOMEBREW_E2E_A365_TAP_COMMIT",
		"DALEC_HOMEBREW_E2E_AVTOOLS_TAP_COMMIT",
		"--arg commit",
		"cloudflared",
		"trycloudflare",
		"CATALOG_SERVICE_ORIGIN",
		"INGESTION_JWS",
		"catalog-worker",
		"--buildkit-address",
		"x-dalec-homebrew",
		"runtime_dependency_order",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("non-core E2E contains forbidden marker %q", forbidden)
		}
	}
	capture := strings.Index(text, "go run ./cmd/metadata-bundle")
	frontend := strings.Index(text, `FRONTEND_REF=$(build_component "V2 frontend"`)
	if capture < 0 || frontend < 0 || capture >= frontend {
		t.Fatalf("non-core E2E must capture the authenticated metadata bundle before building the V2 frontend")
	}
}

func TestNonCoreE2ESpecContainsQualifiedAndCoreRoots(t *testing.T) {
	root := repositoryRoot(t)
	spec, err := os.ReadFile(filepath.Join(root, "examples", "ci-noncore-multi-package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(spec)
	for _, want := range []string{
		"svt/avtools/libdf: {}",
		"sozercan/repo/a365: {}",
		"hello: {}",
		"matches: ['[0-9]+\\.[0-9]+\\.[0-9]+']",
		"dalec-homebrew-resolution/v2",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("non-core E2E spec is missing %q", want)
		}
	}
	const nonCanonicalDeclarationOrder = `    svt/avtools/libdf: {}
    sozercan/repo/a365: {}
    hello: {}`
	if !strings.Contains(text, nonCanonicalDeclarationOrder) {
		t.Fatal("non-core E2E fixture must retain noncanonical YAML declaration order")
	}
	decoded, err := dalec.LoadSpec(spec)
	if err != nil {
		t.Fatalf("decode non-core E2E spec: %v", err)
	}
	a365, ok := decoded.GetPackageDeps("").GetRuntime()["sozercan/repo/a365"]
	if !ok || len(a365.Version) != 0 {
		t.Fatalf("a365 dependency must track the current Formula without a version constraint: %+v", a365)
	}
	if len(decoded.Tests) != 1 || len(decoded.Tests[0].Steps) != 3 {
		t.Fatalf("unexpected E2E test shape: %+v", decoded.Tests)
	}
	versionCheck := decoded.Tests[0].Steps[2]
	if versionCheck.Command != "a365 --version" || len(versionCheck.Stdout.Matches) != 1 || versionCheck.Stdout.Matches[0] != `[0-9]+\.[0-9]+\.[0-9]+` {
		t.Fatalf("a365 version smoke test must use a version-shape regex: %+v", versionCheck)
	}
}

func TestExamplesUseReleaseApprovedDalecFrontend(t *testing.T) {
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "release", "dalec-frontend.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pin struct {
		Index  string `json:"index"`
		Module struct {
			Version string `json:"version"`
		} `json:"module"`
	}
	if err := json.Unmarshal(data, &pin); err != nil {
		t.Fatal(err)
	}
	if pin.Index == "" || pin.Module.Version == "" {
		t.Fatal("release Dalec frontend pin is incomplete")
	}

	matches, err := filepath.Glob(filepath.Join(root, "examples", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no YAML examples found")
	}
	want := "# syntax=" + pin.Index + "\n"
	for _, path := range matches {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(contents), want) {
			t.Errorf("%s must use release-approved upstream Dalec %s (%s)", filepath.Base(path), pin.Module.Version, pin.Index)
		}
	}
}

func TestPublicBuildDocsAuthenticateReleaseInputs(t *testing.T) {
	root := repositoryRoot(t)
	const (
		indexBuildArg        = `--build-arg "DALEC_HOMEBREW_FRONTEND_INDEX_REF=$DALEC_HOMEBREW_INDEX"`
		metadataBuildArg     = `--build-arg "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST"`
		metadataBuildContext = `--build-context "dalec-homebrew-metadata=$DALEC_HOMEBREW_METADATA_BUNDLE"`
		indexReference       = "DALEC_HOMEBREW_FRONTEND_INDEX_REF=ghcr.io/sozercan/dalec-homebrew@sha256:<dalec-homebrew-index-digest>"
		metadataReference    = "DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST=sha256:<metadata-bundle-manifest-digest>"
		metadataContext      = "--build-context dalec-homebrew-metadata=/path/to/verified/metadata-bundle"
		childReference       = "ghcr.io/sozercan/dalec-homebrew@sha256:<dalec-homebrew-child-digest>"
	)

	readDoc := func(relative string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	readme := readDoc("README.md")
	usage := readDoc(filepath.Join("docs", "usage.md"))

	for _, want := range []string{
		"components.json",
		"inputs.json",
		"metadata-bundle-manifest.json",
		"metadata-formula.jws.json",
		"metadata-migrations.jws.json",
		"metadata-bundle.digest",
		"SHA256SUMS.bundle",
		"cosign verify-blob",
		`--certificate-identity-regexp '^https://github\.com/sozercan/dalec-homebrew/\.github/workflows/release\.yml@refs/heads/main$'`,
		"min >= (now - 7 * 24 * 60 * 60)",
		`DALEC_HOMEBREW_METADATA_BUNDLE/manifest.json`,
		`DALEC_HOMEBREW_METADATA_BUNDLE/formula.jws.json`,
		`DALEC_HOMEBREW_METADATA_BUNDLE/formula_tap_migrations.jws.json`,
		`test "$DALEC_HOMEBREW_METADATA_BUNDLE_DIGEST" = "sha256:$manifest_sha256"`,
		`jq -er '.frontend.index'`,
		`.frontend.platforms[]`,
		`DALEC_SYNTAX="$(jq -er '.dalec_frontend.index'`,
		`DALEC_FRONTEND_VERSION="$(jq -er '.dalec_frontend.module.version'`,
		`# syntax=$DALEC_SYNTAX`,
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md authenticated release preparation is missing %q", want)
		}
	}
	for _, want := range []string{
		"../README.md#2-prepare-a-verified-release",
		"Release-approved upstream Dalec digest",
		"Exact `dalec-homebrew` platform child",
		"Matching `dalec-homebrew` parent index",
		"Three-file Homebrew metadata bundle",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("docs/usage.md release-input reference is missing %q", want)
		}
	}

	for relative, text := range map[string]string{"README.md": readme, filepath.Join("docs", "usage.md"): usage} {
		for _, forbidden := range []string{
			"ghcr.io/sozercan/dalec-homebrew:latest",
			"DALEC_SYNTAX=ghcr.io/project-dalec/dalec/frontend:latest",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s must not use mutable frontend assignment %q", relative, forbidden)
			}
		}

		found := 0
		for _, tail := range strings.Split(text, "```console")[1:] {
			block, _, ok := strings.Cut(tail, "```")
			if !ok {
				continue
			}
			for _, command := range []string{"curl -fsSL", "cosign verify-blob", "sha256sum --check", "docker buildx build"} {
				position := strings.Index(block, command)
				if position >= 0 && !strings.Contains(block[:position], "set -euo pipefail") {
					t.Errorf("%s command %q can continue after an authentication or input failure:\n%s", relative, command, block)
				}
			}
			if !strings.Contains(block, "docker buildx build") {
				continue
			}
			found++
			for _, want := range []string{"--target homebrew/image", indexBuildArg, metadataBuildArg, metadataBuildContext} {
				if !strings.Contains(block, want) {
					t.Errorf("%s build command is missing %q:\n%s", relative, want, block)
				}
			}
		}
		if found != 1 {
			t.Errorf("%s has %d docker buildx build commands, want 1", relative, found)
		}
	}

	for _, relative := range []string{filepath.Join("examples", "forwarded-hello.yaml"), filepath.Join("examples", "hello.yaml")} {
		text := readDoc(relative)
		for _, want := range []string{indexReference, metadataReference, metadataContext, childReference} {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing %q", relative, want)
			}
		}
	}
}
