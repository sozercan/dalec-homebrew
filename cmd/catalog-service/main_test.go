package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRejectsMissingAndPositionalArguments(t *testing.T) {
	if err := run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing argument error=%v", err)
	}
	if err := run(context.Background(), []string{"unexpected"}); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("positional argument error=%v", err)
	}
}

func TestRunLoadsFixtureBeforeListening(t *testing.T) {
	args := []string{
		"--store", t.TempDir(),
		"--origin", "https://catalog.example",
		"--signing-key", "missing-key.pem",
		"--signing-key-id", "catalog-test-1",
		"--fixture", "missing-fixture.json",
		"--service-version", "test",
		"--service-digest", "sha256:" + strings.Repeat("a", 64),
		"--extractor-version", "test",
		"--extractor-digest", "sha256:" + strings.Repeat("b", 64),
		"--listen", "invalid-listen-address",
	}
	err := run(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "load static catalog generator") {
		t.Fatalf("error=%v", err)
	}
}

func TestRunRequiresExactlyOneGeneratorRoute(t *testing.T) {
	base := []string{
		"--store", t.TempDir(),
		"--origin", "https://catalog.example",
		"--signing-key", "missing-key.pem",
		"--signing-key-id", "catalog-test-1",
		"--service-version", "test",
		"--service-digest", "sha256:" + strings.Repeat("a", 64),
		"--extractor-version", "test",
		"--extractor-digest", "sha256:" + strings.Repeat("b", 64),
	}
	if err := run(context.Background(), base); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("missing generator route error=%v", err)
	}
	withBuildKit := append(append([]string{}, base...), "--buildkit-address", "unix:///missing")
	if err := run(context.Background(), withBuildKit); err == nil || !strings.Contains(err.Error(), "--extractor-ref") {
		t.Fatalf("incomplete BuildKit route error=%v", err)
	}
	withBoth := append(append([]string{}, withBuildKit...), "--fixture", "fixture.json")
	if err := run(context.Background(), withBoth); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("multiple generator routes error=%v", err)
	}
}
