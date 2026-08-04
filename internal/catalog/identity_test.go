package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

func TestParseFormulaIDCanonicalizesCore(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		input string
		want  FormulaID
	}{
		{input: "hello", want: "homebrew/core/hello"},
		{input: "homebrew/core/hello", want: "homebrew/core/hello"},
		{input: "acme/tools/widget", want: "acme/tools/widget"},
		{input: "acme/dev_tools/python@3.14", want: "acme/dev_tools/python@3.14"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := ParseFormulaID(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("ParseFormulaID(%q) = %q, want %q", test.input, got, test.want)
			}
			if got.Tap() == "" || got.Name() == "" {
				t.Fatalf("missing components for %q", got)
			}
		})
	}
}

func TestParseFormulaIDRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"",
		".",
		"..",
		"owner/tap",
		"owner/tap/formula/extra",
		"Owner/tap/formula",
		"owner/Tap/formula",
		"owner/tap/Formula",
		"owner/tap/../formula",
		"https://github.com/acme/homebrew-tools",
		"owner:tap:formula",
		`owner\tap\formula`,
		"owner/tap/formula\x00",
		"owner/tap/fórmula",
		"-owner/tap/formula",
		"owner/-tap/formula",
		"owner/tap/-formula",
		strings.Repeat("a", maxGitHubOwnerBytes+1) + "/tap/formula",
		"owner/" + strings.Repeat("a", maxTapNameBytes+1) + "/formula",
		"owner/tap/" + strings.Repeat("a", maxFormulaNameBytes+1),
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseFormulaID(input); err == nil {
				t.Fatalf("accepted malformed Formula ID %q", input)
			}
		})
	}
}

func TestParseFormulaIDsRejectsCanonicalDuplicates(t *testing.T) {
	t.Parallel()
	_, err := ParseFormulaIDs([]string{"hello", "homebrew/core/hello"})
	if err == nil || !strings.Contains(err.Error(), "duplicate canonical") {
		t.Fatalf("err = %v", err)
	}
}

func TestFormulaIDJSONRequiresCanonicalQualification(t *testing.T) {
	t.Parallel()
	var id FormulaID
	if err := json.Unmarshal([]byte(`"hello"`), &id); err == nil {
		t.Fatal("bare Formula ID accepted in protocol JSON")
	}
	if err := json.Unmarshal([]byte(`"homebrew/core/hello"`), &id); err != nil {
		t.Fatal(err)
	}
	if id != "homebrew/core/hello" {
		t.Fatalf("id = %q", id)
	}
}

func TestTapIDDefaultRepository(t *testing.T) {
	t.Parallel()
	id, err := ParseTapID("acme/dev-tools")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := id.DefaultGitHubRepository(), "https://github.com/acme/homebrew-dev-tools"; got != want {
		t.Fatalf("repository = %q, want %q", got, want)
	}
	for _, bad := range []string{"acme", "acme/dev/tools", "acme/Dev", "acme/..", "acme/dev:tools"} {
		if _, err := ParseTapID(bad); err == nil {
			t.Errorf("accepted tap ID %q", bad)
		}
	}
}

func TestSharedFormulaIDConversions(t *testing.T) {
	t.Parallel()
	shared, err := formulaid.Parse("acme/tools/widget")
	if err != nil {
		t.Fatal(err)
	}
	wire := FormulaIDFromShared(shared)
	if wire != "acme/tools/widget" {
		t.Fatalf("wire ID = %q", wire)
	}
	roundTrip, err := wire.Shared()
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip != shared {
		t.Fatalf("round trip = %#v, want %#v", roundTrip, shared)
	}
	wireTap := TapIDFromShared(shared.Tap())
	roundTripTap, err := wireTap.Shared()
	if err != nil {
		t.Fatal(err)
	}
	if roundTripTap != shared.Tap() {
		t.Fatalf("tap round trip = %#v, want %#v", roundTripTap, shared.Tap())
	}
}
