package oci

import (
	"context"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

func TestAuthenticatedFormulaIdentityAndReference(t *testing.T) {
	t.Parallel()

	id, err := formulaid.Parse("acme/tools/tool@1+preview")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := DefaultGitHubRepository(id.Tap())
	if err != nil {
		t.Fatal(err)
	}
	rootURL, err := HomebrewGHCRRootURL(id.Tap())
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	identity, err := NewAuthenticatedFormulaIdentity(
		id,
		"acme/tools/tool@1+preview",
		repository,
		commit,
		"Formula/tool@1+preview.rb",
	)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID() != id || identity.Tap() != id.Tap() || identity.Name() != "tool@1+preview" {
		t.Fatalf("identity = %#v", identity)
	}
	if identity.Publisher() != "acme" || identity.SourceRepository() != "https://github.com/acme/homebrew-tools" {
		t.Fatalf("publisher/repository = %q / %q", identity.Publisher(), identity.SourceRepository())
	}
	if identity.SourceCommit() != commit || identity.FormulaPath() != "Formula/tool@1+preview.rb" {
		t.Fatalf("source identity = %q / %q", identity.SourceCommit(), identity.FormulaPath())
	}
	if want := repository + "/blob/" + commit + "/Formula/tool@1+preview.rb"; identity.SourceURL() != want {
		t.Fatalf("source URL = %q, want %q", identity.SourceURL(), want)
	}

	formula := AuthenticatedFormula{
		Identity:      identity,
		BottleRootURL: rootURL,
		StableVersion: "2.4.1",
		Revision:      3,
		BottleRebuild: 2,
	}
	reference, err := ResolveAuthenticatedFormulaReference(formula)
	if err != nil {
		t.Fatal(err)
	}
	if reference.Name != "tool@1+preview" || reference.FullName != id.String() {
		t.Fatalf("reference identity = %#v", reference)
	}
	if reference.Repository != "acme/tools/tool/1xpreview" {
		t.Fatalf("repository = %q", reference.Repository)
	}
	if reference.CanonicalRepository != "ghcr.io/acme/tools/tool/1xpreview" {
		t.Fatalf("canonical repository = %q", reference.CanonicalRepository)
	}
	if reference.PkgVersion != "2.4.1_3" || reference.IndexTag != "2.4.1_3-2" {
		t.Fatalf("version routing = %#v", reference)
	}

	repositoryPath, err := RepositoryPathForFormulaID(id)
	if err != nil {
		t.Fatal(err)
	}
	if repositoryPath != reference.Repository {
		t.Fatalf("RepositoryPathForFormulaID() = %q, want %q", repositoryPath, reference.Repository)
	}
	canonical, err := CanonicalRepositoryForFormulaID(id)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != reference.CanonicalRepository {
		t.Fatalf("CanonicalRepositoryForFormulaID() = %q, want %q", canonical, reference.CanonicalRepository)
	}
}

func TestMatchHomebrewGHCRRootFailsClosedOnPrefixEscape(t *testing.T) {
	t.Parallel()

	tap, err := formulaid.ParseTap("acme/tools")
	if err != nil {
		t.Fatal(err)
	}
	matched, err := MatchHomebrewGHCRRoot("https://ghcr.io/v2/acme/tools", tap)
	if err != nil || !matched {
		t.Fatalf("canonical root matched=%v, err=%v", matched, err)
	}
	matched, err = MatchHomebrewGHCRRoot("https://bottles.example.test/acme/tools", tap)
	if err != nil || matched {
		t.Fatalf("non-GHCR root matched=%v, err=%v", matched, err)
	}

	for name, rootURL := range map[string]string{
		"parent traversal":    "https://ghcr.io/v2/acme/tools/../escape",
		"encoded traversal":   "https://ghcr.io/v2/acme/tools/%2e%2e/escape",
		"extra component":     "https://ghcr.io/v2/acme/tools/escape",
		"different tap":       "https://ghcr.io/v2/acme/other",
		"prefix confusion":    "https://ghcr.io/v2/acme/tools-extra",
		"query":               "https://ghcr.io/v2/acme/tools?repository=other",
		"fragment":            "https://ghcr.io/v2/acme/tools#other",
		"userinfo":            "https://user@ghcr.io/v2/acme/tools",
		"scheme downgrade":    "http://ghcr.io/v2/acme/tools",
		"noncanonical host":   "https://GHCR.IO/v2/acme/tools",
		"trailing-dot host":   "https://ghcr.io./v2/acme/tools",
		"explicit port":       "https://ghcr.io:443/v2/acme/tools",
		"double slash prefix": "https://ghcr.io/v2/acme//tools",
	} {
		name, rootURL := name, rootURL
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			matched, err := MatchHomebrewGHCRRoot(rootURL, tap)
			if err == nil || matched {
				t.Fatalf("MatchHomebrewGHCRRoot(%q) = %v, %v; want fail-closed error", rootURL, matched, err)
			}
		})
	}
}

func TestAuthenticatedFormulaIdentityRejectsMismatches(t *testing.T) {
	t.Parallel()

	id, err := formulaid.Parse("acme/tools/widget")
	if err != nil {
		t.Fatal(err)
	}
	validRepository := "https://github.com/acme/homebrew-tools"
	validCommit := strings.Repeat("a", 40)

	for name, test := range map[string]struct {
		fullName   string
		repository string
		commit     string
		path       string
		want       string
	}{
		"full name": {
			fullName: "other/tools/widget", repository: validRepository, commit: validCommit, path: "Formula/widget.rb", want: "does not match Formula ID",
		},
		"source publisher": {
			fullName: id.String(), repository: "https://github.com/other/homebrew-tools", commit: validCommit, path: "Formula/widget.rb", want: "does not match default tap repository",
		},
		"source tap": {
			fullName: id.String(), repository: "https://github.com/acme/homebrew-other", commit: validCommit, path: "Formula/widget.rb", want: "does not match default tap repository",
		},
		"short commit": {
			fullName: id.String(), repository: validRepository, commit: "abc", path: "Formula/widget.rb", want: "40 lowercase hexadecimal",
		},
		"uppercase commit": {
			fullName: id.String(), repository: validRepository, commit: strings.Repeat("A", 40), path: "Formula/widget.rb", want: "40 lowercase hexadecimal",
		},
		"path traversal": {
			fullName: id.String(), repository: validRepository, commit: validCommit, path: "Formula/../widget.rb", want: "not clean and contained",
		},
		"wrong basename": {
			fullName: id.String(), repository: validRepository, commit: validCommit, path: "Formula/other.rb", want: "does not match Formula",
		},
	} {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			identity, err := NewAuthenticatedFormulaIdentity(id, test.fullName, test.repository, test.commit, test.path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewAuthenticatedFormulaIdentity() = %#v, %v; want %q", identity, err, test.want)
			}
			if identity != (AuthenticatedFormulaIdentity{}) {
				t.Fatalf("invalid constructor returned non-zero identity %#v", identity)
			}
		})
	}
}

func TestResolveAuthenticatedFormulaBindsTapIdentityAndReplayDigest(t *testing.T) {
	t.Parallel()

	fixture := newAuthenticatedRegistryFixture(t, authenticatedRegistryFixtureOptions{})
	result, err := fixture.client.ResolveAuthenticated(context.Background(), fixture.formula, fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reference.Repository != "acme/tools/fixture" || result.Reference.CanonicalRepository != "ghcr.io/acme/tools/fixture" {
		t.Fatalf("repository identity = %#v", result.Reference)
	}
	if result.Formula.Identity.ID().String() != "acme/tools/fixture" || result.Reference.FullName != "acme/tools/fixture" {
		t.Fatalf("Formula identity = %#v / %#v", result.Formula.Identity, result.Reference)
	}
	if len(result.Tab.Dependencies) != 1 || result.Tab.Dependencies[0].FullName != "acme/tools/dep" {
		t.Fatalf("tap-aware bottle dependencies = %#v", result.Tab.Dependencies)
	}
	if result.Layer.Digest.String() != "sha256:"+result.HomebrewSHA256 {
		t.Fatalf("layer/checksum chain = %s / %s", result.Layer.Digest, result.HomebrewSHA256)
	}
	if want := result.Reference.CanonicalRepository + "@" + result.Layer.Digest.String(); result.ReplayReference() != want {
		t.Fatalf("replay reference = %q, want %q", result.ReplayReference(), want)
	}
	if strings.Contains(result.ReplayReference(), ":"+result.Reference.IndexTag) {
		t.Fatalf("replay reference unexpectedly uses mutable index tag: %q", result.ReplayReference())
	}
	if fixture.tokenRequests.Load() != 1 || fixture.challengeCount.Load() != 1 {
		t.Fatalf("auth requests = token %d, challenge %d", fixture.tokenRequests.Load(), fixture.challengeCount.Load())
	}

	originalChecksum := result.Formula.BottleFiles[BottleTagX8664Linux].SHA256
	fixture.formula.BottleFiles[BottleTagX8664Linux] = BottleFile{Cellar: "mutated", SHA256: strings.Repeat("f", 64)}
	if result.Formula.BottleFiles[BottleTagX8664Linux].SHA256 != originalChecksum {
		t.Fatal("authenticated result retained caller-owned bottle map")
	}
}

func TestResolveAuthenticatedRejectsIdentityAnnotationMismatch(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		options authenticatedRegistryFixtureOptions
		want    string
	}{
		"publisher": {
			options: authenticatedRegistryFixtureOptions{mutateIndexAnnotations: func(annotations map[string]string) {
				annotations[ocispec.AnnotationVendor] = "other"
			}},
			want: ocispec.AnnotationVendor,
		},
		"source repository": {
			options: authenticatedRegistryFixtureOptions{mutateIndexAnnotations: func(annotations map[string]string) {
				annotations[ocispec.AnnotationSource] = strings.Replace(annotations[ocispec.AnnotationSource], "acme/homebrew-tools", "other/homebrew-tools", 1)
			}},
			want: ocispec.AnnotationSource,
		},
		"exact commit": {
			options: authenticatedRegistryFixtureOptions{mutateIndexAnnotations: func(annotations map[string]string) {
				annotations[ocispec.AnnotationRevision] = strings.Repeat("b", 40)
			}},
			want: ocispec.AnnotationRevision,
		},
		"Formula path": {
			options: authenticatedRegistryFixtureOptions{mutateManifestAnnotations: func(annotations map[string]string) {
				annotations[ocispec.AnnotationSource] = strings.Replace(annotations[ocispec.AnnotationSource], "Formula/fixture.rb", "Formula/other.rb", 1)
			}},
			want: ocispec.AnnotationSource,
		},
		"full name": {
			options: authenticatedRegistryFixtureOptions{mutateIndexAnnotations: func(annotations map[string]string) {
				annotations[ocispec.AnnotationTitle] = "other/tools/fixture"
			}},
			want: ocispec.AnnotationTitle,
		},
	} {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAuthenticatedRegistryFixture(t, test.options)
			_, err := fixture.client.ResolveAuthenticated(context.Background(), fixture.formula, fixture.target)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveAuthenticated() error = %v, want annotation mismatch for %s", err, test.want)
			}
		})
	}
}

func TestResolveAuthenticatedRejectsDescriptorAndChecksumMutation(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		options authenticatedRegistryFixtureOptions
		want    string
	}{
		"manifest descriptor bytes": {
			options: authenticatedRegistryFixtureOptions{mutateManifest: true},
			want:    "does not match descriptor digest",
		},
		"config descriptor bytes": {
			options: authenticatedRegistryFixtureOptions{mutateConfig: true},
			want:    "does not match descriptor digest",
		},
		"authenticated checksum": {
			options: authenticatedRegistryFixtureOptions{authenticatedChecksum: strings.Repeat("b", 64)},
			want:    "does not match authenticated checksum",
		},
		"selected checksum": {
			options: authenticatedRegistryFixtureOptions{mutateChildAnnotations: func(annotations map[string]string) {
				annotations[annotationBottleDigest] = strings.Repeat("b", 64)
			}},
			want: "does not match authenticated checksum",
		},
		"layer checksum": {
			options: authenticatedRegistryFixtureOptions{manifestLayerDigest: digest.FromString("mutated bottle layer")},
			want:    "bottle layer digest",
		},
	} {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAuthenticatedRegistryFixture(t, test.options)
			_, err := ResolveAuthenticated(context.Background(), fixture.client, fixture.formula, fixture.target)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveAuthenticated() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDiscoverAuthenticatedFormulaIdentityFromVerifiedIndex(t *testing.T) {
	fixture := newAuthenticatedRegistryFixture(t, authenticatedRegistryFixtureOptions{})
	formula := fixture.formula
	identity, err := fixture.client.DiscoverAuthenticatedFormulaIdentity(t.Context(), formula.Identity.ID(), formula.Identity.HomebrewFullName(), formula.StableVersion, formula.Revision, formula.BottleRebuild)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID() != formula.Identity.ID() || identity.SourceRepository() != formula.Identity.SourceRepository() || identity.SourceCommit() != formula.Identity.SourceCommit() || identity.FormulaPath() != formula.Identity.FormulaPath() {
		t.Fatalf("discovered identity=%+v want=%+v", identity, formula.Identity)
	}
}
