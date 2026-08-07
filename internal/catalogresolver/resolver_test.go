package catalogresolver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

type fakeCore map[string]metadata.Formula

func (f fakeCore) Lookup(name string) (metadata.Match, error) {
	formula, ok := f[name]
	if !ok {
		return metadata.Match{}, &metadata.LookupError{Name: name, Err: metadata.ErrFormulaNotFound}
	}
	return metadata.Match{Requested: name, Canonical: formula.Name, Kind: metadata.MatchCanonical, Formula: formula}, nil
}

func coreFormula(name string, deps ...string) metadata.Formula {
	return metadata.Formula{Name: name, FullName: "homebrew/core/" + name, Tap: "homebrew/core", StableVersion: "1", License: "MIT", Dependencies: deps, Bottle: &metadata.Bottle{Files: []metadata.BottleFile{{Tag: "x86_64_linux", URL: "https://ghcr.io/v2/homebrew/core/" + name, SHA256: strings.Repeat("a", 64), Cellar: ":any"}}}}
}

func tapCatalog(t *testing.T, tapName string, formulae []catalog.Formula, aliases []catalog.ScopedMapping, migrations []catalog.Migration) *catalog.TapCatalog {
	t.Helper()
	tap, err := catalog.ParseTapID(tapName)
	if err != nil {
		t.Fatal(err)
	}
	d := "sha256:" + strings.Repeat("a", 64)
	return &catalog.TapCatalog{SchemaVersion: catalog.TapCatalogSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("b", 40), TreeDigest: d, ArchiveDigest: d}, PublishedAt: time.Unix(1_800_000_000, 0).UTC(), Sequence: 1, Formulae: formulae, Aliases: aliases, Migrations: migrations}
}

func externalFormula(t *testing.T, idValue string, deps ...catalog.Dependency) catalog.Formula {
	t.Helper()
	id, err := catalog.ParseFormulaID(idValue)
	if err != nil {
		t.Fatal(err)
	}
	d := "sha256:" + strings.Repeat("c", 64)
	return catalog.Formula{ID: id, Name: id.Name(), HomebrewFullName: string(id), SourcePath: "Formula/" + id.Name() + ".rb", SourceDigest: d, StableVersion: "1", License: "MIT", Dependencies: deps, Bottle: &catalog.BottleDeclaration{RootURL: "https://bottles.example", Files: []catalog.BottleFile{{Tag: "x86_64_linux", URL: "https://bottles.example/" + id.Name() + ".tgz", SHA256: "sha256:" + strings.Repeat("d", 64), Cellar: ":any"}}}}
}

func prebuiltA365Formula(t *testing.T) catalog.Formula {
	t.Helper()
	policy, err := policyv2.LoadTapPolicy()
	if err != nil {
		t.Fatal(err)
	}
	authorization, ok := policy.PrebuiltArchiveForFormula("sozercan/repo/a365")
	if !ok {
		t.Fatal("missing a365 prebuilt archive policy")
	}
	id, err := catalog.ParseFormulaID(authorization.FormulaID)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]catalog.PrebuiltArchiveFile, 0, len(authorization.Platforms))
	for _, platform := range authorization.Platforms {
		tag := "x86_64_linux"
		if platform.Platform == "linux/arm64" {
			tag = "arm64_linux"
		}
		files = append(files, catalog.PrebuiltArchiveFile{Tag: tag, URL: platform.URL, SHA256: platform.SHA256, Format: authorization.Archive.Format})
	}
	return catalog.Formula{
		ID:               id,
		Name:             id.Name(),
		HomebrewFullName: string(id),
		SourcePath:       "Formula/a365.rb",
		SourceDigest:     authorization.FormulaSourceDigest,
		StableVersion:    authorization.Version,
		License:          "MIT",
		PrebuiltArchive:  &catalog.PrebuiltArchiveDeclaration{Files: files},
	}
}

func dep(t *testing.T, raw, normalized string) catalog.Dependency {
	t.Helper()
	id, err := catalog.ParseFormulaID(normalized)
	if err != nil {
		t.Fatal(err)
	}
	return catalog.Dependency{Raw: raw, ID: id}
}

func TestBareDependencyPrefersCoreThenOwningTap(t *testing.T) {
	widget := externalFormula(t, "acme/tools/widget", dep(t, "shared", "homebrew/core/shared"), dep(t, "local", "acme/tools/local"))
	local := externalFormula(t, "acme/tools/local")
	shadow := externalFormula(t, "acme/tools/shared")
	document := tapCatalog(t, "acme/tools", []catalog.Formula{widget, local, shadow}, nil, nil)
	tap, _ := catalog.ParseTapID("acme/tools")
	resolver, err := New(fakeCore{"shared": coreFormula("shared")}, map[catalog.TapID]*catalog.TapCatalog{tap: document})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := catalog.ParseFormulaID("acme/tools/widget")
	closure, err := resolver.Resolve([]catalog.FormulaID{root}, catalog.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(closure.Nodes) != 3 {
		t.Fatalf("nodes=%+v", closure.Nodes)
	}
	widgetNode := nodeByID(t, closure, root)
	if got := []catalog.FormulaID{widgetNode.Dependencies[0].ID, widgetNode.Dependencies[1].ID}; !(containsID(got, "homebrew/core/shared") && containsID(got, "acme/tools/local")) || containsID(got, "acme/tools/shared") {
		t.Fatalf("dependencies=%+v", widgetNode.Dependencies)
	}
}

func TestQualifiedCrossTapDependencyAndMigration(t *testing.T) {
	widget := externalFormula(t, "acme/tools/widget", dep(t, "other/lib/libfoo", "other/lib/libfoo"))
	foo := externalFormula(t, "other/lib/libfoo")
	old, _ := catalog.ParseFormulaID("acme/tools/old-widget")
	newID, _ := catalog.ParseFormulaID("acme/tools/widget")
	acmeTap, _ := catalog.ParseTapID("acme/tools")
	otherTap, _ := catalog.ParseTapID("other/lib")
	resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{
		acmeTap:  tapCatalog(t, "acme/tools", []catalog.Formula{widget}, nil, []catalog.Migration{{From: old, RawTarget: "acme/tools/widget", To: newID}}),
		otherTap: tapCatalog(t, "other/lib", []catalog.Formula{foo}, nil, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	closure, err := resolver.Resolve([]catalog.FormulaID{old}, catalog.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(closure.Nodes) != 2 || closure.Requested[0] != newID {
		t.Fatalf("closure=%+v", closure)
	}
	if len(closure.RequestedMappings) != 1 || closure.RequestedMappings[0].Requested != old || closure.RequestedMappings[0].Resolved != newID {
		t.Fatalf("requested mappings=%+v", closure.RequestedMappings)
	}
}

func TestDoesNotSearchUnrelatedTap(t *testing.T) {
	widget := externalFormula(t, "acme/tools/widget", dep(t, "missing", "acme/tools/missing"))
	unrelated := externalFormula(t, "other/lib/missing")
	acmeTap, _ := catalog.ParseTapID("acme/tools")
	otherTap, _ := catalog.ParseTapID("other/lib")
	resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{acmeTap: tapCatalog(t, "acme/tools", []catalog.Formula{widget}, nil, nil), otherTap: tapCatalog(t, "other/lib", []catalog.Formula{unrelated}, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := catalog.ParseFormulaID("acme/tools/widget")
	if _, err := resolver.Resolve([]catalog.FormulaID{root}, catalog.Platform{OS: "linux", Architecture: "amd64"}); err == nil || !strings.Contains(err.Error(), "acme/tools/missing") {
		t.Fatalf("err=%v", err)
	}
}

func TestRejectsDependencyCycleAndRackCollision(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		a := externalFormula(t, "acme/tools/a", dep(t, "b", "acme/tools/b"))
		b := externalFormula(t, "acme/tools/b", dep(t, "a", "acme/tools/a"))
		tap, _ := catalog.ParseTapID("acme/tools")
		resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, "acme/tools", []catalog.Formula{a, b}, nil, nil)})
		if err != nil {
			t.Fatal(err)
		}
		root, _ := catalog.ParseFormulaID("acme/tools/a")
		if _, err := resolver.Resolve([]catalog.FormulaID{root}, catalog.Platform{OS: "linux", Architecture: "amd64"}); err == nil || !strings.Contains(err.Error(), "dependency cycle") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("rack", func(t *testing.T) {
		a := externalFormula(t, "acme/tools/shared", dep(t, "other/lib/shared", "other/lib/shared"))
		b := externalFormula(t, "other/lib/shared")
		acmeTap, _ := catalog.ParseTapID("acme/tools")
		otherTap, _ := catalog.ParseTapID("other/lib")
		resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{acmeTap: tapCatalog(t, "acme/tools", []catalog.Formula{a}, nil, nil), otherTap: tapCatalog(t, "other/lib", []catalog.Formula{b}, nil, nil)})
		if err != nil {
			t.Fatal(err)
		}
		root, _ := catalog.ParseFormulaID("acme/tools/shared")
		if _, err := resolver.Resolve([]catalog.FormulaID{root}, catalog.Platform{OS: "linux", Architecture: "amd64"}); err == nil || !strings.Contains(err.Error(), "share Cellar rack") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestMissingCoreFallsBackOnlyOnNotFound(t *testing.T) {
	badCore := errorCore{err: errors.New("signature state invalid")}
	local := externalFormula(t, "acme/tools/local")
	widget := externalFormula(t, "acme/tools/widget", dep(t, "local", "acme/tools/local"))
	tap, _ := catalog.ParseTapID("acme/tools")
	resolver, err := New(badCore, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, "acme/tools", []catalog.Formula{local, widget}, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := catalog.ParseFormulaID("acme/tools/widget")
	if _, err := resolver.Resolve([]catalog.FormulaID{root}, catalog.Platform{OS: "linux", Architecture: "amd64"}); err == nil || !strings.Contains(err.Error(), "signature state invalid") {
		t.Fatalf("err=%v", err)
	}
}

type errorCore struct{ err error }

func (e errorCore) Lookup(string) (metadata.Match, error) { return metadata.Match{}, e.err }

func nodeByID(t *testing.T, closure catalog.ClosureResult, id catalog.FormulaID) catalog.Node {
	t.Helper()
	for _, node := range closure.Nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("missing node %s", id)
	return catalog.Node{}
}

func containsID(values []catalog.FormulaID, want string) bool {
	for _, id := range values {
		if string(id) == want {
			return true
		}
	}
	return false
}

func TestRejectsCatalogDependencyNormalizationMismatch(t *testing.T) {
	wrong, _ := catalog.ParseFormulaID("acme/tools/shared")
	widget := externalFormula(t, "acme/tools/widget", catalog.Dependency{Raw: "shared", ID: wrong})
	shadow := externalFormula(t, "acme/tools/shared")
	tap, _ := catalog.ParseTapID("acme/tools")
	resolver, err := New(fakeCore{"shared": coreFormula("shared")}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, "acme/tools", []catalog.Formula{widget, shadow}, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := catalog.ParseFormulaID("acme/tools/widget")
	if _, err := resolver.Resolve([]catalog.FormulaID{root}, catalog.Platform{OS: "linux", Architecture: "amd64"}); err == nil || !strings.Contains(err.Error(), "signed catalog claimed") {
		t.Fatalf("err=%v", err)
	}
}

func TestMigrationToCoreRetainsNormalizationTap(t *testing.T) {
	dummy := externalFormula(t, "acme/tools/dummy")
	old, _ := catalog.ParseFormulaID("acme/tools/old")
	coreID, _ := catalog.ParseFormulaID("shared")
	tap, _ := catalog.ParseTapID("acme/tools")
	resolver, err := New(fakeCore{"shared": coreFormula("shared")}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, "acme/tools", []catalog.Formula{dummy}, nil, []catalog.Migration{{From: old, RawTarget: string(coreID), To: coreID}})})
	if err != nil {
		t.Fatal(err)
	}
	closure, err := resolver.Resolve([]catalog.FormulaID{old}, catalog.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(closure.RequestedMappings) != 1 || closure.RequestedMappings[0].Resolved != coreID || len(closure.NormalizationTaps) != 1 || closure.NormalizationTaps[0] != tap {
		t.Fatalf("closure=%+v", closure)
	}
}

func TestPrebuiltA365RequestedRoot(t *testing.T) {
	formula := prebuiltA365Formula(t)
	tap := formula.ID.Tap()
	resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, string(tap), []catalog.Formula{formula}, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range []catalog.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}} {
		t.Run(platform.Architecture, func(t *testing.T) {
			closure, err := resolver.Resolve([]catalog.FormulaID{formula.ID}, platform)
			if err != nil {
				t.Fatal(err)
			}
			if len(closure.Nodes) != 1 || closure.Nodes[0].ID != formula.ID || len(closure.Nodes[0].Dependencies) != 0 || closure.Nodes[0].BottleRebuild != 0 {
				t.Fatalf("closure=%+v", closure)
			}
		})
	}
}

func TestPrebuiltA365RejectsPolicyMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*catalog.Formula)
		want   string
	}{
		{name: "version", mutate: func(formula *catalog.Formula) { formula.StableVersion = "0.3.4" }, want: "stable version"},
		{name: "source digest", mutate: func(formula *catalog.Formula) { formula.SourceDigest = "sha256:" + strings.Repeat("e", 64) }, want: "source digest"},
		{name: "license", mutate: func(formula *catalog.Formula) { formula.License = "Apache-2.0" }, want: "license"},
		{name: "dependency", mutate: func(formula *catalog.Formula) {
			formula.Dependencies = []catalog.Dependency{dep(t, "hello", "homebrew/core/hello")}
		}, want: "requires none"},
		{name: "url", mutate: func(formula *catalog.Formula) {
			formula.PrebuiltArchive.Files[0].URL = "https://github.com/sozercan/a365cli/releases/download/v0.3.3/substitution.tar.gz"
		}, want: "URL"},
		{name: "digest", mutate: func(formula *catalog.Formula) {
			formula.PrebuiltArchive.Files[0].SHA256 = "sha256:" + strings.Repeat("f", 64)
		}, want: "digest"},
		{name: "native bottle exists", mutate: func(formula *catalog.Formula) {
			formula.PrebuiltArchive.Files = formula.PrebuiltArchive.Files[:1]
			formula.Bottle = &catalog.BottleDeclaration{RootURL: "https://bottles.example", Files: []catalog.BottleFile{{Tag: "arm64_linux", URL: "https://bottles.example/a365.arm64.tar.gz", SHA256: "sha256:" + strings.Repeat("d", 64), Cellar: ":any"}}}
		}, want: "requires no bottle"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			formula := prebuiltA365Formula(t)
			test.mutate(&formula)
			tap := formula.ID.Tap()
			resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, string(tap), []catalog.Formula{formula}, nil, nil)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolver.Resolve([]catalog.FormulaID{formula.ID}, catalog.Platform{OS: "linux", Architecture: "amd64"})
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "stable bottle") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestPrebuiltArchiveRejectsSpoofFormulaID(t *testing.T) {
	formula := prebuiltA365Formula(t)
	formula.ID, _ = catalog.ParseFormulaID("acme/tools/a365")
	formula.HomebrewFullName = string(formula.ID)
	tap := formula.ID.Tap()
	resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, string(tap), []catalog.Formula{formula}, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve([]catalog.FormulaID{formula.ID}, catalog.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "no exact release-policy authorization") {
		t.Fatalf("err=%v", err)
	}
}

func TestNativeBottleTakesPrecedenceOverPrebuiltPolicy(t *testing.T) {
	formula := prebuiltA365Formula(t)
	formula.StableVersion = "9.9.9"
	formula.SourceDigest = "sha256:" + strings.Repeat("e", 64)
	formula.License = "Apache-2.0"
	formula.PrebuiltArchive.Files = formula.PrebuiltArchive.Files[1:]
	formula.Bottle = &catalog.BottleDeclaration{RootURL: "https://bottles.example", Rebuild: 7, Files: []catalog.BottleFile{{Tag: "x86_64_linux", URL: "https://bottles.example/a365.x86_64.tar.gz", SHA256: "sha256:" + strings.Repeat("d", 64), Cellar: ":any"}}}
	tap := formula.ID.Tap()
	resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, string(tap), []catalog.Formula{formula}, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	closure, err := resolver.Resolve([]catalog.FormulaID{formula.ID}, catalog.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if node := nodeByID(t, closure, formula.ID); node.BottleRebuild != 7 || node.FormulaVersion != "9.9.9" {
		t.Fatalf("node=%+v", node)
	}
}

func TestPrebuiltArchiveRejectsWrongArchitecture(t *testing.T) {
	formula := prebuiltA365Formula(t)
	formula.PrebuiltArchive.Files = formula.PrebuiltArchive.Files[1:]
	tap := formula.ID.Tap()
	resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, string(tap), []catalog.Formula{formula}, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve([]catalog.FormulaID{formula.ID}, catalog.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "does not declare target tag x86_64_linux") {
		t.Fatalf("err=%v", err)
	}
}

func TestPrebuiltArchiveRejectsTransitiveUse(t *testing.T) {
	a365 := prebuiltA365Formula(t)
	parent := externalFormula(t, "sozercan/repo/parent", dep(t, "a365", "sozercan/repo/a365"))
	tap := a365.ID.Tap()
	resolver, err := New(fakeCore{}, map[catalog.TapID]*catalog.TapCatalog{tap: tapCatalog(t, string(tap), []catalog.Formula{a365, parent}, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve([]catalog.FormulaID{parent.ID}, catalog.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "explicitly requested resolved root") {
		t.Fatalf("err=%v", err)
	}
}
