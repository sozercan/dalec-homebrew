package frontend

import (
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/config"
)

func TestNewComponentsV2RetainsReleaseBoundFetcherReference(t *testing.T) {
	const fetcherIndex = "ghcr.io/example/bottle-fetcher@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	components := newComponentsV2(config.Config{FrontendIndexRef: "frontend-index", BottleFetcherRef: fetcherIndex}, "frontend-child", "runtime-base-child", "materializer-child")
	if components.BottleFetcherRef != fetcherIndex {
		t.Fatalf("bottle fetcher reference=%q, want release-bound index %q", components.BottleFetcherRef, fetcherIndex)
	}
	if components.FrontendIndexRef != "frontend-index" || components.FrontendRef != "frontend-child" {
		t.Fatalf("frontend bindings=%q, %q, want index and executing child", components.FrontendIndexRef, components.FrontendRef)
	}
}
