package buildfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"docker.io/cloudflare/cloudflared:2026.7.3@sha256:e39ee8da81ad5e05d77f38d2f51c60ca51bf2a8450ac3abab50c17fdb91d91bf",
		"A365_TAP_COMMIT: 9c84216d0c239c0fabf214ab995bf07960727cab",
		"AVTOOLS_TAP_COMMIT: 44ef54859495a94d474483a64308bfafb9f1e4d9",
		"DALEC_HOMEBREW_E2E_SPEC: examples/ci-noncore-multi-package.yaml",
		"DALEC_HOMEBREW_E2E_A365_TAP_COMMIT: ${{ env.A365_TAP_COMMIT }}",
		"DALEC_HOMEBREW_E2E_AVTOOLS_TAP_COMMIT: ${{ env.AVTOOLS_TAP_COMMIT }}",
		"run: ./scripts/noncore-e2e.sh",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("CI workflow is missing %q", want)
		}
	}
	for _, forbidden := range []string{"examples/ci-multi-package.yaml", "Multi-package container E2E\n"} {
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
		"--buildkit-address tcp://catalog-worker:1234",
		"--extractor-ref \"$EXTRACTOR_REF\"",
		"--service-digest \"$SERVICE_DIGEST\"",
		"--extractor-digest \"$EXTRACTOR_DIGEST\"",
		"--tap-commit \"sozercan/repo=$A365_TAP_COMMIT\"",
		"--tap-commit \"svt/avtools=$AVTOOLS_TAP_COMMIT\"",
		"cloudflared did not publish a valid HTTPS catalog origin",
		"trycloudflare",
		"docker run --rm --network none",
		".requested == $id and .id == $id",
		".tap == $tap",
		".pkg_version == \"0.5.6+d375b2d\"",
		".bottle.transport.https.fetch_policy_version == \"homebrew-bottle-fetch-v1\"",
		".signer.algorithm == \"PS512\"",
		"--arg commit \"$AVTOOLS_TAP_COMMIT\"",
		"--arg commit \"$A365_TAP_COMMIT\"",
		".commit == $commit",
		".bottle.prebuilt_derivation.formula_source.transport.tap.commit == $commit",
		".artifact_id == $id",
		".formula_id == $id",
		"A365_ID=sozercan/repo/a365",
		".bottle.prebuilt_derivation.policy_version == \"prebuilt-derived-bottle-v1\"",
		"endswith(\"/bin/a365\")",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("non-core E2E script is missing %q", want)
		}
	}
	if strings.Contains(text, "--fixture") {
		t.Fatal("non-core E2E must use the production BuildKit catalog generator, not fixture mode")
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
		"CI resolves the stable Formulae from exact tap commits pinned in ci.yml.",
		"svt/avtools/libdf: {}",
		"sozercan/repo/a365: {}",
		"hello: {}",
		"dalec-homebrew-resolution/v2",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("non-core E2E spec is missing %q", want)
		}
	}
}
