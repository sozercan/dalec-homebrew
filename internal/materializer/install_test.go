package materializer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
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
	node := resolution.Node{Name: "python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"}
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
			node:    resolution.Node{Name: "python@3.13", FormulaVersion: "3.13.9", PkgVersion: "3.13.9"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "malformed formula name",
			node:    resolution.Node{Name: "python@3.14-extra", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "mismatched formula version",
			node:    resolution.Node{Name: "python@3.14", FormulaVersion: "3.13.9", PkgVersion: "3.13.9"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "outside scripts subtree",
			node:    resolution.Node{Name: "python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "scripts prefix spoof",
			node:    resolution.Node{Name: "python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts-evil/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
		},
		{
			name:    "missing verified formula evidence",
			node:    resolution.Node{Name: "python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
			omitFormula: true,
		},
		{
			name:    "content mutation",
			node:    resolution.Node{Name: "python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o644,
			expectedDigest: strings.Repeat("a", 64), actualDigest: strings.Repeat("b", 64),
		},
		{
			name:    "group writable",
			node:    resolution.Node{Name: "python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
			kegPath: "lib/python3.14/venv/scripts/common/Activate.ps1", expectedMode: 0o444, actualMode: 0o664,
		},
		{
			name:    "made executable",
			node:    resolution.Node{Name: "python@3.14", FormulaVersion: "3.14.6", PkgVersion: "3.14.6"},
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
	node := resolution.Node{Name: "glibc", PkgVersion: "2.39_1"}
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
			if _, err := allowedPostInstallKegPaths(resolution.Node{Name: "glibc"}, "Cellar/glibc/2.39_1", after); err == nil {
				t.Fatal("unsafe generated locale content accepted")
			}
		})
	}
}

func TestReconcileGlibcStillRejectsOtherGeneratedKegPaths(t *testing.T) {
	node := resolution.Node{Name: "glibc", PkgVersion: "2.39_1"}
	verified := bottle.Result{Name: "glibc", PkgVersion: "2.39_1", KegPrefix: "glibc/2.39_1"}
	after := map[string]fileState{
		"Cellar/glibc/2.39_1/lib/locale":   {Type: "directory", Mode: os.ModeDir | 0o755, UID: 1000, GID: 1000, OwnershipKnown: true},
		"Cellar/glibc/2.39_1/bin/injected": {Type: "regular", Mode: 0o755, UID: 1000, GID: 1000, OwnershipKnown: true},
	}
	if err := reconcileInstalledKeg("/home/linuxbrew/.linuxbrew", node, verified, after); err == nil || !strings.Contains(err.Error(), "unattributed path") {
		t.Fatalf("unexpected error: %v", err)
	}
}
