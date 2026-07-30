package materializer

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
)

func TestClassifyCurrentKegAndGlobalLink(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(prefix, "Cellar/hello/1/bin")
	if err := os.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keg, "hello"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/hello/1", filepath.Join(prefix, "opt/hello")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/hello/1/bin/hello", filepath.Join(prefix, "bin/hello")); err != nil {
		t.Fatal(err)
	}
	after, err := snapshot(prefix)
	if err != nil {
		t.Fatal(err)
	}
	changes := diff(map[string]fileState{}, after)
	if err := classify(prefix, resolution.Node{Name: "hello", PkgVersion: "1"}, nil, after, changes); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyRejectsOtherKeg(t *testing.T) {
	prefix := t.TempDir()
	after := map[string]fileState{"Cellar/jq/1/bin/jq": {Type: "regular", Mode: 0o755}}
	changes := []Change{{Path: "Cellar/jq/1/bin/jq", Kind: "created"}}
	if err := classify(prefix, resolution.Node{Name: "hello", PkgVersion: "1"}, nil, after, changes); err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyReceiptBottle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "INSTALL_RECEIPT.json")
	if err := os.WriteFile(p, []byte(`{"built_as_bottle":true,"poured_from_bottle":true,"runtime_dependencies":[],"source":{"spec":"stable","tap":"homebrew/core","versions":{"stable":"1.2.3","version_scheme":0}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyReceipt(p, resolution.Node{Name: "x", FormulaVersion: "1.2.3"}); err != nil {
		t.Fatal(err)
	}
}

func TestReplacingSBOMRegeneratesManifestEvidence(t *testing.T) {
	result := &runtimefs.Result{RuntimeManifest: runtimefs.RuntimeManifest{SchemaVersion: runtimefs.ManifestSchemaVersion}}
	doc := runtimefs.SPDXDocument{SPDXVersion: "SPDX-2.3", SPDXID: "SPDXRef-DOCUMENT"}
	if err := replaceSBOM(result, doc); err != nil {
		t.Fatal(err)
	}
	if result.RuntimeManifest.SBOMDigest == "" || result.RuntimeManifest.SBOMDigest != result.Evidence.SBOMDigest {
		t.Fatalf("manifest=%+v evidence=%+v", result.RuntimeManifest, result.Evidence)
	}
	var decoded runtimefs.RuntimeManifest
	if err := json.Unmarshal(result.Evidence.RuntimeManifest, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SBOMDigest != result.Evidence.SBOMDigest {
		t.Fatal("serialized manifest has stale SBOM digest")
	}
}

func TestClassifyRejectsOverwriteOfEarlierSharedPath(t *testing.T) {
	before := map[string]fileState{"bin/tool": {Type: "symlink", Link: "../Cellar/a/1/bin/tool"}}
	after := map[string]fileState{}
	err := classify("/prefix", resolution.Node{Name: "b", PkgVersion: "1"}, before, after, []Change{{Path: "bin/tool", Kind: "removed"}})
	if err == nil {
		t.Fatal("shared path removal accepted")
	}
}

func TestReconcileInstalledKegRejectsUnexpectedContentChange(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/bin/tool", KegPath: "bin/tool", Type: bottle.EntryRegular, Mode: 0o755, SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	after := map[string]fileState{"Cellar/hello/1/bin": {Type: "directory"}, "Cellar/hello/1/bin/tool": {Type: "regular", Mode: 0o755, Digest: strings.Repeat("b", 64)}}
	if err := reconcileInstalledKeg("/", node, verified, after); err == nil {
		t.Fatal("unexpected keg mutation accepted")
	}
	node.Bottle.Tab.ChangedFiles = []string{"bin/tool"}
	if err := reconcileInstalledKeg("/", node, verified, after); err != nil {
		t.Fatalf("declared relocation rejected: %v", err)
	}
}

func TestPreinstallReceiptMayBeRewrittenDuringPour(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/INSTALL_RECEIPT.json", KegPath: "INSTALL_RECEIPT.json", Type: bottle.EntryRegular, Mode: 0o644, SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	after := map[string]fileState{"Cellar/hello/1/INSTALL_RECEIPT.json": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("b", 64)}}
	if err := reconcileInstalledKeg("/", node, verified, after); err != nil {
		t.Fatal(err)
	}
}

func TestPreinstallPackageSBOMMayBeRewrittenDuringPour(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/sbom.spdx.json", KegPath: "sbom.spdx.json", Type: bottle.EntryRegular, Mode: 0o644, SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	after := map[string]fileState{"Cellar/hello/1/sbom.spdx.json": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("b", 64)}}
	if err := reconcileInstalledKeg("/", node, verified, after); err != nil {
		t.Fatal(err)
	}
}

func TestNestedSBOMNamedPayloadRequiresDeclaredRelocation(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/share/component.spdx.json", KegPath: "share/component.spdx.json", Type: bottle.EntryRegular, Mode: 0o644, SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	after := map[string]fileState{"Cellar/hello/1/share/component.spdx.json": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("b", 64)}}
	if err := reconcileInstalledKeg("/", node, verified, after); err == nil {
		t.Fatal("nested SBOM-named payload mutation was accepted")
	}
}

func TestRelocatableBottleFileMayBeRewrittenDuringPour(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/bin/hello", KegPath: "bin/hello", Type: bottle.EntryRegular, Mode: 0o755, SHA256: "sha256:" + strings.Repeat("a", 64), Relocatable: true}}}
	after := map[string]fileState{"Cellar/hello/1/bin/hello": {Type: "regular", Mode: 0o755, Digest: strings.Repeat("b", 64)}}
	if err := reconcileInstalledKeg("/", node, verified, after); err != nil {
		t.Fatal(err)
	}
}

func TestRelocatableFormulaMetadataMayNotBeRewritten(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/.brew/hello.rb", KegPath: ".brew/hello.rb", Type: bottle.EntryRegular, Mode: 0o644, SHA256: "sha256:" + strings.Repeat("a", 64), Relocatable: true}}}
	after := map[string]fileState{"Cellar/hello/1/.brew/hello.rb": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("b", 64)}}
	if err := reconcileInstalledKeg("/", node, verified, after); err == nil {
		t.Fatal("relocatable Formula metadata mutation was accepted")
	}
}

func TestClassifyValidatesOptLinksAndAliases(t *testing.T) {
	prefix := "/prefix"
	base := map[string]fileState{"opt": {Type: "directory"}, "opt/hello": {Type: "symlink", Link: "../Cellar/hello/1"}, "etc": {Type: "directory"}, "var": {Type: "directory"}}
	addTestKeg(base)
	bad := maps.Clone(base)
	bad["opt/hello"] = fileState{Type: "symlink", Link: "../Cellar/other/1"}
	if err := classify(prefix, resolution.Node{Name: "hello", PkgVersion: "1"}, nil, bad, []Change{{Path: "opt", Kind: "created"}, {Path: "opt/hello", Kind: "created"}}); err == nil {
		t.Fatal("escaping opt link accepted")
	}
	withAlias := maps.Clone(base)
	withAlias["opt/hi"] = fileState{Type: "symlink", Link: "../Cellar/hello/1"}
	changes := []Change{{Path: "opt", Kind: "created"}, {Path: "opt/hello", Kind: "created"}, {Path: "opt/hi", Kind: "created"}}
	if err := classify(prefix, resolution.Node{Name: "hello", PkgVersion: "1"}, nil, withAlias, changes, map[string]struct{}{"hello": {}, "hi": {}}); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyAllowsContainedIncludeLinks(t *testing.T) {
	prefix := "/prefix"
	after := map[string]fileState{"opt": {Type: "directory"}, "opt/hello": {Type: "symlink", Link: "../Cellar/hello/1"}, "etc": {Type: "directory"}, "var": {Type: "directory"}, "include": {Type: "directory"}, "include/hello.h": {Type: "symlink", Link: "../Cellar/hello/1/include/hello.h"}}
	addTestKeg(after)
	changes := []Change{{Path: "opt", Kind: "created"}, {Path: "opt/hello", Kind: "created"}, {Path: "include", Kind: "created"}, {Path: "include/hello.h", Kind: "created"}}
	if err := classify(prefix, resolution.Node{Name: "hello", PkgVersion: "1"}, nil, after, changes); err != nil {
		t.Fatal(err)
	}
}

func TestChangedBottleSymlinkMustRemainInsideKeg(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1", Bottle: resolution.Bottle{Tab: resolution.BottleTab{ChangedFiles: []string{"lib/link"}}}}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/lib/link", KegPath: "lib/link", Type: bottle.EntrySymlink, SymlinkTarget: "target"}}}
	after := map[string]fileState{"Cellar/hello/1/lib": {Type: "directory"}, "Cellar/hello/1/lib/link": {Type: "symlink", Link: "/outside"}, "outside": {Type: "regular"}}
	if err := reconcileInstalledKeg("/", node, verified, after); err == nil {
		t.Fatal("escaping changed symlink accepted")
	}
}

func TestClassifyRejectsEscapingStateSymlink(t *testing.T) {
	after := map[string]fileState{"opt": {Type: "directory"}, "opt/hello": {Type: "symlink", Link: "../Cellar/hello/1"}, "etc": {Type: "directory"}, "var": {Type: "directory"}, "etc/config": {Type: "symlink", Link: "/etc/shadow"}}
	addTestKeg(after)
	changes := []Change{{Path: "opt", Kind: "created"}, {Path: "opt/hello", Kind: "created"}, {Path: "etc/config", Kind: "created"}}
	if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, nil, after, changes); err == nil {
		t.Fatal("escaping etc symlink accepted")
	}
}

func TestClassifyAllowsPrunableHomebrewStateLinkToCurrentKeg(t *testing.T) {
	after := map[string]fileState{
		"Cellar":                    {Type: "directory"},
		"Cellar/hello":              {Type: "directory"},
		"Cellar/hello/1":            {Type: "directory"},
		"opt":                       {Type: "directory"},
		"opt/hello":                 {Type: "symlink", Link: "../Cellar/hello/1"},
		"etc":                       {Type: "directory"},
		"var":                       {Type: "directory"},
		"var/homebrew":              {Type: "directory"},
		"var/homebrew/linked":       {Type: "directory"},
		"var/homebrew/linked/hello": {Type: "symlink", Link: "../../../Cellar/hello/1"},
	}
	changes := []Change{
		{Path: "Cellar", Kind: "created"},
		{Path: "Cellar/hello", Kind: "created"},
		{Path: "Cellar/hello/1", Kind: "created"},
		{Path: "opt", Kind: "created"},
		{Path: "opt/hello", Kind: "created"},
		{Path: "etc", Kind: "created"},
		{Path: "var", Kind: "created"},
		{Path: "var/homebrew", Kind: "created"},
		{Path: "var/homebrew/linked", Kind: "created"},
		{Path: "var/homebrew/linked/hello", Kind: "created"},
	}
	if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, nil, after, changes); err != nil {
		t.Fatal(err)
	}
	after["var/homebrew/linked/hello"] = fileState{Type: "symlink", Link: "../../../Cellar/other/1"}
	if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, nil, after, changes); err == nil {
		t.Fatal("package-manager state link to another keg accepted")
	}
}

func TestClassifyAllowsPrunableInfoIndexUpdates(t *testing.T) {
	before := map[string]fileState{
		"Cellar":         {Type: "directory"},
		"Cellar/hello":   {Type: "directory"},
		"Cellar/hello/1": {Type: "directory"},
		"opt":            {Type: "directory"},
		"opt/hello":      {Type: "symlink", Link: "../Cellar/hello/1"},
		"etc":            {Type: "directory"},
		"var":            {Type: "directory"},
		"share":          {Type: "directory"},
		"share/info":     {Type: "directory"},
		"share/info/dir": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("a", 64)},
	}
	after := maps.Clone(before)
	after["share/info/dir"] = fileState{Type: "regular", Mode: 0o644, Digest: strings.Repeat("b", 64)}
	if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, before, after, []Change{{Path: "share/info/dir", Kind: "modified"}}); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyAllowsGlibcToReplaceRuntimeLoader(t *testing.T) {
	prefix := "/prefix"
	before := map[string]fileState{
		"lib":       {Type: "directory"},
		"lib/ld.so": {Type: "symlink", Link: "/lib64/ld-linux-x86-64.so.2"},
	}
	after := map[string]fileState{
		"Cellar":                   {Type: "directory"},
		"Cellar/glibc":             {Type: "directory"},
		"Cellar/glibc/2":           {Type: "directory"},
		"Cellar/glibc/2/bin":       {Type: "directory"},
		"Cellar/glibc/2/bin/ld.so": {Type: "regular", Mode: 0o755},
		"opt":                      {Type: "directory"},
		"opt/glibc":                {Type: "symlink", Link: "../Cellar/glibc/2"},
		"etc":                      {Type: "directory"},
		"var":                      {Type: "directory"},
		"lib":                      {Type: "directory"},
		"lib/ld.so":                {Type: "symlink", Link: "/prefix/opt/glibc/bin/ld.so"},
	}
	changes := diff(before, after)
	if err := classify(prefix, resolution.Node{Name: "glibc", PkgVersion: "2"}, before, after, changes); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyReceiptRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(`{"built_as_bottle":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "INSTALL_RECEIPT.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := verifyReceipt(link, resolution.Node{Name: "x"}); err == nil {
		t.Fatal("receipt symlink accepted")
	}
}

func TestReconcileUsesConfiguredPrefixForAbsoluteSymlinks(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1", Bottle: resolution.Bottle{Tab: resolution.BottleTab{ChangedFiles: []string{"lib/link"}}}}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/lib/link", KegPath: "lib/link", Type: bottle.EntrySymlink, SymlinkTarget: "target"}, {Path: "hello/1/lib/target", KegPath: "lib/target", Type: bottle.EntryRegular, Mode: 0o644, SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	base := map[string]fileState{"Cellar": {Type: "directory"}, "Cellar/hello": {Type: "directory"}, "Cellar/hello/1": {Type: "directory"}, "Cellar/hello/1/lib": {Type: "directory"}, "Cellar/hello/1/lib/target": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("a", 64)}}
	bad := maps.Clone(base)
	bad["Cellar/hello/1/lib/link"] = fileState{Type: "symlink", Link: "/Cellar/hello/1/lib/target"}
	if err := reconcileInstalledKeg("/prefix", node, verified, bad); err == nil {
		t.Fatal("absolute symlink outside configured prefix accepted")
	}
	good := maps.Clone(base)
	good["Cellar/hello/1/lib/link"] = fileState{Type: "symlink", Link: "/prefix/Cellar/hello/1/lib/target"}
	if err := reconcileInstalledKeg("/prefix", node, verified, good); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializerPrefixMustBeAbsolute(t *testing.T) {
	if _, err := normalizeMaterializerPrefix("relative/prefix"); err == nil {
		t.Fatal("relative prefix accepted")
	}
}

func TestSnapshotMissingRootFails(t *testing.T) {
	if _, err := snapshotContext(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing snapshot root accepted")
	}
}

func TestReconcileRejectsPermissionChanges(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/file", KegPath: "file", Type: bottle.EntryRegular, Mode: 0o644, SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	after := map[string]fileState{"Cellar/hello/1/file": {Type: "regular", Mode: 0o666, Digest: strings.Repeat("a", 64)}}
	if err := reconcileInstalledKeg("/prefix", node, verified, after); err == nil {
		t.Fatal("permission change accepted")
	}
}

func TestReconcileRejectsCrossKegHardlinkAlias(t *testing.T) {
	node := resolution.Node{Name: "b", PkgVersion: "1"}
	verified := bottle.Result{Name: "b", PkgVersion: "1", KegPrefix: "b/1", Inventory: []bottle.InventoryEntry{{Path: "b/1/file", KegPath: "file", Type: bottle.EntryRegular, Mode: 0o644, SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	after := map[string]fileState{"Cellar/b/1/file": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("a", 64), Inode: "1:2", Links: 2}, "Cellar/a/1/file": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("a", 64), Inode: "1:2", Links: 2}}
	if err := reconcileInstalledKeg("/prefix", node, verified, after); err == nil {
		t.Fatal("cross-keg hardlink accepted")
	}
}

func TestClassifyRejectsSharedRootModeChange(t *testing.T) {
	before := map[string]fileState{"lib": {Type: "directory", Mode: 0o755}}
	after := map[string]fileState{"lib": {Type: "directory", Mode: 0o777}}
	if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, before, after, []Change{{Path: "lib", Kind: "modified"}}); err == nil {
		t.Fatal("shared root mode change accepted")
	}
}

func TestClassifyRejectsPrefixAndCellarModeChanges(t *testing.T) {
	for _, name := range []string{".", "Cellar"} {
		before := map[string]fileState{name: {Type: "directory", Mode: 0o755}}
		after := map[string]fileState{name: {Type: "directory", Mode: 0o777}}
		if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, before, after, []Change{{Path: name, Kind: "modified"}}); err == nil {
			t.Errorf("mode change for %s accepted", name)
		}
	}
}

func TestClassifyRejectsRegularFileInGlobalLib(t *testing.T) {
	after := map[string]fileState{"opt": {Type: "directory"}, "opt/hello": {Type: "symlink", Link: "../Cellar/hello/1"}, "etc": {Type: "directory"}, "var": {Type: "directory"}, "lib": {Type: "directory"}, "lib/evil.so": {Type: "regular", Mode: 0o644}}
	addTestKeg(after)
	changes := []Change{{Path: "opt", Kind: "created"}, {Path: "opt/hello", Kind: "created"}, {Path: "etc", Kind: "created"}, {Path: "var", Kind: "created"}, {Path: "lib", Kind: "created"}, {Path: "lib/evil.so", Kind: "created"}}
	if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, nil, after, changes); err == nil {
		t.Fatal("regular global library accepted")
	}
}

func TestReconcileRejectsUndeclaredGlobalHardlink(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	verified := bottle.Result{Name: "hello", PkgVersion: "1", KegPrefix: "hello/1", Inventory: []bottle.InventoryEntry{{Path: "hello/1/file", KegPath: "file", Type: bottle.EntryRegular, Mode: 0o644, SHA256: "sha256:" + strings.Repeat("a", 64)}}}
	after := map[string]fileState{"Cellar/hello/1/file": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("a", 64), Inode: "1:2", Links: 2}, "lib/file": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("a", 64), Inode: "1:2", Links: 2}}
	if err := reconcileInstalledKeg("/prefix", node, verified, after); err == nil {
		t.Fatal("undeclared global hardlink accepted")
	}
}

func TestClassifyRejectsNonDirectoryGlobalRoot(t *testing.T) {
	after := map[string]fileState{"opt": {Type: "directory"}, "opt/hello": {Type: "symlink", Link: "../Cellar/hello/1"}, "etc": {Type: "directory"}, "var": {Type: "directory"}, "share": {Type: "regular", Mode: 0o644}}
	addTestKeg(after)
	changes := []Change{{Path: "opt", Kind: "created"}, {Path: "opt/hello", Kind: "created"}, {Path: "etc", Kind: "created"}, {Path: "var", Kind: "created"}, {Path: "share", Kind: "created"}}
	if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, nil, after, changes); err == nil {
		t.Fatal("non-directory global root accepted")
	}
}

func TestPreinstallSymlinkValidationRejectsExternalRedirect(t *testing.T) {
	snapshot := map[string]fileState{"lib": {Type: "symlink", Link: "/outside"}, "outside": {Type: "directory"}}
	if err := validatePreinstallSymlinks("/prefix", snapshot, testRecord("amd64")); err == nil {
		t.Fatal("external writable-root redirect accepted")
	}
}

func TestPreinstallSymlinkResolutionDoesNotCleanBeforeFollowing(t *testing.T) {
	snapshot := map[string]fileState{"etc": {Type: "directory"}, "etc/redir": {Type: "symlink", Link: "../pivot/../victim"}, "pivot": {Type: "symlink", Link: "/outside/base"}, "victim": {Type: "directory"}}
	if err := validatePreinstallSymlinks("/prefix", snapshot, testRecord("amd64")); err == nil {
		t.Fatal("symlink traversal through intermediate redirect accepted")
	}
}

func TestPreinstallSymlinkValidationAllowsBoundRuntimeLoader(t *testing.T) {
	snapshot := map[string]fileState{
		"lib":       {Type: "directory"},
		"lib/ld.so": {Type: "symlink", Link: "/lib64/ld-linux-x86-64.so.2"},
	}
	if err := validatePreinstallSymlinks("/prefix", snapshot, testRecord("amd64")); err != nil {
		t.Fatal(err)
	}
	snapshot["lib/ld.so"] = fileState{Type: "symlink", Link: "/lib/ld-linux-aarch64.so.1"}
	if err := validatePreinstallSymlinks("/prefix", snapshot, testRecord("amd64")); err == nil {
		t.Fatal("wrong-platform runtime loader accepted")
	}
}

func TestPreinstallSymlinkValidationAllowsBrewedGlibcLoader(t *testing.T) {
	snapshot := map[string]fileState{
		"lib":                      {Type: "directory"},
		"lib/ld.so":                {Type: "symlink", Link: "/prefix/opt/glibc/bin/ld.so"},
		"opt":                      {Type: "directory"},
		"opt/glibc":                {Type: "symlink", Link: "../Cellar/glibc/2"},
		"Cellar":                   {Type: "directory"},
		"Cellar/glibc":             {Type: "directory"},
		"Cellar/glibc/2":           {Type: "directory"},
		"Cellar/glibc/2/bin":       {Type: "directory"},
		"Cellar/glibc/2/bin/ld.so": {Type: "regular", Mode: 0o755},
	}
	record := testRecord("amd64", resolution.Node{Name: "glibc", PkgVersion: "2"})
	if err := validatePreinstallSymlinks("/prefix", snapshot, record); err != nil {
		t.Fatal(err)
	}
}

func testRecord(architecture string, nodes ...resolution.Node) *resolution.Record {
	return &resolution.Record{Input: resolution.Input{Platform: resolution.Platform{OS: "linux", Architecture: architecture}}, Nodes: nodes}
}

func addTestKeg(snapshot map[string]fileState) {
	snapshot["Cellar"] = fileState{Type: "directory"}
	snapshot["Cellar/hello"] = fileState{Type: "directory"}
	snapshot["Cellar/hello/1"] = fileState{Type: "directory"}
	snapshot["Cellar/hello/1/include"] = fileState{Type: "directory"}
	snapshot["Cellar/hello/1/include/hello.h"] = fileState{Type: "regular", Mode: 0o644}
}

func TestSnapshotResolverAllowsFiniteRepeatedSymlink(t *testing.T) {
	snapshot := map[string]fileState{"a": {Type: "symlink", Link: "dir"}, "dir": {Type: "directory"}, "dir/file": {Type: "regular"}, "x": {Type: "symlink", Link: "a/../a/file"}}
	resolved, err := resolveSnapshotPath("/prefix", snapshot, "x")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "dir/file" {
		t.Fatalf("resolved=%q", resolved)
	}
}

func TestSnapshotResolverPreservesDirectorySuffixAndRoot(t *testing.T) {
	snapshot := map[string]fileState{".": {Type: "directory"}, "file": {Type: "regular"}, "to-root": {Type: "symlink", Link: "."}, "bad": {Type: "symlink", Link: "file/."}}
	resolved, err := resolveSnapshotPath("/prefix", snapshot, "to-root")
	if err != nil || resolved != "." {
		t.Fatalf("root resolution=%q err=%v", resolved, err)
	}
	if _, err := resolveSnapshotPath("/prefix", snapshot, "bad"); err == nil {
		t.Fatal("file/. accepted without directory")
	}
}
