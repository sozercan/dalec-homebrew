package runtimefs

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	digest "github.com/opencontainers/go-digest"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestHomebrewRepositoryPruneEvidenceIsCompacted(t *testing.T) {
	fx := newFixture(t)
	writeFile(t, filepath.Join(fx.source, "Homebrew/Library/metadata.txt"), []byte("metadata\n"), 0o644)
	mustMkdirAll(t, filepath.Join(fx.source, "Homebrew/empty"))
	if err := os.Symlink("metadata.txt", filepath.Join(fx.source, "Homebrew/Library/metadata-link")); err != nil {
		t.Fatal(err)
	}

	// These use the same broad prune reason or generic omission path, but are not
	// descendants of the fully excluded Homebrew repository and must stay
	// individually inspectable.
	writeFile(t, filepath.Join(fx.source, "Library/legacy.txt"), []byte("legacy\n"), 0o644)
	writeFile(t, filepath.Join(fx.source, ".git/config"), []byte("config\n"), 0o600)
	writeFile(t, filepath.Join(fx.source, "include/hello.h"), []byte("header\n"), 0o644)

	result, err := Assemble(fx.source, testOutput(t, "runtime"), fx.record, fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	manifest := result.PruneManifest
	if PruneSchemaVersion != "dalec-homebrew-prune-manifest/v2" {
		t.Fatalf("unexpected compiled prune schema %q", PruneSchemaVersion)
	}
	if manifest.SchemaVersion != PruneSchemaVersion {
		t.Fatalf("prune schema = %q", manifest.SchemaVersion)
	}
	if len(manifest.Subtrees) != 1 {
		t.Fatalf("prune subtrees = %#v", manifest.Subtrees)
	}
	subtree := manifest.Subtrees[0]
	if subtree.Path != homebrewRepositoryRoot || subtree.Reason != PruneRepository {
		t.Fatalf("repository subtree = %#v", subtree)
	}
	if subtree.CommitmentSchema != PruneSubtreeCommitmentSchemaVersion {
		t.Fatalf("commitment schema = %q", subtree.CommitmentSchema)
	}
	if _, err := digest.Parse(subtree.CommitmentDigest); err != nil {
		t.Fatalf("commitment digest = %q: %v", subtree.CommitmentDigest, err)
	}
	count, regularBytes := sourceSubtreeStats(t, filepath.Join(fx.source, homebrewRepositoryRoot))
	if subtree.EntryCount != count || subtree.RegularBytes != regularBytes {
		t.Fatalf("subtree stats = count %d bytes %d, want count %d bytes %d", subtree.EntryCount, subtree.RegularBytes, count, regularBytes)
	}
	for _, entry := range manifest.Entries {
		if isWithin(entry.Path, homebrewRepositoryRoot) {
			t.Fatalf("compacted repository path remains explicit: %#v", entry)
		}
	}

	assertPruned(t, manifest, "Library/legacy.txt", PruneRepository)
	assertPruned(t, manifest, ".git/config", PruneRepository)
	assertPruned(t, manifest, "include/hello.h", PruneNotAllowlisted)
	assertPruned(t, manifest, "Cellar/hello/1.0/.brew/hello.rb", PruneFormulaMetadata)
	assertPruned(t, manifest, "Cellar/hello/1.0/.brew/hello.spdx.json", PrunePackageSBOM)
	assertPruned(t, manifest, "Cellar/hello/1.0/INSTALL_RECEIPT.json", PruneReceipt)
	assertPruned(t, manifest, "lib/ld.so", PruneRuntimeBase)
}

func TestPruneEvidencePartitionsSourceTree(t *testing.T) {
	fx := newFixture(t)
	writeFile(t, filepath.Join(fx.source, "Homebrew/Library/a"), []byte("a"), 0o644)
	mustMkdirAll(t, filepath.Join(fx.source, "Homebrew/Library/empty"))

	result, err := Assemble(fx.source, testOutput(t, "runtime"), fx.record, fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PruneManifest.Subtrees) != 1 {
		t.Fatalf("prune subtrees = %#v", result.PruneManifest.Subtrees)
	}
	subtree := result.PruneManifest.Subtrees[0]

	retained := make(map[string]struct{}, len(result.Inventory.Entries))
	for _, entry := range result.Inventory.Entries {
		retained[entry.Path] = struct{}{}
	}
	explicitlyPruned := make(map[string]struct{}, len(result.PruneManifest.Entries))
	for _, entry := range result.PruneManifest.Entries {
		if _, duplicate := explicitlyPruned[entry.Path]; duplicate {
			t.Fatalf("duplicate explicit prune path %q", entry.Path)
		}
		explicitlyPruned[entry.Path] = struct{}{}
		if _, overlap := retained[entry.Path]; overlap {
			t.Fatalf("path %q is both retained and explicitly pruned", entry.Path)
		}
	}

	total := 0
	summarized := 0
	regularBytes := int64(0)
	err = filepath.WalkDir(fx.source, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == fx.source {
			return nil
		}
		rel, err := filepath.Rel(fx.source, current)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		total++
		_, isRetained := retained[rel]
		_, isExplicit := explicitlyPruned[rel]
		isSummarized := isWithin(rel, subtree.Path)
		partitions := 0
		for _, present := range []bool{isRetained, isExplicit, isSummarized} {
			if present {
				partitions++
			}
		}
		if partitions != 1 {
			t.Fatalf("source path %q belongs to %d partitions (retained=%v explicit=%v summarized=%v)", rel, partitions, isRetained, isExplicit, isSummarized)
		}
		if isSummarized {
			summarized++
			info, err := os.Lstat(current)
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				regularBytes += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != len(retained)+len(explicitlyPruned)+subtree.EntryCount {
		t.Fatalf("partition count = %d, retained %d explicit %d summarized %d", total, len(retained), len(explicitlyPruned), subtree.EntryCount)
	}
	if summarized != subtree.EntryCount || regularBytes != subtree.RegularBytes {
		t.Fatalf("summarized source stats = count %d bytes %d, manifest count %d bytes %d", summarized, regularBytes, subtree.EntryCount, subtree.RegularBytes)
	}
}

func TestPruneSubtreeCommitmentIsOrderIndependent(t *testing.T) {
	entries := commitmentFixtureEntries()
	first, err := commitPrunedSubtree(homebrewRepositoryRoot, PruneRepository, entries)
	if err != nil {
		t.Fatal(err)
	}
	reversed := cloneSourceEntries(entries)
	slices.Reverse(reversed)
	second, err := commitPrunedSubtree(homebrewRepositoryRoot, PruneRepository, reversed)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("order changed commitment:\nfirst  %#v\nsecond %#v", first, second)
	}
	if first.EntryCount != len(entries) || first.RegularBytes != entries[2].size {
		t.Fatalf("commitment stats = %#v", first)
	}
}

func TestPruneSubtreeCommitmentBindsEveryTupleField(t *testing.T) {
	baselineEntries := commitmentFixtureEntries()
	baseline, err := commitPrunedSubtree(homebrewRepositoryRoot, PruneRepository, baselineEntries)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func([]*sourceEntry)
	}{
		{name: "path", mutate: func(entries []*sourceEntry) { entries[2].rel = "Homebrew/Library/renamed" }},
		{name: "type", mutate: func(entries []*sourceEntry) {
			entries[2].typeName = TypeSymlink
			entries[2].linkSource = "payload"
			entries[2].sha256 = ""
		}},
		{name: "mode", mutate: func(entries []*sourceEntry) { entries[2].mode = 0o600 }},
		{name: "setuid mode", mutate: func(entries []*sourceEntry) { entries[2].mode |= os.ModeSetuid }},
		{name: "setgid mode", mutate: func(entries []*sourceEntry) { entries[2].mode |= os.ModeSetgid }},
		{name: "sticky mode", mutate: func(entries []*sourceEntry) { entries[1].mode |= os.ModeSticky }},
		{name: "size", mutate: func(entries []*sourceEntry) { entries[2].size++ }},
		{name: "content", mutate: func(entries []*sourceEntry) { entries[2].sha256 = sha256String("changed") }},
		{name: "link target", mutate: func(entries []*sourceEntry) { entries[3].linkSource = "other" }},
		{name: "hardlink target", mutate: func(entries []*sourceEntry) { entries[4].hardlinkTo = "Homebrew/Library/other" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries := cloneSourceEntries(baselineEntries)
			tc.mutate(entries)
			changed, err := commitPrunedSubtree(homebrewRepositoryRoot, PruneRepository, entries)
			if err != nil {
				t.Fatal(err)
			}
			if changed.CommitmentDigest == baseline.CommitmentDigest {
				t.Fatalf("%s mutation did not change commitment %s", tc.name, baseline.CommitmentDigest)
			}
		})
	}
	differentReason, err := commitPrunedSubtree(homebrewRepositoryRoot, PruneTooling, cloneSourceEntries(baselineEntries))
	if err != nil {
		t.Fatal(err)
	}
	if differentReason.CommitmentDigest == baseline.CommitmentDigest {
		t.Fatal("prune reason did not change commitment")
	}
}

func TestHomebrewRepositoryCommitmentChangesWithScannedContent(t *testing.T) {
	firstFixture := newFixture(t)
	first, err := Assemble(firstFixture.source, testOutput(t, "first"), firstFixture.record, firstFixture.opts)
	if err != nil {
		t.Fatal(err)
	}
	secondFixture := newFixture(t)
	writeFile(t, filepath.Join(secondFixture.source, "Homebrew/bin/brew"), []byte("changed\n"), 0o755)
	second, err := Assemble(secondFixture.source, testOutput(t, "second"), secondFixture.record, secondFixture.opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.PruneManifest.Subtrees[0].CommitmentDigest == second.PruneManifest.Subtrees[0].CommitmentDigest {
		t.Fatal("repository content tampering did not change the scanned commitment")
	}
}

func TestHomebrewRepositoryCompactionFallsBackForSensitivePaths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sourceEntry)
	}{
		{name: "package attribution", mutate: func(entry *sourceEntry) { entry.packageName = "hello" }},
		{name: "metadata export", mutate: func(entry *sourceEntry) { entry.metadataExport = "formula" }},
		{name: "mixed prune reason", mutate: func(entry *sourceEntry) { entry.pruneReason = PruneTooling }},
		{name: "retained descendant", mutate: func(entry *sourceEntry) { entry.retain = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := &sourceEntry{rel: homebrewRepositoryRoot, typeName: TypeDirectory, mode: 0o755, pruneReason: PruneRepository}
			file := &sourceEntry{rel: "Homebrew/file", typeName: TypeRegular, mode: 0o644, size: 4, sha256: sha256String("data"), pruneReason: PruneRepository}
			tc.mutate(file)
			scan := &sourceScan{
				entries: []*sourceEntry{root, file},
				pruned:  []*sourceEntry{root},
				byPath:  map[string]*sourceEntry{root.rel: root, file.rel: file},
			}
			if !file.retain {
				scan.pruned = append(scan.pruned, file)
			}
			manifest, err := buildPruneManifest(scan, &resolution.Record{PolicyVersion: "test", SourceDateEpoch: 1}, &normalizedPolicy{installPrefix: DefaultInstallPrefix, digest: "sha256:test"}, "sha256:resolution")
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.Subtrees) != 0 {
				t.Fatalf("sensitive subtree was compacted: %#v", manifest.Subtrees)
			}
			if len(manifest.Entries) != len(scan.pruned) {
				t.Fatalf("explicit entries = %d, want %d", len(manifest.Entries), len(scan.pruned))
			}
		})
	}
}

func commitmentFixtureEntries() []*sourceEntry {
	content := sha256String("data")
	return []*sourceEntry{
		{rel: "Homebrew", typeName: TypeDirectory, mode: 0o755, size: 4096},
		{rel: "Homebrew/Library", typeName: TypeDirectory, mode: 0o755, size: 4096},
		{rel: "Homebrew/Library/file", typeName: TypeRegular, mode: 0o644, size: 4, sha256: content},
		{rel: "Homebrew/Library/link", typeName: TypeSymlink, mode: 0o777, size: 4, linkSource: "file"},
		{rel: "Homebrew/Library/hardlink", typeName: TypeHardlink, mode: 0o644, size: 4, sha256: content, hardlinkTo: "Homebrew/Library/file"},
	}
}

func cloneSourceEntries(entries []*sourceEntry) []*sourceEntry {
	out := make([]*sourceEntry, len(entries))
	for i, entry := range entries {
		copy := *entry
		out[i] = &copy
	}
	return out
}

func sourceSubtreeStats(t *testing.T, root string) (int, int64) {
	t.Helper()
	count := 0
	regularBytes := int64(0)
	if err := filepath.WalkDir(root, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			regularBytes += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count, regularBytes
}
