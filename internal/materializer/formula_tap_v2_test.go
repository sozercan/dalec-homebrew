package materializer

import (
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

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
