package cataloggenerator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	sigbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/proto"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func (b *ProductionArtifactBuilder) provenanceForHTTPS(ctx context.Context, bottleURL, sourceRepository string, artifactDigest digest.Digest) (catalog.Provenance, error) {
	parsed, err := url.Parse(bottleURL)
	if err != nil {
		return catalog.Provenance{}, err
	}
	parsed.Path += ".sigstore.json"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	probe, found, err := b.fetcher.ProbeOptional(ctx, parsed.String())
	if err != nil {
		return catalog.Provenance{}, fmt.Errorf("probe HTTPS Sigstore provenance: %w", err)
	}
	if !found {
		return catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: catalog.ChecksumProvenanceWaiver}}, nil
	}
	if probe.Size > maxSigstoreBundleBytes {
		return catalog.Provenance{}, fmt.Errorf("Sigstore bundle size %d exceeds %d", probe.Size, maxSigstoreBundleBytes)
	}
	var payload bytes.Buffer
	evidence, err := b.fetcher.FetchObserved(ctx, parsed.String(), probe.Size, path.Base(parsed.Path), uniqueSorted(probe.RedirectHostSequence), &payload)
	if err != nil {
		return catalog.Provenance{}, err
	}
	verified, err := b.verifySigstoreBundleForRepository(ctx, payload.Bytes(), sourceRepository, artifactDigest)
	if err != nil {
		return catalog.Provenance{}, err
	}
	verified.BundleDigest = "sha256:" + evidence.SHA256
	return catalog.Provenance{Verified: verified}, nil
}

const (
	annotationSigstoreBundleURL            = "dev.sigstore.bundle.url"
	annotationSigstoreBundleDigest         = "dev.sigstore.bundle.digest"
	maxSigstoreBundleBytes           int64 = 4 << 20
	maxGitHubRepositoryMetadataBytes int64 = 1 << 20
)

func (b *ProductionArtifactBuilder) provenanceForOCI(ctx context.Context, ociRepository, sourceRepository string, layer ocispec.Descriptor, index, manifest ocispec.Descriptor, manifestBodyAnnotations map[string]string) (catalog.Provenance, error) {
	bundleURL, bundleDigest, present, err := provenanceAnnotations(index.Annotations, manifest.Annotations, manifestBodyAnnotations)
	if err != nil {
		return catalog.Provenance{}, err
	}
	if !present {
		data, digest, found, err := b.registry.FetchSigstoreBundleReferrer(ctx, ociRepository, manifest)
		if err != nil {
			return catalog.Provenance{}, fmt.Errorf("discover OCI Sigstore provenance: %w", err)
		}
		if !found {
			return catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: catalog.ChecksumProvenanceWaiver}}, nil
		}
		verified, err := b.verifySigstoreBundleForRepository(ctx, data, sourceRepository, layer.Digest)
		if err != nil {
			return catalog.Provenance{}, err
		}
		verified.BundleDigest = digest.String()
		return catalog.Provenance{Verified: verified}, nil
	}
	parsedDigest, err := digest.Parse(bundleDigest)
	if err != nil || parsedDigest.Algorithm() != digest.SHA256 || parsedDigest.Validate() != nil {
		return catalog.Provenance{}, errors.New("Sigstore bundle annotation has an invalid sha256 digest")
	}
	probe, err := b.fetcher.Probe(ctx, bundleURL)
	if err != nil {
		return catalog.Provenance{}, fmt.Errorf("probe Sigstore bundle: %w", err)
	}
	if probe.Size > maxSigstoreBundleBytes {
		return catalog.Provenance{}, fmt.Errorf("Sigstore bundle size %d exceeds %d", probe.Size, maxSigstoreBundleBytes)
	}
	var payload bytes.Buffer
	evidence, err := b.fetcher.FetchObserved(ctx, bundleURL, probe.Size, path.Base(strings.Split(bundleURL, "?")[0]), uniqueSorted(probe.RedirectHostSequence), &payload)
	if err != nil {
		return catalog.Provenance{}, fmt.Errorf("fetch Sigstore bundle: %w", err)
	}
	actualBundleDigest := "sha256:" + evidence.SHA256
	if actualBundleDigest != parsedDigest.String() {
		return catalog.Provenance{}, fmt.Errorf("Sigstore bundle digest %s does not match annotation %s", actualBundleDigest, parsedDigest)
	}
	verified, err := b.verifySigstoreBundleForRepository(ctx, payload.Bytes(), sourceRepository, layer.Digest)
	if err != nil {
		return catalog.Provenance{}, err
	}
	verified.BundleDigest = actualBundleDigest
	return catalog.Provenance{Verified: verified}, nil
}

func provenanceAnnotations(annotationSets ...map[string]string) (string, string, bool, error) {
	values := func(annotations map[string]string) (string, string, bool) {
		if annotations == nil {
			return "", "", false
		}
		url := strings.TrimSpace(annotations[annotationSigstoreBundleURL])
		digest := strings.TrimSpace(annotations[annotationSigstoreBundleDigest])
		return url, digest, url != "" || digest != ""
	}
	var selectedURL, selectedDigest string
	present := false
	for _, annotations := range annotationSets {
		url, digest, setPresent := values(annotations)
		if !setPresent {
			continue
		}
		if url == "" || digest == "" {
			return "", "", false, errors.New("Sigstore provenance annotations must include URL and digest together")
		}
		if present && (url != selectedURL || digest != selectedDigest) {
			return "", "", false, errors.New("OCI Sigstore provenance annotations disagree")
		}
		selectedURL, selectedDigest, present = url, digest, true
	}
	return selectedURL, selectedDigest, present, nil
}

func (b *ProductionArtifactBuilder) verifySigstoreBundleForRepository(ctx context.Context, data []byte, sourceRepository string, artifactDigest digest.Digest) (*catalog.VerifiedProvenance, error) {
	defaultBranch, err := b.githubDefaultBranch(ctx, sourceRepository)
	if err != nil {
		return nil, fmt.Errorf("resolve GitHub default branch for Sigstore identity: %w", err)
	}
	return verifySigstoreBundle(data, sourceRepository, defaultBranch, artifactDigest)
}

func (b *ProductionArtifactBuilder) githubDefaultBranch(ctx context.Context, sourceRepository string) (string, error) {
	if b == nil || b.fetcher == nil {
		return "", errors.New("artifact fetcher is unavailable")
	}
	metadataURL, err := githubRepositoryMetadataURL(sourceRepository)
	if err != nil {
		return "", err
	}
	probe, err := b.fetcher.Probe(ctx, metadataURL)
	if err != nil {
		return "", fmt.Errorf("probe GitHub repository metadata: %w", err)
	}
	if probe.Size <= 0 || probe.Size > maxGitHubRepositoryMetadataBytes {
		return "", fmt.Errorf("GitHub repository metadata size %d is outside 1..%d bytes", probe.Size, maxGitHubRepositoryMetadataBytes)
	}
	var payload bytes.Buffer
	if _, err := b.fetcher.FetchObserved(ctx, metadataURL, probe.Size, "github-repository.json", uniqueSorted(probe.RedirectHostSequence), &payload); err != nil {
		return "", fmt.Errorf("fetch GitHub repository metadata: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload.Bytes()))
	var metadata struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := decoder.Decode(&metadata); err != nil {
		return "", fmt.Errorf("decode GitHub repository metadata: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("GitHub repository metadata contains multiple JSON values")
		}
		return "", fmt.Errorf("decode trailing GitHub repository metadata: %w", err)
	}
	if err := validateDefaultBranch(metadata.DefaultBranch); err != nil {
		return "", err
	}
	return metadata.DefaultBranch, nil
}

func githubRepositoryMetadataURL(sourceRepository string) (string, error) {
	parsed, err := url.Parse(sourceRepository)
	if err != nil {
		return "", errors.New("source repository URL is invalid")
	}
	if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", errors.New("source repository must be a canonical public GitHub HTTPS URL")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parsed.Path != "/"+parts[0]+"/"+parts[1] {
		return "", errors.New("source repository must identify exactly one GitHub repository")
	}
	return (&url.URL{Scheme: "https", Host: "api.github.com", Path: "/repos/" + parts[0] + "/" + parts[1]}).String(), nil
}

func validateDefaultBranch(branch string) error {
	if branch == "" || len(branch) > 1024 || strings.TrimSpace(branch) != branch {
		return errors.New("GitHub repository metadata has an invalid default branch")
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f {
			return errors.New("GitHub repository metadata has an invalid default branch")
		}
	}
	return nil
}

func sigstoreCertificateIdentity(policy *policyv2.TapPolicy, sourceRepository, defaultBranch string) (verify.CertificateIdentity, error) {
	if policy == nil || !policy.DefaultBranchOnly {
		return verify.CertificateIdentity{}, errors.New("Sigstore policy does not require default-branch provenance")
	}
	if _, err := githubRepositoryMetadataURL(sourceRepository); err != nil {
		return verify.CertificateIdentity{}, err
	}
	if err := validateDefaultBranch(defaultBranch); err != nil {
		return verify.CertificateIdentity{}, err
	}
	identityRegex := "^" + regexp.QuoteMeta(sourceRepository) + `/\.github/workflows/[^@\r\n]+@refs/heads/` + regexp.QuoteMeta(defaultBranch) + "$"
	return verify.NewShortCertificateIdentity(policy.SigstoreIssuer, "", "", identityRegex)
}

func verifySigstoreBundle(data []byte, sourceRepository, defaultBranch string, artifactDigest digest.Digest) (*catalog.VerifiedProvenance, error) {
	if int64(len(data)) == 0 || int64(len(data)) > maxSigstoreBundleBytes {
		return nil, errors.New("Sigstore bundle size is invalid")
	}
	var bundle sigbundle.Bundle
	if err := bundle.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("decode Sigstore bundle: %w", err)
	}
	trustedRootJSON, err := policyv2.SigstoreTrustedRoot()
	if err != nil {
		return nil, err
	}
	trustedRoot, err := root.NewTrustedRootFromJSON(trustedRootJSON)
	if err != nil {
		return nil, fmt.Errorf("load release-pinned Sigstore trusted root: %w", err)
	}
	verifier, err := verify.NewVerifier(trustedRoot, verify.WithObserverTimestamps(1), verify.WithTransparencyLog(1), verify.WithSignedCertificateTimestamps(1))
	if err != nil {
		return nil, err
	}
	policy, err := policyv2.LoadTapPolicy()
	if err != nil {
		return nil, err
	}
	certificateIdentity, err := sigstoreCertificateIdentity(policy, sourceRepository, defaultBranch)
	if err != nil {
		return nil, err
	}
	digestBytes, err := hex.DecodeString(artifactDigest.Encoded())
	if err != nil {
		return nil, err
	}
	result, err := verifier.Verify(&bundle, verify.NewPolicy(verify.WithArtifactDigest("sha256", digestBytes), verify.WithCertificateIdentity(certificateIdentity)))
	if err != nil {
		return nil, fmt.Errorf("verify Sigstore/in-toto provenance: %w", err)
	}
	if result.Statement == nil || result.Signature == nil || result.Signature.Certificate == nil {
		return nil, errors.New("verified Sigstore bundle has no in-toto statement or certificate identity")
	}
	statement, err := proto.MarshalOptions{Deterministic: true}.Marshal(result.Statement)
	if err != nil {
		return nil, err
	}
	statementSum := sha256.Sum256(statement)
	return &catalog.VerifiedProvenance{
		PolicyVersion:   catalog.VerifiedProvenancePolicy,
		SubjectDigest:   artifactDigest.String(),
		StatementDigest: "sha256:" + hex.EncodeToString(statementSum[:]),
		SignerIdentity:  result.Signature.Certificate.SubjectAlternativeName,
		Issuer:          policy.SigstoreIssuer,
	}, nil
}
