package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/release"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func TestRunWritesCanonicalManifestToStdout(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run(validArgs(), &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	want := canonicalManifest(t)
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("stdout mismatch\n got: %s\nwant: %s", stdout.Bytes(), want)
	}

	manifest, err := release.Decode(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("decode generated manifest: %v", err)
	}
	for name, component := range map[string]release.Component{
		"frontend":     manifest.Frontend,
		"runtime base": manifest.RuntimeBase,
		"materializer": manifest.Materializer,
	} {
		if got := component.Platforms[0].Platform.Architecture; got != "amd64" {
			t.Errorf("%s first platform = %q, want amd64", name, got)
		}
		if got := component.Platforms[1].Platform.Architecture; got != "arm64" {
			t.Errorf("%s second platform = %q, want arm64", name, got)
		}
	}
}

func TestRunWritesCanonicalV2ManifestToStdout(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run(validV2Args(t), &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	want := canonicalV2Manifest(t)
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("stdout mismatch\n got: %s\nwant: %s", stdout.Bytes(), want)
	}
	manifest, err := release.Decode(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("decode generated V2 manifest: %v", err)
	}
	if !manifest.SupportsNonCoreTaps() || manifest.BottleFetcher == nil || manifest.CatalogExtractor == nil {
		t.Fatalf("generated manifest is not a complete service-free V2 tuple: %+v", manifest)
	}
	if manifest.CatalogServiceOrigin != "" || manifest.IngestionJWSKeyPolicyDigest != "" {
		t.Fatalf("generated V2 manifest retained hosted-service bindings: %+v", manifest)
	}
}

func TestRunWritesCanonicalManifestToFile(t *testing.T) {
	output := filepath.Join(t.TempDir(), "components.json")
	args := append(validArgs(), "--output", output)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := canonicalManifest(t)
	if !bytes.Equal(got, want) {
		t.Fatalf("file mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestRunRejectsV2InputsWithoutV2Schema(t *testing.T) {
	args := append(validArgs(), "--bottle-fetcher-index", pinned("ghcr.io/example/bottle-fetcher", 'a'))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "require --schema-version=v2") {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunRejectsIncompleteV2Inputs(t *testing.T) {
	args := withoutFlag(validV2Args(t), "--catalog-extractor-index")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "catalog_extractor index") {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunRequiresMetadataBundleDigestForNewV2Manifest(t *testing.T) {
	args := withoutFlag(validV2Args(t), "--metadata-bundle-digest")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--metadata-bundle-digest is required") {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunRejectsUnsupportedSchemaVersion(t *testing.T) {
	args := append(validArgs(), "--schema-version", "v3")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unsupported --schema-version") {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunRejectsEmptyOutputPath(t *testing.T) {
	args := append(validArgs(), "--output=")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `write output ""`) {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunRejectsMissingManifestInput(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(withoutFlag(validArgs(), "--frontend-index"), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "frontend index: digest-pinned reference is required") {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunCanonicalizesBeforeOpeningOutput(t *testing.T) {
	output := filepath.Join(t.TempDir(), "components.json")
	const sentinel = "existing release manifest"
	if err := os.WriteFile(output, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	args := setFlag(validArgs(), "--materializer-arm64", pinned("ghcr.io/example/other", '9'))
	args = append(args, "--output", output)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "materializer linux/arm64 child uses a different repository") {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	got, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != sentinel {
		t.Fatalf("invalid manifest changed output to %q", got)
	}
}

func validArgs() []string {
	return []string{
		"--frontend-index", pinned("ghcr.io/example/frontend", '1'),
		"--frontend-amd64", pinned("ghcr.io/example/frontend", '2'),
		"--frontend-arm64", pinned("ghcr.io/example/frontend", '3'),
		"--runtime-base-index", pinned("ghcr.io/example/runtime-base", '4'),
		"--runtime-base-amd64", pinned("ghcr.io/example/runtime-base", '5'),
		"--runtime-base-arm64", pinned("ghcr.io/example/runtime-base", '6'),
		"--materializer-index", pinned("ghcr.io/example/materializer", '7'),
		"--materializer-amd64", pinned("ghcr.io/example/materializer", '8'),
		"--materializer-arm64", pinned("ghcr.io/example/materializer", '9'),
		"--homebrew-commit", strings.Repeat("b", 40),
		"--portable-ruby-version", "4.0.6",
		"--verification-keys-digest", "sha256:" + strings.Repeat("a", 64),
		"--dalec-module", "v0.21.5",
		"--buildkit-module", "v0.31.2",
	}
}

func validV2Args(t *testing.T) []string {
	t.Helper()
	tapDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDigest, err := policyv2.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return append(validArgs(),
		"--schema-version", "v2",
		"--metadata-bundle-digest", "sha256:"+strings.Repeat("0", 64),
		"--bottle-fetcher-index", pinned("ghcr.io/example/bottle-fetcher", 'a'),
		"--bottle-fetcher-amd64", pinned("ghcr.io/example/bottle-fetcher", 'b'),
		"--bottle-fetcher-arm64", pinned("ghcr.io/example/bottle-fetcher", 'c'),
		"--catalog-extractor-index", pinned("ghcr.io/example/catalog-extractor", 'd'),
		"--catalog-extractor-amd64", pinned("ghcr.io/example/catalog-extractor", 'e'),
		"--catalog-extractor-arm64", pinned("ghcr.io/example/catalog-extractor", 'f'),
		"--tap-policy-digest", tapDigest,
		"--executable-runtime-policy-digest", runtimeDigest,
		"--supported-catalog-policy-versions", release.CatalogPolicyVersionV1,
		"--supported-fetch-policy-versions", release.BottleFetchPolicyVersionV1,
		"--supported-provenance-policy-versions", strings.Join([]string{
			release.ChecksumWaiverPolicyVersionV1,
			release.CoreWaiverPolicyVersionV1,
			release.HTTPSSourceWaiverPolicyVersionV1,
			release.PrebuiltWaiverPolicyVersionV1,
			release.SigstoreProvenancePolicyVersionV1,
		}, ","),
	)
}

func canonicalV2Manifest(t *testing.T) []byte {
	t.Helper()
	tapDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDigest, err := policyv2.Digest()
	if err != nil {
		t.Fatal(err)
	}
	fetcher := testComponent("ghcr.io/example/bottle-fetcher", 'a', 'b', 'c')
	extractor := testComponent("ghcr.io/example/catalog-extractor", 'd', 'e', 'f')
	manifest := &release.Manifest{
		SchemaVersion:                  release.SchemaVersionV2,
		PolicyVersion:                  release.RuntimePolicyVersionV2,
		Frontend:                       testComponent("ghcr.io/example/frontend", '1', '2', '3'),
		RuntimeBase:                    testComponent("ghcr.io/example/runtime-base", '4', '5', '6'),
		Materializer:                   testComponent("ghcr.io/example/materializer", '7', '8', '9'),
		BottleFetcher:                  &fetcher,
		CatalogExtractor:               &extractor,
		HomebrewCommit:                 strings.Repeat("b", 40),
		PortableRubyVersion:            "4.0.6",
		VerificationKeysDigest:         "sha256:" + strings.Repeat("a", 64),
		MetadataBundleDigest:           "sha256:" + strings.Repeat("0", 64),
		DalecModule:                    "v0.21.5",
		BuildKitModule:                 "v0.31.2",
		TapPolicyDigest:                tapDigest,
		ExecutableRuntimePolicyDigest:  runtimeDigest,
		SupportedCatalogPolicyVersions: []string{release.CatalogPolicyVersionV1},
		SupportedFetchPolicyVersions:   []string{release.BottleFetchPolicyVersionV1},
		SupportedProvenancePolicyVersions: []string{
			release.ChecksumWaiverPolicyVersionV1,
			release.CoreWaiverPolicyVersionV1,
			release.HTTPSSourceWaiverPolicyVersionV1,
			release.PrebuiltWaiverPolicyVersionV1,
			release.SigstoreProvenancePolicyVersionV1,
		},
	}
	data, err := release.Canonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func canonicalManifest(t *testing.T) []byte {
	t.Helper()
	manifest := &release.Manifest{
		SchemaVersion:          release.SchemaVersion,
		PolicyVersion:          resolution.PolicyVersion,
		Frontend:               testComponent("ghcr.io/example/frontend", '1', '2', '3'),
		RuntimeBase:            testComponent("ghcr.io/example/runtime-base", '4', '5', '6'),
		Materializer:           testComponent("ghcr.io/example/materializer", '7', '8', '9'),
		HomebrewCommit:         strings.Repeat("b", 40),
		PortableRubyVersion:    "4.0.6",
		VerificationKeysDigest: "sha256:" + strings.Repeat("a", 64),
		DalecModule:            "v0.21.5",
		BuildKitModule:         "v0.31.2",
	}
	data, err := release.Canonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testComponent(repo string, index, amd64, arm64 rune) release.Component {
	return release.Component{
		Index: pinned(repo, index),
		Platforms: []release.PlatformRef{
			{Platform: resolution.Platform{OS: "linux", Architecture: "amd64"}, Ref: pinned(repo, amd64)},
			{Platform: resolution.Platform{OS: "linux", Architecture: "arm64"}, Ref: pinned(repo, arm64)},
		},
	}
}

func pinned(repo string, fill rune) string {
	return repo + "@sha256:" + strings.Repeat(string(fill), 64)
}

func withoutFlag(args []string, name string) []string {
	for i := 0; i < len(args); i += 2 {
		if args[i] == name {
			return append(append([]string(nil), args[:i]...), args[i+2:]...)
		}
	}
	return append([]string(nil), args...)
}

func setFlag(args []string, name, value string) []string {
	args = append([]string(nil), args...)
	for i := 0; i < len(args); i += 2 {
		if args[i] == name {
			args[i+1] = value
			return args
		}
	}
	return args
}
