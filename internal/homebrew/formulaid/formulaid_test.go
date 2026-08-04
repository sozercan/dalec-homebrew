package formulaid

import (
	"errors"
	"strings"
	"testing"
)

func TestParseCanonicalizesCoreFormula(t *testing.T) {
	t.Parallel()

	bare, err := Parse("hello")
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := Parse("homebrew/core/hello")
	if err != nil {
		t.Fatal(err)
	}
	if bare != explicit {
		t.Fatalf("bare = %#v, explicit = %#v", bare, explicit)
	}
	if got := bare.String(); got != "homebrew/core/hello" {
		t.Fatalf("String() = %q", got)
	}
	if got := bare.Name(); got != "hello" {
		t.Fatalf("Name() = %q", got)
	}
	if got := bare.Tap(); got != CoreTap() {
		t.Fatalf("Tap() = %#v", got)
	}
	if got := bare.Tap().Owner(); got != "homebrew" {
		t.Fatalf("Tap().Owner() = %q", got)
	}
	if got := bare.Tap().Name(); got != "core" {
		t.Fatalf("Tap().Name() = %q", got)
	}
	if got := bare.Tap().String(); got != "homebrew/core" {
		t.Fatalf("Tap().String() = %q", got)
	}
}

func TestParseQualifiedFormula(t *testing.T) {
	t.Parallel()

	const input = "acme-labs/tools_extra/widget@2+tls"
	id, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := id.String(); got != input {
		t.Fatalf("String() = %q", got)
	}
	if got := id.Name(); got != "widget@2+tls" {
		t.Fatalf("Name() = %q", got)
	}
	if got := id.Tap().String(); got != "acme-labs/tools_extra" {
		t.Fatalf("Tap().String() = %q", got)
	}

	tap, err := ParseTap("acme-labs/tools_extra")
	if err != nil {
		t.Fatal(err)
	}
	constructed, err := New(tap, "widget@2+tls")
	if err != nil {
		t.Fatal(err)
	}
	if constructed != id {
		t.Fatalf("New() = %#v, Parse() = %#v", constructed, id)
	}
}

func TestComponentLengthBoundaries(t *testing.T) {
	t.Parallel()

	owner := strings.Repeat("a", maxOwnerBytes)
	tap := strings.Repeat("b", maxTapBytes)
	formula := strings.Repeat("c", maxFormulaBytes)
	if _, err := Parse(owner + "/" + tap + "/" + formula); err != nil {
		t.Fatalf("maximum-length components rejected: %v", err)
	}

	for _, input := range []string{
		strings.Repeat("a", maxOwnerBytes+1) + "/tap/formula",
		"owner/" + strings.Repeat("b", maxTapBytes+1) + "/formula",
		"owner/tap/" + strings.Repeat("c", maxFormulaBytes+1),
	} {
		input := input
		t.Run(input[:min(len(input), 40)], func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(input); !errors.Is(err, ErrInvalidFormulaID) {
				t.Fatalf("error = %v, want ErrInvalidFormulaID", err)
			}
		})
	}
}

func TestOverlongErrorsDoNotEchoUnboundedInput(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("a", 1<<20)
	tests := []struct {
		name string
		call func() error
	}{
		{name: "Formula parse", call: func() error { _, err := Parse(huge); return err }},
		{name: "tap parse", call: func() error { _, err := ParseTap(huge); return err }},
		{name: "tap constructor", call: func() error { _, err := NewTap(huge, "tap"); return err }},
		{name: "Formula constructor", call: func() error { _, err := New(CoreTap(), huge); return err }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.call()
			if err == nil {
				t.Fatal("expected error")
			}
			if len(err.Error()) > 256 {
				t.Fatalf("error echoed unbounded input: %d bytes", len(err.Error()))
			}
		})
	}
}

func TestParseRejectsMalformedFormulaIDs(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":                     "",
		"empty owner":               "/tools/widget",
		"empty tap":                 "acme//widget",
		"empty formula":             "acme/tools/",
		"dot bare":                  ".",
		"dotdot bare":               "..",
		"dot owner":                 "./tools/widget",
		"dotdot owner":              "../tools/widget",
		"dot tap":                   "acme/./widget",
		"dotdot tap":                "acme/../widget",
		"dot formula":               "acme/tools/.",
		"dotdot formula":            "acme/tools/..",
		"two components":            "acme/tools",
		"four components":           "acme/tools/widget/extra",
		"absolute path":             "/acme/tools/widget",
		"URL":                       "https://github.com/acme/homebrew-tools",
		"URL-like host":             "github.com/acme/widget",
		"git URL-like owner":        "git@github.com/acme/widget",
		"colon":                     "acme/tools:beta/widget",
		"backslash":                 `acme/tools/widget\\evil`,
		"uppercase owner":           "Acme/tools/widget",
		"uppercase tap":             "acme/Tools/widget",
		"uppercase formula":         "acme/tools/Widget",
		"owner control":             "acme\ncorp/tools/widget",
		"tap control":               "acme/tool\x00s/widget",
		"formula control":           "acme/tools/wid\tget",
		"Unicode owner":             "acmé/tools/widget",
		"Unicode tap":               "acme/töols/widget",
		"Unicode formula":           "acme/tools/widgét",
		"leading whitespace":        " hello",
		"trailing whitespace":       "hello ",
		"qualified whitespace":      "acme/tools/widget name",
		"leading owner hyphen":      "-acme/tools/widget",
		"trailing owner hyphen":     "acme-/tools/widget",
		"consecutive owner hyphens": "acme--labs/tools/widget",
		"leading tap separator":     "acme/-tools/widget",
		"leading Formula separator": "acme/tools/@widget",
		"multiple version markers":  "acme/tools/widget@@2",
		"trailing version marker":   "acme/tools/widget@",
	}
	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			id, err := Parse(input)
			if !errors.Is(err, ErrInvalidFormulaID) {
				t.Fatalf("Parse(%q) = %#v, %v; want ErrInvalidFormulaID", input, id, err)
			}
			if id != (FormulaID{}) {
				t.Fatalf("Parse(%q) returned non-zero ID %#v", input, id)
			}
		})
	}
}

func TestParseTap(t *testing.T) {
	t.Parallel()

	tap, err := ParseTap("acme-labs/tools_extra")
	if err != nil {
		t.Fatal(err)
	}
	if tap.Owner() != "acme-labs" || tap.Name() != "tools_extra" || tap.String() != "acme-labs/tools_extra" {
		t.Fatalf("tap = %#v", tap)
	}
	constructed, err := NewTap("acme-labs", "tools_extra")
	if err != nil {
		t.Fatal(err)
	}
	if constructed != tap {
		t.Fatalf("NewTap() = %#v, ParseTap() = %#v", constructed, tap)
	}

	for _, input := range []string{
		"",
		"acme",
		"acme/tools/extra",
		"acme/",
		"/tools",
		"github.com/tools",
		"Acme/tools",
		"acme/Tools",
		"acme/tool\\name",
	} {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseTap(input); !errors.Is(err, ErrInvalidTap) {
				t.Fatalf("ParseTap(%q) error = %v, want ErrInvalidTap", input, err)
			}
		})
	}
}

func TestParseRootsPreservesOrderAndRejectsCanonicalDuplicates(t *testing.T) {
	t.Parallel()

	roots, err := ParseRoots([]string{"acme/tools/widget", "hello", "other/tap/tool"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(roots))
	for i, id := range roots {
		got[i] = id.String()
	}
	want := []string{"acme/tools/widget", "homebrew/core/hello", "other/tap/tool"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("roots = %v, want %v", got, want)
	}

	for name, input := range map[string][]string{
		"implicit and explicit core": {"hello", "homebrew/core/hello"},
		"same external root":         {"acme/tools/widget", "acme/tools/widget"},
	} {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ids, err := ParseRoots(input)
			if !errors.Is(err, ErrDuplicateRoot) {
				t.Fatalf("ParseRoots() = %v, %v; want ErrDuplicateRoot", ids, err)
			}
			if ids != nil {
				t.Fatalf("ParseRoots() returned partial IDs %v", ids)
			}
		})
	}
}

func TestZeroValuesAndInvalidConstruction(t *testing.T) {
	t.Parallel()

	var tap Tap
	if tap.Owner() != "" || tap.Name() != "" || tap.String() != "" {
		t.Fatalf("zero Tap = %#v", tap)
	}
	var id FormulaID
	if id.Tap() != (Tap{}) || id.Name() != "" || id.String() != "" {
		t.Fatalf("zero FormulaID = %#v", id)
	}
	if _, err := New(tap, "hello"); !errors.Is(err, ErrInvalidFormulaID) {
		t.Fatalf("New(zero tap) error = %v, want ErrInvalidFormulaID", err)
	}
}

func TestParseRootsNil(t *testing.T) {
	t.Parallel()

	roots, err := ParseRoots(nil)
	if err != nil {
		t.Fatal(err)
	}
	if roots != nil {
		t.Fatalf("ParseRoots(nil) = %v", roots)
	}
}
