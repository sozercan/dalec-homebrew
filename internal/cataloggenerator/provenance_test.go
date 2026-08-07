package cataloggenerator

import (
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func TestProvenanceAnnotationsRequireBoundPair(t *testing.T) {
	url, digest, present, err := provenanceAnnotations(nil, nil)
	if err != nil || present || url != "" || digest != "" {
		t.Fatalf("empty annotations = %q %q %v %v", url, digest, present, err)
	}
	_, _, _, err = provenanceAnnotations(map[string]string{annotationSigstoreBundleURL: "https://example.test/bundle"}, nil)
	if err == nil || !strings.Contains(err.Error(), "URL and digest") {
		t.Fatalf("err=%v", err)
	}
	url, digest, present, err = provenanceAnnotations(map[string]string{annotationSigstoreBundleURL: "https://example.test/bundle", annotationSigstoreBundleDigest: "sha256:" + strings.Repeat("a", 64)}, nil)
	if err != nil || !present || url == "" || digest == "" {
		t.Fatalf("annotations = %q %q %v %v", url, digest, present, err)
	}
}

func TestReleaseSigstoreTrustedRootLoads(t *testing.T) {
	data, err := policyv2.SigstoreTrustedRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.NewTrustedRootFromJSON(data); err != nil {
		t.Fatal(err)
	}
}

func TestSigstoreCertificateIdentityRequiresDefaultBranch(t *testing.T) {
	policy, err := policyv2.LoadTapPolicy()
	if err != nil {
		t.Fatal(err)
	}
	const repository = "https://github.com/acme/homebrew-tools"
	identity, err := sigstoreCertificateIdentity(policy, repository, "main")
	if err != nil {
		t.Fatal(err)
	}
	certificateFor := func(san string) certificate.Summary {
		return certificate.Summary{
			SubjectAlternativeName: san,
			Extensions:             certificate.Extensions{Issuer: policy.SigstoreIssuer},
		}
	}
	accepted := repository + "/.github/workflows/release.yml@refs/heads/main"
	if err := identity.Verify(certificateFor(accepted)); err != nil {
		t.Fatalf("default-branch identity rejected: %v", err)
	}
	for name, san := range map[string]string{
		"feature branch": repository + "/.github/workflows/release.yml@refs/heads/feature",
		"tag":            repository + "/.github/workflows/release.yml@refs/tags/v1.0.0",
		"pull request":   repository + "/.github/workflows/release.yml@refs/pull/1/merge",
		"other repo":     "https://github.com/acme/homebrew-other/.github/workflows/release.yml@refs/heads/main",
	} {
		t.Run(name, func(t *testing.T) {
			if err := identity.Verify(certificateFor(san)); err == nil {
				t.Fatalf("identity accepted %q", san)
			}
		})
	}
}

func TestGitHubDefaultBranchUsesBoundedRepositoryMetadata(t *testing.T) {
	const repository = "https://github.com/acme/homebrew-tools"
	metadataURL, err := githubRepositoryMetadataURL(repository)
	if err != nil {
		t.Fatal(err)
	}
	builder := &ProductionArtifactBuilder{fetcher: &fakeArtifactFetcher{bodies: map[string][]byte{
		metadataURL: []byte(`{"default_branch":"release/stable","ignored":true}`),
	}}}
	branch, err := builder.githubDefaultBranch(t.Context(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if branch != "release/stable" {
		t.Fatalf("default branch = %q", branch)
	}

	builder.fetcher = &fakeArtifactFetcher{bodies: map[string][]byte{
		metadataURL: []byte(`{"default_branch":"main"} {"default_branch":"other"}`),
	}}
	if _, err := builder.githubDefaultBranch(t.Context(), repository); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("trailing metadata error = %v", err)
	}
}

func TestGitHubRepositoryMetadataURLRequiresCanonicalRepository(t *testing.T) {
	want := "https://api.github.com/repos/acme/homebrew-tools"
	got, err := githubRepositoryMetadataURL("https://github.com/acme/homebrew-tools")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("metadata URL = %q, want %q", got, want)
	}
	for _, repository := range []string{
		"http://github.com/acme/homebrew-tools",
		"https://github.com/acme/homebrew-tools/",
		"https://github.com/acme/homebrew-tools?token=secret",
		"https://example.com/acme/homebrew-tools",
	} {
		t.Run(repository, func(t *testing.T) {
			if _, err := githubRepositoryMetadataURL(repository); err == nil {
				t.Fatalf("accepted %q", repository)
			}
		})
	}
}
