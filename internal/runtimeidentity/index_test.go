package runtimeidentity

import (
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestIndexSeparatesFormulaAndRackIdentity(t *testing.T) {
	nodes := []resolution.NodeV2{{ID: "homebrew/core/hello", Tap: "homebrew/core", Name: "hello", HomebrewFullName: "homebrew/core/hello"}, {ID: "acme/tools/widget", Tap: "acme/tools", Name: "widget", HomebrewFullName: "acme/tools/widget"}}
	index, err := New(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := index.FormulaIDForRack("widget"); !ok || id != "acme/tools/widget" {
		t.Fatalf("rack lookup=%q ok=%v", id, ok)
	}
	if node, ok := index.Node("homebrew/core/hello"); !ok || node.Name != "hello" {
		t.Fatalf("node=%+v ok=%v", node, ok)
	}
}

func TestIndexRejectsRackCollision(t *testing.T) {
	_, err := New([]resolution.NodeV2{{ID: "homebrew/core/shared", Tap: "homebrew/core", Name: "shared", HomebrewFullName: "homebrew/core/shared"}, {ID: "acme/tools/shared", Tap: "acme/tools", Name: "shared", HomebrewFullName: "acme/tools/shared"}})
	if err == nil {
		t.Fatal("rack collision accepted")
	}
}
