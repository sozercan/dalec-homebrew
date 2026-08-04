package llbutil

import (
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestFetchRequestV2BindsSignedTransport(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	node := resolution.NodeV2{ID: "acme/tools/widget", Bottle: resolution.BottleV2{Filename: "widget.tgz", Transport: resolution.BottleTransport{HTTPS: &resolution.HTTPSTransport{URL: "https://bottles.example/widget.tgz", ExpectedSize: 7, SHA256: d, Filename: "widget.tgz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: resolution.HTTPSFetchPolicyVersionV1}}}}
	request, err := fetchRequestV2(node)
	if err != nil {
		t.Fatal(err)
	}
	if request.ArtifactID != node.ID.String() || request.SHA256 != strings.TrimPrefix(d, "sha256:") || request.FetchPolicyVersion != fetcher.FetchPolicyVersion {
		t.Fatalf("request=%+v", request)
	}
}

func TestFetchRequestV2RejectsFilenameMismatch(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	node := resolution.NodeV2{ID: "acme/tools/widget", Bottle: resolution.BottleV2{Filename: "widget.tgz", Transport: resolution.BottleTransport{HTTPS: &resolution.HTTPSTransport{URL: "https://bottles.example/widget.tgz", ExpectedSize: 7, SHA256: d, Filename: "other.tgz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: resolution.HTTPSFetchPolicyVersionV1}}}}
	if _, err := fetchRequestV2(node); err == nil {
		t.Fatal("filename mismatch accepted")
	}
}

func TestV2LLBBindingsRejectComponentAndPlatformMismatch(t *testing.T) {
	record := &resolution.RecordV2{Input: resolution.Input{Platform: resolution.Platform{OS: "linux", Architecture: "amd64"}}, Components: resolution.ComponentsV2{BottleFetcherRef: "example/fetcher@sha256:" + strings.Repeat("a", 64)}}
	if _, err := BottleStatesV2("example/other@sha256:"+strings.Repeat("b", 64), ocispec.Platform{OS: "linux", Architecture: "amd64"}, record); err == nil {
		t.Fatal("mismatched fetcher reference accepted")
	}
	if err := validateV2PlatformBinding(record, ocispec.Platform{OS: "linux", Architecture: "arm64"}); err == nil {
		t.Fatal("mismatched platform accepted")
	}
}
