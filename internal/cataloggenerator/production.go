package cataloggenerator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogartifactstore"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

type ProductionConfig struct {
	BuildKitAddress      string
	ExtractorRef         string
	HomebrewCommit       string
	Metadata             metadata.Config
	Fetcher              fetcher.Config
	CatalogServiceOrigin string
	ArtifactStore        *catalogartifactstore.Store
	CacheDir             string
	CacheMaxAge          time.Duration
	VerificationIdentity string
}

type officialCoreProvider struct {
	config         metadata.Config
	homebrewCommit string
}

func (p *officialCoreProvider) Snapshot(ctx context.Context, request *catalog.Request) (CoreSnapshot, error) {
	if request == nil {
		return nil, errors.New("nil catalog request")
	}
	if p.homebrewCommit == "" || request.HomebrewCommit != p.homebrewCommit {
		return nil, fmt.Errorf("request Homebrew commit %q does not match generator release %q", request.HomebrewCommit, p.homebrewCommit)
	}
	snapshot, err := metadata.Fetch(ctx, p.config)
	if err != nil {
		return nil, fmt.Errorf("authenticate official core metadata: %w", err)
	}
	if snapshot.Info().Digest != request.CoreSnapshotDigest {
		return nil, errors.New("official core snapshot digest changed before catalog generation")
	}
	return snapshot, nil
}

func NewProduction(ctx context.Context, cfg ProductionConfig) (*Generator, error) {
	if !validCommit(cfg.HomebrewCommit) {
		return nil, errors.New("release-pinned Homebrew commit is required")
	}
	extractor, err := NewBuildKitExtractor(ctx, BuildKitExtractorConfig{Address: cfg.BuildKitAddress, ExtractorRef: cfg.ExtractorRef, HomebrewCommit: cfg.HomebrewCommit})
	if err != nil {
		return nil, err
	}
	artifacts, err := NewProductionArtifactBuilder(cfg.Fetcher, cfg.CatalogServiceOrigin, cfg.ArtifactStore)
	if err != nil {
		_ = extractor.Close()
		return nil, err
	}
	var extractorForGenerator TapExtractor = extractor
	var artifactsForGenerator ArtifactBuilder = artifacts
	if cfg.CacheDir != "" {
		if cfg.VerificationIdentity == "" {
			_ = extractor.Close()
			return nil, errors.New("verification identity is required with persistent ingestion cache")
		}
		tapPolicyDigest, err := policyv2.TapPolicyDigest()
		if err != nil {
			_ = extractor.Close()
			return nil, err
		}
		cacheRoot := filepath.Join(cfg.CacheDir, cacheKey([]byte(cfg.VerificationIdentity+"\x00"+tapPolicyDigest+"\x00"+cfg.HomebrewCommit)))
		maxAge := cfg.CacheMaxAge
		if maxAge == 0 {
			maxAge = time.Hour
		}
		extractorForGenerator, err = newCachedTapExtractor(cacheRoot, maxAge, extractor)
		if err != nil {
			_ = extractor.Close()
			return nil, err
		}
		artifactsForGenerator, err = newCachedArtifactBuilder(cacheRoot, artifacts)
		if err != nil {
			_ = extractor.Close()
			return nil, err
		}
	}
	generator, err := NewGenerator(Config{
		Extractor:  extractorForGenerator,
		Core:       &officialCoreProvider{config: cfg.Metadata, homebrewCommit: cfg.HomebrewCommit},
		Artifacts:  artifactsForGenerator,
		OwnedClose: []interface{ Close() error }{extractor},
	})
	if err != nil {
		_ = extractor.Close()
		return nil, err
	}
	return generator, nil
}

func validCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range []byte(value) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
