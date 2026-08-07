package bottle

import (
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestExpectationFromNodeV2UsesFullIdentityAndRackDependencies(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	dep := resolution.NodeV2{ID: "other/lib/shared", Tap: "other/lib", Name: "shared", HomebrewFullName: "other/lib/shared", PkgVersion: "1"}
	node := resolution.NodeV2{ID: "acme/tools/widget", Tap: "acme/tools", Name: "widget", HomebrewFullName: "acme/tools/widget", FormulaVersion: "1", PkgVersion: "1", Dependencies: []resolution.RequirementV2{{ID: dep.ID, Direct: true}}, Bottle: resolution.BottleV2{Tag: "x86_64_linux", Filename: "widget.tgz", Size: 1, SHA256: d, Transport: resolution.BottleTransport{HTTPS: &resolution.HTTPSTransport{URL: "https://bottles.example/widget.tgz", ExpectedSize: 1, SHA256: d, Filename: "widget.tgz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: resolution.HTTPSFetchPolicyVersionV1}}}}
	expectation, err := ExpectationFromNodeV2(node, []resolution.NodeV2{dep, node})
	if err != nil {
		t.Fatal(err)
	}
	if expectation.FullName != "acme/tools/widget" || expectation.ExpectedTap != "acme/tools" || len(expectation.AllowedExternalSymlinkFormulae) != 1 || expectation.AllowedExternalSymlinkFormulae[0] != "shared" {
		t.Fatalf("expectation=%+v", expectation)
	}
}

func TestExpectationFromNodeV2RejectsRackCollision(t *testing.T) {
	a := resolution.NodeV2{ID: "homebrew/core/shared", Tap: "homebrew/core", Name: "shared", HomebrewFullName: "homebrew/core/shared", PkgVersion: "1"}
	b := resolution.NodeV2{ID: "acme/tools/shared", Tap: "acme/tools", Name: "shared", HomebrewFullName: "acme/tools/shared", PkgVersion: "1"}
	if _, err := ExpectationFromNodeV2(a, []resolution.NodeV2{a, b}); err == nil {
		t.Fatal("rack collision accepted")
	}
}

func TestResolvedInstalledDependenciesAcceptsCanonicalNonCoreIdentity(t *testing.T) {
	node := resolution.Node{Name: "widget", FullName: "acme/tools/widget", FormulaVersion: "1", PkgVersion: "1"}
	dependencies, err := resolvedInstalledDependencies(node, []resolution.Node{node})
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) != 0 {
		t.Fatalf("dependencies=%v", dependencies)
	}
}
