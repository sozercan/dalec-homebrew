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
		"docker.io/library/nginx:1.29.1-alpine@sha256:42a516af16b852e33b7682d5ef8acbd5d13fe08fecadc7ed98605ba5e3b26ab8",
		"DALEC_HOMEBREW_E2E_SPEC: examples/ci-noncore-multi-package.yaml",
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
		"frontend-test-ca",
		"FRONTEND_TEST_CA_CERTIFICATE_BASE64",
		"docker run --rm --network none",
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
		"svt/avtools/libdf: {}",
		"svt/avtools/libsrf-proxy-filter: {}",
		"hello: {}",
		"dalec-homebrew-resolution/v2",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("non-core E2E spec is missing %q", want)
		}
	}
}
