package metadata

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseCatalogAndLookupSemantics(t *testing.T) {
	catalog, err := ParseCatalog(validFormulaPayload(t), validMigrationPayload(t))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if catalog.Len() != 4 {
		t.Fatalf("Len = %d, want 4", catalog.Len())
	}
	if got, want := catalog.CanonicalNames(), []string{"python@3.14", "source-only", "tool", "tool@1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical names = %v, want %v", got, want)
	}
	if got := catalog.Aliases()["tool-current"]; got != "tool" {
		t.Fatalf("tool-current alias = %q", got)
	}
	if got := catalog.OldNames()["oldtool"]; got != "tool" {
		t.Fatalf("oldtool = %q", got)
	}

	formula, ok := catalog.Formula("tool")
	if !ok {
		t.Fatal("tool missing")
	}
	if formula.FullName != "homebrew/core/tool" || formula.StableVersion != "2.0" || formula.PkgVersion() != "2.0_1" || formula.VersionScheme != 2 {
		t.Fatalf("unexpected Formula: %#v", formula)
	}
	if got, want := formula.Dependencies, []string{"tool@1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependencies = %v, want %v", got, want)
	}
	if got, want := formula.DependenciesFor("x86_64_linux"), []string{"python@3.14", "tool@1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("x86 dependencies = %v, want %v", got, want)
	}
	if got, want := formula.DependenciesFor("arm64_linux"), []string{"tool@1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("arm dependencies = %v, want %v", got, want)
	}
	if !formula.KegOnlyFor("x86_64_linux") || formula.KegOnlyFor("arm64_linux") {
		t.Fatalf("unexpected keg_only variation behavior")
	}
	file, err := formula.BottleFor("x86_64_linux")
	if err != nil || file.Tag != "all" {
		t.Fatalf("BottleFor fallback = %#v, %v", file, err)
	}

	checks := []struct {
		name      string
		canonical string
		kind      MatchKind
	}{
		{name: "tool", canonical: "tool", kind: MatchCanonical},
		{name: "tool-current", canonical: "tool", kind: MatchAlias},
		{name: "oldtool", canonical: "tool", kind: MatchOldName},
		{name: "legacy", canonical: "tool", kind: MatchMigration},
		{name: "legacy2", canonical: "tool", kind: MatchMigration},
		{name: "tool@1", canonical: "tool@1", kind: MatchCanonical},
		// An unversioned alias may identify the current canonical Formula even
		// when Homebrew's canonical name itself is versioned.
		{name: "python", canonical: "python@3.14", kind: MatchAlias},
	}
	for _, check := range checks {
		t.Run("lookup "+check.name, func(t *testing.T) {
			match, err := catalog.Lookup(check.name)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if match.Canonical != check.canonical || match.Kind != check.kind || match.Formula.Name != check.canonical {
				t.Fatalf("match = %#v", match)
			}
		})
	}

	for _, name := range []string{"tool@2", "python@3"} {
		t.Run("version-looking alias rejected "+name, func(t *testing.T) {
			_, err := catalog.Lookup(name)
			if !errors.Is(err, ErrVersionedFormulaNotExplicit) {
				t.Fatalf("error = %v, want ErrVersionedFormulaNotExplicit", err)
			}
		})
	}
	for _, name := range []string{"caskish", "caskchain"} {
		if _, err := catalog.Lookup(name); !errors.Is(err, ErrOutOfCore) || !strings.Contains(err.Error(), "homebrew/cask") {
			t.Fatalf("external migration %q error = %v, want ErrOutOfCore with final target", name, err)
		}
	}
	if _, err := catalog.Lookup("source-only"); !errors.Is(err, ErrBottleUnavailable) {
		t.Fatalf("source-only error = %v, want ErrBottleUnavailable", err)
	}
	if _, err := catalog.Lookup("missing"); !errors.Is(err, ErrFormulaNotFound) {
		t.Fatalf("missing error = %v, want ErrFormulaNotFound", err)
	}
	if _, err := catalog.Lookup("homebrew/core/tool"); !errors.Is(err, ErrInvalidFormulaName) {
		t.Fatalf("qualified name error = %v, want ErrInvalidFormulaName", err)
	}
}

func TestCatalogIsImmutableToCallers(t *testing.T) {
	catalog, err := ParseCatalog(validFormulaPayload(t), validMigrationPayload(t))
	if err != nil {
		t.Fatal(err)
	}

	names := catalog.CanonicalNames()
	names[0] = "mutated"
	if catalog.CanonicalNames()[0] == "mutated" {
		t.Fatal("canonical name slice aliases catalog storage")
	}
	aliases := catalog.Aliases()
	aliases["tool-current"] = "mutated"
	if catalog.Aliases()["tool-current"] != "tool" {
		t.Fatal("alias map aliases catalog storage")
	}
	formula, _ := catalog.Formula("tool")
	formula.Aliases[0] = "mutated"
	formula.Dependencies[0] = "mutated"
	formula.Variations[0].Dependencies[0] = "mutated"
	formula.Bottle.Files[0].SHA256 = strings.Repeat("f", 64)
	fresh, _ := catalog.Formula("tool")
	if slices.Contains(fresh.Aliases, "mutated") || slices.Contains(fresh.Dependencies, "mutated") || slices.Contains(fresh.Variations[0].Dependencies, "mutated") || fresh.Bottle.Files[0].SHA256 == strings.Repeat("f", 64) {
		t.Fatalf("Formula copy mutated catalog: %#v", fresh)
	}

	match, err := catalog.Lookup("tool")
	if err != nil {
		t.Fatal(err)
	}
	match.Formula.OldNames[0] = "mutated"
	matchAgain, _ := catalog.Lookup("tool")
	if matchAgain.Formula.OldNames[0] == "mutated" {
		t.Fatal("Lookup result aliases catalog storage")
	}
}

func TestParseCatalogRejectsDuplicateAndInvalidIdentities(t *testing.T) {
	tests := []struct {
		name       string
		formulae   func(t *testing.T) []byte
		migrations func(t *testing.T) []byte
		contains   string
	}{
		{
			name: "duplicate canonical",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae = append(formulae, formulae[0])
				return mustJSON(t, formulae)
			},
			contains: "duplicate canonical",
		},
		{
			name: "alias collision",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[1]["aliases"] = []string{"tool-current"}
				return mustJSON(t, formulae)
			},
			contains: "duplicate identity",
		},
		{
			name: "alias collides with canonical",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[1]["aliases"] = []string{"tool"}
				return mustJSON(t, formulae)
			},
			contains: "duplicate identity",
		},
		{
			name: "migration collides with alias",
			migrations: func(t *testing.T) []byte {
				return mustJSON(t, map[string]string{"tool-current": "homebrew/core/tool"})
			},
			contains: "duplicate identity",
		},
		{
			name: "out of core tap",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[0]["tap"] = "someone/tap"
				return mustJSON(t, formulae)
			},
			contains: "outside homebrew/core",
		},
		{
			name: "out of core full name",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[0]["full_name"] = "someone/tap/tool"
				return mustJSON(t, formulae)
			},
			contains: "outside homebrew/core",
		},
		{
			name: "invalid formula name",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[0]["name"] = "bad/name"
				formulae[0]["full_name"] = "bad/name"
				return mustJSON(t, formulae)
			},
			contains: "invalid Formula name",
		},
		{
			name: "missing stable version",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[0]["versions"] = map[string]any{"stable": "", "bottle": true}
				return mustJSON(t, formulae)
			},
			contains: "stable version is empty",
		},
		{
			name: "claimed bottle missing metadata",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[0]["bottle"] = map[string]any{}
				return mustJSON(t, formulae)
			},
			contains: "bottle.stable is missing",
		},
		{
			name: "missing versioned Formula",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[0]["versioned_formulae"] = []string{"tool@404"}
				return mustJSON(t, formulae)
			},
			contains: "references missing versioned Formula",
		},
		{
			name: "self versioned reference",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[1]["versioned_formulae"] = []string{"tool@1"}
				return mustJSON(t, formulae)
			},
			contains: "references itself",
		},
		{
			name: "duplicate alias within formula",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[0]["aliases"] = []string{"same", "same"}
				return mustJSON(t, formulae)
			},
			contains: "duplicate alias",
		},
		{
			name: "missing dependency",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[0]["dependencies"] = []string{"not-in-core-snapshot"}
				return mustJSON(t, formulae)
			},
			contains: "references missing dependency",
		},
		{
			name: "dependency cycle",
			formulae: func(t *testing.T) []byte {
				formulae := formulaMaps(t)
				formulae[1]["dependencies"] = []string{"tool"}
				return mustJSON(t, formulae)
			},
			contains: "dependency cycle in base metadata: tool -> tool@1 -> tool",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			formulae := validFormulaPayload(t)
			if tc.formulae != nil {
				formulae = tc.formulae(t)
			}
			migrations := validMigrationPayload(t)
			if tc.migrations != nil {
				migrations = tc.migrations(t)
			}
			_, err := ParseCatalog(formulae, migrations)
			if !errors.Is(err, ErrInvalidCatalog) || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v, want ErrInvalidCatalog containing %q", err, tc.contains)
			}
		})
	}
}

func TestParseCatalogRejectsMigrationCyclesAndDuplicateJSONKeys(t *testing.T) {
	formulae := validFormulaPayload(t)
	for _, tc := range []struct {
		name       string
		migrations []byte
		contains   string
	}{
		{name: "cycle", migrations: []byte(`{"a":"b","b":"c","c":"a"}`), contains: "migration cycle: a -> b -> c -> a"},
		{name: "unknown core target", migrations: []byte(`{"a":"homebrew/core/missing"}`), contains: "targets unknown homebrew/core Formula"},
		{name: "duplicate JSON key", migrations: []byte(`{"a":"homebrew/core/tool","a":"homebrew/core/tool@1"}`), contains: "duplicate JSON member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCatalog(formulae, tc.migrations)
			if !errors.Is(err, ErrInvalidCatalog) || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v, want containing %q", err, tc.contains)
			}
		})
	}

	// Duplicate keys inside a Formula object must also be rejected before the
	// last-one-wins behavior of encoding/json can hide them.
	duplicateFormula := []byte(`[{
		"name":"tool","name":"other","full_name":"tool","tap":"homebrew/core",
		"versions":{"stable":"1","bottle":false},"revision":0,"version_scheme":0,
		"oldnames":[],"aliases":[],"versioned_formulae":[],"dependencies":[],"variations":{},"bottle":{},"keg_only":false
	}]`)
	if _, err := ParseCatalog(duplicateFormula, []byte(`{}`)); !errors.Is(err, ErrInvalidCatalog) || !strings.Contains(err.Error(), "duplicate JSON member") {
		t.Fatalf("duplicate Formula error = %v", err)
	}
}

func TestCatalogWrapperPayloads(t *testing.T) {
	formulaWrapper := mustJSON(t, map[string]any{
		"generated_date": "2026-07-28",
		"formulae":       json.RawMessage(validFormulaPayload(t)),
	})
	migrationWrapper := mustJSON(t, map[string]any{
		"generated_date": "2026-07-28",
		"migrations":     json.RawMessage(validMigrationPayload(t)),
	})
	catalog, err := ParseCatalog(formulaWrapper, migrationWrapper)
	if err != nil {
		t.Fatalf("ParseCatalog wrappers: %v", err)
	}
	if catalog.Len() != 4 {
		t.Fatalf("Len = %d", catalog.Len())
	}
}

func formulaMaps(t *testing.T) []map[string]any {
	t.Helper()
	var formulae []map[string]any
	if err := json.Unmarshal(validFormulaPayload(t), &formulae); err != nil {
		t.Fatalf("decode Formula fixture: %v", err)
	}
	return formulae
}

func TestSameNameCoreMigrationIsRedundant(t *testing.T) {
	formula := Formula{Name: "tool", FullName: "homebrew/core/tool", Tap: "homebrew/core", StableVersion: "1"}
	cat, err := buildCatalog([]Formula{formula}, []Migration{{Name: "tool", Target: "homebrew/core", TargetName: "tool", InCore: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.canonical["tool"]; !ok {
		t.Fatal("canonical formula removed")
	}
	if _, ok := cat.migrations["tool"]; ok {
		t.Fatal("redundant migration retained")
	}
}
