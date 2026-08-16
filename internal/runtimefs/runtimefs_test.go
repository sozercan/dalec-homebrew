package runtimefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

type chownCall struct {
	path    string
	uid     int
	gid     int
	symlink bool
}

type fixture struct {
	source string
	record *resolution.Record
	opts   Options
	calls  *[]chownCall
}

func TestAssembleCleanRuntimeAndEvidence(t *testing.T) {
	fx := newFixture(t)
	output := testOutput(t, "runtime")

	result, err := Assemble(fx.source, output, fx.record, fx.opts)
	if err != nil {
		t.Fatal(err)
	}

	mustExist(t, filepath.Join(output, "Cellar/hello/1.0/bin/hello"))
	mustExist(t, filepath.Join(output, "Cellar/hello/1.0/share/doc/hello/LICENSE"))
	mustNotExist(t, filepath.Join(output, "Cellar/hello/1.0/.brew"))
	mustNotExist(t, filepath.Join(output, "Cellar/hello/1.0/INSTALL_RECEIPT.json"))
	mustNotExist(t, filepath.Join(output, "bin/brew"))
	mustNotExist(t, filepath.Join(output, "Homebrew"))
	mustNotExist(t, filepath.Join(output, "var/homebrew"))

	optTarget, err := os.Readlink(filepath.Join(output, "opt/hello"))
	if err != nil {
		t.Fatal(err)
	}
	if optTarget != DefaultInstallPrefix+"/Cellar/hello/1.0" {
		t.Fatalf("opt target = %q", optTarget)
	}
	binTarget, err := os.Readlink(filepath.Join(output, "bin/hello"))
	if err != nil {
		t.Fatal(err)
	}
	if binTarget != "../Cellar/hello/1.0/bin/hello" {
		t.Fatalf("bin target = %q", binTarget)
	}

	cellarLib, err := os.Stat(filepath.Join(output, "Cellar/hello/1.0/lib/libhello.so"))
	if err != nil {
		t.Fatal(err)
	}
	globalLib, err := os.Stat(filepath.Join(output, "lib/libhello.so"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(cellarLib, globalLib) {
		t.Fatal("global library hardlink relationship was not preserved")
	}

	assertMode(t, filepath.Join(output, "Cellar/hello/1.0/bin/hello"), 0o555)
	assertMode(t, filepath.Join(output, "Cellar/hello/1.0/share/doc/hello/LICENSE"), 0o444)
	assertMode(t, filepath.Join(output, "etc/hello.conf"), 0o644)
	assertMode(t, filepath.Join(output, "var/hello"), 0o755)
	assertMode(t, filepath.Join(output, "var/hello/state.db"), 0o644)
	assertMtime(t, filepath.Join(output, "Cellar/hello/1.0/bin/hello"), fx.record.SourceDateEpoch)

	if err := Verify(output, fx.record, &result.Inventory, fx.opts); err != nil {
		t.Fatalf("verify assembled output: %v", err)
	}
	if len(*fx.calls) == 0 {
		t.Fatal("injected chown was not called")
	}
	assertChownSuffix(t, *fx.calls, "Cellar/hello/1.0/bin/hello", 0, 0, false)
	assertChownSuffix(t, *fx.calls, "var/hello/state.db", fx.record.Runtime.UID, fx.record.Runtime.GID, false)
	assertChownSuffix(t, *fx.calls, "opt/hello", 0, 0, true)

	if !slices.IsSortedFunc(result.Inventory.Entries, func(a, b InventoryEntry) int { return strings.Compare(a.Path, b.Path) }) {
		t.Fatal("inventory is not sorted")
	}
	if !slices.IsSortedFunc(result.PruneManifest.Entries, func(a, b PruneEntry) int { return strings.Compare(a.Path, b.Path) }) {
		t.Fatal("prune manifest is not sorted")
	}
	assertPruned(t, result.PruneManifest, "bin/brew", PruneBrewExecutable)
	assertPruned(t, result.PruneManifest, "Cellar/hello/1.0/.brew/hello.rb", PruneFormulaMetadata)
	assertPruned(t, result.PruneManifest, "Cellar/hello/1.0/INSTALL_RECEIPT.json", PruneReceipt)
	assertPruned(t, result.PruneManifest, "lib/ld.so", PruneRuntimeBase)
	assertPruned(t, result.PruneManifest, "share/info/dir", PruneManagerState)

	if len(result.RuntimeManifest.Packages) != 1 || result.RuntimeManifest.Packages[0].Name != "hello" {
		t.Fatalf("runtime packages = %#v", result.RuntimeManifest.Packages)
	}
	metadata := result.RuntimeManifest.Packages[0].ExportedMetadata
	if len(metadata) != 3 {
		t.Fatalf("exported metadata count = %d, want 3: %#v", len(metadata), metadata)
	}
	if result.RuntimeManifest.InventoryDigest != result.Evidence.InventoryDigest || result.RuntimeManifest.PruneManifestDigest != result.Evidence.PruneDigest || result.RuntimeManifest.SBOMDigest != result.Evidence.SBOMDigest {
		t.Fatal("runtime manifest evidence digests disagree")
	}
	if result.SBOM.SPDXVersion != "SPDX-2.3" || len(result.SBOM.Packages) != 1 {
		t.Fatalf("unexpected SBOM: %#v", result.SBOM)
	}
	if result.SBOM.Packages[0].FilesAnalyzed || len(result.SBOM.Packages[0].HasFiles) != 0 {
		t.Fatal("unanalyzed package must not claim contained files")
	}
	for _, relationship := range result.SBOM.Relationships {
		if relationship.RelationshipType == "CONTAINS" {
			t.Fatal("unanalyzed package has CONTAINS relationship")
		}
	}
	for _, file := range result.SBOM.Files {
		if !strings.HasPrefix(file.FileName, "./") {
			t.Fatalf("SPDX file name is not relative: %q", file.FileName)
		}
	}
	if !sbomContainsFile(result.SBOM, "./Cellar/hello/1.0/share/doc/hello/LICENSE", "TEXT") {
		t.Fatal("SBOM does not preserve the license file")
	}

	for name, data := range map[string][]byte{
		InventoryFileName: result.Evidence.Inventory,
		PruneFileName:     result.Evidence.Prune,
		ManifestFileName:  result.Evidence.RuntimeManifest,
		SBOMFileName:      result.Evidence.SBOM,
	} {
		if !json.Valid(data) {
			t.Fatalf("%s is invalid JSON", name)
		}
		if bytes.HasSuffix(data, []byte("\n")) {
			t.Fatalf("%s is not canonical newline-free JSON", name)
		}
	}

	evidenceDir := filepath.Join(t.TempDir(), "evidence")
	if err := result.WriteEvidence(evidenceDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{InventoryFileName, PruneFileName, ManifestFileName, SBOMFileName} {
		mustExist(t, filepath.Join(evidenceDir, name))
		assertMode(t, filepath.Join(evidenceDir, name), 0o444)
		assertMtime(t, filepath.Join(evidenceDir, name), fx.record.SourceDateEpoch)
	}
}

func TestCustomInstallPrefixAndExistingEmptyOutput(t *testing.T) {
	fx := newFixture(t)
	fx.opts.InstallPrefix = "/opt/dalec-homebrew"
	fx.record.Runtime.GeneratedPATH = []string{"/opt/dalec-homebrew/bin"}
	fx.record.Runtime.WritablePaths = []string{"/opt/dalec-homebrew/var/hello"}
	digest, err := PolicyDigest(fx.opts.Allowlist, fx.opts.InstallPrefix, fx.record.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	fx.record.PruningPolicyDigest = digest
	output := testOutput(t, "runtime")
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Assemble(fx.source, output, fx.record, fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(output, "opt/hello"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "/opt/dalec-homebrew/Cellar/hello/1.0" {
		t.Fatalf("rewritten opt target = %q", target)
	}
	if result.Inventory.Prefix != "/opt/dalec-homebrew" || result.RuntimeManifest.Prefix != "/opt/dalec-homebrew" {
		t.Fatalf("custom prefix missing from evidence: %#v %#v", result.Inventory.Prefix, result.RuntimeManifest.Prefix)
	}
	if err := Verify(output, fx.record, &result.Inventory, fx.opts); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceIsDeterministicAcrossAssemblies(t *testing.T) {
	fx := newFixture(t)
	firstOutput := testOutput(t, "one")
	first, err := Assemble(fx.source, firstOutput, fx.record, fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	secondOutput := testOutput(t, "two")
	second, err := Assemble(fx.source, secondOutput, fx.record, fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	pairs := [][2][]byte{
		{first.Evidence.Inventory, second.Evidence.Inventory},
		{first.Evidence.Prune, second.Evidence.Prune},
		{first.Evidence.RuntimeManifest, second.Evidence.RuntimeManifest},
		{first.Evidence.SBOM, second.Evidence.SBOM},
	}
	for i, pair := range pairs {
		if !bytes.Equal(pair[0], pair[1]) {
			t.Fatalf("evidence payload %d differs across identical assemblies", i)
		}
	}
}

func TestVerifyRejectsTamperedOutput(t *testing.T) {
	fx := newFixture(t)
	output := testOutput(t, "runtime")
	result, err := Assemble(fx.source, output, fx.record, fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(output, "Cellar/hello/1.0/bin/hello")
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Verify(output, fx.record, &result.Inventory, fx.opts); errorCode(err) != CodeVerification && errorCode(err) != CodeUnexpectedWritable {
		t.Fatalf("verify error = %v, code = %q", err, errorCode(err))
	}
}

func TestRejectsDanglingAndEscapingRetainedSymlinks(t *testing.T) {
	t.Run("dangling", func(t *testing.T) {
		fx := newFixture(t)
		link := filepath.Join(fx.source, "bin/hello")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../Cellar/hello/1.0/bin/missing", link); err != nil {
			t.Fatal(err)
		}
		_, err := Assemble(fx.source, filepath.Join(t.TempDir(), "out"), fx.record, fx.opts)
		if errorCode(err) != CodeDanglingLink {
			t.Fatalf("error = %v, code = %q", err, errorCode(err))
		}
	})

	t.Run("escaping", func(t *testing.T) {
		fx := newFixture(t)
		link := filepath.Join(fx.source, "bin/hello")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/passwd", link); err != nil {
			t.Fatal(err)
		}
		_, err := Assemble(fx.source, filepath.Join(t.TempDir(), "out"), fx.record, fx.opts)
		if errorCode(err) != CodeUnsafeLink {
			t.Fatalf("error = %v, code = %q", err, errorCode(err))
		}
	})
}

func TestRejectsUnexpectedKegAndSetID(t *testing.T) {
	t.Run("extra keg", func(t *testing.T) {
		fx := newFixture(t)
		writeFile(t, filepath.Join(fx.source, "Cellar/evil/9/bin/evil"), []byte("evil"), 0o755)
		_, err := Assemble(fx.source, filepath.Join(t.TempDir(), "out"), fx.record, fx.opts)
		if errorCode(err) != CodeUnexpectedKeg {
			t.Fatalf("error = %v, code = %q", err, errorCode(err))
		}
	})

	t.Run("setid", func(t *testing.T) {
		fx := newFixture(t)
		filename := filepath.Join(fx.source, "Cellar/hello/1.0/bin/hello")
		if err := os.Chmod(filename, 0o4755); err != nil {
			t.Skipf("setting setuid mode is unavailable: %v", err)
		}
		info, err := os.Lstat(filename)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSetuid == 0 {
			t.Skip("filesystem cleared setuid bit")
		}
		_, err = Assemble(fx.source, filepath.Join(t.TempDir(), "out"), fx.record, fx.opts)
		if errorCode(err) != CodeUnsafeMode {
			t.Fatalf("error = %v, code = %q", err, errorCode(err))
		}
	})
}

func TestRejectsHardlinkAcrossWritableBoundary(t *testing.T) {
	fx := newFixture(t)
	if err := os.Link(
		filepath.Join(fx.source, "Cellar/hello/1.0/lib/libhello.so"),
		filepath.Join(fx.source, "var/hello/code-link"),
	); err != nil {
		t.Fatal(err)
	}
	_, err := Assemble(fx.source, filepath.Join(t.TempDir(), "out"), fx.record, fx.opts)
	if errorCode(err) != CodeUnexpectedWritable {
		t.Fatalf("error = %v, code = %q", err, errorCode(err))
	}
}

func TestWritableRulesMustMatchResolution(t *testing.T) {
	fx := newFixture(t)
	fx.record.Runtime.WritablePaths = nil
	_, err := Assemble(fx.source, filepath.Join(t.TempDir(), "out"), fx.record, fx.opts)
	if errorCode(err) != CodeInvalidOptions {
		t.Fatalf("error = %v, code = %q", err, errorCode(err))
	}
}

func TestRuntimeBaseLoaderTargetMustMatchPlatform(t *testing.T) {
	fx := newFixture(t)
	loader := filepath.Join(fx.source, "lib/ld.so")
	if err := os.Remove(loader); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/lib/ld-linux-aarch64.so.1", loader); err != nil {
		t.Fatal(err)
	}
	_, err := Assemble(fx.source, filepath.Join(t.TempDir(), "out"), fx.record, fx.opts)
	if errorCode(err) != CodeUnsafeLink {
		t.Fatalf("error = %v, code = %q", err, errorCode(err))
	}
}

func TestBrewedGlibcLoaderIsRetained(t *testing.T) {
	policy := &normalizedPolicy{
		installPrefix: DefaultInstallPrefix,
		architecture:  "amd64",
		allowlist:     normalizedAllowlist{Lib: true},
		nodes:         map[string]resolution.Node{"glibc": {Name: "glibc", FullName: "homebrew/core/glibc", PkgVersion: "2"}},
	}
	entry := &sourceEntry{rel: "lib/ld.so", typeName: TypeSymlink, linkSource: DefaultInstallPrefix + "/opt/glibc/bin/ld.so"}
	if err := classifyRetention(entry, policy); err != nil {
		t.Fatal(err)
	}
	if !entry.retain || entry.pruneReason != "" {
		t.Fatalf("retain=%v prune=%q", entry.retain, entry.pruneReason)
	}
	policy.nodes = map[string]resolution.Node{}
	if err := classifyRetention(entry, policy); errorCode(err) != CodeUnsafeLink {
		t.Fatalf("error=%v code=%q", err, errorCode(err))
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "prefix")
	keg := filepath.Join(root, "Cellar/hello/1.0")
	writeFile(t, filepath.Join(keg, "bin/hello"), []byte("#!/bin/sh\necho hello\n"), 0o755)
	writeFile(t, filepath.Join(keg, "lib/libhello.so"), []byte("library\n"), 0o644)
	writeFile(t, filepath.Join(keg, "share/doc/hello/LICENSE"), []byte("MIT License\n"), 0o644)
	writeFile(t, filepath.Join(keg, ".brew/hello.rb"), []byte("class Hello < Formula\nend\n"), 0o644)
	writeFile(t, filepath.Join(keg, ".brew/hello.spdx.json"), []byte(`{"spdxVersion":"SPDX-2.3"}`), 0o644)
	writeFile(t, filepath.Join(keg, "INSTALL_RECEIPT.json"), []byte(`{
		"name":"hello",
		"full_name":"homebrew/core/hello",
		"pkg_version":"1.0",
		"revision":0,
		"bottle_rebuild":0,
		"built_as_bottle":true,
		"poured_from_bottle":true,
		"arch":"x86_64",
		"runtime_dependencies":[],
		"source":{"spec":"stable","tap":"homebrew/core","versions":{"stable":"1.0","version_scheme":0}}
	}`), 0o644)

	mustMkdirAll(t, filepath.Join(root, "opt"))
	if err := os.Symlink(keg, filepath.Join(root, "opt/hello")); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(root, "bin"))
	if err := os.Symlink("../Cellar/hello/1.0/bin/hello", filepath.Join(root, "bin/hello")); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(root, "lib"))
	if err := os.Symlink("/lib64/ld-linux-x86-64.so.2", filepath.Join(root, "lib/ld.so")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(keg, "lib/libhello.so"), filepath.Join(root, "lib/libhello.so")); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(root, "share/doc"))
	if err := os.Symlink("../../Cellar/hello/1.0/share/doc/hello", filepath.Join(root, "share/doc/hello")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "share/info/dir"), []byte("generated info index\n"), 0o644)
	writeFile(t, filepath.Join(root, "etc/hello.conf"), []byte("enabled=true\n"), 0o666)
	writeFile(t, filepath.Join(root, "var/hello/state.db"), []byte("state\n"), 0o666)

	writeFile(t, filepath.Join(root, "Homebrew/bin/brew"), []byte("manager\n"), 0o755)
	if err := os.Symlink("../Homebrew/bin/brew", filepath.Join(root, "bin/brew")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "var/homebrew/cache/download"), []byte("cache\n"), 0o644)
	writeFile(t, filepath.Join(root, "logs/install.log"), []byte("log\n"), 0o644)
	writeFile(t, filepath.Join(root, "libexec/dalec-homebrew-materializer"), []byte("tool\n"), 0o755)

	record := validResolutionRecord()
	calls := []chownCall{}
	opts := Options{
		InstallPrefix: DefaultInstallPrefix,
		Allowlist: Allowlist{
			Cellar: true,
			Opt:    true,
			Bin:    true,
			Lib:    true,
			Share:  true,
			Etc:    []PathRule{{Path: "hello.conf", Package: "hello", Required: true}},
			Var:    []PathRule{{Path: "hello", Package: "hello", Writable: true, Required: true}},
		},
		Chown: func(path string, uid, gid int, symlink bool) error {
			calls = append(calls, chownCall{path: path, uid: uid, gid: gid, symlink: symlink})
			return nil
		},
	}
	digest, err := PolicyDigest(opts.Allowlist, opts.InstallPrefix, record.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	record.PruningPolicyDigest = digest
	return fixture{source: root, record: record, opts: opts, calls: &calls}
}

func TestPruneOptionalDependencyTooling(t *testing.T) {
	entry := func(rel, packageName string) *sourceEntry {
		return &sourceEntry{rel: rel, typeName: TypeSymlink, retain: true, packageName: packageName}
	}
	t.Run("transitive llvm tooling", func(t *testing.T) {
		scan := &sourceScan{entries: []*sourceEntry{entry("bin/scan-build-py", "llvm@21")}}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"llvm@21": {Name: "llvm@21", FullName: "homebrew/core/llvm@21"}}, requested: map[string]struct{}{}})
		if scan.entries[0].retain || scan.entries[0].pruneReason != PruneOptionalTooling {
			t.Fatalf("optional LLVM tool was not pruned: %#v", scan.entries[0])
		}
	})
	t.Run("requested llvm tooling", func(t *testing.T) {
		scan := &sourceScan{entries: []*sourceEntry{entry("bin/scan-build-py", "llvm@21")}}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"llvm@21": {Name: "llvm@21", FullName: "homebrew/core/llvm@21"}}, requested: map[string]struct{}{"llvm@21": {}}})
		if !scan.entries[0].retain {
			t.Fatal("requested LLVM tool was pruned")
		}
	})
	t.Run("transitive core libpsl tooling", func(t *testing.T) {
		scan := &sourceScan{entries: []*sourceEntry{entry("bin/psl-make-dafsa", "libpsl")}}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"libpsl": {Name: "libpsl", FullName: "homebrew/core/libpsl"}}, requested: map[string]struct{}{}})
		if scan.entries[0].retain || scan.entries[0].pruneReason != PruneOptionalTooling {
			t.Fatalf("optional libpsl tool was not pruned: %#v", scan.entries[0])
		}
	})
	t.Run("requested core libpsl tooling", func(t *testing.T) {
		scan := &sourceScan{entries: []*sourceEntry{entry("bin/psl-make-dafsa", "libpsl")}}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"libpsl": {Name: "libpsl", FullName: "homebrew/core/libpsl"}}, requested: map[string]struct{}{"libpsl": {}}})
		if !scan.entries[0].retain {
			t.Fatal("requested libpsl tool was pruned")
		}
	})
	t.Run("other libpsl path", func(t *testing.T) {
		scan := &sourceScan{entries: []*sourceEntry{entry("bin/psl", "libpsl")}}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"libpsl": {Name: "libpsl", FullName: "homebrew/core/libpsl"}}, requested: map[string]struct{}{}})
		if !scan.entries[0].retain {
			t.Fatal("unlisted libpsl executable was pruned")
		}
	})
	t.Run("copied libpsl executable", func(t *testing.T) {
		copied := entry("bin/psl-make-dafsa", "libpsl")
		copied.typeName = TypeRegular
		scan := &sourceScan{entries: []*sourceEntry{copied}}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"libpsl": {Name: "libpsl", FullName: "homebrew/core/libpsl"}}, requested: map[string]struct{}{}})
		if !scan.entries[0].retain {
			t.Fatal("non-symlink libpsl executable was pruned")
		}
	})
	t.Run("V1 non-core libpsl spoof", func(t *testing.T) {
		scan := &sourceScan{entries: []*sourceEntry{entry("bin/psl-make-dafsa", "libpsl")}}
		node := resolution.Node{Name: "libpsl", FullName: "acme/tools/libpsl"}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"libpsl": node}, requested: map[string]struct{}{}})
		if !scan.entries[0].retain {
			t.Fatal("non-core V1 libpsl received optional-tooling prune capability")
		}
	})
	t.Run("V2 exact core libpsl capability", func(t *testing.T) {
		scan := &sourceScan{entries: []*sourceEntry{entry("bin/psl-make-dafsa", "libpsl")}}
		node := resolution.Node{Name: "libpsl", FullName: "homebrew/core/libpsl", PolicyFormulaID: "homebrew/core/libpsl"}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"libpsl": node}, requested: map[string]struct{}{}})
		if scan.entries[0].retain || scan.entries[0].pruneReason != PruneOptionalTooling {
			t.Fatalf("V2-authorized libpsl tool was not pruned: %#v", scan.entries[0])
		}
	})
	t.Run("V2 non-core libpsl spoof", func(t *testing.T) {
		scan := &sourceScan{entries: []*sourceEntry{entry("bin/psl-make-dafsa", "libpsl")}}
		node := resolution.Node{Name: "libpsl", FullName: "homebrew/core/libpsl", PolicyFormulaID: "acme/tools/libpsl"}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{nodes: map[string]resolution.Node{"libpsl": node}, requested: map[string]struct{}{}})
		if !scan.entries[0].retain {
			t.Fatal("non-core V2 libpsl received optional-tooling prune capability")
		}
	})
	t.Run("unrelated package", func(t *testing.T) {
		other := entry("bin/scan-build-py", "other")
		scan := &sourceScan{entries: []*sourceEntry{other}}
		pruneOptionalDependencyTooling(scan, &normalizedPolicy{requested: map[string]struct{}{}})
		if !other.retain {
			t.Fatal("unrelated global executable was pruned")
		}
	})
}

func validResolutionRecord() *resolution.Record {
	tm := time.Unix(1_800_000_000, 0).UTC()
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	layer := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	desc := func(value string) resolution.Descriptor {
		return resolution.Descriptor{Digest: value, Size: 123, MediaType: "application/test"}
	}
	return &resolution.Record{
		SchemaVersion: resolution.SchemaVersion,
		PolicyVersion: resolution.PolicyVersion,
		Input: resolution.Input{
			DalecSpecDigest: d,
			Platform:        resolution.Platform{OS: "linux", Architecture: "amd64"},
		},
		Metadata: resolution.MetadataSnapshot{
			Digest: d, FormulaDigest: d, MigrationDigest: d, FormulaEnvelopeDigest: d, MigrationEnvelopeDigest: d, FormulaFreshnessSource: "signed-payload", MigrationFreshnessSource: "signed-payload",
			GeneratedAt:  tm,
			FetchedAt:    tm,
			FormulaURL:   "https://example.invalid/formula",
			MigrationURL: "https://example.invalid/migrations",
			Signatures:   []resolution.Signature{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}}, FormulaSignatures: []resolution.Signature{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}}, MigrationSignatures: []resolution.Signature{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}},
		},
		ResolvedAt:      tm,
		SourceDateEpoch: tm.Unix(),
		Requested:       []resolution.RequestedRoot{{Requested: "hello", Canonical: "hello"}},
		Nodes: []resolution.Node{{
			Name:            "hello",
			FullName:        "homebrew/core/hello",
			FormulaVersion:  "1.0",
			PkgVersion:      "1.0",
			License:         "MIT",
			ExecutablePaths: []string{"bin/hello"},
			Bottle: resolution.Bottle{
				Tag:        "x86_64_linux",
				Filename:   "hello--1.0.x86_64_linux.bottle.tar.gz",
				Repository: "ghcr.io/homebrew/core/hello",
				Index:      desc(d),
				Manifest: func() resolution.Descriptor {
					value := desc(d)
					value.Platform = &resolution.Platform{OS: "linux", Architecture: "amd64"}
					return value
				}(),
				Config:         desc(d),
				Layer:          desc(layer),
				HomebrewSHA256: strings.TrimPrefix(layer, "sha256:"),
				Tab:            resolution.BottleTab{Arch: "x86_64"},
			},
		}},
		InstallOrder: []string{"hello"},
		Components:   resolution.Components{FrontendRef: "ghcr.io/x/f@" + d, RuntimeBaseRef: "ghcr.io/x/b@" + d, MaterializerRef: "ghcr.io/x/m@" + d, HomebrewCommit: strings.Repeat("a", 40), RubyRuntime: "portable-ruby-4.0.6", VerificationKeys: d, DalecModule: "v1", BuildKitModule: "v1"},
		Runtime: resolution.RuntimePolicy{
			User:          "linuxbrew",
			UID:           1000,
			GID:           1000,
			GeneratedPATH: []string{DefaultInstallPrefix + "/bin"},
			WritablePaths: []string{DefaultInstallPrefix + "/var/hello"},
			CPUBaseline:   "core2",
		},
		AttestationPolicy: resolution.AttestationPolicy{Waiver: "homebrew-jws-and-verified-oci-chain-v1"},
	}
}

func testOutput(t *testing.T, name string) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), name)
	t.Cleanup(func() { makeTreeRemovable(output) })
	return output
}

func writeFile(t *testing.T, filename string, data []byte, mode os.FileMode) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(filename))
	if err := os.WriteFile(filename, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustExist(t *testing.T, filename string) {
	t.Helper()
	if _, err := os.Lstat(filename); err != nil {
		t.Fatalf("expected %s to exist: %v", filename, err)
	}
}

func mustNotExist(t *testing.T, filename string) {
	t.Helper()
	if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, got %v", filename, err)
	}
}

func assertMode(t *testing.T, filename string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %04o, want %04o", filename, info.Mode().Perm(), want)
	}
}

func assertMtime(t *testing.T, filename string, want int64) {
	t.Helper()
	info, err := os.Lstat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Unix() != want {
		t.Fatalf("%s mtime = %d, want %d", filename, info.ModTime().Unix(), want)
	}
}

func assertChownSuffix(t *testing.T, calls []chownCall, suffix string, uid, gid int, symlink bool) {
	t.Helper()
	suffix = filepath.FromSlash(suffix)
	for _, call := range calls {
		if strings.HasSuffix(call.path, suffix) && call.uid == uid && call.gid == gid && call.symlink == symlink {
			return
		}
	}
	t.Fatalf("missing chown call for %s to %d:%d symlink=%v; calls=%#v", suffix, uid, gid, symlink, calls)
}

func assertPruned(t *testing.T, manifest PruneManifest, name string, reason PruneReason) {
	t.Helper()
	for _, entry := range manifest.Entries {
		if entry.Path == name {
			if entry.Reason != reason {
				t.Fatalf("prune reason for %s = %q, want %q", name, entry.Reason, reason)
			}
			return
		}
	}
	t.Fatalf("missing prune entry for %s", name)
}

func sbomContainsFile(sbom SPDXDocument, filename, fileType string) bool {
	for _, file := range sbom.Files {
		if file.FileName == filename && slices.Contains(file.FileTypes, fileType) {
			return true
		}
	}
	return false
}

func TestRejectsMismatchedGlobalCopyAttribution(t *testing.T) {
	fx := newFixture(t)
	global := filepath.Join(fx.source, "lib/libhello.so")
	if err := os.Remove(global); err != nil {
		t.Fatal(err)
	}
	writeFile(t, global, []byte("attacker-controlled replacement\n"), 0o644)
	_, err := Assemble(fx.source, testOutput(t, "runtime"), fx.record, fx.opts)
	if errorCode(err) != CodeUnattributed {
		t.Fatalf("error=%v code=%s", err, errorCode(err))
	}
}

func TestRejectsSourceOutputOverlapThroughSymlinkAlias(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	source := filepath.Join(real, "source")
	mustMkdirAll(t, source)
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	_, _, err := normalizeHostRoots(source, filepath.Join(alias, "source", "out"))
	if errorCode(err) != CodeInvalidOptions {
		t.Fatalf("error=%v code=%s", err, errorCode(err))
	}
}

func TestPackagePayloadNamedLikeVerifierIsRetained(t *testing.T) {
	fx := newFixture(t)
	payload := filepath.Join(fx.source, "Cellar/hello/1.0/bin/record-verify")
	writeFile(t, payload, []byte("#!/bin/sh\n"), 0o755)
	global := filepath.Join(fx.source, "bin/record-verify")
	if err := os.Symlink("../Cellar/hello/1.0/bin/record-verify", global); err != nil {
		t.Fatal(err)
	}
	output := testOutput(t, "runtime")
	if _, err := Assemble(fx.source, output, fx.record, fx.opts); err != nil {
		t.Fatal(err)
	}
	mustExist(t, filepath.Join(output, "Cellar/hello/1.0/bin/record-verify"))
	mustExist(t, filepath.Join(output, "bin/record-verify"))
}

func TestPURLVersionedFormulaEscaping(t *testing.T) {
	if got := purlEscape("python@3.13"); got != "python%403.13" {
		t.Fatalf("got %q", got)
	}
	if got := purlEscape("1.0+1"); got != "1.0%2B1" {
		t.Fatalf("got %q", got)
	}
}

func TestRejectsExecutableInWritableRuntimeState(t *testing.T) {
	fx := newFixture(t)
	writeFile(t, filepath.Join(fx.source, "var/hello/hook"), []byte("#!/bin/sh\n"), 0o755)
	_, err := Assemble(fx.source, testOutput(t, "runtime"), fx.record, fx.opts)
	if errorCode(err) != CodeUnexpectedWritable {
		t.Fatalf("error=%v code=%s", err, errorCode(err))
	}
}

func TestInventoryAbsoluteLinkOutsidePrefixIsRejected(t *testing.T) {
	if _, _, err := normalizeLinkTarget("bin/tool", "/lib/tool", "", DefaultInstallPrefix); err == nil {
		t.Fatal("absolute inventory link outside prefix accepted")
	}
}

func TestNestedReceiptAndBrewDirectoriesArePayload(t *testing.T) {
	fx := newFixture(t)
	writeFile(t, filepath.Join(fx.source, "Cellar/hello/1.0/share/template/INSTALL_RECEIPT.json"), []byte("payload\n"), 0o644)
	writeFile(t, filepath.Join(fx.source, "Cellar/hello/1.0/share/.brew/data.txt"), []byte("payload\n"), 0o644)
	output := testOutput(t, "runtime")
	if _, err := Assemble(fx.source, output, fx.record, fx.opts); err != nil {
		t.Fatal(err)
	}
	mustExist(t, filepath.Join(output, "Cellar/hello/1.0/share/template/INSTALL_RECEIPT.json"))
	mustExist(t, filepath.Join(output, "Cellar/hello/1.0/share/.brew/data.txt"))
}

func TestRejectsGlobalCopyWithExecutableModeMismatch(t *testing.T) {
	fx := newFixture(t)
	keg := filepath.Join(fx.source, "Cellar/hello/1.0/share/mode-data")
	writeFile(t, keg, []byte("same\n"), 0o644)
	global := filepath.Join(fx.source, "share/mode-data")
	writeFile(t, global, []byte("same\n"), 0o755)
	_, err := Assemble(fx.source, testOutput(t, "runtime"), fx.record, fx.opts)
	if errorCode(err) != CodeUnattributed {
		t.Fatalf("error=%v code=%s", err, errorCode(err))
	}
}

func TestOwnerRulesDoNotOverrideSymlinkTargetPackage(t *testing.T) {
	target := &sourceEntry{rel: "Cellar/jq/1/bin/tool", typeName: TypeRegular, retain: true, sha256: "x"}
	link := &sourceEntry{rel: "bin/tool", typeName: TypeSymlink, retain: true, linkResolved: target.rel}
	scan := &sourceScan{entries: []*sourceEntry{target, link}, byPath: map[string]*sourceEntry{target.rel: target, link.rel: link}}
	policy := &normalizedPolicy{nodes: map[string]resolution.Node{"hello": {Name: "hello", PkgVersion: "1"}, "jq": {Name: "jq", PkgVersion: "1"}}, allowlist: normalizedAllowlist{Owners: []normalizedRule{{Path: "bin/tool", Package: "hello"}}}}
	if err := attributeEntries(scan, policy); err != nil {
		t.Fatal(err)
	}
	if link.packageName != "jq" {
		t.Fatalf("symlink attributed to %q", link.packageName)
	}
}

func TestCellarSymlinkIsAttributedToOwningKeg(t *testing.T) {
	for _, tc := range []struct {
		name        string
		profile     string
		linkPackage string
	}{
		{name: "V2 minimal", profile: policyv2.RuntimeProfileMinimalV1, linkPackage: "app"},
		{name: "V1 legacy", linkPackage: "dep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &sourceEntry{rel: "Cellar/dep/1/include/dep.h", typeName: TypeRegular, retain: true, sha256: "x"}
			link := &sourceEntry{rel: "Cellar/app/1/libexec/include/dep.h", typeName: TypeSymlink, retain: true, linkResolved: target.rel}
			scan := &sourceScan{entries: []*sourceEntry{target, link}, byPath: map[string]*sourceEntry{target.rel: target, link.rel: link}}
			policy := &normalizedPolicy{
				nodes: map[string]resolution.Node{
					"app": {Name: "app", PkgVersion: "1"},
					"dep": {Name: "dep", PkgVersion: "1"},
				},
				allowlist: normalizedAllowlist{PruningProfile: tc.profile},
			}
			if err := attributeEntries(scan, policy); err != nil {
				t.Fatal(err)
			}
			if target.packageName != "dep" || link.packageName != tc.linkPackage {
				t.Fatalf("target=%q link=%q", target.packageName, link.packageName)
			}
		})
	}
}

func TestSymlinkCanInheritOwnerAttributedGeneratedTarget(t *testing.T) {
	target := &sourceEntry{rel: "lib/node_modules/npm/bin/npm-cli.js", typeName: TypeRegular, retain: true, sha256: "same", mode: 0o755}
	link := &sourceEntry{rel: "bin/npm", typeName: TypeSymlink, retain: true, linkResolved: target.rel}
	scan := &sourceScan{entries: []*sourceEntry{link, target}, byPath: map[string]*sourceEntry{target.rel: target, link.rel: link}}
	policy := &normalizedPolicy{
		nodes:     map[string]resolution.Node{"node": {Name: "node", FullName: "homebrew/core/node", PkgVersion: "26.5.1"}},
		allowlist: normalizedAllowlist{Owners: []normalizedRule{{Path: "lib/node_modules/npm", Package: "node"}}},
	}
	if err := attributeEntries(scan, policy); err != nil {
		t.Fatal(err)
	}
	if target.packageName != "node" || link.packageName != "node" {
		t.Fatalf("target=%q link=%q", target.packageName, link.packageName)
	}
}

func TestOwnerRulesDoNotOverrideExactGlobalCopyPackage(t *testing.T) {
	target := &sourceEntry{rel: "Cellar/jq/1/share/mime/packages/jq.xml", typeName: TypeRegular, retain: true, sha256: "same", mode: 0o644}
	global := &sourceEntry{rel: "share/mime/packages/jq.xml", typeName: TypeRegular, retain: true, sha256: "same", mode: 0o644}
	scan := &sourceScan{entries: []*sourceEntry{target, global}, byPath: map[string]*sourceEntry{target.rel: target, global.rel: global}}
	policy := &normalizedPolicy{
		nodes: map[string]resolution.Node{
			"jq":               {Name: "jq", PkgVersion: "1"},
			"shared-mime-info": {Name: "shared-mime-info", FullName: "homebrew/core/shared-mime-info", PkgVersion: "1"},
		},
		allowlist: normalizedAllowlist{Owners: []normalizedRule{{Path: "share/mime", Package: "shared-mime-info"}}},
	}
	if err := attributeEntries(scan, policy); err != nil {
		t.Fatal(err)
	}
	if global.packageName != "jq" {
		t.Fatalf("exact global copy attributed to %q", global.packageName)
	}
}

func TestWriteEvidenceRejectsNonemptyDestinationWithoutPartialUpdate(t *testing.T) {
	fx := newFixture(t)
	result, err := Assemble(fx.source, testOutput(t, "runtime"), fx.record, fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "evidence")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "sentinel")
	writeFile(t, sentinel, []byte("old"), 0o644)
	if err := result.WriteEvidence(dir); err == nil {
		t.Fatal("nonempty evidence destination accepted")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "old" {
		t.Fatalf("sentinel changed: %q %v", data, err)
	}
	for _, name := range []string{InventoryFileName, PruneFileName, ManifestFileName, SBOMFileName} {
		mustNotExist(t, filepath.Join(dir, name))
	}
}

func TestRequestedExecutableMustExistAndBeExposed(t *testing.T) {
	fx := newFixture(t)
	if err := os.Remove(filepath.Join(fx.source, "bin/hello")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fx.source, "Cellar/hello/1.0/bin/hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := Assemble(fx.source, testOutput(t, "runtime"), fx.record, fx.opts); err == nil {
		t.Fatal("missing requested executable accepted")
	}
}

func TestGlobalExecutableCanResolveThroughOptSymlink(t *testing.T) {
	fx := newFixture(t)
	global := filepath.Join(fx.source, "bin/hello")
	if err := os.Remove(global); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../opt/hello/bin/hello", global); err != nil {
		t.Fatal(err)
	}
	output := testOutput(t, "runtime")
	if _, err := Assemble(fx.source, output, fx.record, fx.opts); err != nil {
		t.Fatal(err)
	}
}

func TestNonCoreRackSpoofsDoNotReceiveCoreRuntimeFSExceptions(t *testing.T) {
	glibc := resolution.Node{Name: "glibc", FullName: "acme/tools/glibc", PkgVersion: "2"}
	policy := &normalizedPolicy{architecture: "amd64", installPrefix: DefaultInstallPrefix, allowlist: normalizedAllowlist{Lib: true}, nodes: map[string]resolution.Node{"glibc": glibc}}
	entry := &sourceEntry{rel: "lib/ld.so", typeName: TypeSymlink, linkSource: path.Join(DefaultInstallPrefix, "opt/glibc/bin/ld.so")}
	if err := classifyRetention(entry, policy); err == nil {
		t.Fatal("non-core glibc retained brewed loader exception")
	}

	scan := &sourceScan{entries: []*sourceEntry{{rel: "bin/scan-build-py", typeName: TypeSymlink, retain: true, packageName: "llvm@21"}}}
	prunePolicy := &normalizedPolicy{nodes: map[string]resolution.Node{"llvm@21": {Name: "llvm@21", FullName: "acme/tools/llvm@21"}}, requested: map[string]struct{}{}}
	pruneOptionalDependencyTooling(scan, prunePolicy)
	if !scan.entries[0].retain {
		t.Fatal("non-core llvm received optional-tooling prune exception")
	}
}
