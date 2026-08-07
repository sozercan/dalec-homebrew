package cataloggenerator

import (
	"context"
	"errors"
	"fmt"

	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogextractor"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

// GatewayTapExtractor evaluates taps as one-shot content-addressed LLB solves
// on the caller's BuildKit worker. Network access is confined to the Git source;
// Formula evaluation uses the same read-only network-disabled extractor graph as
// the dedicated-worker implementation.
type GatewayTapExtractor struct {
	client         gwclient.Client
	extractorRef   string
	homebrewCommit string
}

func NewGatewayTapExtractor(client gwclient.Client, extractorRef, homebrewCommit string) (*GatewayTapExtractor, error) {
	if client == nil {
		return nil, errors.New("gateway client is required")
	}
	if err := resolution.ValidatePinnedReference(extractorRef); err != nil {
		return nil, fmt.Errorf("catalog extractor reference: %w", err)
	}
	if !validCommit(homebrewCommit) {
		return nil, errors.New("release-pinned Homebrew commit is required")
	}
	return &GatewayTapExtractor{client: client, extractorRef: extractorRef, homebrewCommit: homebrewCommit}, nil
}

func (e *GatewayTapExtractor) Extract(ctx context.Context, tap catalog.TapID) (*catalogextractor.ExtractedTap, error) {
	if e == nil || e.client == nil {
		return nil, errors.New("gateway tap extractor is unavailable")
	}
	state, err := extractionState(e.extractorRef, e.homebrewCommit, tap)
	if err != nil {
		return nil, err
	}
	definition, err := state.Marshal(ctx)
	if err != nil {
		return nil, fmt.Errorf("marshal build-local tap extraction: %w", err)
	}
	result, err := e.client.Solve(ctx, gwclient.SolveRequest{Definition: definition.ToPB()})
	if err != nil {
		return nil, fmt.Errorf("solve build-local tap extraction: %w", err)
	}
	ref, err := result.SingleRef()
	if err != nil {
		return nil, fmt.Errorf("read build-local tap extraction reference: %w", err)
	}
	stat, err := ref.StatFile(ctx, gwclient.StatRequest{Path: "/" + extractedTapFilename})
	if err != nil {
		return nil, fmt.Errorf("stat build-local extracted tap: %w", err)
	}
	if stat.Size <= 0 || stat.Size > catalog.MaxCatalogDocumentBytes {
		return nil, fmt.Errorf("build-local extracted tap size %d is outside 1..%d", stat.Size, catalog.MaxCatalogDocumentBytes)
	}
	data, err := ref.ReadFile(ctx, gwclient.ReadRequest{Filename: "/" + extractedTapFilename, Range: &gwclient.FileRange{Length: int(stat.Size)}})
	if err != nil {
		return nil, fmt.Errorf("read build-local extracted tap: %w", err)
	}
	extracted, err := catalogextractor.DecodeExtractedTap(data)
	if err != nil {
		return nil, fmt.Errorf("decode build-local tap extraction: %w", err)
	}
	if err := verifyExtractedTap(extracted, tap); err != nil {
		return nil, err
	}
	return extracted, nil
}
