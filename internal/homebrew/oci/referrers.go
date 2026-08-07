package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	SigstoreBundleArtifactTypeV03 = "application/vnd.dev.sigstore.bundle.v0.3+json"
	SigstoreBundleArtifactTypeV01 = "application/vnd.dev.sigstore.bundle+json;version=0.1"
	maxSigstoreBundleBytes        = 4 << 20
)

// FetchSigstoreBundleReferrer discovers one OCI 1.1 Sigstore-bundle referrer
// whose subject is the exact bottle layer descriptor. Absence returns found
// false; malformed or ambiguous advertised evidence fails closed.
func (client *Client) FetchSigstoreBundleReferrer(ctx context.Context, repository string, subject ocispec.Descriptor) (data []byte, bundleDigest digest.Digest, found bool, err error) {
	if client == nil {
		return nil, "", false, errors.New("nil OCI client")
	}
	if err := validateDescriptor(subject, "", client.limits.BlobBytes); err != nil {
		return nil, "", false, fmt.Errorf("Sigstore subject descriptor: %w", err)
	}
	requestURL, err := client.registryURL(repository, "referrers", subject.Digest.String())
	if err != nil {
		return nil, "", false, err
	}
	response, err := client.doRegistryGET(ctx, repository, requestURL, ocispec.MediaTypeImageIndex)
	if err != nil {
		return nil, "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, "", false, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", false, client.statusError(response)
	}
	if err := validateIdentityEncoding(response); err != nil {
		return nil, "", false, err
	}
	mediaType, err := responseMediaType(response)
	if err != nil {
		return nil, "", false, err
	}
	if mediaType != ocispec.MediaTypeImageIndex {
		return nil, "", false, fmt.Errorf("OCI referrers response has media type %q", mediaType)
	}
	if response.ContentLength > client.limits.IndexBytes {
		return nil, "", false, errors.New("OCI referrers response exceeds index limit")
	}
	body, err := readBounded(response.Body, client.limits.IndexBytes)
	if err != nil {
		return nil, "", false, err
	}
	var index ocispec.Index
	if err := decodeJSON(body, &index); err != nil {
		return nil, "", false, fmt.Errorf("decode OCI referrers index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != ocispec.MediaTypeImageIndex {
		return nil, "", false, errors.New("OCI referrers response is not a canonical image index")
	}
	candidates := make([]ocispec.Descriptor, 0)
	for _, descriptor := range index.Manifests {
		if descriptor.ArtifactType != SigstoreBundleArtifactTypeV03 && descriptor.ArtifactType != SigstoreBundleArtifactTypeV01 {
			continue
		}
		if err := validateDescriptor(descriptor, ocispec.MediaTypeImageManifest, client.limits.ManifestBytes); err != nil {
			return nil, "", false, fmt.Errorf("Sigstore referrer descriptor: %w", err)
		}
		candidates = append(candidates, cloneDescriptor(descriptor))
	}
	if len(candidates) == 0 {
		return nil, "", false, nil
	}
	slices.SortFunc(candidates, func(a, b ocispec.Descriptor) int { return strings.Compare(a.Digest.String(), b.Digest.String()) })
	if len(candidates) != 1 {
		return nil, "", false, fmt.Errorf("OCI subject has %d Sigstore bundle referrers, expected exactly one", len(candidates))
	}
	manifestContent, err := client.FetchManifest(ctx, repository, candidates[0])
	if err != nil {
		return nil, "", false, err
	}
	var manifest ocispec.Manifest
	if err := decodeJSON(manifestContent.Bytes, &manifest); err != nil {
		return nil, "", false, err
	}
	if manifest.ArtifactType != candidates[0].ArtifactType || manifest.Subject == nil || manifest.Subject.Digest != subject.Digest || manifest.Subject.Size != subject.Size {
		return nil, "", false, errors.New("Sigstore referrer manifest is not bound to the bottle subject")
	}
	if len(manifest.Layers) != 1 {
		return nil, "", false, errors.New("Sigstore referrer manifest must contain exactly one bundle layer")
	}
	bundleLayer := manifest.Layers[0]
	if bundleLayer.MediaType != SigstoreBundleArtifactTypeV03 && bundleLayer.MediaType != SigstoreBundleArtifactTypeV01 {
		return nil, "", false, fmt.Errorf("unsupported Sigstore bundle layer media type %q", bundleLayer.MediaType)
	}
	if bundleLayer.Size <= 0 || bundleLayer.Size > maxSigstoreBundleBytes {
		return nil, "", false, fmt.Errorf("Sigstore bundle layer size %d is outside 1..%d", bundleLayer.Size, maxSigstoreBundleBytes)
	}
	content, err := client.FetchBlob(ctx, repository, bundleLayer)
	if err != nil {
		return nil, "", false, err
	}
	return content.Bytes, bundleLayer.Digest, true, nil
}
