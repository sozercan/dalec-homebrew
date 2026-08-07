package spec

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/project-dalec/dalec"
)

func TestForwardingOrderSurvivesDalecReserialization(t *testing.T) {
	frontendRef := "ghcr.io/example/dalec-homebrew@sha256:" + strings.Repeat("a", 64)
	data := `x-dalec-homebrew:
  schema_version: dalec-homebrew-forwarding/v1
  target: homebrew
  runtime_dependency_order: [zlib, hello]
targets:
  homebrew:
    frontend:
      image: ` + frontendRef + `
    dependencies:
      runtime:
        zlib: {}
        hello: {}
`
	original := load(t, data)
	forwarded, err := yaml.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded := load(t, string(forwarded))
	selection, err := ValidateForwarded(decoded, "homebrew", "amd64", Forwarding{Source: frontendRef})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{selection.Roots[0].Requested, selection.Roots[1].Requested}
	if strings.Join(got, ",") != "zlib,hello" {
		t.Fatalf("requested roots=%v, want preserved extension order", got)
	}
}

func TestForwardingOrderFailures(t *testing.T) {
	frontendRef := "ghcr.io/example/dalec-homebrew@sha256:" + strings.Repeat("a", 64)
	base := func(extension string) string {
		return extension + `
targets:
  homebrew:
    frontend:
      image: ` + frontendRef + `
    dependencies:
      runtime:
        hello: {}
        zlib: {}
`
	}
	tests := []struct {
		name      string
		extension string
		want      string
	}{
		{name: "missing", extension: "", want: `required extension "x-dalec-homebrew" is missing`},
		{name: "schema", extension: `x-dalec-homebrew:
  schema_version: other
  target: homebrew
  runtime_dependency_order: [hello, zlib]`, want: "schema_version must be exactly"},
		{name: "target", extension: `x-dalec-homebrew:
  schema_version: dalec-homebrew-forwarding/v1
  target: other
  runtime_dependency_order: [hello, zlib]`, want: `does not match selected target "homebrew"`},
		{name: "missing dependency", extension: `x-dalec-homebrew:
  schema_version: dalec-homebrew-forwarding/v1
  target: homebrew
  runtime_dependency_order: [hello]`, want: "has 1 entries"},
		{name: "extra dependency", extension: `x-dalec-homebrew:
  schema_version: dalec-homebrew-forwarding/v1
  target: homebrew
  runtime_dependency_order: [hello, zlib, jq]`, want: `undeclared dependency "jq"`},
		{name: "duplicate", extension: `x-dalec-homebrew:
  schema_version: dalec-homebrew-forwarding/v1
  target: homebrew
  runtime_dependency_order: [hello, hello]`, want: "duplicate canonical Formula root"},
		{name: "unknown field", extension: `x-dalec-homebrew:
  schema_version: dalec-homebrew-forwarding/v1
  target: homebrew
  runtime_dependency_order: [hello, zlib]
  unknown: true`, want: "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := dalec.LoadSpec([]byte(base(tt.extension)))
			if err != nil {
				t.Fatal(err)
			}
			_, err = ValidateForwarded(spec, "homebrew", "amd64", Forwarding{Source: frontendRef})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestForwardingOrderRejectsDuplicateFields(t *testing.T) {
	frontendRef := "ghcr.io/example/dalec-homebrew@sha256:" + strings.Repeat("a", 64)
	data := `x-dalec-homebrew:
  schema_version: dalec-homebrew-forwarding/v1
  target: homebrew
  runtime_dependency_order: [hello]
  runtime_dependency_order: [hello]
targets:
  homebrew:
    frontend:
      image: ` + frontendRef + `
    dependencies:
      runtime:
        hello: {}
`
	spec, err := dalec.LoadSpec([]byte(data))
	if err == nil {
		_, err = ValidateForwarded(spec, "homebrew", "amd64", Forwarding{Source: frontendRef})
	}
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "mapping key") {
		t.Fatalf("error=%v, want duplicate mapping-key rejection", err)
	}
}
