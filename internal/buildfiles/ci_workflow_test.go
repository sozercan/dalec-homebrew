package buildfiles

import (
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
		`--catalog-extractor-ref "$EXTRACTOR_REF"`,
		`--build-arg "CATALOG_EXTRACTOR_REF=$EXTRACTOR_REF"`,
		"docker run --rm --network none",
		".components.catalog_extractor_ref as $extractor",
		".extraction.policy_version == \"build-local-tap-extraction-v1\"",
		".extraction.extractor_ref == $extractor",
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
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("non-core E2E contains forbidden release/CI tap control %q", forbidden)
		}
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
