package cataloggenerator

import (
	"context"
	"strings"
	"sync/atomic"
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

type blockingArtifactBuilder struct {
	started chan<- catalog.FormulaID
	release <-chan struct{}
	calls   atomic.Int32
}

func (b *blockingArtifactBuilder) Build(ctx context.Context, request *catalog.Request, core CoreSnapshot, catalogs map[catalog.TapID]*catalog.TapCatalog, node catalog.Node, platform catalog.Platform) (catalog.BottleArtifact, error) {
	b.calls.Add(1)
	select {
	case b.started <- node.ID:
	case <-ctx.Done():
		return catalog.BottleArtifact{}, ctx.Err()
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return catalog.BottleArtifact{}, ctx.Err()
	}
	return (fakeArtifacts{}).Build(ctx, request, core, catalogs, node, platform)
}

func TestArtifactVerificationCacheBuildsDistinctKeysConcurrently(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	formula := func(name string) catalog.Formula {
		id, _ := catalog.ParseFormulaID("acme/tools/" + name)
		return catalog.Formula{ID: id, Name: name, HomebrewFullName: string(id), SourcePath: "Formula/" + name + ".rb", SourceDigest: digest, StableVersion: "1"}
	}
	formulae := []catalog.Formula{formula("alpha"), formula("beta")}
	catalogs := map[catalog.TapID]*catalog.TapCatalog{tap: {
		SchemaVersion: catalog.TapCatalogSchemaVersion,
		Tap:           catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("b", 40), TreeDigest: digest, ArchiveDigest: digest},
		PublishedAt:   time.Unix(1, 0),
		Sequence:      1,
		Formulae:      formulae,
	}}
	platform := catalog.Platform{OS: "linux", Architecture: "amd64"}
	request := &catalog.Request{HomebrewCommit: strings.Repeat("c", 40)}
	core := &fakeSnapshot{info: metadata.SnapshotInfo{Digest: digest}}
	started := make(chan catalog.FormulaID, len(formulae))
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	inner := &blockingArtifactBuilder{started: started, release: release}
	cached, err := newCachedArtifactBuilder(t.TempDir(), inner)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, len(formulae))
	for _, entry := range formulae {
		node := catalog.Node{ID: entry.ID, Tap: tap, Name: entry.Name, HomebrewFullName: entry.HomebrewFullName, FormulaVersion: "1", PkgVersion: "1"}
		go func() {
			_, err := cached.Build(t.Context(), request, core, catalogs, node, platform)
			errs <- err
		}()
	}
	seen := make(map[catalog.FormulaID]struct{}, len(formulae))
	for range formulae {
		select {
		case id := <-started:
			seen[id] = struct{}{}
		case <-time.After(2 * time.Second):
			t.Fatal("distinct artifact cache keys were serialized")
		}
	}
	close(release)
	released = true
	for range formulae {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != len(formulae) || inner.calls.Load() != int32(len(formulae)) {
		t.Fatalf("started=%v calls=%d", seen, inner.calls.Load())
	}
	cached.mu.Lock()
	remainingLocks := len(cached.keyLocks)
	cached.mu.Unlock()
	if remainingLocks != 0 {
		t.Fatalf("artifact cache retained %d idle key locks", remainingLocks)
	}
}

func TestArtifactVerificationCacheSerializesOneKey(t *testing.T) {
	cached, err := newCachedArtifactBuilder(t.TempDir(), &countingArtifactBuilder{})
	if err != nil {
		t.Fatal(err)
	}
	unlockFirst := cached.lockArtifactKey("same-key")
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		unlockSecond := cached.lockArtifactKey("same-key")
		close(acquired)
		unlockSecond()
		close(done)
	}()
	select {
	case <-acquired:
		unlockFirst()
		t.Fatal("the same artifact cache key was acquired concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	unlockFirst()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting artifact cache key did not resume")
	}
}
