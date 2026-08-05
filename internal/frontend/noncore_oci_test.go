package frontend

import (
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestCatalogBottleTabCanonicalizesDependencyOrder(t *testing.T) {
	tab := resolution.BottleTab{Dependencies: []resolution.RuntimeDependency{
		{FullName: "zeta/tools/z", Version: "1", PkgVersion: "1"},
		{FullName: "alpha/tools/a", Version: "1", PkgVersion: "1"},
	}}
	converted, err := catalogBottleTab(tab)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Dependencies) != 2 || converted.Dependencies[0].ID != "alpha/tools/a" || converted.Dependencies[1].ID != "zeta/tools/z" {
		t.Fatalf("dependency order = %+v", converted.Dependencies)
	}
}
