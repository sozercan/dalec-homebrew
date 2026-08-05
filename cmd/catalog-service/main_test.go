package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
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

func TestRunRejectsMalformedTapCommitPins(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tests := []struct {
		name    string
		value   string
		message string
	}{
		{name: "missing separator", value: "acme/tools", message: "TAP=COMMIT"},
		{name: "missing commit", value: "acme/tools=", message: "TAP=COMMIT"},
		{name: "malformed tap", value: "Acme/tools=" + commit, message: "invalid Homebrew tap"},
		{name: "short commit", value: "acme/tools=abc", message: "lowercase 40-hex"},
		{name: "uppercase commit", value: "acme/tools=" + strings.Repeat("A", 40), message: "lowercase 40-hex"},
		{name: "non-hex commit", value: "acme/tools=" + strings.Repeat("g", 40), message: "lowercase 40-hex"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := run(context.Background(), []string{"--tap-commit", test.value})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRunRejectsCoreAndDuplicateTapCommitPins(t *testing.T) {
	commit := strings.Repeat("a", 40)
	if err := run(context.Background(), []string{"--tap-commit", "homebrew/core=" + commit}); err == nil || !strings.Contains(err.Error(), "homebrew/core") {
		t.Fatalf("core pin error=%v", err)
	}
	args := []string{"--tap-commit", "acme/tools=" + commit, "--tap-commit", "acme/tools=" + strings.Repeat("b", 40)}
	if err := run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate pin error=%v", err)
	}
}

func TestRunBoundsTapCommitPins(t *testing.T) {
	args := make([]string, 0, 2*(catalog.MaxTaps+1))
	for i := 0; i <= catalog.MaxTaps; i++ {
		args = append(args, "--tap-commit", fmt.Sprintf("acme/tap%d=%s", i, strings.Repeat("a", 40)))
	}
	err := run(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("limit %d", catalog.MaxTaps)) {
		t.Fatalf("error=%v", err)
	}
}

func TestRunRejectsTapCommitPinsWithFixture(t *testing.T) {
	args := []string{
		"--store", t.TempDir(),
		"--origin", "https://catalog.example",
		"--signing-key", "missing-key.pem",
		"--signing-key-id", "catalog-test-1",
		"--fixture", "missing-fixture.json",
		"--tap-commit", "acme/tools=" + strings.Repeat("a", 40),
		"--service-version", "test",
		"--service-digest", "sha256:" + strings.Repeat("a", 64),
		"--extractor-version", "test",
		"--extractor-digest", "sha256:" + strings.Repeat("b", 64),
	}
	err := run(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "not supported with --fixture") {
		t.Fatalf("error=%v", err)
	}
}
