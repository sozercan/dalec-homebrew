package oci

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestClientBoundsIndexResponses(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("x"), 33)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", ocispec.MediaTypeImageIndex)
		_, _ = writer.Write(body)
	}))
	t.Cleanup(server.Close)

	limits := DefaultLimits()
	limits.IndexBytes = 32
	client, err := NewClient(server.URL, WithLimits(limits))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FetchIndex(context.Background(), "homebrew/core/test", "1.0")
	if err == nil || !strings.Contains(err.Error(), "exceeds 32-byte limit") {
		t.Fatalf("expected bounded-read error, got %v", err)
	}
}

func TestClientVerifiesBlobDigestAndSize(t *testing.T) {
	t.Parallel()

	expected := []byte("expected")
	mutated := []byte("mutated!")
	descriptor := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(expected),
		Size:      int64(len(expected)),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write(mutated)
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FetchBlob(context.Background(), "homebrew/core/test", descriptor)
	if err == nil || !strings.Contains(err.Error(), "does not match descriptor digest") {
		t.Fatalf("expected digest verification error, got %v", err)
	}
}

func TestParseBearerChallengeQuotedValues(t *testing.T) {
	t.Parallel()

	challenge, err := parseBearerChallenge([]string{`Basic realm="legacy", Bearer realm="https://registry.example/token",service="ghcr.io",scope="repository:homebrew/core/jq:pull"`})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.Realm != "https://registry.example/token" || challenge.Service != "ghcr.io" || challenge.Scope != "repository:homebrew/core/jq:pull" {
		t.Fatalf("unexpected challenge: %#v", challenge)
	}
}
