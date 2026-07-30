package metadata

import (
	"encoding/json"
	"testing"
)

func TestLinuxUsesFromMacOSAndRecommendedDependencies(t *testing.T) {
	raw := rawFormula{Name: "tool", FullName: "tool", Tap: "homebrew/core", Versions: rawVersions{Stable: "1", Bottle: false}, Dependencies: []string{"direct"}, RecommendedDependencies: []string{"recommended"}, UsesFromMacOS: []json.RawMessage{json.RawMessage(`"python"`), json.RawMessage(`{"bison":"build"}`), json.RawMessage(`{"cups":"no_linkage"}`)}}
	f, err := normalizeFormula(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cups", "direct", "python", "recommended"}
	if len(f.Dependencies) != len(want) {
		t.Fatalf("deps=%v", f.Dependencies)
	}
	for i := range want {
		if f.Dependencies[i] != want[i] {
			t.Fatalf("deps=%v", f.Dependencies)
		}
	}
}

func TestLinuxDependencyAliasesCanonicalized(t *testing.T) {
	python := Formula{Name: "python@3.14", FullName: "homebrew/core/python@3.14", Tap: "homebrew/core", StableVersion: "3.14", Aliases: []string{"python"}}
	tool := Formula{Name: "tool", FullName: "homebrew/core/tool", Tap: "homebrew/core", StableVersion: "1", Dependencies: []string{"python"}}
	cat, err := buildCatalog([]Formula{python, tool}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved := cat.canonical["tool"]
	if got := resolved.Dependencies; len(got) != 1 || got[0] != "python@3.14" {
		t.Fatalf("deps=%v", got)
	}
}

func TestNumericUserSpellingRemainsAnAlias(t *testing.T) {
	formula := Formula{Name: "sevenzip", FullName: "homebrew/core/sevenzip", Tap: "homebrew/core", StableVersion: "1", Aliases: []string{"7zip"}}
	cat, err := buildCatalog([]Formula{formula}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cat.aliases["7zip"]; got != "sevenzip" {
		t.Fatalf("alias=%q", got)
	}
}
