package cataloggenerator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogextractor"
	"github.com/sozercan/dalec-homebrew/internal/catalogresolver"
	"github.com/sozercan/dalec-homebrew/internal/catalogservice"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type CoreProvider interface {
	Snapshot(context.Context, *catalog.Request) (CoreSnapshot, error)
}

type CoreSnapshot interface {
	Lookup(string) (metadata.Match, error)
	Info() metadata.SnapshotInfo
}

type ArtifactBuilder interface {
	Build(context.Context, *catalog.Request, CoreSnapshot, map[catalog.TapID]*catalog.TapCatalog, catalog.Node, catalog.Platform) (catalog.BottleArtifact, error)
}

type Config struct {
	Extractor  TapExtractor
	Core       CoreProvider
	Artifacts  ArtifactBuilder
	OwnedClose []interface{ Close() error }
}

// Generator is the shared fixed-point tap catalog and artifact generator.
// It ingests only the taps reached by the requested closure and independently
// binds every selected bottle before returning unsigned data to the signer.
type Generator struct {
	extractor TapExtractor
	core      CoreProvider
	artifacts ArtifactBuilder
	owned     []interface{ Close() error }
	closeOnce sync.Once
	closeErr  error
}

func NewGenerator(cfg Config) (*Generator, error) {
	if cfg.Extractor == nil || cfg.Core == nil || cfg.Artifacts == nil {
		return nil, errors.New("extractor, core provider, and artifact builder are required")
	}
	return &Generator{extractor: cfg.Extractor, core: cfg.Core, artifacts: cfg.Artifacts, owned: slices.Clone(cfg.OwnedClose)}, nil
}

func (g *Generator) Close() error {
	if g == nil {
		return nil
	}
	g.closeOnce.Do(func() {
		var errs []error
		for i := len(g.owned) - 1; i >= 0; i-- {
			if g.owned[i] != nil {
				errs = append(errs, g.owned[i].Close())
			}
		}
		g.closeErr = errors.Join(errs...)
	})
	return g.closeErr
}

func (g *Generator) Generate(ctx context.Context, request *catalog.Request) (*catalogservice.GeneratedSet, error) {
	if g == nil {
		return nil, catalogservice.NewFailureError(catalog.FailureUnavailable, "catalog generator is unavailable", nil)
	}
	if err := catalog.ValidateRequest(request); err != nil {
		return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "catalog request is invalid", err)
	}
	core, err := g.core.Snapshot(ctx, request)
	if err != nil {
		return nil, classifyGenerationError(err)
	}
	if core.Info().Digest != request.CoreSnapshotDigest {
		return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "verified core snapshot does not match the request", nil)
	}
	targets, err := request.NormalizedTargets()
	if err != nil {
		return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "catalog request is invalid", err)
	}

	catalogs := make(map[catalog.TapID]*catalog.TapCatalog)
	pending := make(map[catalog.TapID]struct{})
	for _, target := range targets {
		for _, root := range target.ExternalRoots {
			pending[root.Tap()] = struct{}{}
		}
	}
	if len(pending) > catalog.MaxTaps {
		return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "catalog request exceeds the tap limit", nil)
	}

	var closures []catalog.ClosureResult
	for {
		if err := ctx.Err(); err != nil {
			return nil, catalogservice.NewFailureError(catalog.FailureTimeout, "catalog generation timed out", err)
		}
		pendingTaps := make([]catalog.TapID, 0, len(pending))
		for tap := range pending {
			if _, done := catalogs[tap]; !done {
				pendingTaps = append(pendingTaps, tap)
			}
		}
		slices.Sort(pendingTaps)
		for _, tap := range pendingTaps {
			if len(catalogs) >= catalog.MaxTaps {
				return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "resolved closure exceeds the tap limit", nil)
			}
			extracted, err := g.extractor.Extract(ctx, tap)
			if err != nil {
				return nil, catalogservice.NewFailureError(catalog.FailureInvalidTap, "tap extraction failed", err)
			}
			document, err := catalogextractor.ToCatalog(extracted, core)
			if err != nil {
				return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "tap catalog is invalid", err)
			}
			if generatedAt := core.Info().GeneratedAt.UTC(); !generatedAt.IsZero() {
				document.PublishedAt = generatedAt
			}
			catalogs[tap] = document
			delete(pending, tap)
		}

		resolver, err := catalogresolver.New(core, catalogs)
		if err != nil {
			return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "catalog resolver rejected extracted taps", err)
		}
		closures = make([]catalog.ClosureResult, len(targets))
		missing := catalog.TapID("")
		for i, target := range targets {
			roots := append(slices.Clone(target.CoreRoots), target.ExternalRoots...)
			if len(roots) == 0 {
				closures[i] = catalog.ClosureResult{Requested: []catalog.FormulaID{}, RequestedMappings: []catalog.RequestedMapping{}, NormalizationTaps: []catalog.TapID{}, Nodes: []catalog.Node{}, InstallOrder: []catalog.FormulaID{}}
				continue
			}
			closure, err := resolver.Resolve(roots, target.Platform)
			if err != nil {
				var missingTap *catalogresolver.MissingTapError
				if errors.As(err, &missingTap) {
					missing = missingTap.Tap
					break
				}
				return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "dependency closure is invalid", err)
			}
			closures[i] = closure
		}
		if missing == "" {
			break
		}
		if missing.IsCore() || len(catalogs)+len(pending) >= catalog.MaxTaps {
			return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "dependency closure exceeds the tap limit", nil)
		}
		pending[missing] = struct{}{}
	}

	results := make([]catalog.PlatformResult, len(targets))
	for i, target := range targets {
		artifacts := make([]catalog.BottleArtifact, 0, len(closures[i].Nodes))
		for _, node := range closures[i].Nodes {
			artifact, err := g.artifacts.Build(ctx, request, core, catalogs, node, target.Platform)
			if err != nil {
				return nil, classifyArtifactError(node.ID, err)
			}
			artifacts = append(artifacts, artifact)
		}
		slices.SortFunc(artifacts, func(a, b catalog.BottleArtifact) int { return strings.Compare(string(a.ID), string(b.ID)) })
		result := catalog.PlatformResult{Platform: target.Platform, Closure: closures[i], Artifacts: artifacts}
		if err := catalog.ValidatePlatformResult(result); err != nil {
			return nil, catalogservice.NewFailureError(catalog.FailurePolicy, "generated platform result is invalid", err)
		}
		results[i] = result
	}

	documents := make([]catalog.TapCatalog, 0, len(catalogs))
	for _, document := range catalogs {
		documents = append(documents, *document)
	}
	slices.SortFunc(documents, func(a, b catalog.TapCatalog) int { return strings.Compare(string(a.Tap.ID), string(b.Tap.ID)) })
	return &catalogservice.GeneratedSet{Catalogs: documents, Results: results}, nil
}

func classifyGenerationError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return catalogservice.NewFailureError(catalog.FailureTimeout, "catalog generation timed out", err)
	}
	return catalogservice.NewFailureError(catalog.FailureUnavailable, "catalog generation dependency is unavailable", err)
}

func classifyArtifactError(id catalog.FormulaID, err error) error {
	message := fmt.Sprintf("bottle verification failed for %s", id)
	if strings.Contains(strings.ToLower(err.Error()), "unavailable") || strings.Contains(strings.ToLower(err.Error()), "no bottle") {
		return catalogservice.NewFailureError(catalog.FailureMissingBottle, message, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return catalogservice.NewFailureError(catalog.FailureTimeout, "catalog generation timed out", err)
	}
	return catalogservice.NewFailureError(catalog.FailurePolicy, message, err)
}
