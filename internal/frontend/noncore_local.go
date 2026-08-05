package frontend

import (
	"context"
	"errors"
	"fmt"
	"slices"

	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/cataloggenerator"
	"github.com/sozercan/dalec-homebrew/internal/catalogresolver"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type invocationCoreProvider struct {
	snapshot       *metadata.Snapshot
	homebrewCommit string
}

func (p invocationCoreProvider) Snapshot(_ context.Context, request *catalog.Request) (cataloggenerator.CoreSnapshot, error) {
	if p.snapshot == nil || request == nil {
		return nil, errors.New("authenticated core snapshot and catalog request are required")
	}
	if request.HomebrewCommit != p.homebrewCommit {
		return nil, fmt.Errorf("catalog request Homebrew commit %q does not match release %q", request.HomebrewCommit, p.homebrewCommit)
	}
	if request.CoreSnapshotDigest != p.snapshot.Info().Digest {
		return nil, errors.New("catalog request core snapshot digest does not match authenticated metadata")
	}
	return p.snapshot, nil
}

// ResolveNonCoreLocally performs one-shot tap ingestion and artifact
// verification inside the current gateway build. No catalog HTTP service,
// signing key, persistent writer, or public origin is involved.
func ResolveNonCoreLocally(ctx context.Context, client gwclient.Client, core *metadata.Snapshot, targets []NonCoreTarget, homebrewCommit, extractorRef string) (*NonCoreResolution, error) {
	if client == nil || core == nil {
		return nil, errors.New("gateway client and authenticated core snapshot are required")
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
		if len(target.ExternalRoots) != 0 {
			hasExternal = true
		}
		requestTargets = append(requestTargets, catalog.PlatformRequest{Platform: target.Platform, ExternalRoots: slices.Clone(target.ExternalRoots), CoreRoots: slices.Clone(target.CoreRoots)})
	}
	if !hasExternal {
		return nil, nil
	}
	request := &catalog.Request{SchemaVersion: catalog.RequestSchemaVersion, Targets: requestTargets, HomebrewCommit: homebrewCommit, CoreSnapshotDigest: core.Info().Digest}
	canonicalTargets, err := request.NormalizedTargets()
	if err != nil {
		return nil, err
	}
	extractor, err := cataloggenerator.NewGatewayTapExtractor(client, extractorRef, homebrewCommit)
	if err != nil {
		return nil, err
	}
	store := cataloggenerator.NewMemoryArtifactStore()
	artifacts, err := cataloggenerator.NewBuildLocalArtifactBuilder(fetcher.Config{}, store)
	if err != nil {
		return nil, err
	}
	generator, err := cataloggenerator.NewGenerator(cataloggenerator.Config{Extractor: extractor, Core: invocationCoreProvider{snapshot: core, homebrewCommit: homebrewCommit}, Artifacts: artifacts})
	if err != nil {
		return nil, err
	}
	generated, err := generator.Generate(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("build-local catalog generation: %w", err)
	}
	catalogs := make(map[catalog.TapID]*catalog.TapCatalog, len(generated.Catalogs))
	var aggregateCatalogBytes int64
	for i := range generated.Catalogs {
		document := generated.Catalogs[i]
		if _, duplicate := catalogs[document.Tap.ID]; duplicate {
			return nil, fmt.Errorf("build-local catalog repeats tap %s", document.Tap.ID)
		}
		canonical, err := catalog.CanonicalTapCatalog(&document)
		if err != nil {
			return nil, err
		}
		if int64(len(canonical)) > catalog.MaxAggregateCatalogBytes-aggregateCatalogBytes {
			return nil, fmt.Errorf("build-local catalog metadata exceeds %d bytes", catalog.MaxAggregateCatalogBytes)
		}
		aggregateCatalogBytes += int64(len(canonical))
		catalogs[document.Tap.ID] = &document
	}
	resolver, err := catalogresolver.New(core, catalogs)
	if err != nil {
		return nil, err
	}
	byPlatform := make(map[string]catalog.PlatformResult, len(generated.Results))
	localBottles := make(map[string]map[catalog.FormulaID][]byte)
	for _, result := range generated.Results {
		key := result.Platform.OS + "/" + result.Platform.Architecture
		if _, duplicate := byPlatform[key]; duplicate {
			return nil, fmt.Errorf("build-local catalog repeats platform %s", key)
		}
		byPlatform[key] = result
		for _, artifact := range result.Artifacts {
			if artifact.Transport.Local == nil {
				continue
			}
			parsed, err := digest.Parse(artifact.Transport.Local.SHA256)
			if err != nil {
				return nil, err
			}
			data, err := store.Bytes(parsed, artifact.Transport.Local.Size)
			if err != nil {
				return nil, err
			}
			if localBottles[key] == nil {
				localBottles[key] = make(map[catalog.FormulaID][]byte)
			}
			localBottles[key][artifact.ID] = data
		}
	}
	for _, target := range canonicalTargets {
		key := target.Platform.OS + "/" + target.Platform.Architecture
		result, ok := byPlatform[key]
		if !ok {
			return nil, fmt.Errorf("build-local catalog omitted platform %s", key)
		}
		roots := append(slices.Clone(target.CoreRoots), target.ExternalRoots...)
		recomputed, err := resolver.Resolve(roots, target.Platform)
		if err != nil {
			return nil, fmt.Errorf("independently resolve build-local platform %s: %w", key, err)
		}
		if err := catalog.CompareClosure(result.Closure, recomputed); err != nil {
			return nil, fmt.Errorf("build-local catalog closure mismatch for %s: %w", key, err)
		}
	}
	if len(byPlatform) != len(canonicalTargets) {
		return nil, errors.New("build-local catalog returned an unexpected platform result")
	}
	return &NonCoreResolution{Request: request, Catalogs: catalogs, ByPlatform: byPlatform, Local: true, LocalBottles: localBottles, ExtractorRef: extractorRef}, nil
}
