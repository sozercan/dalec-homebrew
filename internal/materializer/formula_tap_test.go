package materializer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestCoreTapFormulaLocationMatchesPinnedHomebrewSharding(t *testing.T) {
	tests := map[string]string{
		"libtiff":     "Formula/lib/libtiff.rb",
		"little-cms2": "Formula/l/little-cms2.rb",
		"python@3.13": "Formula/p/python@3.13.rb",
		"4ti2":        "Formula/4/4ti2.rb",
		"foo+bar":     "Formula/f/foo+bar.rb",
	}
	for name, want := range tests {
		shard, filename, err := coreTapFormulaLocation(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := filepath.ToSlash(filepath.Join("Formula", shard, filename)); got != want {
			t.Fatalf("%s staged at %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"", "Bad", ".bad", "@bad", "../bad", "bad/name", "bad\\name", "bad!"} {
		if _, _, err := coreTapFormulaLocation(name); err == nil {
			t.Fatalf("unsafe Formula name %q accepted", name)
		}
	}
}

func TestStageVerifiedFormulaClosurePublishesSealedExactSourcesAndEvidence(t *testing.T) {
	prefix := formulaTapFixture(t)
	options := formulaTapTestOptions()
	epoch := time.Unix(1_800_000_000, 0).UTC()
	nodes := []resolution.Node{
		{Name: "python@3.13", FullName: "homebrew/core/python@3.13", PkgVersion: "3.13.9"},
		{Name: "libtiff", FullName: "homebrew/core/libtiff", PkgVersion: "4.7.2"},
	}
	record := &resolution.Record{SourceDateEpoch: epoch.Unix(), Nodes: nodes}
	verified := map[string]bottle.Result{}
	sources := map[string][]byte{}
	for _, node := range nodes {
		source := []byte("class " + map[string]string{"libtiff": "Libtiff", "python@3.13": "PythonAT313"}[node.Name] + " < Formula\nend\n")
		result := formulaStageResult(node, source)
		verified[node.Name] = result
		sources[node.Name] = source
	}

	var points []formulaTapStagePoint
	options.checkpoint = func(point formulaTapStagePoint) error {
		points = append(points, point)
		tapRoot := filepath.Join(prefix, filepath.FromSlash(coreTapRoot))
		switch point {
		case formulaTapBeforePublish:
			assertPathAbsent(t, filepath.Join(tapRoot, publishedFormulaDirName))
			assertFormulaTapMetadata(t, filepath.Join(tapRoot, formulaTapStagingName), 0o555, epoch)
		case formulaTapAfterPublish:
			assertPathAbsent(t, filepath.Join(tapRoot, formulaTapStagingName))
			assertFormulaTapMetadata(t, filepath.Join(tapRoot, publishedFormulaDirName), 0o555, epoch)
		}
		return nil
	}
	evidence, err := stageVerifiedFormulaClosure(prefix, record, verified, sources, options)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(points, formulaTapBeforePublish) || !slices.Contains(points, formulaTapAfterPublish) {
		t.Fatalf("transactional publication checkpoints missing: %v", points)
	}
	if len(evidence) != 2 || evidence[0].Formula != "libtiff" || evidence[1].Formula != "python@3.13" {
		t.Fatalf("staged evidence is not deterministic: %#v", evidence)
	}
	encoded, err := json.Marshal(Evidence{VerifiedBottles: []bottle.Result{verified["libtiff"]}, StagedFormulae: evidence})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if strings.Contains(string(encoded), string(source)) {
			t.Fatalf("Formula source leaked into materialization evidence: %s", encoded)
		}
	}

	for _, item := range evidence {
		filename := filepath.Join(prefix, filepath.FromSlash(coreTapRoot), filepath.FromSlash(item.TapPath))
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(data, sources[item.Formula]) {
			t.Fatalf("%s source changed", item.Formula)
		}
		assertFormulaTapMetadata(t, filename, 0o444, epoch)
		assertFormulaTapMetadata(t, filepath.Dir(filename), 0o555, epoch)
	}
	assertFormulaTapMetadata(t, filepath.Join(prefix, filepath.FromSlash(coreTapFormulaRoot)), 0o555, epoch)
	assertFormulaTapMetadata(t, filepath.Join(prefix, filepath.FromSlash(coreTapRoot)), 0o555, epoch)
}

func TestStageVerifiedFormulaClosureRejectsUnverifiedOrMutableTapState(t *testing.T) {
	epoch := time.Unix(1_800_000_000, 0).UTC()
	node := resolution.Node{Name: "hello", FullName: "homebrew/core/hello", PkgVersion: "1"}
	source := []byte("class Hello < Formula\nend\n")
	result := formulaStageResult(node, source)
	baseRecord := &resolution.Record{SourceDateEpoch: epoch.Unix(), Nodes: []resolution.Node{node}}

	tests := map[string]func(string, *resolution.Record, map[string]bottle.Result, map[string][]byte){
		"digest mismatch": func(_ string, _ *resolution.Record, results map[string]bottle.Result, _ map[string][]byte) {
			value := results["hello"]
			value.Formula.SHA256 = "sha256:" + strings.Repeat("f", 64)
			results["hello"] = value
		},
		"size mismatch": func(_ string, _ *resolution.Record, results map[string]bottle.Result, _ map[string][]byte) {
			value := results["hello"]
			value.Formula.Size++
			results["hello"] = value
		},
		"missing source": func(_ string, _ *resolution.Record, _ map[string]bottle.Result, sources map[string][]byte) {
			delete(sources, "hello")
		},
		"non-core identity": func(_ string, record *resolution.Record, _ map[string]bottle.Result, _ map[string][]byte) {
			record.Nodes[0].FullName = "someone/tap/hello"
		},
		"pre-existing live Formula tree": func(prefix string, _ *resolution.Record, _ map[string]bottle.Result, _ map[string][]byte) {
			parent := filepath.Join(prefix, filepath.FromSlash(coreTapRoot))
			mustChmod(t, parent, 0o755)
			if err := os.Mkdir(filepath.Join(parent, publishedFormulaDirName), 0o555); err != nil {
				t.Fatal(err)
			}
			mustChmod(t, parent, 0o555)
		},
		"pre-existing private transaction": func(prefix string, _ *resolution.Record, _ map[string]bottle.Result, _ map[string][]byte) {
			parent := filepath.Join(prefix, filepath.FromSlash(coreTapRoot))
			mustChmod(t, parent, 0o755)
			if err := os.Mkdir(filepath.Join(parent, formulaTapStagingName), 0o700); err != nil {
				t.Fatal(err)
			}
			mustChmod(t, parent, 0o555)
		},
		"writable repository path": func(prefix string, _ *resolution.Record, _ map[string]bottle.Result, _ map[string][]byte) {
			mustChmod(t, filepath.Join(prefix, filepath.FromSlash(coreTapRoot)), 0o755)
		},
		"writable prefix anchor": func(prefix string, _ *resolution.Record, _ map[string]bottle.Result, _ map[string][]byte) {
			mustChmod(t, prefix, 0o777)
		},
		"writable prefix ancestor": func(prefix string, _ *resolution.Record, _ map[string]bottle.Result, _ map[string][]byte) {
			mustChmod(t, filepath.Dir(prefix), 0o777)
		},
		"writable protected brew": func(prefix string, _ *resolution.Record, _ map[string]bottle.Result, _ map[string][]byte) {
			mustChmod(t, filepath.Join(prefix, filepath.FromSlash(protectedHomebrewBrew)), 0o755)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			prefix := formulaTapFixture(t)
			record := cloneFormulaStageRecord(t, baseRecord)
			results := map[string]bottle.Result{"hello": result}
			sources := map[string][]byte{"hello": append([]byte(nil), source...)}
			mutate(prefix, record, results, sources)
			if _, err := stageVerifiedFormulaClosure(prefix, record, results, sources, formulaTapTestOptions()); err == nil {
				t.Fatal("unsafe Formula staging input accepted")
			}
		})
	}
}

func TestProtectedPrefixRejectsRuntimeOwnedReplacementAnchor(t *testing.T) {
	prefix := formulaTapFixture(t)
	options := formulaTapTestOptions()
	options.runtimeUID = options.ownerUID
	options.runtimeGID = options.ownerGID
	if err := validateProtectedHomebrewRepository(prefix, options, false); err == nil || !strings.Contains(err.Error(), "owned by runtime uid") {
		t.Fatalf("runtime-owned prefix anchor was not rejected: %v", err)
	}
}

func TestStageVerifiedFormulaClosureRollsBackEveryInjectedFailure(t *testing.T) {
	points := []formulaTapStagePoint{
		formulaTapAfterParentWritable,
		formulaTapAfterPrivateRoot,
		formulaTapAfterFormulaFile,
		formulaTapAfterTreeSealed,
		formulaTapBeforePublish,
		formulaTapAfterPublish,
		formulaTapAfterParentSealed,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			prefix := formulaTapFixture(t)
			options := formulaTapTestOptions()
			epoch := time.Unix(1_800_000_000, 0).UTC()
			node := resolution.Node{Name: "hello", FullName: "homebrew/core/hello", PkgVersion: "1"}
			source := []byte("class Hello < Formula\nend\n")
			record := &resolution.Record{SourceDateEpoch: epoch.Unix(), Nodes: []resolution.Node{node}}
			verified := map[string]bottle.Result{"hello": formulaStageResult(node, source)}
			sources := map[string][]byte{"hello": source}
			parent := filepath.Join(prefix, filepath.FromSlash(coreTapRoot))
			original, err := os.Stat(parent)
			if err != nil {
				t.Fatal(err)
			}
			options.checkpoint = func(got formulaTapStagePoint) error {
				if got == point {
					return errors.New("injected failure")
				}
				return nil
			}
			if _, err := stageVerifiedFormulaClosure(prefix, record, verified, sources, options); err == nil || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("injected failure at %s was not returned: %v", point, err)
			}
			assertPathAbsent(t, filepath.Join(parent, publishedFormulaDirName))
			assertPathAbsent(t, filepath.Join(parent, formulaTapStagingName))
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("rollback left core tap entries: %v", entries)
			}
			after, err := os.Stat(parent)
			if err != nil {
				t.Fatal(err)
			}
			delta := after.ModTime().Sub(original.ModTime())
			if delta < 0 {
				delta = -delta
			}
			// Darwin's descriptor-based futimes API has microsecond precision.
			if after.Mode().Perm() != original.Mode().Perm() || delta > time.Microsecond {
				t.Fatalf("rollback did not restore parent metadata: before=%o/%s after=%o/%s", original.Mode().Perm(), original.ModTime(), after.Mode().Perm(), after.ModTime())
			}
			options.checkpoint = nil
			if _, err := stageVerifiedFormulaClosure(prefix, record, verified, sources, options); err != nil {
				t.Fatalf("retry after rollback failed: %v", err)
			}
		})
	}
}

func TestStageVerifiedFormulaClosureProductionOwnerRemainsRoot(t *testing.T) {
	if os.Geteuid() == 0 && os.Getegid() == 0 {
		t.Skip("test host already runs as root")
	}
	prefix := formulaTapFixture(t)
	node := resolution.Node{Name: "hello", FullName: "homebrew/core/hello", PkgVersion: "1"}
	source := []byte("class Hello < Formula\nend\n")
	record := &resolution.Record{SourceDateEpoch: 1_800_000_000, Nodes: []resolution.Node{node}}
	options := formulaTapTestOptions()
	options.ownerUID, options.ownerGID = 0, 0
	if _, err := stageVerifiedFormulaClosure(prefix, record, map[string]bottle.Result{"hello": formulaStageResult(node, source)}, map[string][]byte{"hello": source}, options); err == nil {
		t.Fatal("non-root-owned fixture satisfied production root ownership policy")
	}
}

func formulaTapFixture(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(base, "prefix")
	if err := os.MkdirAll(filepath.Join(prefix, filepath.FromSlash(coreTapRoot)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "Homebrew/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	brew := filepath.Join(prefix, filepath.FromSlash(protectedHomebrewBrew))
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	brewReal := filepath.Join(prefix, filepath.FromSlash(protectedHomebrewBrewReal))
	if err := os.WriteFile(brewReal, []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	logicalDir := filepath.Join(prefix, protectedHomebrewLogicalDir)
	if err := os.Mkdir(logicalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+filepath.FromSlash(protectedHomebrewBrewReal), filepath.Join(prefix, filepath.FromSlash(protectedHomebrewLogical))); err != nil {
		t.Fatal(err)
	}
	for current := filepath.Join(prefix, filepath.FromSlash(coreTapRoot)); current != prefix; current = filepath.Dir(current) {
		mustChmod(t, current, 0o555)
	}
	mustChmod(t, filepath.Join(prefix, "Homebrew/bin"), 0o555)
	mustChmod(t, filepath.Join(prefix, "Homebrew"), 0o555)
	mustChmod(t, logicalDir, 0o555)
	mustChmod(t, prefix, 0o755)
	t.Cleanup(func() {
		_ = filepath.Walk(prefix, func(name string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(name, 0o755)
			}
			return nil
		})
		_ = os.Chmod(base, 0o700)
	})
	return prefix
}

func formulaTapTestOptions() formulaTapStageOptions {
	runtimeUID := os.Geteuid() + 1
	if runtimeUID == 0 {
		runtimeUID++
	}
	runtimeGID := os.Getegid() + 1
	if runtimeGID == 0 {
		runtimeGID++
	}
	return formulaTapStageOptions{
		ownerUID: os.Geteuid(), ownerGID: os.Getegid(),
		runtimeUID: runtimeUID, runtimeGID: runtimeGID,
	}
}

func formulaStageResult(node resolution.Node, source []byte) bottle.Result {
	sum := sha256.Sum256(source)
	kegPrefix := pathJoin(node.Name, node.PkgVersion)
	return bottle.Result{
		Name: node.Name, PkgVersion: node.PkgVersion, KegPrefix: kegPrefix,
		Formula: bottle.FormulaEvidence{
			Path:   pathJoin(kegPrefix, ".brew", node.Name+".rb"),
			SHA256: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(source)),
		},
	}
}

func pathJoin(parts ...string) string { return strings.Join(parts, "/") }

func assertFormulaTapMetadata(t *testing.T, filename string, mode os.FileMode, epoch time.Time) {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode.Perm() || !info.ModTime().Equal(epoch) {
		t.Fatalf("%s metadata = mode %o mtime %s", filename, info.Mode().Perm(), info.ModTime())
	}
	uid, gid, known := snapshotOwnership(info)
	if !known || int(uid) != os.Geteuid() || int(gid) != os.Getegid() {
		t.Fatalf("%s ownership is not the fixture owner", filename)
	}
}

func assertPathAbsent(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or could not be inspected: %v", name, err)
	}
}

func mustChmod(t *testing.T, name string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}

func cloneFormulaStageRecord(t *testing.T, record *resolution.Record) *resolution.Record {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var cloned resolution.Record
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return &cloned
}
