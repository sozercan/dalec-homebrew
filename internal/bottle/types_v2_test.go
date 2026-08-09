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

func TestExpectationFromNodeV2ProjectsExactCertifiSharedCARule(t *testing.T) {
	caCertificates := resolution.NodeV2{ID: "homebrew/core/ca-certificates", Tap: "homebrew/core", Name: "ca-certificates", HomebrewFullName: "homebrew/core/ca-certificates", PkgVersion: "2026"}
	certifi := resolution.NodeV2{
		ID: "homebrew/core/certifi", Tap: "homebrew/core", Name: "certifi", HomebrewFullName: "homebrew/core/certifi", PkgVersion: "2026.7.22",
		Dependencies: []resolution.RequirementV2{{ID: caCertificates.ID, Direct: true}},
	}
	expectation, err := ExpectationFromNodeV2(certifi, []resolution.NodeV2{caCertificates, certifi})
	if err != nil {
		t.Fatal(err)
	}
	if len(expectation.AllowedExternalSymlinkRules) != 1 || expectation.AllowedExternalSymlinkRules[0] != ExternalSymlinkRuleCertifiSharedCA {
		t.Fatalf("certifi rules = %v", expectation.AllowedExternalSymlinkRules)
	}

	for name, mutate := range map[string]func(*resolution.NodeV2, *resolution.NodeV2){
		"non-core owner": func(owner, _ *resolution.NodeV2) {
			owner.ID, owner.Tap, owner.HomebrewFullName = "acme/tools/certifi", "acme/tools", "acme/tools/certifi"
		},
		"spoofed Homebrew owner": func(owner, _ *resolution.NodeV2) {
			owner.HomebrewFullName = "acme/tools/certifi"
		},
		"indirect dependency": func(owner, _ *resolution.NodeV2) {
			owner.Dependencies[0].Direct = false
		},
		"non-core dependency": func(owner, dependency *resolution.NodeV2) {
			dependency.ID, dependency.Tap, dependency.HomebrewFullName = "acme/tools/ca-certificates", "acme/tools", "acme/tools/ca-certificates"
			owner.Dependencies[0].ID = dependency.ID
		},
	} {
		t.Run(name, func(t *testing.T) {
			owner, dependency := certifi, caCertificates
			owner.Dependencies = append([]resolution.RequirementV2(nil), certifi.Dependencies...)
			mutate(&owner, &dependency)
			got, err := ExpectationFromNodeV2(owner, []resolution.NodeV2{dependency, owner})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.AllowedExternalSymlinkRules) != 0 {
				t.Fatalf("unexpected certifi rules = %v", got.AllowedExternalSymlinkRules)
			}
		})
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
