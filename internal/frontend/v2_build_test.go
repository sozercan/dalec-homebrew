package frontend

import (
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/config"
)

func TestNewComponentsV2RetainsReleaseBoundFetcherReference(t *testing.T) {
	const fetcherIndex = "ghcr.io/example/bottle-fetcher@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	components := newComponentsV2(config.Config{BottleFetcherRef: fetcherIndex}, "frontend-index", "frontend-child", "runtime-base-child", "materializer-child")
	if components.BottleFetcherRef != fetcherIndex {
		t.Fatalf("bottle fetcher reference=%q, want release-bound index %q", components.BottleFetcherRef, fetcherIndex)
	}
}
