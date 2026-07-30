package oci

import "testing"

func TestResolveFormulaReferenceEscapesNameAndVersions(t *testing.T) {
	t.Parallel()

	formula := Formula{
		Name:          "tool@1+preview",
		FullName:      "homebrew/core/tool@1+preview",
		StableVersion: "2.4.1",
		Revision:      3,
		BottleRebuild: 2,
	}
	reference, err := ResolveFormulaReference(formula)
	if err != nil {
		t.Fatal(err)
	}
	if reference.Repository != "homebrew/core/tool/1xpreview" {
		t.Fatalf("repository = %q", reference.Repository)
	}
	if reference.CanonicalRepository != "ghcr.io/homebrew/core/tool/1xpreview" {
		t.Fatalf("canonical repository = %q", reference.CanonicalRepository)
	}
	if reference.PkgVersion != "2.4.1_3" {
		t.Fatalf("pkg version = %q", reference.PkgVersion)
	}
	if reference.IndexTag != "2.4.1_3-2" {
		t.Fatalf("index tag = %q", reference.IndexTag)
	}
	child, err := ChildTag(reference.PkgVersion, BottleTagX8664Linux, formula.BottleRebuild)
	if err != nil {
		t.Fatal(err)
	}
	if child != "2.4.1_3.x86_64_linux.2" {
		t.Fatalf("child tag = %q", child)
	}
	filename, err := BottleFilename(formula.Name, reference.PkgVersion, BottleTagX8664Linux, formula.BottleRebuild)
	if err != nil {
		t.Fatal(err)
	}
	if filename != "tool@1+preview--2.4.1_3.x86_64_linux.bottle.2.tar.gz" {
		t.Fatalf("filename = %q", filename)
	}
}

func TestResolveFormulaReferenceRejectsTapSyntax(t *testing.T) {
	t.Parallel()

	_, err := ResolveFormulaReference(Formula{Name: "user/tap/formula", StableVersion: "1.0"})
	if err == nil {
		t.Fatal("expected tap syntax to be rejected")
	}
}
