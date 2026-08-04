package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"
)

// CompareClosure verifies that a recomputed closure is byte-for-byte identical
// after canonicalization to the signed service closure.
func CompareClosure(signed, recomputed ClosureResult) error {
	signedBytes, err := CanonicalClosureResult(signed)
	if err != nil {
		return fmt.Errorf("signed closure: %w", err)
	}
	recomputedBytes, err := CanonicalClosureResult(recomputed)
	if err != nil {
		return fmt.Errorf("recomputed closure: %w", err)
	}
	if bytes.Equal(signedBytes, recomputedBytes) {
		return nil
	}
	signedDigest, _ := ClosureResultDigest(signed)
	recomputedDigest, _ := ClosureResultDigest(recomputed)
	return fmt.Errorf("closure mismatch: signed %s, recomputed %s", signedDigest, recomputedDigest)
}

// ComparePlatformResult verifies a recomputed platform closure and artifact set
// against the signed result after deterministic canonicalization.
func ComparePlatformResult(signed, recomputed PlatformResult) error {
	signedBytes, err := CanonicalPlatformResult(signed)
	if err != nil {
		return fmt.Errorf("signed platform result: %w", err)
	}
	recomputedBytes, err := CanonicalPlatformResult(recomputed)
	if err != nil {
		return fmt.Errorf("recomputed platform result: %w", err)
	}
	if bytes.Equal(signedBytes, recomputedBytes) {
		return nil
	}
	signedDigest, _ := PlatformResultDigest(signed)
	recomputedDigest, _ := PlatformResultDigest(recomputed)
	return fmt.Errorf("platform result mismatch for %s: signed %s, recomputed %s", signed.Platform.key(), signedDigest, recomputedDigest)
}

// ComparePlatformResults compares a complete multi-platform result set without
// depending on completion order.
func ComparePlatformResults(signed, recomputed []PlatformResult) error {
	signedBytes, err := canonicalPlatformResults(signed)
	if err != nil {
		return fmt.Errorf("signed platform results: %w", err)
	}
	recomputedBytes, err := canonicalPlatformResults(recomputed)
	if err != nil {
		return fmt.Errorf("recomputed platform results: %w", err)
	}
	if bytes.Equal(signedBytes, recomputedBytes) {
		return nil
	}
	return errors.New("platform result set mismatch")
}

// ValidateCatalogSetBinding verifies that a signed payload is bound to the
// canonical request and that every result carries exactly the requested roots
// and platforms.
func ValidateCatalogSetBinding(payload *CatalogSetPayload, request *Request) error {
	if err := ValidateCatalogSetPayload(payload); err != nil {
		return fmt.Errorf("catalog-set payload: %w", err)
	}
	if err := ValidateRequest(request); err != nil {
		return fmt.Errorf("catalog request: %w", err)
	}
	requestDigest, err := RequestDigest(request)
	if err != nil {
		return err
	}
	if payload.RequestDigest != requestDigest.String() {
		return fmt.Errorf("payload request_digest %s does not match request %s", payload.RequestDigest, requestDigest)
	}
	if payload.CoreSnapshotDigest != request.CoreSnapshotDigest {
		return fmt.Errorf("payload core_snapshot_digest %s does not match request %s", payload.CoreSnapshotDigest, request.CoreSnapshotDigest)
	}
	targets, err := normalizedRequestTargets(request)
	if err != nil {
		return err
	}
	requestedByPlatform := make(map[string][]FormulaID, len(targets))
	for _, target := range targets {
		roots := append(slices.Clone(target.CoreRoots), target.ExternalRoots...)
		slices.Sort(roots)
		requestedByPlatform[target.Platform.key()] = roots
	}
	if len(payload.Results) != len(requestedByPlatform) {
		return fmt.Errorf("payload has %d platform results for %d requested platforms", len(payload.Results), len(requestedByPlatform))
	}
	for _, result := range payload.Results {
		requestedRoots, present := requestedByPlatform[result.Platform.key()]
		if !present {
			return fmt.Errorf("payload contains unrequested platform %s", result.Platform.key())
		}
		roots := make([]FormulaID, 0, len(result.Closure.RequestedMappings))
		if len(result.Closure.RequestedMappings) == 0 {
			roots = slices.Clone(result.Closure.Requested)
		} else {
			for _, mapping := range result.Closure.RequestedMappings {
				roots = append(roots, mapping.Requested)
			}
		}
		slices.Sort(roots)
		if !slices.Equal(roots, requestedRoots) {
			return fmt.Errorf("payload platform %s requested roots do not match request target", result.Platform.key())
		}
	}
	return nil
}

// DecodeReferencedTapCatalog checks the signed byte size and digest before
// decoding, then requires the served document itself to use canonical JSON.
func DecodeReferencedTapCatalog(reference CatalogReference, data []byte) (*TapCatalog, error) {
	if err := ValidateCatalogReference(reference); err != nil {
		return nil, fmt.Errorf("catalog reference: %w", err)
	}
	if int64(len(data)) != reference.Size {
		return nil, fmt.Errorf("catalog size %d does not match signed size %d", len(data), reference.Size)
	}
	if got := digest.FromBytes(data).String(); got != reference.SHA256 {
		return nil, fmt.Errorf("catalog digest %s does not match signed digest %s", got, reference.SHA256)
	}
	catalog, err := DecodeTapCatalog(data)
	if err != nil {
		return nil, err
	}
	canonical, err := CanonicalTapCatalog(catalog)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("served tap catalog is not canonical JSON")
	}
	if err := VerifyCatalogReference(reference, catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

// VerifyCatalogReference verifies that catalog's canonical bytes and embedded
// source identity exactly match a signed catalog reference.
func VerifyCatalogReference(reference CatalogReference, catalog *TapCatalog) error {
	if err := ValidateCatalogReference(reference); err != nil {
		return fmt.Errorf("catalog reference: %w", err)
	}
	if err := ValidateTapCatalog(catalog); err != nil {
		return fmt.Errorf("tap catalog: %w", err)
	}
	if reference.Tap != catalog.Tap {
		return errors.New("catalog tap source does not match signed reference")
	}
	if !reference.PublishedAt.Equal(catalog.PublishedAt) {
		return fmt.Errorf("catalog published_at %s does not match signed reference %s", catalog.PublishedAt, reference.PublishedAt)
	}
	if reference.Sequence != catalog.Sequence {
		return fmt.Errorf("catalog sequence %d does not match signed reference %d", catalog.Sequence, reference.Sequence)
	}
	data, err := CanonicalTapCatalog(catalog)
	if err != nil {
		return err
	}
	if int64(len(data)) != reference.Size {
		return fmt.Errorf("catalog size %d does not match signed size %d", len(data), reference.Size)
	}
	digest, err := TapCatalogDigest(catalog)
	if err != nil {
		return err
	}
	if digest.String() != reference.SHA256 {
		return fmt.Errorf("catalog digest %s does not match signed digest %s", digest, reference.SHA256)
	}
	return nil
}

// ValidateCatalogReferenceOrigin enforces that a signed catalog URL uses the
// configured catalog-service origin. Digest/path validation remains part of
// ValidateCatalogReference.
func ValidateCatalogReferenceOrigin(reference CatalogReference, serviceOrigin string) error {
	if err := ValidateCatalogReference(reference); err != nil {
		return err
	}
	origin, err := url.Parse(serviceOrigin)
	if err != nil {
		return fmt.Errorf("parse service origin: %w", err)
	}
	if origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("service origin must contain only an HTTPS scheme and authority")
	}
	if err := validatePublicHostname(origin.Hostname()); err != nil {
		return fmt.Errorf("service origin: %w", err)
	}
	if port := origin.Port(); port != "" && port != "443" {
		return fmt.Errorf("service origin port %q is not allowed", port)
	}
	catalogURL, _ := url.Parse(reference.URL)
	if !strings.EqualFold(catalogURL.Scheme, origin.Scheme) || !strings.EqualFold(catalogURL.Host, origin.Host) {
		return fmt.Errorf("catalog URL origin %s://%s does not match configured origin %s://%s", catalogURL.Scheme, catalogURL.Host, origin.Scheme, origin.Host)
	}
	return nil
}

func canonicalPlatformResults(values []PlatformResult) ([]byte, error) {
	if values == nil || len(values) == 0 || len(values) > 2 {
		return nil, errors.New("platform results must contain one or two entries")
	}
	clone, err := cloneValue(values)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(clone))
	for i := range clone {
		if err := ValidatePlatformResult(clone[i]); err != nil {
			return nil, fmt.Errorf("results[%d]: %w", i, err)
		}
		key := clone[i].Platform.key()
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("duplicate platform %s", key)
		}
		seen[key] = struct{}{}
		canonicalizePlatformResult(&clone[i])
	}
	slices.SortFunc(clone, func(left, right PlatformResult) int {
		return comparePlatform(left.Platform, right.Platform)
	})
	return encodeCanonical(clone)
}
