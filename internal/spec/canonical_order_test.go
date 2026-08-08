package spec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestCanonicalRootOrderSurvivesDalecReserialization(t *testing.T) {
	frontendRef := "ghcr.io/example/dalec-homebrew@sha256:" + strings.Repeat("a", 64)
	data := `targets:
  homebrew:
    frontend:
      image: ` + frontendRef + `
    dependencies:
      runtime:
        zlib: {}
        acme/tools/widget: {}
        hello: {}
`
	original := load(t, data)
	forwarded, err := yaml.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded := load(t, string(forwarded))
	selection, err := ValidateForwarded(decoded, "homebrew", "amd64", Forwarding{Source: frontendRef}, Capabilities{NonCoreTaps: true})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(selection.Roots))
	for i, root := range selection.Roots {
		got[i] = root.Requested
	}
	want := []string{"acme/tools/widget", "hello", "zlib"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requested roots=%v, want canonical Formula-ID order %v", got, want)
	}
}

func TestCanonicalRootOrderIgnoresYAMLDeclarationOrder(t *testing.T) {
	const first = `dependencies:
  runtime:
    zlib: {}
    acme/tools/widget: {}
    hello: {}
`
	const second = `dependencies:
  runtime:
    hello: {}
    zlib: {}
    acme/tools/widget: {}
`
	var got [][]Root
	for _, data := range []string{first, second} {
		selection, err := Validate(load(t, data), "", "amd64", Capabilities{NonCoreTaps: true})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, selection.Roots)
	}
	if !reflect.DeepEqual(got[0], got[1]) {
		t.Fatalf("canonical roots differ by YAML declaration order:\nfirst=%+v\nsecond=%+v", got[0], got[1])
	}
	ids := make([]string, len(got[0]))
	for i, root := range got[0] {
		ids[i] = root.ID.String()
	}
	want := []string{"acme/tools/widget", "homebrew/core/hello", "homebrew/core/zlib"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("canonical IDs=%v, want %v", ids, want)
	}
}

func TestObsoleteForwardingExtensionRejected(t *testing.T) {
	data := `x-dalec-homebrew:
  runtime_dependency_order: [hello]
dependencies:
  runtime:
    hello: {}
`
	_, err := Validate(load(t, data), "", "amd64")
	if err == nil || !strings.Contains(err.Error(), `top-level extension "x-dalec-homebrew" is unsupported`) {
		t.Fatalf("error=%v, want obsolete extension rejection", err)
	}
}
