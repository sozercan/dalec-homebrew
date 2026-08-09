package materializer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
)

func TestDirectorySnapshotEqualityPreservesInodeIdentity(t *testing.T) {
	before := fileState{Type: "directory", Mode: os.ModeDir | 0o755, Inode: "1:10", Links: 2, Size: 4096, UID: 1000, GID: 1000, OwnershipKnown: true}
	after := before
	after.Links = 9
	after.Size = 8192
	if !snapshotStatesEqual(before, after) {
		t.Fatal("structural directory link-count/size changes were treated as replacement")
	}
	after.Inode = "1:11"
	if snapshotStatesEqual(before, after) {
		t.Fatal("directory inode replacement was ignored")
	}
}

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

func TestValidateNoPrefixBrewEnv(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		if err := validateNoPrefixBrewEnv(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("real directory without override", func(t *testing.T) {
		prefix := t.TempDir()
		if err := os.MkdirAll(filepath.Join(prefix, "etc/homebrew"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := validateNoPrefixBrewEnv(prefix); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("override file", func(t *testing.T) {
		prefix := t.TempDir()
		if err := os.MkdirAll(filepath.Join(prefix, "etc/homebrew"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "etc/homebrew/brew.env"), []byte("HOMEBREW_BASH_COMMAND=/tmp/hook\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateNoPrefixBrewEnv(prefix); err == nil {
			t.Fatal("prefix brew.env override accepted")
		}
	})
	t.Run("symlinked directory", func(t *testing.T) {
		prefix := t.TempDir()
		if err := os.MkdirAll(filepath.Join(prefix, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(prefix, "etc/homebrew")); err != nil {
			t.Fatal(err)
		}
		if err := validateNoPrefixBrewEnv(prefix); err == nil {
			t.Fatal("symlinked Homebrew environment directory accepted")
		}
	})
}

func TestInstallEnvForcesProtectedSystemConfiguration(t *testing.T) {
	env := installEnv("/prefix")
	if !slices.Contains(env, "HOMEBREW_SYSTEM_ENV_TAKES_PRIORITY=1") {
		t.Fatal("materializer environment does not prioritize the protected system brew.env")
	}
	for _, value := range env {
		if strings.HasPrefix(value, "HOMEBREW_BASH_COMMAND=") {
			t.Fatalf("materializer environment unexpectedly supplies a bash hook: %q", value)
		}
	}
}

func TestVerifyReceiptBottle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "INSTALL_RECEIPT.json")
	if err := os.WriteFile(p, []byte(`{"built_as_bottle":true,"poured_from_bottle":true,"runtime_dependencies":[],"source":{"spec":"stable","tap":"homebrew/core","versions":{"stable":"1.2.3","version_scheme":0}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyReceipt(p, resolution.Node{Name: "x", FormulaVersion: "1.2.3"}, nil); err != nil {
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
	if err := classify(prefix, resolution.Node{Name: "hello", PkgVersion: "1"}, nil, withAlias, changes, classifyOptions{optNames: map[string]struct{}{"hello": {}, "hi": {}}}); err != nil {
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

func TestValidateExternalBottleSymlinkTargetsRequiresExactDependencyKeg(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1", Dependencies: []resolution.Requirement{{Name: "python@3.14", Direct: true}}}
	python := resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", PkgVersion: "3.14.1"}
	verified := bottle.Result{Inventory: []bottle.InventoryEntry{{Path: "hello/1/libexec/bin/python3.14", KegPath: "libexec/bin/python3.14", Type: bottle.EntrySymlink, PrefixTarget: "opt/python@3.14/bin/python3.14"}}}
	snapshot := map[string]fileState{
		"Cellar":                                   {Type: "directory"},
		"Cellar/python@3.14":                       {Type: "directory"},
		"Cellar/python@3.14/3.14.1":                {Type: "directory"},
		"Cellar/python@3.14/3.14.1/bin":            {Type: "directory"},
		"Cellar/python@3.14/3.14.1/bin/python3.14": {Type: "regular", Mode: 0o755},
		"opt":             {Type: "directory"},
		"opt/python@3.14": {Type: "symlink", Link: "../Cellar/python@3.14/3.14.1"},
	}
	closure := []resolution.Node{node, python}
	if err := validateExternalBottleSymlinkTargets("/prefix", snapshot, node, verified, closure, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	redirected := maps.Clone(snapshot)
	redirected["opt/python@3.14"] = fileState{Type: "symlink", Link: "../Cellar/python@3.14/9.9"}
	redirected["Cellar/python@3.14/9.9"] = fileState{Type: "directory"}
	redirected["Cellar/python@3.14/9.9/bin"] = fileState{Type: "directory"}
	redirected["Cellar/python@3.14/9.9/bin/python3.14"] = fileState{Type: "regular", Mode: 0o755}
	if err := validateExternalBottleSymlinkTargets("/prefix", redirected, node, verified, closure, 1000, 1000); err == nil {
		t.Fatal("redirected dependency opt target accepted before pour")
	}
	unsigned := node
	unsigned.Dependencies = []resolution.Requirement{{Name: "python@3.14", Direct: false}}
	if err := validateExternalBottleSymlinkTargets("/prefix", snapshot, unsigned, verified, []resolution.Node{unsigned, python}, 1000, 1000); err == nil {
		t.Fatal("external target without signed direct dependency accepted before pour")
	}
}

func TestValidateAndReconcileCertifiSharedCALinks(t *testing.T) {
	node, caCertificates, verified, state := certifiSharedCAFixture()
	closure := []resolution.Node{caCertificates, node}
	if err := validateExternalBottleSymlinkTargets("/prefix", state, node, verified, closure, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInstalledKeg("/prefix", node, verified, state, reconcileKegOptions{closure: closure, runtimeUID: 1000, runtimeGID: 1000}); err != nil {
		t.Fatal(err)
	}
	v2 := node
	v2.PolicyFormulaID = "homebrew/core/certifi"
	if err := validateExternalBottleSymlinkTargets("/prefix", state, v2, verified, []resolution.Node{caCertificates, v2}, 1000, 1000); err != nil {
		t.Fatalf("V2 exact policy rule: %v", err)
	}
}

func TestClassifyAllowsOnlyVerifiedCertifiSharedCAGlobalLinks(t *testing.T) {
	node, caCertificates, verified, state := certifiSharedCAFixture()
	for key, value := range map[string]fileState{
		"var":         {Type: "directory"},
		"opt":         {Type: "directory"},
		"opt/certifi": {Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../Cellar/certifi/2026.7.22"},
		"lib":         {Type: "directory"},
	} {
		state[key] = value
	}
	for _, minor := range []string{"12", "13", "14"} {
		root := "lib/python3." + minor + "/site-packages/certifi"
		state["lib/python3."+minor] = fileState{Type: "directory"}
		state["lib/python3."+minor+"/site-packages"] = fileState{Type: "directory"}
		state[root] = fileState{Type: "directory"}
		state[root+"/cacert.pem"] = fileState{
			Type: "symlink", Mode: os.ModeSymlink | 0o777,
			Link: "../../../../Cellar/certifi/2026.7.22/" + root + "/cacert.pem",
		}
	}
	before := map[string]fileState{}
	for _, rel := range []string{
		"Cellar", "Cellar/ca-certificates", "Cellar/ca-certificates/2026",
		"etc", "etc/ca-certificates", bottle.CertifiSharedCATarget, "var", "opt", "lib",
	} {
		before[rel] = state[rel]
	}
	options := classifyOptions{
		closure:    []resolution.Node{caCertificates, node},
		verified:   verified,
		runtimeUID: 1000,
		runtimeGID: 1000,
	}
	changes := diff(before, state)
	if err := classify("/prefix", node, before, state, changes, options); err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if strings.HasSuffix(change.Path, "/certifi/cacert.pem") && !strings.HasPrefix(change.Path, "Cellar/") && change.Classification != "certifi-shared-ca-link" {
			t.Fatalf("global CA link %q classification = %q", change.Path, change.Classification)
		}
	}

	global12 := "lib/python3.12/site-packages/certifi/cacert.pem"
	keg12 := "Cellar/certifi/2026.7.22/" + global12
	tests := map[string]func(*resolution.Node, map[string]fileState){
		"bypasses keg source": func(_ *resolution.Node, candidate map[string]fileState) {
			value := candidate[global12]
			value.Link = "/prefix/" + bottle.CertifiSharedCATarget
			candidate[global12] = value
		},
		"targets different keg alias": func(_ *resolution.Node, candidate map[string]fileState) {
			value := candidate[global12]
			value.Link = "../../../../Cellar/certifi/2026.7.22/lib/python3.13/site-packages/certifi/cacert.pem"
			candidate[global12] = value
		},
		"rewrites verified keg alias": func(_ *resolution.Node, candidate map[string]fileState) {
			value := candidate[keg12]
			value.Link = "../../../python3.14/site-packages/certifi/./cacert.pem"
			candidate[keg12] = value
		},
		"non-core owner": func(candidateNode *resolution.Node, _ map[string]fileState) {
			candidateNode.FullName = "acme/tools/certifi"
		},
		"indirect dependency": func(candidateNode *resolution.Node, _ map[string]fileState) {
			candidateNode.Dependencies = []resolution.Requirement{{Name: "ca-certificates"}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidateNode := node
			candidate := maps.Clone(state)
			mutate(&candidateNode, candidate)
			candidateOptions := options
			candidateOptions.closure = []resolution.Node{caCertificates, candidateNode}
			if err := classify("/prefix", candidateNode, before, candidate, diff(before, candidate), candidateOptions); err == nil {
				t.Fatal("unauthorized certifi global CA link accepted")
			}
		})
	}
}

func TestValidateCertifiSharedCATargetRejectsUnsafeStateAndIdentity(t *testing.T) {
	tests := map[string]func(*resolution.Node, *[]resolution.Node, *bottle.Result, map[string]fileState){
		"missing target": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			delete(state, bottle.CertifiSharedCATarget)
		},
		"symlink target": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			state[bottle.CertifiSharedCATarget] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "/etc/shadow", Links: 1, OwnershipKnown: true, UID: 1000, GID: 1000}
		},
		"directory target": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			state[bottle.CertifiSharedCATarget] = fileState{Type: "directory", Mode: os.ModeDir | 0o755, Links: 1, OwnershipKnown: true, UID: 1000, GID: 1000}
		},
		"empty target": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.Size = 0
			state[bottle.CertifiSharedCATarget] = value
		},
		"oversize target": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.Size = certifiSharedCAMaxBytes + 1
			state[bottle.CertifiSharedCATarget] = value
		},
		"executable target": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.Mode = 0o755
			state[bottle.CertifiSharedCATarget] = value
		},
		"writable target": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.Mode = 0o664
			state[bottle.CertifiSharedCATarget] = value
		},
		"special target mode": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.Mode = os.ModeSetuid | 0o644
			state[bottle.CertifiSharedCATarget] = value
		},
		"hardlinked target": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.Links = 2
			state[bottle.CertifiSharedCATarget] = value
		},
		"missing digest": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.Digest = ""
			state[bottle.CertifiSharedCATarget] = value
		},
		"malformed digest": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.Digest = strings.Repeat("z", 64)
			state[bottle.CertifiSharedCATarget] = value
		},
		"unknown owner": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.OwnershipKnown = false
			state[bottle.CertifiSharedCATarget] = value
		},
		"wrong owner": func(_ *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, state map[string]fileState) {
			value := state[bottle.CertifiSharedCATarget]
			value.UID = 0
			state[bottle.CertifiSharedCATarget] = value
		},
		"non-core certifi": func(node *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, _ map[string]fileState) {
			node.FullName = "acme/tools/certifi"
		},
		"V2 policy spoof": func(node *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, _ map[string]fileState) {
			node.PolicyFormulaID = "acme/tools/certifi"
		},
		"indirect dependency": func(node *resolution.Node, _ *[]resolution.Node, _ *bottle.Result, _ map[string]fileState) {
			node.Dependencies[0].Direct = false
		},
		"missing closure dependency": func(_ *resolution.Node, closure *[]resolution.Node, _ *bottle.Result, _ map[string]fileState) {
			*closure = (*closure)[1:]
		},
		"non-core closure dependency": func(_ *resolution.Node, closure *[]resolution.Node, _ *bottle.Result, _ map[string]fileState) {
			(*closure)[0].FullName = "acme/tools/ca-certificates"
		},
		"duplicate closure rack": func(_ *resolution.Node, closure *[]resolution.Node, _ *bottle.Result, _ map[string]fileState) {
			*closure = append(*closure, resolution.Node{Name: "ca-certificates", FullName: "homebrew/core/ca-certificates", PkgVersion: "other"})
		},
		"wrong source path": func(_ *resolution.Node, _ *[]resolution.Node, verified *bottle.Result, _ map[string]fileState) {
			verified.Inventory[0].KegPath = "lib/python3.14/site-packages/certifi/other.pem"
		},
		"non-symlink inventory": func(_ *resolution.Node, _ *[]resolution.Node, verified *bottle.Result, _ map[string]fileState) {
			verified.Inventory[0].Type = bottle.EntryRegular
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			node, caCertificates, verified, state := certifiSharedCAFixture()
			closure := []resolution.Node{caCertificates, node}
			node.Dependencies = append([]resolution.Requirement(nil), node.Dependencies...)
			verified.Inventory = append([]bottle.InventoryEntry(nil), verified.Inventory...)
			mutate(&node, &closure, &verified, state)
			if err := validateExternalBottleSymlinkTargets("/prefix", state, node, verified, closure, 1000, 1000); err == nil {
				t.Fatal("unsafe shared CA target accepted")
			}
		})
	}
}

func TestReconcileCertifiSharedCALinkRejectsRewrites(t *testing.T) {
	node, caCertificates, verified, state := certifiSharedCAFixture()
	closure := []resolution.Node{caCertificates, node}
	rewritten := maps.Clone(state)
	direct := "Cellar/certifi/2026.7.22/lib/python3.14/site-packages/certifi/cacert.pem"
	rewritten[direct] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "/prefix/etc/ca-certificates/cert.pem"}
	changed := node
	changed.Bottle.Tab.ChangedFiles = []string{"lib/python3.14/site-packages/certifi/cacert.pem"}
	if err := reconcileInstalledKeg("/prefix", changed, verified, rewritten, reconcileKegOptions{closure: []resolution.Node{caCertificates, changed}, runtimeUID: 1000, runtimeGID: 1000}); err == nil {
		t.Fatal("rewritten certifi CA link accepted as a changed file")
	}
	rewrittenAlias := maps.Clone(state)
	alias := "Cellar/certifi/2026.7.22/lib/python3.13/site-packages/certifi/cacert.pem"
	rewrittenAlias[alias] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../../../python3.14/site-packages/certifi/./cacert.pem"}
	if err := reconcileInstalledKeg("/prefix", node, verified, rewrittenAlias, reconcileKegOptions{closure: closure, runtimeUID: 1000, runtimeGID: 1000}); err == nil {
		t.Fatal("rewritten certifi CA alias accepted")
	}
	unsafeTarget := maps.Clone(state)
	value := unsafeTarget[bottle.CertifiSharedCATarget]
	value.Mode = 0o666
	unsafeTarget[bottle.CertifiSharedCATarget] = value
	if err := reconcileInstalledKeg("/prefix", node, verified, unsafeTarget, reconcileKegOptions{closure: closure, runtimeUID: 1000, runtimeGID: 1000}); err == nil {
		t.Fatal("unsafe post-pour shared CA target accepted")
	}
}

func certifiSharedCAFixture() (resolution.Node, resolution.Node, bottle.Result, map[string]fileState) {
	node := resolution.Node{
		Name: "certifi", FullName: "homebrew/core/certifi", PkgVersion: "2026.7.22",
		Dependencies: []resolution.Requirement{{Name: "ca-certificates", Direct: true}},
	}
	caCertificates := resolution.Node{Name: "ca-certificates", FullName: "homebrew/core/ca-certificates", PkgVersion: "2026"}
	directTarget := "../../../../../../../" + bottle.CertifiSharedCATarget
	verified := bottle.Result{
		Name: "certifi", PkgVersion: "2026.7.22", KegPrefix: "certifi/2026.7.22",
		Inventory: []bottle.InventoryEntry{
			{Path: "certifi/2026.7.22/lib/python3.14/site-packages/certifi/cacert.pem", KegPath: "lib/python3.14/site-packages/certifi/cacert.pem", Type: bottle.EntrySymlink, SymlinkTarget: directTarget, PrefixTarget: bottle.CertifiSharedCATarget},
			{Path: "certifi/2026.7.22/lib/python3.12/site-packages/certifi/cacert.pem", KegPath: "lib/python3.12/site-packages/certifi/cacert.pem", Type: bottle.EntrySymlink, SymlinkTarget: "../../../python3.14/site-packages/certifi/cacert.pem", PrefixTarget: bottle.CertifiSharedCATarget},
			{Path: "certifi/2026.7.22/lib/python3.13/site-packages/certifi/cacert.pem", KegPath: "lib/python3.13/site-packages/certifi/cacert.pem", Type: bottle.EntrySymlink, SymlinkTarget: "../../../python3.14/site-packages/certifi/cacert.pem", PrefixTarget: bottle.CertifiSharedCATarget},
		},
	}
	state := map[string]fileState{
		"Cellar":                                  {Type: "directory"},
		"Cellar/certifi":                          {Type: "directory"},
		"Cellar/certifi/2026.7.22":                {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib":            {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.12": {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.12/site-packages":                    {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.12/site-packages/certifi":            {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.12/site-packages/certifi/cacert.pem": {Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../../../python3.14/site-packages/certifi/cacert.pem"},
		"Cellar/certifi/2026.7.22/lib/python3.13":                                  {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.13/site-packages":                    {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.13/site-packages/certifi":            {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.13/site-packages/certifi/cacert.pem": {Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../../../python3.14/site-packages/certifi/cacert.pem"},
		"Cellar/certifi/2026.7.22/lib/python3.14":                                  {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.14/site-packages":                    {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.14/site-packages/certifi":            {Type: "directory"},
		"Cellar/certifi/2026.7.22/lib/python3.14/site-packages/certifi/cacert.pem": {Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: directTarget},
		"Cellar/ca-certificates":                                                   {Type: "directory"},
		"Cellar/ca-certificates/2026":                                              {Type: "directory"},
		"etc":                                                                      {Type: "directory"},
		"etc/ca-certificates":                                                      {Type: "directory"},
		bottle.CertifiSharedCATarget:                                               {Type: "regular", Mode: 0o644, Size: 4, Digest: strings.Repeat("a", 64), Inode: "ca-certificates", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true},
	}
	return node, caCertificates, verified, state
}

func TestReconcileInstalledKegAllowsSignedDependencyOptSymlink(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1", Dependencies: []resolution.Requirement{{Name: "python@3.14", Direct: true}}}
	python := resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", PkgVersion: "3.14.1"}
	verified := bottle.Result{
		Name:       "hello",
		PkgVersion: "1",
		KegPrefix:  "hello/1",
		Inventory: []bottle.InventoryEntry{
			{Path: "hello/1/libexec/bin/python", KegPath: "libexec/bin/python", Type: bottle.EntrySymlink, SymlinkTarget: "python3.14", PrefixTarget: "opt/python@3.14/bin/python3.14"},
			{Path: "hello/1/libexec/bin/python3.14", KegPath: "libexec/bin/python3.14", Type: bottle.EntrySymlink, SymlinkTarget: "../../../../../opt/python@3.14/bin/python3.14", PrefixTarget: "opt/python@3.14/bin/python3.14"},
		},
	}
	after := map[string]fileState{
		"Cellar":                                   {Type: "directory"},
		"Cellar/hello":                             {Type: "directory"},
		"Cellar/hello/1":                           {Type: "directory"},
		"Cellar/hello/1/libexec":                   {Type: "directory"},
		"Cellar/hello/1/libexec/bin":               {Type: "directory"},
		"Cellar/hello/1/libexec/bin/python":        {Type: "symlink", Link: "python3.14"},
		"Cellar/hello/1/libexec/bin/python3.14":    {Type: "symlink", Link: "../../../../../opt/python@3.14/bin/python3.14"},
		"Cellar/python@3.14":                       {Type: "directory"},
		"Cellar/python@3.14/3.14.1":                {Type: "directory"},
		"Cellar/python@3.14/3.14.1/bin":            {Type: "directory"},
		"Cellar/python@3.14/3.14.1/bin/python3.14": {Type: "regular", Mode: 0o755},
		"opt":             {Type: "directory"},
		"opt/python@3.14": {Type: "symlink", Link: "../Cellar/python@3.14/3.14.1"},
	}
	if err := reconcileInstalledKeg("/prefix", node, verified, after, reconcileKegOptions{closure: []resolution.Node{node, python}}); err != nil {
		t.Fatal(err)
	}
	changed := node
	changed.Bottle.Tab.ChangedFiles = []string{"libexec/bin/python3.14"}
	rewritten := maps.Clone(after)
	rewritten["Cellar/hello/1/libexec/bin/python3.14"] = fileState{Type: "symlink", Link: "/prefix/opt/python@3.14/bin/python3.14"}
	if err := reconcileInstalledKeg("/prefix", changed, verified, rewritten, reconcileKegOptions{closure: []resolution.Node{changed, python}}); err == nil {
		t.Fatal("rewritten dependency opt symlink accepted as a changed file")
	}
	if err := reconcileInstalledKeg("/prefix", node, verified, after); err == nil {
		t.Fatal("dependency opt symlink accepted without resolved closure")
	}
	unsigned := node
	unsigned.Dependencies = []resolution.Requirement{{Name: "python@3.14", Direct: false}}
	if err := reconcileInstalledKeg("/prefix", unsigned, verified, after, reconcileKegOptions{closure: []resolution.Node{unsigned, python}}); err == nil {
		t.Fatal("dependency opt symlink accepted without signed direct dependency edge")
	}
	redirected := maps.Clone(after)
	redirected["opt/python@3.14"] = fileState{Type: "symlink", Link: "../Cellar/python@3.14/9.9"}
	redirected["Cellar/python@3.14/9.9"] = fileState{Type: "directory"}
	redirected["Cellar/python@3.14/9.9/bin"] = fileState{Type: "directory"}
	redirected["Cellar/python@3.14/9.9/bin/python3.14"] = fileState{Type: "regular", Mode: 0o755}
	if err := reconcileInstalledKeg("/prefix", node, verified, redirected, reconcileKegOptions{closure: []resolution.Node{node, python}}); err == nil {
		t.Fatal("dependency opt symlink accepted after opt redirected to a different keg")
	}
}

func TestReconcileInstalledKegAllowsNodeNPMPostInstallLinkRewrite(t *testing.T) {
	content := []byte("npm\n")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	node := resolution.Node{Name: "node", FullName: "homebrew/core/node", PkgVersion: "1"}
	verified := bottle.Result{
		Name:       "node",
		PkgVersion: "1",
		KegPrefix:  "node/1",
		Inventory: []bottle.InventoryEntry{
			{Path: "node/1/bin/npm", KegPath: "bin/npm", Type: bottle.EntrySymlink, SymlinkTarget: "../libexec/lib/node_modules/npm/bin/npm-cli.js"},
			{Path: "node/1/libexec/lib/node_modules/npm/bin/npm-cli.js", KegPath: "libexec/lib/node_modules/npm/bin/npm-cli.js", Type: bottle.EntryRegular, Mode: 0o755, Size: int64(len(content)), SHA256: "sha256:" + digestHex},
		},
	}
	directory := fileState{Type: "directory"}
	regular := fileState{Type: "regular", Mode: 0o755, Size: int64(len(content)), Digest: digestHex, Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
	after := map[string]fileState{
		"Cellar":                                         directory,
		"Cellar/node":                                    directory,
		"Cellar/node/1":                                  directory,
		"Cellar/node/1/bin":                              directory,
		"Cellar/node/1/bin/npm":                          {Type: "symlink", Link: "/prefix/lib/node_modules/npm/bin/npm-cli.js"},
		"Cellar/node/1/libexec":                          directory,
		"Cellar/node/1/libexec/lib":                      directory,
		"Cellar/node/1/libexec/lib/node_modules":         directory,
		"Cellar/node/1/libexec/lib/node_modules/npm":     directory,
		"Cellar/node/1/libexec/lib/node_modules/npm/bin": directory,
		"Cellar/node/1/libexec/lib/node_modules/npm/bin/npm-cli.js": regular,
		"lib":                                 directory,
		"lib/node_modules":                    directory,
		"lib/node_modules/npm":                directory,
		"lib/node_modules/npm/bin":            directory,
		"lib/node_modules/npm/bin/npm-cli.js": regular,
	}
	if err := reconcileInstalledKeg("/prefix", node, verified, after); err != nil {
		t.Fatal(err)
	}
	mutated := maps.Clone(after)
	global := mutated["lib/node_modules/npm/bin/npm-cli.js"]
	global.Digest = strings.Repeat("f", 64)
	mutated["lib/node_modules/npm/bin/npm-cli.js"] = global
	if err := reconcileInstalledKeg("/prefix", node, verified, mutated); err == nil {
		t.Fatal("Node npm link rewrite accepted with modified generated target")
	}
	redirected := maps.Clone(after)
	redirected["Cellar/node/1/bin/npm"] = fileState{Type: "symlink", Link: "/prefix/lib/node_modules/npm/bin/other.js"}
	redirected["lib/node_modules/npm/bin/other.js"] = regular
	if err := reconcileInstalledKeg("/prefix", node, verified, redirected); err == nil {
		t.Fatal("Node npm link rewrite accepted with unexpected target")
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

func TestClassifyAllowsCurrentKegBashCompletionLink(t *testing.T) {
	node := resolution.Node{Name: "kubernetes-cli", PkgVersion: "1.36.3"}
	after := map[string]fileState{
		"Cellar":                                                     {Type: "directory"},
		"Cellar/kubernetes-cli":                                      {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3":                               {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3/etc":                           {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3/etc/bash_completion.d":         {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3/etc/bash_completion.d/kubectl": {Type: "regular", Mode: 0o644},
		"opt":                           {Type: "directory"},
		"opt/kubernetes-cli":            {Type: "symlink", Link: "../Cellar/kubernetes-cli/1.36.3"},
		"etc":                           {Type: "directory"},
		"etc/bash_completion.d":         {Type: "directory"},
		"etc/bash_completion.d/kubectl": {Type: "symlink", Link: "../../Cellar/kubernetes-cli/1.36.3/etc/bash_completion.d/kubectl"},
		"var":                           {Type: "directory"},
	}
	changes := []Change{{Path: "etc/bash_completion.d/kubectl", Kind: "created"}}
	if err := classify("/prefix", node, nil, after, changes); err != nil {
		t.Fatal(err)
	}
	if changes[0].Classification != "configuration" {
		t.Fatalf("classification=%q", changes[0].Classification)
	}
}

func TestClassifyAllowsCurrentKegBashCompletionAlias(t *testing.T) {
	node := resolution.Node{Name: "util-linux", PkgVersion: "2.42.2"}
	after := map[string]fileState{
		"Cellar":                       {Type: "directory"},
		"Cellar/util-linux":            {Type: "directory"},
		"Cellar/util-linux/2.42.2":     {Type: "directory"},
		"Cellar/util-linux/2.42.2/etc": {Type: "directory"},
		"Cellar/util-linux/2.42.2/etc/bash_completion.d":       {Type: "directory"},
		"Cellar/util-linux/2.42.2/etc/bash_completion.d/last":  {Type: "regular", Mode: 0o644},
		"Cellar/util-linux/2.42.2/etc/bash_completion.d/lastb": {Type: "symlink", Link: "last"},
		"opt":                         {Type: "directory"},
		"opt/util-linux":              {Type: "symlink", Link: "../Cellar/util-linux/2.42.2"},
		"etc":                         {Type: "directory"},
		"etc/bash_completion.d":       {Type: "directory"},
		"etc/bash_completion.d/lastb": {Type: "symlink", Link: "../../Cellar/util-linux/2.42.2/etc/bash_completion.d/lastb"},
		"var":                         {Type: "directory"},
	}
	changes := []Change{{Path: "etc/bash_completion.d/lastb", Kind: "created"}}
	if err := classify("/prefix", node, nil, after, changes); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyRejectsUnsafeBashCompletionAliases(t *testing.T) {
	node := resolution.Node{Name: "util-linux", PkgVersion: "2.42.2"}
	base := map[string]fileState{
		"Cellar":                       {Type: "directory"},
		"Cellar/util-linux":            {Type: "directory"},
		"Cellar/util-linux/2.42.2":     {Type: "directory"},
		"Cellar/util-linux/2.42.2/etc": {Type: "directory"},
		"Cellar/util-linux/2.42.2/etc/bash_completion.d":       {Type: "directory"},
		"Cellar/util-linux/2.42.2/etc/bash_completion.d/last":  {Type: "regular", Mode: 0o644},
		"Cellar/util-linux/2.42.2/etc/bash_completion.d/lastb": {Type: "symlink", Link: "last"},
		"Cellar/other":                              {Type: "directory"},
		"Cellar/other/1":                            {Type: "directory"},
		"Cellar/other/1/etc":                        {Type: "directory"},
		"Cellar/other/1/etc/bash_completion.d":      {Type: "directory"},
		"Cellar/other/1/etc/bash_completion.d/last": {Type: "regular", Mode: 0o644},
		"opt":                         {Type: "directory"},
		"opt/util-linux":              {Type: "symlink", Link: "../Cellar/util-linux/2.42.2"},
		"etc":                         {Type: "directory"},
		"etc/bash_completion.d":       {Type: "directory"},
		"etc/bash_completion.d/lastb": {Type: "symlink", Link: "../../Cellar/util-linux/2.42.2/etc/bash_completion.d/lastb"},
		"var":                         {Type: "directory"},
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]fileState)
	}{
		{
			name: "global link bypasses keg alias",
			mutate: func(after map[string]fileState) {
				state := after["etc/bash_completion.d/lastb"]
				state.Link = "../../Cellar/util-linux/2.42.2/etc/bash_completion.d/last"
				after["etc/bash_completion.d/lastb"] = state
			},
		},
		{
			name: "keg alias crosses into another keg",
			mutate: func(after map[string]fileState) {
				state := after["Cellar/util-linux/2.42.2/etc/bash_completion.d/lastb"]
				state.Link = "../../../../../other/1/etc/bash_completion.d/last"
				after["Cellar/util-linux/2.42.2/etc/bash_completion.d/lastb"] = state
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := maps.Clone(base)
			tc.mutate(after)
			if err := classify("/prefix", node, nil, after, []Change{{Path: "etc/bash_completion.d/lastb", Kind: "created"}}); err == nil {
				t.Fatal("unsafe completion alias accepted")
			}
		})
	}
}

func TestClassifyRejectsUnsafeBashCompletionLinks(t *testing.T) {
	node := resolution.Node{Name: "kubernetes-cli", PkgVersion: "1.36.3"}
	base := map[string]fileState{
		"Cellar":                                                     {Type: "directory"},
		"Cellar/kubernetes-cli":                                      {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3":                               {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3/bin":                           {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3/bin/kubectl":                   {Type: "regular", Mode: 0o755},
		"Cellar/kubernetes-cli/1.36.3/etc":                           {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3/etc/bash_completion.d":         {Type: "directory"},
		"Cellar/kubernetes-cli/1.36.3/etc/bash_completion.d/kubectl": {Type: "regular", Mode: 0o644},
		"Cellar/other":                                               {Type: "directory"},
		"Cellar/other/1":                                             {Type: "directory"},
		"Cellar/other/1/etc":                                         {Type: "directory"},
		"Cellar/other/1/etc/bash_completion.d":                       {Type: "directory"},
		"Cellar/other/1/etc/bash_completion.d/kubectl":               {Type: "regular", Mode: 0o644},
		"opt":                   {Type: "directory"},
		"opt/kubernetes-cli":    {Type: "symlink", Link: "../Cellar/kubernetes-cli/1.36.3"},
		"etc":                   {Type: "directory"},
		"etc/bash_completion.d": {Type: "directory"},
		"var":                   {Type: "directory"},
	}
	for _, tc := range []struct {
		name string
		path string
		link string
	}{
		{name: "outside completion tree", path: "etc/kubectl", link: "../Cellar/kubernetes-cli/1.36.3/etc/bash_completion.d/kubectl"},
		{name: "different current-keg file", path: "etc/bash_completion.d/kubectl", link: "../../Cellar/kubernetes-cli/1.36.3/bin/kubectl"},
		{name: "other keg", path: "etc/bash_completion.d/kubectl", link: "../../Cellar/other/1/etc/bash_completion.d/kubectl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := maps.Clone(base)
			after[tc.path] = fileState{Type: "symlink", Link: tc.link}
			if err := classify("/prefix", node, nil, after, []Change{{Path: tc.path, Kind: "created"}}); err == nil {
				t.Fatalf("unsafe completion link %s -> %s accepted", tc.path, tc.link)
			}
		})
	}
}

func TestClassifyAllowsCurrentGlibcLoaderConfigurationLink(t *testing.T) {
	node := resolution.Node{Name: "glibc", PkgVersion: "2.42"}
	after := loaderConfigurationSnapshot(node)
	changes := []Change{{Path: "etc/ld.so.conf", Kind: "created"}}
	if err := classify("/prefix", node, nil, after, changes, loaderConfigurationOptions(node)); err != nil {
		t.Fatal(err)
	}
	if changes[0].Classification != "configuration" {
		t.Fatalf("classification=%q", changes[0].Classification)
	}
}

func TestClassifyRejectsUnsafeGlibcLoaderConfigurationLinks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		node   resolution.Node
		path   string
		mutate func(map[string]fileState, *classifyOptions)
	}{
		{
			name: "another formula",
			node: resolution.Node{Name: "hello", PkgVersion: "1"},
			path: "etc/ld.so.conf",
		},
		{
			name: "another configuration path",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/alternate.conf",
		},
		{
			name: "opt indirection",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(after map[string]fileState, _ *classifyOptions) {
				after["etc/ld.so.conf"] = fileState{Type: "symlink", Link: "../opt/glibc/etc/ld.so.conf"}
			},
		},
		{
			name: "different current-keg file",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(after map[string]fileState, _ *classifyOptions) {
				after["Cellar/glibc/2.42/etc/alternate.conf"] = fileState{Type: "regular", Mode: 0o644}
				after["etc/ld.so.conf"] = fileState{Type: "symlink", Link: "/prefix/Cellar/glibc/2.42/etc/alternate.conf"}
			},
		},
		{
			name: "other glibc version",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(after map[string]fileState, _ *classifyOptions) {
				after["Cellar/glibc/2.41"] = fileState{Type: "directory"}
				after["Cellar/glibc/2.41/etc"] = fileState{Type: "directory"}
				after["Cellar/glibc/2.41/etc/ld.so.conf"] = fileState{Type: "regular", Mode: 0o644}
				after["etc/ld.so.conf"] = fileState{Type: "symlink", Link: "/prefix/Cellar/glibc/2.41/etc/ld.so.conf"}
			},
		},
		{
			name: "other formula keg",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(after map[string]fileState, _ *classifyOptions) {
				after["Cellar/other"] = fileState{Type: "directory"}
				after["Cellar/other/1"] = fileState{Type: "directory"}
				after["Cellar/other/1/etc"] = fileState{Type: "directory"}
				after["Cellar/other/1/etc/ld.so.conf"] = fileState{Type: "regular", Mode: 0o644}
				after["etc/ld.so.conf"] = fileState{Type: "symlink", Link: "/prefix/Cellar/other/1/etc/ld.so.conf"}
			},
		},
		{
			name: "missing verified inventory",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(_ map[string]fileState, options *classifyOptions) {
				options.verified.Inventory = nil
			},
		},
		{
			name: "mismatched verified identity",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(_ map[string]fileState, options *classifyOptions) {
				options.verified.PkgVersion = "2.41"
				options.verified.KegPrefix = "glibc/2.41"
			},
		},
		{
			name: "non-regular current-keg source",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(after map[string]fileState, _ *classifyOptions) {
				after["Cellar/glibc/2.42/etc/ld.so.conf"] = fileState{Type: "directory"}
			},
		},
		{
			name: "non-regular verified inventory",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(_ map[string]fileState, options *classifyOptions) {
				options.verified.Inventory[0].Type = bottle.EntrySymlink
			},
		},
		{
			name: "source mode differs from verified inventory",
			node: resolution.Node{Name: "glibc", PkgVersion: "2.42"},
			path: "etc/ld.so.conf",
			mutate: func(after map[string]fileState, _ *classifyOptions) {
				state := after["Cellar/glibc/2.42/etc/ld.so.conf"]
				state.Mode = 0o600
				after["Cellar/glibc/2.42/etc/ld.so.conf"] = state
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := loaderConfigurationSnapshot(tc.node)
			options := loaderConfigurationOptions(tc.node)
			if tc.path != "etc/ld.so.conf" {
				delete(after, "etc/ld.so.conf")
				after[path.Join("Cellar", tc.node.Name, tc.node.PkgVersion, tc.path)] = fileState{Type: "regular", Mode: 0o644}
				after[tc.path] = fileState{Type: "symlink", Link: path.Join("/prefix", "Cellar", tc.node.Name, tc.node.PkgVersion, tc.path)}
				options.verified.Inventory[0].KegPath = tc.path
			}
			if tc.mutate != nil {
				tc.mutate(after, &options)
			}
			if err := classify("/prefix", tc.node, nil, after, []Change{{Path: tc.path, Kind: "created"}}, options); err == nil {
				t.Fatalf("unsafe loader configuration link %s accepted", tc.path)
			}
		})
	}
}

func TestClassifyRejectsReplacingExistingGlibcLoaderConfiguration(t *testing.T) {
	node := resolution.Node{Name: "glibc", PkgVersion: "2.42"}
	after := loaderConfigurationSnapshot(node)
	before := maps.Clone(after)
	before["etc/ld.so.conf"] = fileState{Type: "regular", Mode: 0o644}
	if err := classify("/prefix", node, before, after, []Change{{Path: "etc/ld.so.conf", Kind: "modified"}}, loaderConfigurationOptions(node)); err == nil {
		t.Fatal("replacement of pre-existing loader configuration accepted")
	}
}

func loaderConfigurationSnapshot(node resolution.Node) map[string]fileState {
	keg := path.Join("Cellar", node.Name, node.PkgVersion)
	source := path.Join(keg, "etc/ld.so.conf")
	return map[string]fileState{
		"Cellar":                    {Type: "directory"},
		path.Dir(keg):               {Type: "directory"},
		keg:                         {Type: "directory"},
		path.Join(keg, "etc"):       {Type: "directory"},
		source:                      {Type: "regular", Mode: 0o644},
		"opt":                       {Type: "directory"},
		path.Join("opt", node.Name): {Type: "symlink", Link: path.Join("..", keg)},
		"etc":                       {Type: "directory"},
		"etc/ld.so.conf":            {Type: "symlink", Link: path.Join("/prefix", source)},
		"var":                       {Type: "directory"},
	}
}

func loaderConfigurationOptions(node resolution.Node) classifyOptions {
	return classifyOptions{verified: bottle.Result{
		Name:       node.Name,
		PkgVersion: node.PkgVersion,
		KegPrefix:  path.Join(node.Name, node.PkgVersion),
		Inventory: []bottle.InventoryEntry{{
			KegPath: "etc/ld.so.conf",
			Type:    bottle.EntryRegular,
			Mode:    0o644,
		}},
	}}
}

func TestValidateNodeNPMRuntimeAndGlobalLinks(t *testing.T) {
	const prefix = "/prefix"
	directory := func() fileState {
		return fileState{Type: "directory", Mode: os.ModeDir | 0o755, UID: 1000, GID: 1000, OwnershipKnown: true}
	}
	regular := func(content string, mode os.FileMode) fileState {
		digest := sha256.Sum256([]byte(content))
		return fileState{Type: "regular", Mode: mode, Size: int64(len(content)), Digest: hex.EncodeToString(digest[:]), Inode: "inode:" + hex.EncodeToString(digest[:]), Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
	}
	symlink := func(target string) fileState {
		return fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: target, Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
	}

	node := resolution.Node{Name: "node", FullName: "homebrew/core/node", PkgVersion: "1"}
	verified := bottle.Result{Name: "node", PkgVersion: "1", KegPrefix: "node/1"}
	sourceRoot := "Cellar/node/1/libexec/lib/node_modules/npm"
	after := map[string]fileState{}
	for _, rel := range []string{
		"Cellar", "Cellar/node", "Cellar/node/1", "Cellar/node/1/bin", "Cellar/node/1/libexec", "Cellar/node/1/libexec/lib", "Cellar/node/1/libexec/lib/node_modules",
		sourceRoot, sourceRoot + "/bin", sourceRoot + "/man", sourceRoot + "/man/man1",
		"lib", nodeNPMRuntimeParent, nodeNPMRuntimeRoot, nodeNPMRuntimeRoot + "/bin", nodeNPMRuntimeRoot + "/man", nodeNPMRuntimeRoot + "/man/man1",
		"bin", "share", "share/man", "share/man/man1",
	} {
		after[rel] = directory()
	}
	after[sourceRoot+"/bin/npm-cli.js"] = regular("npm\n", 0o755)
	after[sourceRoot+"/bin/npx-cli.js"] = regular("npx\n", 0o755)
	after[sourceRoot+"/man/man1/npm.1"] = regular("npm manual\n", 0o644)
	after[sourceRoot+"/npmrc"] = regular("private=true\n", 0o644)
	after[nodeNPMRuntimeRoot+"/bin/npm-cli.js"] = after[sourceRoot+"/bin/npm-cli.js"]
	after[nodeNPMRuntimeRoot+"/bin/npx-cli.js"] = after[sourceRoot+"/bin/npx-cli.js"]
	after[nodeNPMRuntimeRoot+"/man/man1/npm.1"] = after[sourceRoot+"/man/man1/npm.1"]
	after[sourceRoot+"/node-link"] = symlink("bin/npm-cli.js")
	after[nodeNPMRuntimeRoot+"/node-link"] = symlink("bin/npm-cli.js")
	after[nodeNPMRuntimeRoot+"/npmrc"] = regular("prefix = /prefix\n", 0o644)
	after["Cellar/node/1/bin/npm"] = symlink("../../../../lib/node_modules/npm/bin/npm-cli.js")
	after["Cellar/node/1/bin/npx"] = symlink("../../../../lib/node_modules/npm/bin/npx-cli.js")
	after["bin/npm"] = symlink("../Cellar/node/1/bin/npm")
	after["bin/npx"] = symlink("../Cellar/node/1/bin/npx")
	after["share/man/man1/npm.1"] = symlink("../../../lib/node_modules/npm/man/man1/npm.1")

	options := classifyOptions{verified: verified, runtimeUID: 1000, runtimeGID: 1000}
	generated, err := validateNodeNPMRuntime(prefix, node, nil, after, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{nodeNPMRuntimeRoot + "/bin/npm-cli.js", nodeNPMRuntimeRoot + "/node-link", nodeNPMRuntimeRoot + "/npmrc"} {
		if _, ok := generated[rel]; !ok {
			t.Fatalf("generated path %q is missing", rel)
		}
	}
	for _, rel := range []string{"bin/npm", "bin/npx", "share/man/man1/npm.1"} {
		resolved, err := resolveSnapshotPath(prefix, after, rel)
		if err != nil || !isNodeNPMGlobalLink(prefix, node, "Cellar/node/1", rel, resolved, after, generated) {
			t.Fatalf("Node npm global link %q rejected: resolved=%q err=%v", rel, resolved, err)
		}
	}

	mutated := maps.Clone(after)
	state := mutated[nodeNPMRuntimeRoot+"/bin/npm-cli.js"]
	state.Digest = strings.Repeat("f", 64)
	mutated[nodeNPMRuntimeRoot+"/bin/npm-cli.js"] = state
	if _, err := validateNodeNPMRuntime(prefix, node, nil, mutated, options); err == nil {
		t.Fatal("modified Node npm runtime copy accepted")
	}
	extra := maps.Clone(after)
	extra[nodeNPMRuntimeRoot+"/injected"] = regular("injected\n", 0o644)
	if _, err := validateNodeNPMRuntime(prefix, node, nil, extra, options); err == nil {
		t.Fatal("extra Node npm runtime path accepted")
	}
	redirected := maps.Clone(after)
	redirected["bin/npm"] = symlink("../lib/node_modules/npm/bin/npm-cli.js")
	resolved, err := resolveSnapshotPath(prefix, redirected, "bin/npm")
	if err != nil {
		t.Fatal(err)
	}
	if isNodeNPMGlobalLink(prefix, node, "Cellar/node/1", "bin/npm", resolved, redirected, generated) {
		t.Fatal("Node npm global command bypassed the current keg")
	}
}

func TestValidateNodeNPMRuntimeRejectsRootOnlySourceTree(t *testing.T) {
	const prefix = "/prefix"
	node, after, options, _ := nodeNPMRuntimeLimitFixture(prefix)

	_, err := validateNodeNPMRuntime(prefix, node, nil, after, options)
	if err == nil || !strings.Contains(err.Error(), "verified Node npm source tree is empty") {
		t.Fatalf("root-only Node npm source tree error = %v", err)
	}
}

func TestValidateNodeNPMRuntimeEntryLimit(t *testing.T) {
	const prefix = "/prefix"
	node, after, options, sourceRoot := nodeNPMRuntimeLimitFixture(prefix)
	state := nodeNPMRuntimeLimitRegular(0)
	for i := 0; i < nodeNPMRuntimeMaxEntries-2; i++ {
		name := fmt.Sprintf("entry-%05d", i)
		after[path.Join(sourceRoot, name)] = state
		after[path.Join(nodeNPMRuntimeRoot, name)] = state
	}
	if _, err := validateNodeNPMRuntime(prefix, node, nil, after, options); err != nil {
		t.Fatalf("Node npm runtime at entry limit rejected: %v", err)
	}

	overLimit := maps.Clone(after)
	name := fmt.Sprintf("entry-%05d", nodeNPMRuntimeMaxEntries-2)
	overLimit[path.Join(sourceRoot, name)] = state
	overLimit[path.Join(nodeNPMRuntimeRoot, name)] = state
	_, err := validateNodeNPMRuntime(prefix, node, nil, overLimit, options)
	want := fmt.Sprintf("Node npm runtime exceeds %d entries", nodeNPMRuntimeMaxEntries)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Node npm runtime above entry limit error = %v, want %q", err, want)
	}
}

func TestValidateNodeNPMRuntimeByteLimit(t *testing.T) {
	const prefix = "/prefix"
	node, after, options, sourceRoot := nodeNPMRuntimeLimitFixture(prefix)
	payloadPath := path.Join(sourceRoot, "payload")
	runtimePayloadPath := path.Join(nodeNPMRuntimeRoot, "payload")
	npmrcSize := after[path.Join(nodeNPMRuntimeRoot, "npmrc")].Size
	payloadLimit := nodeNPMRuntimeMaxBytes - npmrcSize
	state := nodeNPMRuntimeLimitRegular(payloadLimit)
	after[payloadPath] = state
	after[runtimePayloadPath] = state
	if _, err := validateNodeNPMRuntime(prefix, node, nil, after, options); err != nil {
		t.Fatalf("Node npm runtime at byte limit rejected: %v", err)
	}

	overLimit := maps.Clone(after)
	state = nodeNPMRuntimeLimitRegular(payloadLimit + 1)
	overLimit[payloadPath] = state
	overLimit[runtimePayloadPath] = state
	_, err := validateNodeNPMRuntime(prefix, node, nil, overLimit, options)
	want := fmt.Sprintf("Node npm runtime exceeds %d bytes", nodeNPMRuntimeMaxBytes)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Node npm runtime above byte limit error = %v, want %q", err, want)
	}
}

func nodeNPMRuntimeLimitFixture(prefix string) (resolution.Node, map[string]fileState, classifyOptions, string) {
	directory := fileState{Type: "directory", Mode: os.ModeDir | 0o755, UID: 1000, GID: 1000, OwnershipKnown: true}
	node := resolution.Node{Name: nodeFormula, FullName: "homebrew/core/" + nodeFormula, PkgVersion: "1"}
	sourceRoot := path.Join("Cellar", node.Name, node.PkgVersion, nodeNPMSourceRoot)
	after := map[string]fileState{
		sourceRoot:           directory,
		nodeNPMRuntimeParent: directory,
		nodeNPMRuntimeRoot:   directory,
	}
	npmrc := []byte("prefix = " + filepath.ToSlash(prefix) + "\n")
	npmrcDigest := sha256.Sum256(npmrc)
	after[path.Join(nodeNPMRuntimeRoot, "npmrc")] = fileState{
		Type: "regular", Mode: 0o644, Size: int64(len(npmrc)), Digest: hex.EncodeToString(npmrcDigest[:]),
		Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true,
	}
	verified := bottle.Result{Name: node.Name, PkgVersion: node.PkgVersion, KegPrefix: path.Join(node.Name, node.PkgVersion)}
	return node, after, classifyOptions{verified: verified, runtimeUID: 1000, runtimeGID: 1000}, sourceRoot
}

func nodeNPMRuntimeLimitRegular(size int64) fileState {
	return fileState{
		Type: "regular", Mode: 0o644, Size: size, Digest: strings.Repeat("a", 64),
		Inode: "inode:node-npm-limit", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true,
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
	if err := classify(prefix, resolution.Node{Name: "glibc", FullName: "homebrew/core/glibc", PkgVersion: "2"}, before, after, changes); err != nil {
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
	if err := verifyReceipt(link, resolution.Node{Name: "x"}, nil); err == nil {
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

func TestDiffIgnoresDirectoryStructuralMetadata(t *testing.T) {
	before := map[string]fileState{
		"share/xml": {
			Type: "directory", Mode: os.ModeDir | 0o755, Size: 4096,
			Inode: "1:2", Links: 2, UID: 1000, GID: 1000, OwnershipKnown: true,
		},
	}
	after := maps.Clone(before)
	state := after["share/xml"]
	state.Size = 8192
	state.Links = 3
	after["share/xml"] = state
	if changes := diff(before, after); len(changes) != 0 {
		t.Fatalf("directory child metadata produced changes: %#v", changes)
	}
}

func TestDiffDetectsDirectorySecurityMetadataChanges(t *testing.T) {
	base := fileState{
		Type: "directory", Mode: os.ModeDir | 0o755, Size: 4096,
		Inode: "1:2", Links: 2, UID: 1000, GID: 1000, OwnershipKnown: true,
	}
	tests := []struct {
		name   string
		mutate func(*fileState)
	}{
		{name: "type", mutate: func(state *fileState) { state.Type = "regular" }},
		{name: "mode", mutate: func(state *fileState) { state.Mode = os.ModeDir | 0o775 }},
		{name: "inode", mutate: func(state *fileState) { state.Inode = "1:3" }},
		{name: "uid", mutate: func(state *fileState) { state.UID = 0 }},
		{name: "gid", mutate: func(state *fileState) { state.GID = 0 }},
		{name: "ownership availability", mutate: func(state *fileState) { state.OwnershipKnown = false }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := map[string]fileState{"share/xml": base}
			after := maps.Clone(before)
			state := after["share/xml"]
			tc.mutate(&state)
			after["share/xml"] = state
			changes := diff(before, after)
			if len(changes) != 1 || changes[0].Path != "share/xml" || changes[0].Kind != "modified" {
				t.Fatalf("security metadata change not detected: %#v", changes)
			}
		})
	}
}

func TestClassifyAllowsChildBelowPreexistingGlobalDirectory(t *testing.T) {
	directory := func(inode string, links uint64) fileState {
		return fileState{
			Type: "directory", Mode: os.ModeDir | 0o755,
			Inode: inode, Links: links, UID: 1000, GID: 1000, OwnershipKnown: true,
		}
	}
	before := map[string]fileState{
		".":         directory("1:1", 10),
		"Cellar":    directory("1:2", 5),
		"opt":       directory("1:3", 2),
		"etc":       directory("1:4", 2),
		"var":       directory("1:5", 2),
		"share":     directory("1:6", 3),
		"share/xml": directory("1:7", 2),
	}
	after := maps.Clone(before)
	for _, path := range []string{".", "Cellar", "share/xml"} {
		state := after[path]
		state.Links++
		state.Size += 4096
		after[path] = state
	}
	after["Cellar/hello"] = directory("1:8", 3)
	after["Cellar/hello/1"] = directory("1:9", 2)
	after["opt/hello"] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../Cellar/hello/1", Inode: "1:10", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
	after["share/xml/dbus-1"] = directory("1:11", 2)

	changes := diff(before, after)
	for _, change := range changes {
		if change.Path == "share/xml" || change.Path == "Cellar" || change.Path == "." {
			t.Fatalf("structural directory metadata was classified as a change: %#v", changes)
		}
	}
	if err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, before, after, changes); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyRejectsSecurityChangeToPreexistingGlobalDirectory(t *testing.T) {
	base := fileState{
		Type: "directory", Mode: os.ModeDir | 0o755,
		Inode: "1:7", Links: 2, UID: 1000, GID: 1000, OwnershipKnown: true,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*fileState)
	}{
		{name: "mode", mutate: func(state *fileState) { state.Mode = os.ModeDir | 0o775 }},
		{name: "owner", mutate: func(state *fileState) { state.UID = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := map[string]fileState{"share/xml": base}
			after := maps.Clone(before)
			state := after["share/xml"]
			tc.mutate(&state)
			after["share/xml"] = state
			changes := diff(before, after)
			if len(changes) != 1 || changes[0].Path != "share/xml" || changes[0].Kind != "modified" {
				t.Fatalf("security change was not classified at share/xml: %#v", changes)
			}
			err := classify("/prefix", resolution.Node{Name: "hello", PkgVersion: "1"}, before, after, changes)
			if err == nil || !strings.Contains(err.Error(), "pre-existing shared path share/xml") {
				t.Fatalf("pre-existing shared-directory security change not rejected: %v", err)
			}
		})
	}
}

func TestSnapshotCapturesDirectoryIdentityAndOwnership(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	states, err := snapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".", "share"} {
		state := states[path]
		if state.Inode == "" {
			t.Skip("platform does not expose stable inode metadata")
		}
		if state.Links == 0 || !state.OwnershipKnown {
			t.Fatalf("%s missing directory identity or ownership: %#v", path, state)
		}
	}
}

func TestClassifyAllowsVerifiedSharedDirectoryExpansion(t *testing.T) {
	node, before, after := sharedDirectoryExpansionFixture()
	changes := diff(before, after)
	if err := classify("/prefix", node, before, after, changes); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyRejectsUnsafeSharedDirectoryExpansions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]fileState, map[string]fileState)
	}{
		{
			name: "expanded root mode changed",
			mutate: func(_ map[string]fileState, after map[string]fileState) {
				state := after["share/xml"]
				state.Mode = os.ModeDir | 0o775
				after["share/xml"] = state
			},
		},
		{
			name: "expanded root owner changed",
			mutate: func(_ map[string]fileState, after map[string]fileState) {
				state := after["share/xml"]
				state.UID = 0
				after["share/xml"] = state
			},
		},
		{
			name: "prior target outside cellar",
			mutate: func(before, _ map[string]fileState) {
				before["outside"] = testSnapshotDirectory("1:20", 2)
				state := before["share/xml"]
				state.Link = "../../outside"
				before["share/xml"] = state
			},
		},
		{
			name: "preserved file omitted",
			mutate: func(_ map[string]fileState, after map[string]fileState) {
				delete(after, "share/xml/fontconfig/fonts.dtd")
			},
		},
		{
			name: "preserved file redirected",
			mutate: func(_ map[string]fileState, after map[string]fileState) {
				state := after["share/xml/fontconfig/fonts.dtd"]
				state.Link = "../../../Cellar/dbus/1/share/xml/dbus-1/busconfig.dtd"
				after["share/xml/fontconfig/fonts.dtd"] = state
			},
		},
		{
			name: "current child redirected",
			mutate: func(_ map[string]fileState, after map[string]fileState) {
				state := after["share/xml/dbus-1"]
				state.Link = "../../Cellar/fontconfig/2/share/xml/fontconfig"
				after["share/xml/dbus-1"] = state
			},
		},
		{
			name: "current child bypasses current-keg symlink",
			mutate: func(_ map[string]fileState, after map[string]fileState) {
				after["Cellar/dbus/1/share/xml/dbus-1"] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../../../../fontconfig/2/share/xml/fontconfig", Inode: "1:18", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
				state := after["share/xml/dbus-1"]
				state.Link = "../../Cellar/fontconfig/2/share/xml/fontconfig"
				after["share/xml/dbus-1"] = state
			},
		},
		{
			name: "unattributed regular file",
			mutate: func(_ map[string]fileState, after map[string]fileState) {
				after["share/xml/injected"] = fileState{Type: "regular", Mode: 0o644, Digest: strings.Repeat("e", 64), Inode: "1:21", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
			},
		},
		{
			name: "unattributed directory",
			mutate: func(_ map[string]fileState, after map[string]fileState) {
				after["share/xml/injected"] = testSnapshotDirectory("1:21", 2)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, before, after := sharedDirectoryExpansionFixture()
			tc.mutate(before, after)
			if err := classify("/prefix", node, before, after, diff(before, after)); err == nil {
				t.Fatal("unsafe shared-directory expansion accepted")
			}
		})
	}
}

func sharedDirectoryExpansionFixture() (resolution.Node, map[string]fileState, map[string]fileState) {
	node := resolution.Node{Name: "dbus", PkgVersion: "1"}
	before := map[string]fileState{
		".":                             testSnapshotDirectory("1:1", 10),
		"Cellar":                        testSnapshotDirectory("1:2", 5),
		"Cellar/fontconfig":             testSnapshotDirectory("1:3", 3),
		"Cellar/fontconfig/2":           testSnapshotDirectory("1:4", 3),
		"Cellar/fontconfig/2/share":     testSnapshotDirectory("1:5", 3),
		"Cellar/fontconfig/2/share/xml": testSnapshotDirectory("1:6", 3),
		"Cellar/fontconfig/2/share/xml/fontconfig": testSnapshotDirectory("1:7", 2),
		"Cellar/fontconfig/2/share/xml/fontconfig/fonts.dtd": {
			Type: "regular", Mode: 0o644, Size: 4, Digest: strings.Repeat("a", 64),
			Inode: "1:8", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true,
		},
		"opt":       testSnapshotDirectory("1:9", 2),
		"etc":       testSnapshotDirectory("1:10", 2),
		"var":       testSnapshotDirectory("1:11", 2),
		"share":     testSnapshotDirectory("1:12", 3),
		"share/xml": {Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../Cellar/fontconfig/2/share/xml", Inode: "1:13", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true},
	}
	after := maps.Clone(before)
	after["Cellar/dbus"] = testSnapshotDirectory("1:14", 3)
	after["Cellar/dbus/1"] = testSnapshotDirectory("1:15", 3)
	after["Cellar/dbus/1/share"] = testSnapshotDirectory("1:16", 3)
	after["Cellar/dbus/1/share/xml"] = testSnapshotDirectory("1:17", 3)
	after["Cellar/dbus/1/share/xml/dbus-1"] = testSnapshotDirectory("1:18", 2)
	after["Cellar/dbus/1/share/xml/dbus-1/busconfig.dtd"] = fileState{
		Type: "regular", Mode: 0o644, Size: 4, Digest: strings.Repeat("b", 64),
		Inode: "1:19", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true,
	}
	after["opt/dbus"] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../Cellar/dbus/1", Inode: "1:20", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
	after["share/xml"] = testSnapshotDirectory("1:21", 4)
	after["share/xml/fontconfig"] = testSnapshotDirectory("1:22", 2)
	after["share/xml/fontconfig/fonts.dtd"] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../../../Cellar/fontconfig/2/share/xml/fontconfig/fonts.dtd", Inode: "1:23", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
	after["share/xml/dbus-1"] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "../../Cellar/dbus/1/share/xml/dbus-1", Inode: "1:24", Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true}
	return node, before, after
}

func testSnapshotDirectory(inode string, links uint64) fileState {
	return fileState{
		Type: "directory", Mode: os.ModeDir | 0o755,
		Inode: inode, Links: links, UID: 1000, GID: 1000, OwnershipKnown: true,
	}
}

func TestReconcileAllowsOwnerWriteForDeclaredChangedFile(t *testing.T) {
	node := resolution.Node{
		Name:       "hello",
		PkgVersion: "1",
		Bottle:     resolution.Bottle{Tab: resolution.BottleTab{ChangedFiles: []string{"lib/config"}}},
	}
	verified := bottle.Result{
		Name:       "hello",
		PkgVersion: "1",
		KegPrefix:  "hello/1",
		Inventory: []bottle.InventoryEntry{{
			Path:    "hello/1/lib/config",
			KegPath: "lib/config",
			Type:    bottle.EntryRegular,
			Mode:    0o444,
			SHA256:  "sha256:" + strings.Repeat("a", 64),
		}},
	}
	after := map[string]fileState{
		"Cellar/hello/1/lib/config": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("b", 64)},
	}
	if err := reconcileInstalledKeg("/prefix", node, verified, after); err != nil {
		t.Fatal(err)
	}
}

func TestAllowsInstallerUmaskTightening(t *testing.T) {
	tests := []struct {
		name     string
		expected fs.FileMode
		actual   fs.FileMode
		want     bool
	}{
		{name: "regular group write removed", expected: 0o664, actual: 0o644, want: true},
		{name: "directory group write removed", expected: 0o775, actual: 0o755, want: true},
		{name: "group and other write removed", expected: 0o777, actual: 0o755, want: true},
		{name: "unchanged", expected: 0o755, actual: 0o755},
		{name: "owner write removed", expected: 0o755, actual: 0o555},
		{name: "group read removed", expected: 0o775, actual: 0o715},
		{name: "group write added", expected: 0o755, actual: 0o775},
		{name: "sticky removed", expected: os.ModeSticky | 0o775, actual: 0o755},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowsInstallerUmaskTightening(tc.expected, tc.actual); got != tc.want {
				t.Fatalf("allowsInstallerUmaskTightening(%#o, %#o)=%t, want %t", tc.expected, tc.actual, got, tc.want)
			}
		})
	}
}

func TestReconcileAllowsInstallerUmaskTighteningForBottleMetadata(t *testing.T) {
	node := resolution.Node{Name: "libdf", PkgVersion: "0.5.6+d375b2d"}
	verified := bottle.Result{
		Name:       node.Name,
		PkgVersion: node.PkgVersion,
		KegPrefix:  node.Name + "/" + node.PkgVersion,
		Inventory: []bottle.InventoryEntry{
			{Path: "libdf/0.5.6+d375b2d/.brew", KegPath: ".brew", Type: bottle.EntryDirectory, Mode: 0o775},
			{Path: "libdf/0.5.6+d375b2d/.brew/libdf.rb", KegPath: ".brew/libdf.rb", Type: bottle.EntryRegular, Mode: 0o664, SHA256: "sha256:" + strings.Repeat("a", 64)},
			{Path: "libdf/0.5.6+d375b2d/INSTALL_RECEIPT.json", KegPath: "INSTALL_RECEIPT.json", Type: bottle.EntryRegular, Mode: 0o664, SHA256: "sha256:" + strings.Repeat("b", 64)},
			{Path: "libdf/0.5.6+d375b2d/sbom.spdx.json", KegPath: "sbom.spdx.json", Type: bottle.EntryRegular, Mode: 0o664, SHA256: "sha256:" + strings.Repeat("c", 64)},
		},
	}
	after := map[string]fileState{
		"Cellar/libdf/0.5.6+d375b2d/.brew":                {Type: "directory", Mode: os.ModeDir | 0o755},
		"Cellar/libdf/0.5.6+d375b2d/.brew/libdf.rb":       {Type: "regular", Mode: 0o644, Digest: strings.Repeat("a", 64)},
		"Cellar/libdf/0.5.6+d375b2d/INSTALL_RECEIPT.json": {Type: "regular", Mode: 0o644, Digest: strings.Repeat("b", 64)},
		"Cellar/libdf/0.5.6+d375b2d/sbom.spdx.json":       {Type: "regular", Mode: 0o644, Digest: strings.Repeat("c", 64)},
	}
	if err := reconcileInstalledKeg("/prefix", node, verified, after); err != nil {
		t.Fatal(err)
	}

	mutated := maps.Clone(after)
	formula := mutated["Cellar/libdf/0.5.6+d375b2d/.brew/libdf.rb"]
	formula.Digest = strings.Repeat("d", 64)
	mutated["Cellar/libdf/0.5.6+d375b2d/.brew/libdf.rb"] = formula
	if err := reconcileInstalledKeg("/prefix", node, verified, mutated); err == nil {
		t.Fatal("Formula metadata content mutation was accepted with umask tightening")
	}
}

func TestReconcileRejectsOtherPermissionChangesForDeclaredChangedFile(t *testing.T) {
	tests := []struct {
		name     string
		expected uint32
		actual   fs.FileMode
	}{
		{name: "remove owner write", expected: 0o644, actual: 0o444},
		{name: "add owner execute", expected: 0o444, actual: 0o544},
		{name: "add group write", expected: 0o444, actual: 0o464},
		{name: "add other write", expected: 0o444, actual: 0o446},
		{name: "add sticky", expected: 0o444, actual: os.ModeSticky | 0o644},
		{name: "add setuid", expected: 0o555, actual: os.ModeSetuid | 0o755},
		{name: "add setgid", expected: 0o555, actual: os.ModeSetgid | 0o755},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := resolution.Node{
				Name:       "hello",
				PkgVersion: "1",
				Bottle:     resolution.Bottle{Tab: resolution.BottleTab{ChangedFiles: []string{"lib/config"}}},
			}
			verified := bottle.Result{
				Name:       "hello",
				PkgVersion: "1",
				KegPrefix:  "hello/1",
				Inventory: []bottle.InventoryEntry{{
					Path:    "hello/1/lib/config",
					KegPath: "lib/config",
					Type:    bottle.EntryRegular,
					Mode:    tc.expected,
					SHA256:  "sha256:" + strings.Repeat("a", 64),
				}},
			}
			after := map[string]fileState{
				"Cellar/hello/1/lib/config": {Type: "regular", Mode: tc.actual, Digest: strings.Repeat("b", 64)},
			}
			if err := reconcileInstalledKeg("/prefix", node, verified, after); err == nil {
				t.Fatal("non-owner-write permission change accepted")
			}
		})
	}
}

func TestReconcileAllowsPythonVenvTemplateOwnerWritePostInstall(t *testing.T) {
	node := resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"}
	paths := []string{
		"lib/python3.14/venv/scripts/common/Activate.ps1",
		"lib/python3.14/venv/scripts/common/activate",
		"lib/python3.14/venv/scripts/common/activate.fish",
		"lib/python3.14/venv/scripts/posix/activate.csh",
	}
	verified := bottle.Result{
		Name:       node.Name,
		PkgVersion: node.PkgVersion,
		KegPrefix:  node.Name + "/" + node.PkgVersion,
		Formula: bottle.FormulaEvidence{
			Path:      node.Name + "/" + node.PkgVersion + "/.brew/" + node.Name + ".rb",
			ClassName: "PythonAT314",
			SHA256:    "sha256:" + strings.Repeat("f", 64),
			Size:      1,
		},
	}
	after := map[string]fileState{}
	for _, kegPath := range paths {
		verified.Inventory = append(verified.Inventory, bottle.InventoryEntry{
			Path:    verified.KegPrefix + "/" + kegPath,
			KegPath: kegPath,
			Type:    bottle.EntryRegular,
			Mode:    0o444,
			SHA256:  "sha256:" + strings.Repeat("a", 64),
		})
		after["Cellar/"+verified.KegPrefix+"/"+kegPath] = fileState{
			Type:   "regular",
			Mode:   0o644,
			Digest: strings.Repeat("a", 64),
		}
	}
	if err := reconcileInstalledKeg("/prefix", node, verified, after); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileRejectsBroaderPythonVenvPostInstallChanges(t *testing.T) {
	tests := []struct {
		name           string
		node           resolution.Node
		kegPath        string
		expectedMode   uint32
		actualMode     fs.FileMode
		expectedDigest string
		actualDigest   string
		omitFormula    bool
	}{
		{
			name:    "wrong formula minor",
			node:    resolution.Node{Name: "python@3.13", FullName: "homebrew/core/python@3.13", FormulaVersion: "3.13.9", PkgVersion: "3.13.9"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "malformed formula name",
			node:    resolution.Node{Name: "python@3.14-extra", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "mismatched formula version",
			node:    resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", FormulaVersion: "3.13.9", PkgVersion: "3.13.9"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "outside scripts subtree",
			node:    resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "scripts prefix spoof",
			node:    resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts-evil/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "missing verified formula evidence",
			node:    resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
			omitFormula: true,
		},
		{
			name:    "content mutation",
			node:    resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
			expectedDigest: strings.Repeat("a", 64), actualDigest: strings.Repeat("b", 64),
		},
		{
			name:    "group writable",
			node:    resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o664,
		},
		{
			name:    "made executable",
			node:    resolution.Node{Name: "python@3.14", FullName: "homebrew/core/python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o744,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expectedDigest := tc.expectedDigest
			if expectedDigest == "" {
				expectedDigest = strings.Repeat("a", 64)
			}
			actualDigest := tc.actualDigest
			if actualDigest == "" {
				actualDigest = expectedDigest
			}
			verified := bottle.Result{
				Name:       tc.node.Name,
				PkgVersion: tc.node.PkgVersion,
				KegPrefix:  tc.node.Name + "/" + tc.node.PkgVersion,
				Inventory: []bottle.InventoryEntry{{
					Path:    tc.node.Name + "/" + tc.node.PkgVersion + "/" + tc.kegPath,
					KegPath: tc.kegPath,
					Type:    bottle.EntryRegular,
					Mode:    tc.expectedMode,
					SHA256:  "sha256:" + expectedDigest,
				}},
			}
			if !tc.omitFormula {
				minor := strings.TrimPrefix(tc.node.Name, "python@")
				verified.Formula = bottle.FormulaEvidence{
					Path:      verified.KegPrefix + "/.brew/" + tc.node.Name + ".rb",
					ClassName: "PythonAT" + strings.ReplaceAll(minor, ".", ""),
					SHA256:    "sha256:" + strings.Repeat("f", 64),
					Size:      1,
				}
			}
			after := map[string]fileState{
				"Cellar/" + verified.KegPrefix + "/" + tc.kegPath: {
					Type: "regular", Mode: tc.actualMode, Digest: actualDigest,
				},
			}
			if err := reconcileInstalledKeg("/prefix", tc.node, verified, after); err == nil {
				t.Fatal("broader Python post-install mutation accepted")
			}
		})
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

func TestReconcileAllowsDeclaredHardlinkGroupPartitions(t *testing.T) {
	node := resolution.Node{Name: "glibc", PkgVersion: "2.39_1"}
	verified := bottle.Result{
		Name:       node.Name,
		PkgVersion: node.PkgVersion,
		KegPrefix:  "glibc/2.39_1",
		Inventory: []bottle.InventoryEntry{
			{Path: "glibc/2.39_1/bin/getconf", KegPath: "bin/getconf", Type: bottle.EntryRegular, Mode: 0o555, SHA256: "sha256:" + strings.Repeat("a", 64)},
			{Path: "glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64", KegPath: "libexec/getconf/POSIX_V6_LP64_OFF64", Type: bottle.EntryHardlink, Mode: 0o555, SHA256: "sha256:" + strings.Repeat("a", 64), ResolvedTarget: "glibc/2.39_1/bin/getconf"},
			{Path: "glibc/2.39_1/libexec/getconf/POSIX_V7_LP64_OFF64", KegPath: "libexec/getconf/POSIX_V7_LP64_OFF64", Type: bottle.EntryHardlink, Mode: 0o555, SHA256: "sha256:" + strings.Repeat("a", 64), ResolvedTarget: "glibc/2.39_1/bin/getconf"},
			{Path: "glibc/2.39_1/libexec/getconf/XBS5_LP64_OFF64", KegPath: "libexec/getconf/XBS5_LP64_OFF64", Type: bottle.EntryHardlink, Mode: 0o555, SHA256: "sha256:" + strings.Repeat("a", 64), ResolvedTarget: "glibc/2.39_1/bin/getconf"},
		},
	}
	paths := []string{
		"Cellar/glibc/2.39_1/bin/getconf",
		"Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64",
		"Cellar/glibc/2.39_1/libexec/getconf/POSIX_V7_LP64_OFF64",
		"Cellar/glibc/2.39_1/libexec/getconf/XBS5_LP64_OFF64",
	}
	for _, tc := range []struct {
		name   string
		inodes []string
		links  []uint64
	}{
		{name: "preserved", inodes: []string{"1:1", "1:1", "1:1", "1:1"}, links: []uint64{4, 4, 4, 4}},
		{name: "partially split", inodes: []string{"1:1", "1:1", "1:2", "1:3"}, links: []uint64{2, 2, 1, 1}},
		{name: "fully split", inodes: []string{"1:1", "1:2", "1:3", "1:4"}, links: []uint64{1, 1, 1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := map[string]fileState{}
			for i, rel := range paths {
				after[rel] = fileState{Type: "regular", Mode: 0o555, Digest: strings.Repeat("a", 64), Inode: tc.inodes[i], Links: tc.links[i]}
			}
			if err := reconcileInstalledKeg("/prefix", node, verified, after); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReconcileRejectsInvalidDeclaredHardlinkGroupPartitions(t *testing.T) {
	node := resolution.Node{Name: "glibc", PkgVersion: "2.39_1"}
	verified := bottle.Result{
		Name:       node.Name,
		PkgVersion: node.PkgVersion,
		KegPrefix:  "glibc/2.39_1",
		Inventory: []bottle.InventoryEntry{
			{Path: "glibc/2.39_1/bin/getconf", KegPath: "bin/getconf", Type: bottle.EntryRegular, Mode: 0o555, SHA256: "sha256:" + strings.Repeat("a", 64)},
			{Path: "glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64", KegPath: "libexec/getconf/POSIX_V6_LP64_OFF64", Type: bottle.EntryHardlink, Mode: 0o555, SHA256: "sha256:" + strings.Repeat("a", 64), ResolvedTarget: "glibc/2.39_1/bin/getconf"},
		},
	}
	base := map[string]fileState{
		"Cellar/glibc/2.39_1/bin/getconf":                         {Type: "regular", Mode: 0o555, Digest: strings.Repeat("a", 64), Inode: "1:1", Links: 1},
		"Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64": {Type: "regular", Mode: 0o555, Digest: strings.Repeat("a", 64), Inode: "1:2", Links: 1},
	}
	for _, tc := range []struct {
		name   string
		mutate func(*bottle.Result, map[string]fileState)
	}{
		{
			name: "undeclared cross-keg alias",
			mutate: func(_ *bottle.Result, after map[string]fileState) {
				state := after["Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64"]
				state.Links = 2
				after["Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64"] = state
				after["Cellar/other/1/alias"] = fileState{Type: "regular", Mode: 0o555, Digest: strings.Repeat("a", 64), Inode: "1:2", Links: 2}
			},
		},
		{
			name: "merged declared groups",
			mutate: func(result *bottle.Result, after map[string]fileState) {
				result.Inventory = append(result.Inventory, bottle.InventoryEntry{Path: "glibc/2.39_1/bin/other", KegPath: "bin/other", Type: bottle.EntryRegular, Mode: 0o555, SHA256: "sha256:" + strings.Repeat("a", 64)})
				state := after["Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64"]
				state.Links = 2
				after["Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64"] = state
				after["Cellar/glibc/2.39_1/bin/other"] = fileState{Type: "regular", Mode: 0o555, Digest: strings.Repeat("a", 64), Inode: "1:2", Links: 2}
			},
		},
		{
			name: "unobserved alias",
			mutate: func(_ *bottle.Result, after map[string]fileState) {
				state := after["Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64"]
				state.Links = 2
				after["Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64"] = state
			},
		},
		{
			name: "divergent relocated content",
			mutate: func(result *bottle.Result, after map[string]fileState) {
				for i := range result.Inventory {
					result.Inventory[i].Relocatable = true
				}
				state := after["Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64"]
				state.Digest = strings.Repeat("b", 64)
				after["Cellar/glibc/2.39_1/libexec/getconf/POSIX_V6_LP64_OFF64"] = state
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := verified
			result.Inventory = append([]bottle.InventoryEntry(nil), verified.Inventory...)
			after := maps.Clone(base)
			tc.mutate(&result, after)
			if err := reconcileInstalledKeg("/prefix", node, result, after); err == nil {
				t.Fatal("invalid installed hardlink partition accepted")
			}
		})
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

const (
	gdkPixbufTestLibrsvg    = "librsvg"
	gdkPixbufTestWebP       = "webp-pixbuf-loader"
	gdkPixbufTestRuntimeUID = uint32(4242)
	gdkPixbufTestRuntimeGID = uint32(4343)
)

var gdkPixbufTestVersions = map[string]string{
	gdkPixbufFormula:     "2.44.7",
	gdkPixbufTestLibrsvg: "2.60.0",
	gdkPixbufTestWebP:    "0.2.7",
}

var gdkPixbufTestModules = map[string]string{
	gdkPixbufFormula:     "libpixbufloader-png.so",
	gdkPixbufTestLibrsvg: "libpixbufloader_svg.so",
	gdkPixbufTestWebP:    "libpixbufloader-webp.so",
}

type gdkPixbufCacheFixture struct {
	prefix      string
	node        resolution.Node
	verified    bottle.Result
	before      map[string]fileState
	after       map[string]fileState
	closureKegs map[string]struct{}
	priorCache  []byte
}

func (fixture gdkPixbufCacheFixture) options() classifyOptions {
	return classifyOptions{
		optNames:            map[string]struct{}{fixture.node.Name: {}},
		closureKegs:         fixture.closureKegs,
		verified:            fixture.verified,
		runtimeUID:          gdkPixbufTestRuntimeUID,
		runtimeGID:          gdkPixbufTestRuntimeGID,
		priorGdkPixbufCache: append([]byte(nil), fixture.priorCache...),
	}
}

func newGdkPixbufCacheFixture(t *testing.T, writer string, includeContributor bool, transform func(string, string) string) gdkPixbufCacheFixture {
	t.Helper()
	prefix := t.TempDir()
	for _, directory := range []string{"etc", "var", "opt", gdkPixbufLoadersDirectoryPath} {
		if err := os.MkdirAll(filepath.Join(prefix, filepath.FromSlash(directory)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	formulae := []string{gdkPixbufFormula}
	if includeContributor {
		formulae = append(formulae, writer)
	}
	globalLoaderDirectory := filepath.Join(prefix, filepath.FromSlash(gdkPixbufLoadersDirectoryPath))
	for _, formula := range formulae {
		target := filepath.Join(prefix, "Cellar", formula, gdkPixbufTestVersions[formula], "lib", "gdk-pixbuf-2.0", "2.10.0", "loaders", gdkPixbufTestModules[formula])
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("loader\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		linkTarget, err := filepath.Rel(globalLoaderDirectory, target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(linkTarget, filepath.Join(globalLoaderDirectory, gdkPixbufTestModules[formula])); err != nil {
			t.Fatal(err)
		}
	}
	writerVersion, ok := gdkPixbufTestVersions[writer]
	if !ok {
		t.Fatalf("unsupported fixture writer %q", writer)
	}
	if err := os.Symlink(path.Join("../Cellar", writer, writerVersion), filepath.Join(prefix, "opt", writer)); err != nil {
		t.Fatal(err)
	}
	content := gdkPixbufCacheContent(prefix, formulae)
	if transform != nil {
		content = transform(prefix, content)
	}
	cachePath := filepath.Join(prefix, filepath.FromSlash(gdkPixbufLoadersCachePath))
	if err := os.WriteFile(cachePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cachePath, 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := snapshot(prefix)
	if err != nil {
		t.Fatal(err)
	}
	cacheState := after[gdkPixbufLoadersCachePath]
	cacheState.UID = gdkPixbufTestRuntimeUID
	cacheState.GID = gdkPixbufTestRuntimeGID
	cacheState.OwnershipKnown = true
	after[gdkPixbufLoadersCachePath] = cacheState

	before := map[string]fileState{}
	var priorCache []byte
	if includeContributor {
		before = maps.Clone(after)
		for rel := range before {
			if snapshotPathWithin(rel, path.Join("Cellar", writer)) || rel == path.Join("opt", writer) || rel == path.Join(gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[writer]) {
				delete(before, rel)
			}
		}
		priorCache = []byte(gdkPixbufCacheContent(prefix, []string{gdkPixbufFormula}))
		prior := cacheState
		prior.Size = int64(len(priorCache))
		sum := sha256.Sum256(priorCache)
		prior.Digest = fmt.Sprintf("%x", sum[:])
		before[gdkPixbufLoadersCachePath] = prior
	}
	closureKegs := map[string]struct{}{}
	for _, formula := range formulae {
		closureKegs[path.Join("Cellar", formula, gdkPixbufTestVersions[formula])] = struct{}{}
	}
	node := resolution.Node{Name: writer, FullName: "homebrew/core/" + writer, PkgVersion: writerVersion}
	if writer != gdkPixbufFormula {
		node.Dependencies = []resolution.Requirement{{Name: gdkPixbufFormula}}
	}
	kegPath := path.Join(gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[writer])
	verified := bottle.Result{
		Name:       writer,
		PkgVersion: writerVersion,
		KegPrefix:  path.Join(writer, writerVersion),
		Inventory: []bottle.InventoryEntry{{
			Path:    path.Join(writer, writerVersion, kegPath),
			KegPath: kegPath,
			Type:    bottle.EntryRegular,
			Mode:    0o444,
			SHA256:  "sha256:" + strings.Repeat("c", sha256.Size*2),
		}},
	}
	return gdkPixbufCacheFixture{prefix: prefix, node: node, verified: verified, before: before, after: after, closureKegs: closureKegs, priorCache: priorCache}
}

func gdkPixbufCacheContent(prefix string, formulae []string) string {
	prefix = filepath.ToSlash(prefix)
	var content strings.Builder
	content.WriteString("# GdkPixbuf Image Loader Modules file\n")
	content.WriteString("# Automatically generated file, do not edit\n")
	content.WriteString("# Created by gdk-pixbuf-query-loaders from gdk-pixbuf-test\n")
	content.WriteString("#\n")
	fmt.Fprintf(&content, "# LoaderDir = %s\n#\n", path.Join(prefix, gdkPixbufLoadersDirectoryPath))
	for _, formula := range formulae {
		fmt.Fprintf(&content, "\"%s\"\n", path.Join(prefix, gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[formula]))
		fmt.Fprintf(&content, "\"%s\" 5 \"gdk-pixbuf\" \"test loader\" \"LGPL\"\n", formula)
		content.WriteString("\"image/test\" \"\"\n")
		content.WriteString("\"test\" \"\"\n")
		content.WriteString("\"abc\" \"\" 100\n\n")
	}
	return content.String()
}

func TestClassifyAllowsControlledGdkPixbufLoaderCacheWriters(t *testing.T) {
	for _, tc := range []struct {
		name               string
		writer             string
		includeContributor bool
		kind               string
	}{
		{name: "gdk-pixbuf creates cache", writer: gdkPixbufFormula, kind: "created"},
		{name: "librsvg underscore loader refreshes cache", writer: gdkPixbufTestLibrsvg, includeContributor: true, kind: "modified"},
		{name: "verified future contributor refreshes cache", writer: gdkPixbufTestWebP, includeContributor: true, kind: "modified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGdkPixbufCacheFixture(t, tc.writer, tc.includeContributor, nil)
			changes := []Change{{Path: gdkPixbufLoadersCachePath, Kind: tc.kind}}
			if err := classify(fixture.prefix, fixture.node, fixture.before, fixture.after, changes, fixture.options()); err != nil {
				t.Fatal(err)
			}
			if changes[0].Classification != "gdk-pixbuf-loader-cache" {
				t.Fatalf("classification=%q", changes[0].Classification)
			}
		})
	}
}

func TestClassifyGdkPixbufLoaderCacheWritersUseExactV2FormulaIDs(t *testing.T) {
	for _, tc := range []struct {
		writer             string
		includeContributor bool
		kind               string
	}{
		{writer: gdkPixbufFormula, kind: "created"},
		{writer: gdkPixbufTestLibrsvg, includeContributor: true, kind: "modified"},
		{writer: gdkPixbufTestWebP, includeContributor: true, kind: "modified"},
	} {
		t.Run(tc.writer, func(t *testing.T) {
			fixture := newGdkPixbufCacheFixture(t, tc.writer, tc.includeContributor, nil)
			changes := []Change{{Path: gdkPixbufLoadersCachePath, Kind: tc.kind}}

			fixture.node.PolicyFormulaID = "homebrew/core/" + tc.writer
			if err := classify(fixture.prefix, fixture.node, fixture.before, fixture.after, changes, fixture.options()); err != nil {
				t.Fatalf("exact core Formula ID rejected: %v", err)
			}

			fixture.node.PolicyFormulaID = "acme/tools/" + tc.writer
			changes[0].Classification = ""
			if err := classify(fixture.prefix, fixture.node, fixture.before, fixture.after, changes, fixture.options()); err == nil {
				t.Fatal("non-core Formula ID received loader-cache writer capability")
			}
		})
	}
}

func TestClassifyRejectsUncontrolledGdkPixbufLoaderCacheWriters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writer string
		kind   string
	}{
		{name: "unrelated formula creation", writer: "hello", kind: "created"},
		{name: "gdk-pixbuf modification", writer: gdkPixbufFormula, kind: "modified"},
		{name: "librsvg creation", writer: gdkPixbufTestLibrsvg, kind: "created"},
		{name: "librsvg removal", writer: gdkPixbufTestLibrsvg, kind: "removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureWriter := tc.writer
			if fixtureWriter != gdkPixbufFormula && fixtureWriter != gdkPixbufTestLibrsvg {
				fixtureWriter = gdkPixbufFormula
			}
			fixture := newGdkPixbufCacheFixture(t, fixtureWriter, fixtureWriter == gdkPixbufTestLibrsvg, nil)
			fixture.node.Name = tc.writer
			fixture.node.PkgVersion = gdkPixbufTestVersions[fixtureWriter]
			changes := []Change{{Path: gdkPixbufLoadersCachePath, Kind: tc.kind}}
			if tc.kind == "removed" {
				fixture.after = maps.Clone(fixture.after)
				delete(fixture.after, gdkPixbufLoadersCachePath)
			}
			if err := classify(fixture.prefix, fixture.node, fixture.before, fixture.after, changes, classifyOptions{
				optNames:    map[string]struct{}{tc.writer: {}},
				closureKegs: fixture.closureKegs,
			}); err == nil {
				t.Fatal("uncontrolled loader-cache mutation accepted")
			}
		})
	}
}

func TestGdkPixbufLoaderBasenameValidation(t *testing.T) {
	for _, name := range []string{"libpixbufloader-png.so", "libpixbufloader_svg.so", "libpixbufloader-webp_2.so"} {
		if !isGdkPixbufLoaderBasename(name) {
			t.Errorf("valid loader basename %q rejected", name)
		}
	}
	for _, name := range []string{"libpixbufloader.so", "libpixbufloader-.so", "libpixbufloader_.so", "libpixbufloader.svg.so", "libpixbufloader-../evil.so", "other-svg.so"} {
		if isGdkPixbufLoaderBasename(name) {
			t.Errorf("unsafe loader basename %q accepted", name)
		}
	}
}

func TestGdkPixbufLoaderCacheRejectsUnverifiedContributorCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*gdkPixbufCacheFixture)
	}{
		{name: "missing gdk dependency", mutate: func(fixture *gdkPixbufCacheFixture) { fixture.node.Dependencies = nil }},
		{name: "hardlink inventory entry", mutate: func(fixture *gdkPixbufCacheFixture) { fixture.verified.Inventory[0].Type = bottle.EntryHardlink }},
		{name: "non-loader inventory path", mutate: func(fixture *gdkPixbufCacheFixture) { fixture.verified.Inventory[0].KegPath = "lib/libother.so" }},
		{name: "mismatched verified bottle", mutate: func(fixture *gdkPixbufCacheFixture) { fixture.verified.Name = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestWebP, true, nil)
			tc.mutate(&fixture)
			changes := []Change{{Path: gdkPixbufLoadersCachePath, Kind: "modified"}}
			if err := classify(fixture.prefix, fixture.node, fixture.before, fixture.after, changes, fixture.options()); err == nil {
				t.Fatal("unverified loader contributor was accepted")
			}
		})
	}
}

func TestGdkPixbufLoaderCacheRequiresExactGlobalModuleSet(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufFormula, false, nil)
	base := "libpixbufloader-extra.so"
	targetRel := path.Join("Cellar", gdkPixbufFormula, gdkPixbufTestVersions[gdkPixbufFormula], gdkPixbufLoadersDirectoryPath, base)
	moduleRel := path.Join(gdkPixbufLoadersDirectoryPath, base)
	fixture.after[targetRel] = fileState{Type: "regular", Mode: 0o444}
	fixture.after[moduleRel] = fileState{Type: "symlink", Link: path.Join("../../../../Cellar", gdkPixbufFormula, gdkPixbufTestVersions[gdkPixbufFormula], gdkPixbufLoadersDirectoryPath, base)}
	state := fixture.after[gdkPixbufLoadersCachePath]
	if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "created", fixture.before, state, fixture.after, fixture.options()); err == nil {
		t.Fatal("cache omitting an installed global loader symlink was accepted")
	}
}

func TestClassifyRejectsLoaderAdditionWithoutCacheRefresh(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestLibrsvg, true, nil)
	changes := []Change{{Path: path.Join(gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[gdkPixbufTestLibrsvg]), Kind: "created"}}
	if err := classify(fixture.prefix, fixture.node, fixture.before, fixture.after, changes, fixture.options()); err == nil {
		t.Fatal("loader addition without cache refresh was accepted")
	}
}

func TestClassifyRejectsLoaderCacheTypeReplacement(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestLibrsvg, true, nil)
	fixture.after[gdkPixbufLoadersCachePath] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "/etc/shadow"}
	changes := []Change{
		{Path: path.Join(gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[gdkPixbufTestLibrsvg]), Kind: "created"},
		{Path: gdkPixbufLoadersCachePath, Kind: "modified"},
	}
	if err := classify(fixture.prefix, fixture.node, fixture.before, fixture.after, changes, fixture.options()); err == nil {
		t.Fatal("loader cache type replacement was accepted")
	}
}

func TestGdkPixbufLoaderCacheRejectsRewrittenExistingBlock(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestLibrsvg, true, func(_ string, content string) string {
		return strings.Replace(content, `"gdk-pixbuf" 5 "gdk-pixbuf" "test loader" "LGPL"`, `"gdk-pixbuf" 5 "gdk-pixbuf" "rewritten loader" "LGPL"`, 1)
	})
	state := fixture.after[gdkPixbufLoadersCachePath]
	if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "modified", fixture.before, state, fixture.after, fixture.options()); err == nil {
		t.Fatal("rewritten pre-existing loader block was accepted")
	}
}

func TestGdkPixbufLoaderCacheRejectsPreviousModuleRemoval(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestLibrsvg, true, nil)
	gdkModule := filepath.Join(fixture.prefix, filepath.FromSlash(path.Join(gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[gdkPixbufFormula])))
	if err := os.Remove(gdkModule); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(fixture.prefix, filepath.FromSlash(gdkPixbufLoadersCachePath))
	if err := os.WriteFile(cachePath, []byte(gdkPixbufCacheContent(fixture.prefix, []string{gdkPixbufTestLibrsvg})), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := snapshot(fixture.prefix)
	if err != nil {
		t.Fatal(err)
	}
	cacheState := after[gdkPixbufLoadersCachePath]
	cacheState.UID, cacheState.GID, cacheState.OwnershipKnown = gdkPixbufTestRuntimeUID, gdkPixbufTestRuntimeGID, true
	after[gdkPixbufLoadersCachePath] = cacheState
	fixture.after = after
	if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "modified", fixture.before, cacheState, fixture.after, fixture.options()); err == nil {
		t.Fatal("refresh removed a previously registered loader")
	}
}

func TestGdkPixbufLoaderCacheRequiresNewVerifiedRegistration(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestWebP, true, nil)
	fixture.before = maps.Clone(fixture.after)
	prior := fixture.before[gdkPixbufLoadersCachePath]
	prior.Digest = strings.Repeat("d", sha256.Size*2)
	fixture.before[gdkPixbufLoadersCachePath] = prior
	state := fixture.after[gdkPixbufLoadersCachePath]
	if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "modified", fixture.before, state, fixture.after, fixture.options()); err == nil {
		t.Fatal("modifier registered no new loader")
	}
}

func TestGdkPixbufLoaderCacheRequiresNewLoaderFromVerifiedInventory(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestWebP, true, nil)
	fixture.verified.Inventory[0].KegPath = path.Join(gdkPixbufLoadersDirectoryPath, "libpixbufloader-other.so")
	state := fixture.after[gdkPixbufLoadersCachePath]
	if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "modified", fixture.before, state, fixture.after, fixture.options()); err == nil {
		t.Fatal("new loader absent from verified bottle inventory was accepted")
	}
}

func TestGdkPixbufLoaderCacheRejectsUnsafeMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fileState)
	}{
		{name: "not regular", mutate: func(state *fileState) { state.Type = "symlink"; state.Mode = os.ModeSymlink | 0o777 }},
		{name: "empty", mutate: func(state *fileState) { state.Size = 0 }},
		{name: "oversized", mutate: func(state *fileState) { state.Size = gdkPixbufLoadersCacheMaxBytes + 1 }},
		{name: "executable", mutate: func(state *fileState) { state.Mode |= 0o100 }},
		{name: "group writable", mutate: func(state *fileState) { state.Mode |= 0o020 }},
		{name: "other writable", mutate: func(state *fileState) { state.Mode |= 0o002 }},
		{name: "setuid", mutate: func(state *fileState) { state.Mode |= os.ModeSetuid }},
		{name: "hard linked", mutate: func(state *fileState) { state.Links = 2 }},
		{name: "unknown owner", mutate: func(state *fileState) { state.OwnershipKnown = false }},
		{name: "wrong uid", mutate: func(state *fileState) { state.UID = 0 }},
		{name: "wrong gid", mutate: func(state *fileState) { state.GID = 0 }},
		{name: "missing digest", mutate: func(state *fileState) { state.Digest = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGdkPixbufCacheFixture(t, gdkPixbufFormula, false, nil)
			state := fixture.after[gdkPixbufLoadersCachePath]
			tc.mutate(&state)
			if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "created", fixture.before, state, fixture.after, fixture.options()); err == nil {
				t.Fatal("unsafe loader-cache metadata accepted")
			}
		})
	}
}

func TestLibrsvgLoaderCacheRefreshRejectsUnsafePriorMetadata(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestLibrsvg, true, nil)
	prior := fixture.before[gdkPixbufLoadersCachePath]
	prior.Mode |= 0o020
	fixture.before[gdkPixbufLoadersCachePath] = prior
	state := fixture.after[gdkPixbufLoadersCachePath]
	if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "modified", fixture.before, state, fixture.after, fixture.options()); err == nil {
		t.Fatal("librsvg refreshed an unsafe pre-existing cache")
	}
}

func TestGdkPixbufLoaderCacheRejectsUnsafeContent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transform func(string, string) string
	}{
		{
			name: "materializer path",
			transform: func(_ string, content string) string {
				return strings.Replace(content, "#\n", "#\n# /run/dalec-homebrew/input/resolution.json\n", 1)
			},
		},
		{
			name: "nul byte",
			transform: func(_ string, content string) string {
				return content + "\x00"
			},
		},
		{
			name: "control byte",
			transform: func(_ string, content string) string {
				return strings.Replace(content, "Automatically generated", "Automatically\tgenerated", 1)
			},
		},
		{
			name: "invalid utf8",
			transform: func(_ string, content string) string {
				return content + string([]byte{0xff})
			},
		},
		{
			name: "path traversal",
			transform: func(prefix, content string) string {
				loaderDir := path.Join(filepath.ToSlash(prefix), gdkPixbufLoadersDirectoryPath)
				return strings.Replace(content, loaderDir, loaderDir+"/../loaders", 1)
			},
		},
		{
			name: "outside prefix",
			transform: func(prefix, content string) string {
				module := path.Join(filepath.ToSlash(prefix), gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[gdkPixbufFormula])
				return strings.Replace(content, module, "/tmp/libpixbufloader-png.so", 1)
			},
		},
		{
			name: "direct keg reference",
			transform: func(prefix, content string) string {
				module := path.Join(filepath.ToSlash(prefix), gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[gdkPixbufFormula])
				kegModule := path.Join(filepath.ToSlash(prefix), "Cellar", gdkPixbufFormula, gdkPixbufTestVersions[gdkPixbufFormula], "lib", "gdk-pixbuf-2.0", "2.10.0", "loaders", gdkPixbufTestModules[gdkPixbufFormula])
				return strings.Replace(content, module, kegModule, 1)
			},
		},
		{
			name: "incomplete block",
			transform: func(_ string, content string) string {
				return strings.Replace(content, "\"gdk-pixbuf\" 5 \"gdk-pixbuf\" \"test loader\" \"LGPL\"\n", "", 1)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGdkPixbufCacheFixture(t, gdkPixbufFormula, false, tc.transform)
			state := fixture.after[gdkPixbufLoadersCachePath]
			if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "created", fixture.before, state, fixture.after, fixture.options()); err == nil {
				t.Fatal("unsafe loader-cache content accepted")
			}
		})
	}
}

func TestGdkPixbufLoaderCacheRejectsTargetsOutsideResolvedClosure(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufTestLibrsvg, true, nil)
	delete(fixture.closureKegs, path.Join("Cellar", gdkPixbufTestLibrsvg, gdkPixbufTestVersions[gdkPixbufTestLibrsvg]))
	state := fixture.after[gdkPixbufLoadersCachePath]
	if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "modified", fixture.before, state, fixture.after, fixture.options()); err == nil {
		t.Fatal("loader target outside the resolved closure was accepted")
	}
}

func TestGdkPixbufLoaderCacheRejectsArbitraryFileInsideClosureKeg(t *testing.T) {
	fixture := newGdkPixbufCacheFixture(t, gdkPixbufFormula, false, nil)
	moduleRel := path.Join(gdkPixbufLoadersDirectoryPath, gdkPixbufTestModules[gdkPixbufFormula])
	arbitraryRel := path.Join("Cellar", gdkPixbufFormula, gdkPixbufTestVersions[gdkPixbufFormula], "lib", "libarbitrary.so")
	fixture.after[arbitraryRel] = fileState{Type: "regular", Mode: 0o444}
	moduleState := fixture.after[moduleRel]
	moduleState.Link = path.Join("../../../../Cellar", gdkPixbufFormula, gdkPixbufTestVersions[gdkPixbufFormula], "lib", "libarbitrary.so")
	fixture.after[moduleRel] = moduleState
	state := fixture.after[gdkPixbufLoadersCachePath]
	if err := validateGdkPixbufLoadersCache(fixture.prefix, fixture.node, gdkPixbufLoadersCachePath, "created", fixture.before, state, fixture.after, fixture.options()); err == nil {
		t.Fatal("arbitrary closure-keg file was accepted as a loader module")
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
	record := testRecord("amd64", resolution.Node{Name: "glibc", FullName: "homebrew/core/glibc", PkgVersion: "2"})
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

func TestNormalizeInstalledReceiptAtomicallyRecordsEvidence(t *testing.T) {
	root, closure, expected := materializerReceiptNormalizationFixture()
	epoch := time.Unix(1_800_000_123, 0).UTC()
	input := materializerInstalledReceipt(t, root, expected[:1])

	prefix := t.TempDir()
	receiptPath := writeMaterializerReceipt(t, prefix, root, input, 0o644)
	beforeInfo, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInode, _ := snapshotInodeMeta(beforeInfo)

	evidence, err := normalizeInstalledReceipt(prefix, root, closure, epoch.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if evidence == nil {
		t.Fatal("incomplete generated receipt was not normalized")
	}
	if evidence.Formula != root.Name || evidence.ReceiptPath != "Cellar/cairo/1.18.4/INSTALL_RECEIPT.json" || evidence.Reason != receiptNormalizationReason {
		t.Fatalf("evidence identity = %#v", evidence)
	}
	if evidence.BeforeSHA256 != sha256Digest(input) || evidence.BeforeRuntimeDependencyCount != 1 || evidence.AfterRuntimeDependencyCount != len(expected) {
		t.Fatalf("evidence before/counts = %#v", evidence)
	}

	output, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.AfterSHA256 != sha256Digest(output) || evidence.BeforeSHA256 == evidence.AfterSHA256 {
		t.Fatalf("evidence digests = %#v", evidence)
	}
	if _, err := bottle.VerifyInstalledReceipt(output, root, closure); err != nil {
		t.Fatalf("committed receipt failed strict verification: %v", err)
	}
	afterInfo, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInode, links := snapshotInodeMeta(afterInfo)
	uid, gid, ownershipKnown := snapshotOwnership(afterInfo)
	if beforeInode == afterInode || links != 1 {
		t.Fatalf("receipt was not atomically replaced: before=%q after=%q links=%d", beforeInode, afterInode, links)
	}
	if !ownershipKnown || int(uid) != os.Geteuid() || int(gid) != os.Getegid() {
		t.Fatalf("normalized receipt ownership = %d:%d known=%v", uid, gid, ownershipKnown)
	}
	if afterInfo.Mode().Perm() != 0o644 || afterInfo.ModTime().Unix() != epoch.Unix() {
		t.Fatalf("normalized receipt metadata = mode %o mtime %s", afterInfo.Mode().Perm(), afterInfo.ModTime())
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(receiptPath), receiptNormalizationTempName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normalization temporary file remains: %v", err)
	}

	encoded, err := json.Marshal(Evidence{ReceiptNormalizations: []ReceiptNormalizationEvidence{*evidence}})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{root.Name, evidence.BeforeSHA256, evidence.AfterSHA256, receiptNormalizationReason} {
		if !strings.Contains(string(encoded), value) {
			t.Fatalf("materialization evidence omitted %q: %s", value, encoded)
		}
	}

	secondPrefix := t.TempDir()
	secondPath := writeMaterializerReceipt(t, secondPrefix, root, input, 0o644)
	secondEvidence, err := normalizeInstalledReceipt(secondPrefix, root, closure, epoch.Unix())
	if err != nil {
		t.Fatal(err)
	}
	secondOutput, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if secondEvidence == nil || secondEvidence.AfterSHA256 != evidence.AfterSHA256 || !bytes.Equal(secondOutput, output) {
		t.Fatal("receipt normalization is not deterministic")
	}
}

func TestNormalizeInstalledReceiptLeavesStrictReceiptUntouched(t *testing.T) {
	root, closure, expected := materializerReceiptNormalizationFixture()
	input := materializerInstalledReceipt(t, root, expected)
	prefix := t.TempDir()
	receiptPath := writeMaterializerReceipt(t, prefix, root, input, 0o644)
	before, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInode, _ := snapshotInodeMeta(before)
	beforeMTime := before.ModTime()

	evidence, err := normalizeInstalledReceipt(prefix, root, closure, time.Unix(1_800_000_123, 0).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if evidence != nil {
		t.Fatalf("valid receipt produced normalization evidence: %#v", evidence)
	}
	after, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInode, _ := snapshotInodeMeta(after)
	output, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeInode != afterInode || !after.ModTime().Equal(beforeMTime) || !bytes.Equal(output, input) {
		t.Fatal("already-valid receipt was modified")
	}
}

func TestNormalizeInstalledReceiptRejectsUnsafeFilesAndTampering(t *testing.T) {
	root, closure, expected := materializerReceiptNormalizationFixture()
	validIncomplete := materializerInstalledReceipt(t, root, expected[:1])
	epoch := time.Unix(1_800_000_123, 0).Unix()

	t.Run("extra dependency", func(t *testing.T) {
		prefix := t.TempDir()
		extra := append([]bottle.ReceiptDependency(nil), expected[:1]...)
		extra = append(extra, bottle.ReceiptDependency{FullName: "unrelated", Version: "9", PkgVersion: "9"})
		input := materializerInstalledReceipt(t, root, extra)
		path := writeMaterializerReceipt(t, prefix, root, input, 0o644)
		before, _ := os.ReadFile(path)
		if _, err := normalizeInstalledReceipt(prefix, root, closure, epoch); err == nil {
			t.Fatal("extra dependency was normalized away")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("tampered receipt changed on rejection")
		}
	})

	t.Run("identity tamper", func(t *testing.T) {
		prefix := t.TempDir()
		input := bytes.Replace(validIncomplete, []byte(`"arch": "x86_64"`), []byte(`"arch": "arm64"`), 1)
		path := writeMaterializerReceipt(t, prefix, root, input, 0o644)
		beforeInfo, _ := os.Lstat(path)
		beforeInode, _ := snapshotInodeMeta(beforeInfo)
		if _, err := normalizeInstalledReceipt(prefix, root, closure, epoch); err == nil {
			t.Fatal("tampered receipt identity was normalized")
		}
		afterInfo, _ := os.Lstat(path)
		afterInode, _ := snapshotInodeMeta(afterInfo)
		if beforeInode != afterInode {
			t.Fatal("tampered receipt was replaced")
		}
	})

	t.Run("receipt symlink", func(t *testing.T) {
		prefix := t.TempDir()
		keg := filepath.Join(prefix, "Cellar", root.Name, root.PkgVersion)
		if err := os.MkdirAll(keg, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, validIncomplete, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(keg, installReceiptFilename)); err != nil {
			t.Fatal(err)
		}
		if _, err := normalizeInstalledReceipt(prefix, root, closure, epoch); err == nil {
			t.Fatal("symlink receipt was followed")
		}
		outsideData, _ := os.ReadFile(outside)
		if !bytes.Equal(outsideData, validIncomplete) {
			t.Fatal("symlink target was modified")
		}
	})

	t.Run("symlinked keg directory", func(t *testing.T) {
		prefix := t.TempDir()
		if err := os.MkdirAll(filepath.Join(prefix, "Cellar"), 0o755); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(outside, root.PkgVersion), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, root.PkgVersion, installReceiptFilename), validIncomplete, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(prefix, "Cellar", root.Name)); err != nil {
			t.Fatal(err)
		}
		if _, err := normalizeInstalledReceipt(prefix, root, closure, epoch); err == nil {
			t.Fatal("symlinked keg directory was followed")
		}
	})

	t.Run("hardlinked receipt", func(t *testing.T) {
		prefix := t.TempDir()
		path := writeMaterializerReceipt(t, prefix, root, validIncomplete, 0o644)
		alias := filepath.Join(filepath.Dir(path), "receipt-alias")
		if err := os.Link(path, alias); err != nil {
			t.Fatal(err)
		}
		if _, err := normalizeInstalledReceipt(prefix, root, closure, epoch); err == nil {
			t.Fatal("hardlinked receipt was replaced")
		}
		aliasData, _ := os.ReadFile(alias)
		if !bytes.Equal(aliasData, validIncomplete) {
			t.Fatal("hardlink alias changed")
		}
	})

	t.Run("preexisting temporary", func(t *testing.T) {
		prefix := t.TempDir()
		path := writeMaterializerReceipt(t, prefix, root, validIncomplete, 0o644)
		temporaryPath := filepath.Join(filepath.Dir(path), receiptNormalizationTempName)
		if err := os.WriteFile(temporaryPath, []byte("sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := normalizeInstalledReceipt(prefix, root, closure, epoch); err == nil {
			t.Fatal("preexisting temporary file was overwritten")
		}
		temporaryData, _ := os.ReadFile(temporaryPath)
		targetData, _ := os.ReadFile(path)
		if string(temporaryData) != "sentinel" || !bytes.Equal(targetData, validIncomplete) {
			t.Fatal("files changed after preexisting temporary rejection")
		}
	})

	t.Run("world writable receipt", func(t *testing.T) {
		prefix := t.TempDir()
		writeMaterializerReceipt(t, prefix, root, validIncomplete, 0o666)
		if _, err := normalizeInstalledReceipt(prefix, root, closure, epoch); err == nil {
			t.Fatal("world-writable receipt was normalized")
		}
	})
}

func materializerReceiptNormalizationFixture() (resolution.Node, []resolution.Node, []bottle.ReceiptDependency) {
	root := resolution.Node{
		Name: "cairo", FullName: "homebrew/core/cairo", FormulaVersion: "1.18.4", PkgVersion: "1.18.4",
		Dependencies: []resolution.Requirement{{Name: "pixman", Minimum: "0.46.4", Direct: true}},
		Bottle: resolution.Bottle{
			Tag: "x86_64_linux",
			Tab: resolution.BottleTab{
				Arch: "x86_64", Compiler: "gcc",
				Dependencies: []resolution.RuntimeDependency{
					{FullName: "libpng", Version: "1.6.50", PkgVersion: "1.6.50", DeclaredDirectly: true},
					{FullName: "obsolete-transitive", Version: "1", PkgVersion: "1"},
				},
			},
		},
	}
	pixman := resolution.Node{Name: "pixman", FullName: "homebrew/core/pixman", FormulaVersion: "0.46.4", PkgVersion: "0.46.4", BottleRebuild: 1}
	libpng := resolution.Node{
		Name: "libpng", FullName: "homebrew/core/libpng", FormulaVersion: "1.6.50", PkgVersion: "1.6.50",
		Dependencies: []resolution.Requirement{{Name: "zlib-ng-compat", Minimum: "2.2.4", Direct: true}},
	}
	zlib := resolution.Node{Name: "zlib-ng-compat", FullName: "homebrew/core/zlib-ng-compat", FormulaVersion: "2.3.3", FormulaRevision: 1, PkgVersion: "2.3.3_1"}
	closure := []resolution.Node{root, pixman, libpng, zlib}
	expected := []bottle.ReceiptDependency{
		{FullName: "libpng", Version: "1.6.50", PkgVersion: "1.6.50"},
		{FullName: "pixman", Version: "0.46.4", BottleRebuild: 1, PkgVersion: "0.46.4"},
		{FullName: "zlib-ng-compat", Version: "2.3.3", Revision: 1, PkgVersion: "2.3.3_1"},
	}
	return root, closure, expected
}

func materializerInstalledReceipt(t *testing.T, node resolution.Node, dependencies []bottle.ReceiptDependency) []byte {
	t.Helper()
	receipt := map[string]any{
		"name":                 node.Name,
		"full_name":            node.FullName,
		"pkg_version":          node.PkgVersion,
		"revision":             node.FormulaRevision,
		"bottle_rebuild":       node.BottleRebuild,
		"homebrew_version":     "6.0.0",
		"built_as_bottle":      true,
		"poured_from_bottle":   true,
		"arch":                 node.Bottle.Tab.Arch,
		"compiler":             node.Bottle.Tab.Compiler,
		"runtime_dependencies": dependencies,
		"source": map[string]any{
			"spec": "stable", "tap": "homebrew/core",
			"versions": map[string]any{"stable": node.FormulaVersion, "version_scheme": node.VersionScheme},
		},
		"custom_homebrew_metadata": map[string]any{"retained": true},
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func writeMaterializerReceipt(t *testing.T, prefix string, node resolution.Node, data []byte, mode os.FileMode) string {
	t.Helper()
	keg := filepath.Join(prefix, "Cellar", node.Name, node.PkgVersion)
	if err := os.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(keg, installReceiptFilename)
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReconcileAllowsBoundGlibcPostInstallLocaleData(t *testing.T) {
	node := resolution.Node{Name: "glibc", FullName: "homebrew/core/glibc", PkgVersion: "2.39_1"}
	verified := bottle.Result{
		Name:       "glibc",
		PkgVersion: "2.39_1",
		KegPrefix:  "glibc/2.39_1",
		Inventory: []bottle.InventoryEntry{{
			Path: "glibc/2.39_1/lib/libc.so.6", KegPath: "lib/libc.so.6",
			Type: bottle.EntryRegular, Mode: 0o755, SHA256: "sha256:" + strings.Repeat("a", 64),
		}},
	}
	after := map[string]fileState{
		"Cellar/glibc/2.39_1/lib":                        {Type: "directory", Mode: os.ModeDir | 0o755, UID: 1000, GID: 1000, OwnershipKnown: true},
		"Cellar/glibc/2.39_1/lib/libc.so.6":              {Type: "regular", Mode: 0o755, Digest: strings.Repeat("a", 64)},
		"Cellar/glibc/2.39_1/lib/locale":                 {Type: "directory", Mode: os.ModeDir | 0o755, UID: 1000, GID: 1000, OwnershipKnown: true},
		"Cellar/glibc/2.39_1/lib/locale/C.utf8":          {Type: "directory", Mode: os.ModeDir | 0o755, UID: 1000, GID: 1000, OwnershipKnown: true},
		"Cellar/glibc/2.39_1/lib/locale/C.utf8/LC_CTYPE": {Type: "regular", Mode: 0o644, Size: 1024, Digest: strings.Repeat("b", 64), Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true},
	}
	if err := reconcileInstalledKeg("/home/linuxbrew/.linuxbrew", node, verified, after); err != nil {
		t.Fatal(err)
	}
}

func TestGlibcPostInstallLocaleDataRejectsUnsafeContent(t *testing.T) {
	root := "Cellar/glibc/2.39_1/lib/locale"
	base := map[string]fileState{
		root:                     {Type: "directory", Mode: os.ModeDir | 0o755, UID: 1000, GID: 1000, OwnershipKnown: true},
		root + "/locale-archive": {Type: "regular", Mode: 0o644, Size: 1024, Links: 1, UID: 1000, GID: 1000, OwnershipKnown: true},
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]fileState)
	}{
		{name: "executable", mutate: func(after map[string]fileState) {
			state := after[root+"/locale-archive"]
			state.Mode = 0o755
			after[root+"/locale-archive"] = state
		}},
		{name: "writable", mutate: func(after map[string]fileState) {
			state := after[root+"/locale-archive"]
			state.Mode = 0o664
			after[root+"/locale-archive"] = state
		}},
		{name: "symlink", mutate: func(after map[string]fileState) {
			after[root+"/locale-archive"] = fileState{Type: "symlink", Mode: os.ModeSymlink | 0o777, Link: "/etc/shadow", OwnershipKnown: true}
		}},
		{name: "large", mutate: func(after map[string]fileState) {
			state := after[root+"/locale-archive"]
			state.Size = glibcLocaleMaxFile + 1
			after[root+"/locale-archive"] = state
		}},
		{name: "hardlink", mutate: func(after map[string]fileState) {
			state := after[root+"/locale-archive"]
			state.Links = 2
			after[root+"/locale-archive"] = state
		}},
		{name: "unknown-owner", mutate: func(after map[string]fileState) {
			state := after[root+"/locale-archive"]
			state.OwnershipKnown = false
			after[root+"/locale-archive"] = state
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := maps.Clone(base)
			tc.mutate(after)
			if _, err := allowedPostInstallKegPaths(resolution.Node{Name: "glibc", FullName: "homebrew/core/glibc"}, "Cellar/glibc/2.39_1", after); err == nil {
				t.Fatal("unsafe generated locale content accepted")
			}
		})
	}
}

func TestReconcileGlibcStillRejectsOtherGeneratedKegPaths(t *testing.T) {
	node := resolution.Node{Name: "glibc", FullName: "homebrew/core/glibc", PkgVersion: "2.39_1"}
	verified := bottle.Result{Name: "glibc", PkgVersion: "2.39_1", KegPrefix: "glibc/2.39_1"}
	after := map[string]fileState{
		"Cellar/glibc/2.39_1/lib/locale":   {Type: "directory", Mode: os.ModeDir | 0o755, UID: 1000, GID: 1000, OwnershipKnown: true},
		"Cellar/glibc/2.39_1/bin/injected": {Type: "regular", Mode: 0o755, UID: 1000, GID: 1000, OwnershipKnown: true},
	}
	if err := reconcileInstalledKeg("/home/linuxbrew/.linuxbrew", node, verified, after); err == nil || !strings.Contains(err.Error(), "unattributed path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

const (
	sharedMimeTestUID = uint32(4242)
	sharedMimeTestGID = uint32(4343)
)

type sharedMimeFixture struct {
	prefix   string
	node     resolution.Node
	verified bottle.Result
	before   map[string]fileState
	after    map[string]fileState
	options  classifyOptions
}

func newSharedMimeFixture(t *testing.T) sharedMimeFixture {
	t.Helper()
	prefix := t.TempDir()
	node := resolution.Node{Name: sharedMimeInfoFormula, FullName: "homebrew/core/" + sharedMimeInfoFormula, PkgVersion: "2.5.1"}
	keg := filepath.Join(prefix, "Cellar", node.Name, node.PkgVersion)
	sourceData := []byte("<mime-info/>\n")
	source := filepath.Join(keg, filepath.FromSlash(sharedMimeVerifiedSourcePath))
	writeSharedMimeTestFile(t, source, sourceData, 0o644)

	for _, directory := range []string{"etc", "var", "opt", "share/mime/packages"} {
		if err := os.MkdirAll(filepath.Join(prefix, filepath.FromSlash(directory)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(path.Join("../Cellar", node.Name, node.PkgVersion), filepath.Join(prefix, "opt", node.Name)); err != nil {
		t.Fatal(err)
	}
	globalSource := filepath.Join(prefix, filepath.FromSlash(sharedMimeVerifiedSourcePath))
	linkTarget, err := filepath.Rel(filepath.Dir(globalSource), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTarget, globalSource); err != nil {
		t.Fatal(err)
	}
	for name := range sharedMimeFixedOutputs {
		data := []byte(name + "\n")
		if name == "icons" {
			data = nil
		}
		writeSharedMimeTestFile(t, filepath.Join(prefix, filepath.FromSlash(path.Join(sharedMimeDatabaseRoot, name))), data, 0o644)
	}
	for mimeType := range sharedMimeGeneratedTypes {
		data := []byte("<?xml version=\"1.0\"?><mime-type xmlns=\"http://www.freedesktop.org/standards/shared-mime-info\" type=\"" + mimeType + "/x-test\"/>\n")
		writeSharedMimeTestFile(t, filepath.Join(prefix, filepath.FromSlash(path.Join(sharedMimeDatabaseRoot, mimeType, "x-test.xml"))), data, 0o644)
	}

	after := snapshotSharedMimeFixture(t, prefix)
	verified := bottle.Result{
		Name:       node.Name,
		PkgVersion: node.PkgVersion,
		KegPrefix:  path.Join(node.Name, node.PkgVersion),
		Inventory: []bottle.InventoryEntry{{
			Path:    path.Join(node.Name, node.PkgVersion, sharedMimeVerifiedSourcePath),
			KegPath: sharedMimeVerifiedSourcePath,
			Type:    bottle.EntryRegular,
			Mode:    0o644,
			Size:    int64(len(sourceData)),
			SHA256:  sha256Digest(sourceData),
		}},
	}
	options := classifyOptions{
		optNames:    map[string]struct{}{node.Name: {}},
		closureKegs: map[string]struct{}{path.Join("Cellar", node.Name, node.PkgVersion): {}},
		verified:    verified,
		runtimeUID:  sharedMimeTestUID,
		runtimeGID:  sharedMimeTestGID,
	}
	return sharedMimeFixture{prefix: prefix, node: node, verified: verified, before: map[string]fileState{}, after: after, options: options}
}

func writeSharedMimeTestFile(t *testing.T, filename string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}

func snapshotSharedMimeFixture(t *testing.T, prefix string) map[string]fileState {
	t.Helper()
	after, err := snapshot(prefix)
	if err != nil {
		t.Fatal(err)
	}
	for rel, state := range after {
		if snapshotPathWithin(rel, sharedMimeDatabaseRoot) {
			state.UID = sharedMimeTestUID
			state.GID = sharedMimeTestGID
			state.OwnershipKnown = true
			after[rel] = state
		}
	}
	return after
}

func TestClassifyAllowsVerifiedSharedMimeDatabaseGeneration(t *testing.T) {
	fixture := newSharedMimeFixture(t)
	changes := diff(fixture.before, fixture.after)
	if err := classify(fixture.prefix, fixture.node, fixture.before, fixture.after, changes, fixture.options); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range changes {
		if change.Path == path.Join(sharedMimeDatabaseRoot, "XMLnamespaces") {
			found = true
			if change.Classification != "shared-mime-database" {
				t.Fatalf("classification=%q", change.Classification)
			}
		}
	}
	if !found {
		t.Fatal("generated shared MIME output was absent from install delta")
	}
}

func TestSharedMimeDatabaseRejectsMissingOrUnsafeGeneratedData(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sharedMimeFixture)
	}{
		{name: "missing fixed output", mutate: func(f *sharedMimeFixture) {
			delete(f.after, path.Join(sharedMimeDatabaseRoot, "XMLnamespaces"))
		}},
		{name: "unexpected path", mutate: func(f *sharedMimeFixture) {
			f.after[path.Join(sharedMimeDatabaseRoot, "nested", "bad", "file.xml")] = fileState{Type: "regular", Mode: 0o644, Size: 1, Digest: strings.Repeat("a", 64), Links: 1, UID: sharedMimeTestUID, GID: sharedMimeTestGID, OwnershipKnown: true}
		}},
		{name: "writable output", mutate: func(f *sharedMimeFixture) {
			p := path.Join(sharedMimeDatabaseRoot, "aliases")
			state := f.after[p]
			state.Mode = 0o666
			f.after[p] = state
		}},
		{name: "wrong owner", mutate: func(f *sharedMimeFixture) {
			p := path.Join(sharedMimeDatabaseRoot, "aliases")
			state := f.after[p]
			state.UID++
			f.after[p] = state
		}},
		{name: "hardlink", mutate: func(f *sharedMimeFixture) {
			p := path.Join(sharedMimeDatabaseRoot, "aliases")
			state := f.after[p]
			state.Links = 2
			f.after[p] = state
		}},
		{name: "pre-existing database", mutate: func(f *sharedMimeFixture) {
			f.before[sharedMimeDatabaseRoot] = f.after[sharedMimeDatabaseRoot]
		}},
		{name: "mismatched verified bottle", mutate: func(f *sharedMimeFixture) {
			f.options.verified.Name = "other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSharedMimeFixture(t)
			tc.mutate(&fixture)
			if _, err := validateSharedMimeInfoDatabase(fixture.prefix, fixture.node, fixture.before, fixture.after, fixture.options); err == nil {
				t.Fatal("unsafe shared MIME database accepted")
			}
		})
	}
}

func TestSharedMimeDatabaseRejectsMalformedGeneratedXML(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "malformed", data: "<mime-type>"},
		{name: "wrong root", data: `<other xmlns="http://www.freedesktop.org/standards/shared-mime-info" type="application/x-test"/>`},
		{name: "wrong namespace", data: `<mime-type xmlns="https://example.invalid" type="application/x-test"/>`},
		{name: "wrong type", data: `<mime-type xmlns="http://www.freedesktop.org/standards/shared-mime-info" type="text/other"/>`},
		{name: "unicode folded type", data: `<mime-type xmlns="http://www.freedesktop.org/standards/shared-mime-info" type="application/x-teſt"/>`},
		{name: "leading character data", data: `junk<mime-type xmlns="http://www.freedesktop.org/standards/shared-mime-info" type="application/x-test"/>`},
		{name: "trailing character data", data: `<mime-type xmlns="http://www.freedesktop.org/standards/shared-mime-info" type="application/x-test"/>junk`},
		{name: "non XML whitespace", data: "\u00a0<mime-type xmlns=\"http://www.freedesktop.org/standards/shared-mime-info\" type=\"application/x-test\"/>"},
		{name: "duplicate type", data: `<mime-type xmlns="http://www.freedesktop.org/standards/shared-mime-info" type="" type="application/x-test"/>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSharedMimeFixture(t)
			filename := filepath.Join(fixture.prefix, filepath.FromSlash(path.Join(sharedMimeDatabaseRoot, "application", "x-test.xml")))
			writeSharedMimeTestFile(t, filename, []byte(tc.data), 0o644)
			fixture.after = snapshotSharedMimeFixture(t, fixture.prefix)
			if _, err := validateSharedMimeInfoDatabase(fixture.prefix, fixture.node, fixture.before, fixture.after, fixture.options); err == nil {
				t.Fatal("invalid generated shared MIME XML accepted")
			}
		})
	}
}

func TestClassifyRejectsUnverifiedGeneratedSharedMimeFile(t *testing.T) {
	node := resolution.Node{Name: "hello", PkgVersion: "1"}
	after := map[string]fileState{
		".":                              {Type: "directory"},
		"Cellar":                         {Type: "directory"},
		"Cellar/hello":                   {Type: "directory"},
		"Cellar/hello/1":                 {Type: "directory"},
		"etc":                            {Type: "directory"},
		"var":                            {Type: "directory"},
		"opt":                            {Type: "directory"},
		"opt/hello":                      {Type: "symlink", Link: "../Cellar/hello/1"},
		"share":                          {Type: "directory"},
		sharedMimeDatabaseRoot:           {Type: "directory"},
		sharedMimeDatabaseRoot + "/evil": {Type: "regular", Mode: 0o644, Size: 1, Digest: strings.Repeat("a", 64), Links: 1},
	}
	if err := classify("/prefix", node, nil, after, diff(nil, after)); err == nil {
		t.Fatal("unverified generated shared MIME file accepted")
	}
}

func TestClassifyRejectsLaterSharedMimePackageCopyWithoutRefresh(t *testing.T) {
	node := resolution.Node{Name: "mime-extension", PkgVersion: "1"}
	rel := "share/mime/packages/extension.xml"
	digest := strings.Repeat("a", 64)
	keg := path.Join("Cellar", node.Name, node.PkgVersion)
	after := map[string]fileState{
		".":                         {Type: "directory"},
		"Cellar":                    {Type: "directory"},
		"Cellar/" + node.Name:       {Type: "directory"},
		keg:                         {Type: "directory"},
		path.Join(keg, rel):         {Type: "regular", Mode: 0o644, Size: 4, Digest: digest},
		"etc":                       {Type: "directory"},
		"var":                       {Type: "directory"},
		"opt":                       {Type: "directory"},
		path.Join("opt", node.Name): {Type: "symlink", Link: path.Join("../Cellar", node.Name, node.PkgVersion)},
		"share":                     {Type: "directory"},
		sharedMimeDatabaseRoot:      {Type: "directory"},
		rel:                         {Type: "regular", Mode: 0o644, Size: 4, Digest: digest},
	}
	verified := bottle.Result{
		Name:       node.Name,
		PkgVersion: node.PkgVersion,
		KegPrefix:  path.Join(node.Name, node.PkgVersion),
		Inventory:  []bottle.InventoryEntry{{KegPath: rel, Type: bottle.EntryRegular, Mode: 0o644, Size: 4, SHA256: "sha256:" + digest}},
	}
	options := classifyOptions{optNames: map[string]struct{}{node.Name: {}}, verified: verified}
	if err := classify("/prefix", node, nil, after, diff(nil, after), options); err == nil {
		t.Fatal("later shared MIME package copy without database refresh was accepted")
	}
}

func TestNonCoreGlibcRackDoesNotReceiveCorePostInstallCapability(t *testing.T) {
	node := resolution.Node{Name: "glibc", FullName: "acme/tools/glibc", PkgVersion: "2"}
	allowed, err := allowedPostInstallKegPaths(node, "Cellar/glibc/2", map[string]fileState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 0 {
		t.Fatalf("non-core glibc received post-install paths: %v", allowed)
	}
	if isBrewedLoaderMutation("/prefix", node, "lib/ld.so", map[string]fileState{"lib/ld.so": {Type: "symlink", Link: "/prefix/opt/glibc/bin/ld.so"}}) {
		t.Fatal("non-core glibc received loader mutation capability")
	}
}

func TestNonCorePythonRackDoesNotReceiveVenvTemplateException(t *testing.T) {
	node := resolution.Node{Name: "python@3.14", FullName: "acme/tools/python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"}
	verified := bottle.Result{Name: node.Name, PkgVersion: node.PkgVersion, KegPrefix: node.Name + "/" + node.PkgVersion, Formula: bottle.FormulaEvidence{Path: node.Name + "/" + node.PkgVersion + "/.brew/" + node.Name + ".rb", ClassName: "PythonAT314", SHA256: "sha256:" + strings.Repeat("a", 64), Size: 1}}
	if isPythonVenvTemplate(node, verified, "lib/python3.14/venv/scripts/common/activate") {
		t.Fatal("non-core Python received venv template exception")
	}
}
