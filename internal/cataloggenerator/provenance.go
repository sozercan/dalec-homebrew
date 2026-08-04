package cataloggenerator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	verified, err := verifySigstoreBundle(payload.Bytes(), sourceRepository, artifactDigest)
	if err != nil {
		return catalog.Provenance{}, err
	}
	verified.BundleDigest = "sha256:" + evidence.SHA256
	return catalog.Provenance{Verified: verified}, nil
}

const (
	annotationSigstoreBundleURL          = "dev.sigstore.bundle.url"
	annotationSigstoreBundleDigest       = "dev.sigstore.bundle.digest"
	maxSigstoreBundleBytes         int64 = 4 << 20
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
		verified, err := verifySigstoreBundle(data, sourceRepository, layer.Digest)
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
	verified, err := verifySigstoreBundle(payload.Bytes(), sourceRepository, layer.Digest)
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

func verifySigstoreBundle(data []byte, sourceRepository string, artifactDigest digest.Digest) (*catalog.VerifiedProvenance, error) {
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
	identityRegex := "^" + regexp.QuoteMeta(sourceRepository) + "/"
	certificateIdentity, err := verify.NewShortCertificateIdentity(policy.SigstoreIssuer, "", "", identityRegex)
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
