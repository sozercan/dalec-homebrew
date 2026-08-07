package frontend

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogclient"
	"github.com/sozercan/dalec-homebrew/internal/catalogresolver"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type catalogSetClient interface {
	Resolve(context.Context, *catalog.Request) (*catalogclient.Result, error)
}

type NonCoreTarget struct {
	Platform      catalog.Platform
	ExternalRoots []catalog.FormulaID
	CoreRoots     []catalog.FormulaID
}

type NonCoreResolution struct {
	Request           *catalog.Request
	Local             bool
	ExtractorRef      string
	LocalBottles      map[string]map[catalog.FormulaID][]byte
	Payload           *catalog.CatalogSetPayload
	Catalogs          map[catalog.TapID]*catalog.TapCatalog
	ByPlatform        map[string]catalog.PlatformResult
	Signatures        []metadata.SignatureInfo
	MinimumSequences  map[catalog.TapID]uint64
	SetPayloadDigest  string
	SetEnvelopeDigest string
}

// ResolveNonCoreCatalogs submits one invocation-wide request only when at least
// one platform has external roots, then independently recomputes every signed
// closure from authenticated core metadata and fetched catalog documents.
func ResolveNonCoreCatalogs(ctx context.Context, client catalogSetClient, core catalogresolver.CoreCatalog, targets []NonCoreTarget, homebrewCommit, coreSnapshotDigest string) (*NonCoreResolution, error) {
	if core == nil {
		return nil, errors.New("core catalog is required")
	}
	requestTargets := make([]catalog.PlatformRequest, 0, len(targets))
	hasExternal := false
	seenPlatforms := map[string]struct{}{}
	for _, target := range targets {
		if err := target.Platform.Validate(); err != nil {
			return nil, err
		}
		key := target.Platform.OS + "/" + target.Platform.Architecture
		if _, duplicate := seenPlatforms[key]; duplicate {
			return nil, fmt.Errorf("duplicate non-core target platform %s", key)
		}
		seenPlatforms[key] = struct{}{}
		roots := make([]catalog.FormulaID, len(target.ExternalRoots))
		copy(roots, target.ExternalRoots)
		if len(roots) > 0 {
			hasExternal = true
		}
		requestTargets = append(requestTargets, catalog.PlatformRequest{Platform: target.Platform, ExternalRoots: roots, CoreRoots: slices.Clone(target.CoreRoots)})
	}
	if !hasExternal {
		return nil, nil
	}
	if client == nil {
		return nil, errors.New("catalog service client is required for external roots")
	}
	request := &catalog.Request{SchemaVersion: catalog.RequestSchemaVersion, Targets: requestTargets, HomebrewCommit: homebrewCommit, CoreSnapshotDigest: coreSnapshotDigest}
	canonicalTargets, err := request.NormalizedTargets()
	if err != nil {
		return nil, err
	}
	result, err := client.Resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Payload == nil {
		return nil, errors.New("catalog service returned an empty authenticated result")
	}
	resolver, err := catalogresolver.New(core, result.Catalogs)
	if err != nil {
		return nil, err
	}
	byPlatform := make(map[string]catalog.PlatformResult, len(result.Payload.Results))
	for _, signed := range result.Payload.Results {
		key := signed.Platform.OS + "/" + signed.Platform.Architecture
		if _, duplicate := byPlatform[key]; duplicate {
			return nil, fmt.Errorf("duplicate signed platform result %s", key)
		}
		byPlatform[key] = signed
	}
	for _, target := range canonicalTargets {
		key := target.Platform.OS + "/" + target.Platform.Architecture
		signed, ok := byPlatform[key]
		if !ok {
			return nil, fmt.Errorf("catalog service omitted platform result %s", key)
		}
		roots := append(slices.Clone(target.CoreRoots), target.ExternalRoots...)
		if len(roots) == 0 {
			if len(signed.Closure.Requested) != 0 || len(signed.Closure.Nodes) != 0 || len(signed.Artifacts) != 0 {
				return nil, fmt.Errorf("catalog service returned non-empty result for filtered platform %s", key)
			}
			continue
		}
		recomputed, err := resolver.Resolve(roots, target.Platform)
		if err != nil {
			return nil, fmt.Errorf("independently resolve platform %s: %w", key, err)
		}
		if err := catalog.CompareClosure(signed.Closure, recomputed); err != nil {
			return nil, fmt.Errorf("catalog service closure mismatch for %s: %w", key, err)
		}
	}
	if len(byPlatform) != len(canonicalTargets) {
		return nil, errors.New("catalog service returned an unexpected platform result")
	}
	return &NonCoreResolution{Request: request, Payload: result.Payload, Catalogs: result.Catalogs, ByPlatform: byPlatform, Signatures: slices.Clone(result.Signatures), MinimumSequences: cloneCatalogSequenceFloors(result.MinimumSequences), SetPayloadDigest: result.SetPayloadDigest, SetEnvelopeDigest: result.SetEnvelopeDigest}, nil
}

func cloneCatalogSequenceFloors(input map[catalog.TapID]uint64) map[catalog.TapID]uint64 {
	result := make(map[catalog.TapID]uint64, len(input))
	for tap, minimum := range input {
		result[tap] = minimum
	}
	return result
}

// metadataCatalogAdapter documents that the official authenticated snapshot is
// the core authority consumed by the independent resolver.
var _ catalogresolver.CoreCatalog = (*metadata.Snapshot)(nil)
