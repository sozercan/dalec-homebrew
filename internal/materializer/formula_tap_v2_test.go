package materializer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestStageFormulaeV2AcceptsVerifiedKegRelativeFormulaPath(t *testing.T) {
	record := materializerRuntimePolicyRecordV2(t)
	node := record.Nodes[0]
	source := []byte("class Hello < Formula\nend\n")
	sum := sha256.Sum256(source)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	verified := map[resolution.FormulaID]bottle.Result{
		node.ID: {
			Name:          node.Name,
			PkgVersion:    node.PkgVersion,
			KegPrefix:     node.Name + "/" + node.PkgVersion,
			Formula:       bottle.FormulaEvidence{Path: node.Name + "/" + node.PkgVersion + "/.brew/" + node.Name + ".rb", SHA256: digest, Size: int64(len(source))},
			FormulaSource: source,
		},
	}
	prefix := t.TempDir()
	makeStagedTreeRemovable(t, prefix)
	relative, err := FormulaTapPathV2(node)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, filepath.Dir(filepath.FromSlash(relative))), 0o755); err != nil {
		t.Fatal(err)
	}
	evidence, err := StageFormulaeV2(prefix, record, verified)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].ID != node.ID || evidence[0].SHA256 != digest {
		t.Fatalf("evidence=%+v", evidence)
	}
	staged, err := os.ReadFile(filepath.Join(prefix, filepath.FromSlash(evidence[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != string(source) {
		t.Fatalf("staged source=%q want=%q", staged, source)
	}
}

func TestStageFormulaeV2ModesIgnoreRestrictiveUmask(t *testing.T) {
	record := materializerRuntimePolicyRecordV2(t)
	node := record.Nodes[0]
	source := []byte("class Hello < Formula\nend\n")
	sum := sha256.Sum256(source)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	verified := map[resolution.FormulaID]bottle.Result{
		node.ID: {
			Name:          node.Name,
			PkgVersion:    node.PkgVersion,
			KegPrefix:     node.Name + "/" + node.PkgVersion,
			Formula:       bottle.FormulaEvidence{Path: node.Name + "/" + node.PkgVersion + "/.brew/" + node.Name + ".rb", SHA256: digest, Size: int64(len(source))},
			FormulaSource: source,
		},
	}
	prefix := t.TempDir()
	makeStagedTreeRemovable(t, prefix)
	oldUmask := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(oldUmask) })
	evidence, err := StageFormulaeV2(prefix, record, verified)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 {
		t.Fatalf("staged Formula evidence=%+v", evidence)
	}
	stagedPath := filepath.Join(prefix, filepath.FromSlash(evidence[0].Path))
	info, err := os.Lstat(stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o444 {
		t.Fatalf("staged Formula mode=%#o, want 0444", got)
	}
	current := prefix
	for _, component := range strings.Split(filepath.Dir(filepath.FromSlash(evidence[0].Path)), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o555 {
			t.Fatalf("staged tap directory %s mode=%#o, want 0555", current, got)
		}
	}
}

func makeStagedTreeRemovable(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			} else {
				_ = os.Chmod(path, 0o644)
			}
			return nil
		})
	})
}

func TestFormulaTapPathV2(t *testing.T) {
	for _, test := range []struct {
		node resolution.NodeV2
		want string
	}{
		{node: resolution.NodeV2{ID: "homebrew/core/hello", Tap: "homebrew/core", Name: "hello"}, want: "Homebrew/Library/Taps/homebrew/homebrew-core/Formula/h/hello.rb"},
		{node: resolution.NodeV2{ID: "acme/tools/widget", Tap: "acme/tools", Name: "widget"}, want: "Homebrew/Library/Taps/acme/homebrew-tools/Formula/widget.rb"},
	} {
		got, err := FormulaTapPathV2(test.node)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("path=%q want=%q", got, test.want)
		}
	}
}

func TestV2TapTrustFileUsesExactNonCoreIDs(t *testing.T) {
	record := &resolution.RecordV2{Nodes: []resolution.NodeV2{{ID: "other/lib/tool", Tap: "other/lib", Name: "tool", HomebrewFullName: "other/lib/tool"}, {ID: "homebrew/core/hello", Tap: "homebrew/core", Name: "hello", HomebrewFullName: "homebrew/core/hello"}, {ID: "acme/tools/widget", Tap: "acme/tools", Name: "widget", HomebrewFullName: "acme/tools/widget"}}}
	data, err := V2TapTrustFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"trustedformulae":["acme/tools/widget","other/lib/tool"]}`+"\n"; got != want {
		t.Fatalf("trust file=%q want=%q", got, want)
	}
	if strings.Contains(string(data), "homebrew/core") {
		t.Fatal("core identity leaked into non-core trust file")
	}
}

func TestFormulaTapPathV2RejectsIdentityMismatch(t *testing.T) {
	if _, err := FormulaTapPathV2(resolution.NodeV2{ID: "acme/tools/widget", Tap: "other/tools", Name: "widget"}); err == nil {
		t.Fatal("tap mismatch accepted")
	}
}
