package materializer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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
