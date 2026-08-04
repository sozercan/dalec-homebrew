package cataloggenerator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogextractor"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type countingExtractor struct {
	value *catalogextractor.ExtractedTap
	calls int
}

func (c *countingExtractor) Extract(context.Context, catalog.TapID) (*catalogextractor.ExtractedTap, error) {
	c.calls++
	return c.value, nil
}

func TestPersistentTapCacheSurvivesWrapperRestart(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	d := "sha256:" + strings.Repeat("a", 64)
	value := &catalogextractor.ExtractedTap{SchemaVersion: catalogextractor.ExtractedTapSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("b", 40), TreeDigest: d, ArchiveDigest: d}, Formulae: []catalogextractor.ExtractedFormula{{SourcePath: "Formula/widget.rb", SourceDigest: d, Platforms: []catalogextractor.ExtractedPlatformFormula{{Tag: "x86_64_linux", Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1"}}}}}
	inner := &countingExtractor{value: value}
	root := t.TempDir()
	first, err := newCachedTapExtractor(root, time.Hour, inner)
	if err != nil {
		t.Fatal(err)
	}
	first.now = func() time.Time { return time.Unix(1000, 0) }
	if _, err := first.Extract(t.Context(), tap); err != nil {
		t.Fatal(err)
	}
	second, err := newCachedTapExtractor(root, time.Hour, inner)
	if err != nil {
		t.Fatal(err)
	}
	second.now = func() time.Time { return time.Unix(1100, 0) }
	if _, err := second.Extract(t.Context(), tap); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("extractor calls=%d", inner.calls)
	}
}

type countingArtifactBuilder struct{ calls int }

func (c *countingArtifactBuilder) Build(ctx context.Context, request *catalog.Request, core CoreSnapshot, catalogs map[catalog.TapID]*catalog.TapCatalog, node catalog.Node, platform catalog.Platform) (catalog.BottleArtifact, error) {
	c.calls++
	return (fakeArtifacts{}).Build(ctx, request, core, catalogs, node, platform)
}

func TestPersistentArtifactVerificationCacheSurvivesWrapperRestart(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	id, _ := catalog.ParseFormulaID("acme/tools/widget")
	d := "sha256:" + strings.Repeat("a", 64)
	formula := catalog.Formula{ID: id, Name: "widget", HomebrewFullName: string(id), SourcePath: "Formula/widget.rb", SourceDigest: d, StableVersion: "1"}
	catalogs := map[catalog.TapID]*catalog.TapCatalog{tap: {SchemaVersion: catalog.TapCatalogSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("b", 40), TreeDigest: d, ArchiveDigest: d}, PublishedAt: time.Unix(1, 0), Sequence: 1, Formulae: []catalog.Formula{formula}}}
	node := catalog.Node{ID: id, Tap: tap, Name: "widget", HomebrewFullName: string(id), FormulaVersion: "1", PkgVersion: "1"}
	platform := catalog.Platform{OS: "linux", Architecture: "amd64"}
	request := &catalog.Request{HomebrewCommit: strings.Repeat("c", 40)}
	core := &fakeSnapshot{info: metadata.SnapshotInfo{Digest: d}}
	inner := &countingArtifactBuilder{}
	root := t.TempDir()
	first, _ := newCachedArtifactBuilder(root, inner)
	if _, err := first.Build(t.Context(), request, core, catalogs, node, platform); err != nil {
		t.Fatal(err)
	}
	second, _ := newCachedArtifactBuilder(root, inner)
	if _, err := second.Build(t.Context(), request, core, catalogs, node, platform); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("artifact builder calls=%d", inner.calls)
	}
}
