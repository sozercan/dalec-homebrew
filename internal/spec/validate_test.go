package spec

import (
	"fmt"
	"strings"
	"testing"

	"github.com/project-dalec/dalec"
)

const baseSpec = `
name: runtime
description: test
website: https://example.invalid
version: 1.0.0
revision: 1
license: Apache-2.0
dependencies:
  runtime:
    jq: {}
    hello: {}
image:
  entrypoint: hello
`

func load(t *testing.T, data string) *dalec.Spec {
	t.Helper()
	s, err := dalec.LoadSpec([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMapFormCanonicalNames(t *testing.T) {
	order, err := RuntimeDependencyNames([]byte(baseSpec), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "hello,jq" {
		t.Fatalf("names=%v", order)
	}
	sel, err := Validate(load(t, baseSpec), "", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got := sel.Roots[0].Name; got != "hello" {
		t.Fatalf("first root %q", got)
	}
}

func TestRuntimeDependencyNamesRejectsListShorthand(t *testing.T) {
	data := "dependencies:\n  runtime: [hello, jq]\n"
	_, err := RuntimeDependencyNames([]byte(data), "")
	if err == nil || !strings.Contains(err.Error(), "must use map form") {
		t.Fatalf("error=%v, want map-form rejection", err)
	}
}

func TestRuntimeDependencyNamesValidatesGlobalAndSelectedShapes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "global list with selected map",
			data: `dependencies:
  runtime: [hello]
targets:
  homebrew:
    dependencies:
      runtime:
        jq: {}
`,
			want: "global dependencies.runtime must use map form",
		},
		{
			name: "selected list",
			data: `dependencies:
  runtime:
    hello: {}
targets:
  homebrew:
    dependencies:
      runtime: [jq]
`,
			want: "target homebrew dependencies.runtime must use map form",
		},
		{
			name: "empty selected list",
			data: `dependencies:
  runtime:
    hello: {}
targets:
  homebrew:
    dependencies:
      runtime: []
`,
			want: "target homebrew dependencies.runtime must use map form",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RuntimeDependencyNames([]byte(tt.data), "homebrew")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestRuntimeDependencyNamesRejectsPresentEmptyScopes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "empty global dependencies",
			data: `dependencies: {}
targets:
  homebrew:
    dependencies:
      runtime:
        jq: {}
`,
			want: "global dependencies.runtime must use map form and contain at least one entry",
		},
		{
			name: "empty selected dependencies",
			data: `dependencies:
  runtime:
    hello: {}
targets:
  homebrew:
    dependencies: {}
`,
			want: "target homebrew dependencies.runtime must use map form and contain at least one entry",
		},
		{
			name: "empty global runtime map",
			data: `dependencies:
  runtime: {}
`,
			want: "global dependencies.runtime must use map form and contain at least one entry",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RuntimeDependencyNames([]byte(tt.data), "homebrew")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestRuntimeDependencyNamesAllowsOmittedScopes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "inherit global",
			data: `dependencies:
  runtime:
    hello: {}
targets:
  homebrew: {}
`,
			want: "hello",
		},
		{
			name: "selected only",
			data: `targets:
  homebrew:
    dependencies:
      runtime:
        jq: {}
`,
			want: "jq",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names, err := RuntimeDependencyNames([]byte(tt.data), "homebrew")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(names, ",") != tt.want {
				t.Fatalf("names=%v, want %q", names, tt.want)
			}
		})
	}
}

func TestBareDependencyOnlySpec(t *testing.T) {
	data := `{"dependencies":{"runtime":{"hello":{}}},"image":{"entrypoint":"/home/linuxbrew/.linuxbrew/bin/hello"}}`
	sel, err := Validate(load(t, data), "", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Roots) != 1 || sel.Roots[0].Name != "hello" {
		t.Fatalf("roots=%v", sel.Roots)
	}
	if sel.Image == nil || sel.Image.Entrypoint != "/home/linuxbrew/.linuxbrew/bin/hello" {
		t.Fatalf("image=%+v", sel.Image)
	}
}

func TestTargetOverrideAndArchFiltering(t *testing.T) {
	data := baseSpec + `
targets:
  prod:
    dependencies:
      runtime:
        ripgrep:
          arch: [arm64]
        hello: {}
`
	sel, err := Validate(load(t, data), "prod", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Roots) != 1 || sel.Roots[0].Name != "hello" {
		t.Fatalf("roots=%v", sel.Roots)
	}
}

func TestValidateForwardedTargetFrontend(t *testing.T) {
	frontendRef := "ghcr.io/example/dalec-homebrew@sha256:" + strings.Repeat("a", 64)
	data := forwardedSpec(frontendRef, "")
	selection, err := ValidateForwarded(load(t, data), "homebrew", "amd64", Forwarding{Source: frontendRef})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Roots) != 1 || selection.Roots[0].Name != "hello" {
		t.Fatalf("roots=%v", selection.Roots)
	}
}

func TestValidateForwardedTargetFrontendFailures(t *testing.T) {
	frontendRef := "ghcr.io/example/dalec-homebrew@sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name       string
		mutateSpec func(*dalec.Spec)
		forwarding Forwarding
		want       string
	}{
		{
			name: "missing selected target",
			mutateSpec: func(spec *dalec.Spec) {
				delete(spec.Targets, "homebrew")
			},
			forwarding: Forwarding{Source: frontendRef},
			want:       `forwarded target "homebrew" is not defined`,
		},
		{
			name: "missing frontend metadata",
			mutateSpec: func(spec *dalec.Spec) {
				target := spec.Targets["homebrew"]
				target.Frontend = nil
				spec.Targets["homebrew"] = target
			},
			forwarding: Forwarding{Source: frontendRef},
			want:       "does not declare frontend routing metadata",
		},
		{
			name:       "missing gateway source",
			forwarding: Forwarding{},
			want:       "missing the gateway source",
		},
		{
			name:       "frontend image mismatch",
			forwarding: Forwarding{Source: "ghcr.io/example/other@sha256:" + strings.Repeat("b", 64)},
			want:       "does not match invoking gateway source",
		},
		{
			name: "target cmdline",
			mutateSpec: func(spec *dalec.Spec) {
				spec.Targets["homebrew"].Frontend.CmdLine = "--unsafe"
			},
			forwarding: Forwarding{Source: frontendRef},
			want:       "frontend cmdline must be empty",
		},
		{
			name:       "invocation cmdline",
			forwarding: Forwarding{Source: frontendRef, CmdLine: "--unsafe"},
			want:       "forwarded invocation cmdline must be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := load(t, forwardedSpec(frontendRef, ""))
			if test.mutateSpec != nil {
				test.mutateSpec(spec)
			}
			_, err := ValidateForwarded(spec, "homebrew", "amd64", test.forwarding)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestRejectVersionAndForbiddenFields(t *testing.T) {
	data := strings.Replace(baseSpec, "    jq: {}\n    hello: {}", "    jq: {}\n    hello:\n      version: ['>=2']", 1) + "\nsources:\n  x:\n    http:\n      url: https://example.invalid/x\n"
	_, err := Validate(load(t, data), "", "amd64")
	if err == nil || !strings.Contains(err.Error(), "historical versions") || !strings.Contains(err.Error(), "sources") {
		t.Fatalf("err=%v", err)
	}
}

func TestRejectNonCoreVersionConstraint(t *testing.T) {
	data := `dependencies:
  runtime:
    sozercan/repo/a365:
      version: ["0.3.3"]
image:
  entrypoint: a365
`
	if _, err := Validate(load(t, data), "", "amd64", Capabilities{NonCoreTaps: true}); err == nil || !strings.Contains(err.Error(), "historical versions") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestRejectHiddenSysextAndVolumeOverlap(t *testing.T) {
	data := baseSpec + `
targets:
  prod:
    dependencies:
      sysext: [bad]
    image:
      volumes:
        /home/linuxbrew: {}
`
	_, err := Validate(load(t, data), "prod", "amd64")
	if err == nil || !strings.Contains(err.Error(), "sysext") || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("err=%v", err)
	}
}

func TestPreflightCanonicalizesExplicitCore(t *testing.T) {
	data := strings.Replace(baseSpec, "hello", "homebrew/core/hello", 1)
	if err := PreflightFormulaNames([]byte(data), ""); err != nil {
		t.Fatal(err)
	}
	sel, err := Validate(load(t, data), "", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got := sel.Roots[0]; got.Name != "hello" || got.ID.String() != "homebrew/core/hello" || got.Requested != "homebrew/core/hello" {
		t.Fatalf("root=%+v", got)
	}
}

func TestQualifiedRootRequiresCapability(t *testing.T) {
	data := strings.Replace(baseSpec, "hello", "acme/tools/widget", 1)
	if err := PreflightFormulaNames([]byte(data), ""); err == nil || !strings.Contains(err.Error(), "capability bindings") {
		t.Fatalf("err=%v", err)
	}
	if err := PreflightFormulaNames([]byte(data), "", Capabilities{NonCoreTaps: true}); err != nil {
		t.Fatal(err)
	}
	sel, err := Validate(load(t, data), "", "amd64", Capabilities{NonCoreTaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := sel.Roots[0]; got.Name != "acme/tools/widget" || got.ID.String() != "acme/tools/widget" {
		t.Fatalf("root=%+v", got)
	}
}

func TestDuplicateCanonicalRootsRejected(t *testing.T) {
	data := `dependencies:
  runtime:
    hello: {}
    homebrew/core/hello: {}
image:
  entrypoint: hello
`
	if err := PreflightFormulaNames([]byte(data), ""); err == nil || !strings.Contains(err.Error(), "duplicate canonical Formula root") {
		t.Fatalf("err=%v", err)
	}
}

func TestRuntimeRootLimitsRejectRawAndDecodedSpecs(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		want  string
	}{
		{
			name:  "runtime roots",
			roots: coreRoots(maxRuntimeRoots + 1),
			want:  "257 canonical runtime roots exceed maximum 256",
		},
		{
			name:  "non-core root taps",
			roots: nonCoreRoots(maxNonCoreRootTaps + 1),
			want:  "17 distinct non-core root taps exceed maximum 16",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := runtimeRootsSpec(test.roots)
			caps := Capabilities{NonCoreTaps: true}
			t.Run("raw preflight", func(t *testing.T) {
				if err := PreflightFormulaNames([]byte(data), "", caps); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("PreflightFormulaNames() error = %v, want %q", err, test.want)
				}
			})
			t.Run("decoded spec", func(t *testing.T) {
				if _, err := Validate(load(t, data), "", "amd64", caps); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("Validate() error = %v, want %q", err, test.want)
				}
			})
		})
	}
}

func TestRuntimeRootLimitBoundariesAccepted(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
	}{
		{name: "runtime roots", roots: coreRoots(maxRuntimeRoots)},
		{name: "non-core root taps", roots: nonCoreRoots(maxNonCoreRootTaps)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := runtimeRootsSpec(test.roots)
			caps := Capabilities{NonCoreTaps: true}
			if err := PreflightFormulaNames([]byte(data), "", caps); err != nil {
				t.Fatalf("PreflightFormulaNames() error = %v", err)
			}
			selection, err := Validate(load(t, data), "", "amd64", caps)
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if len(selection.Roots) != len(test.roots) {
				t.Fatalf("Validate() selected %d roots, want %d", len(selection.Roots), len(test.roots))
			}
		})
	}
}

func coreRoots(count int) []string {
	roots := make([]string, count)
	for i := range roots {
		roots[i] = fmt.Sprintf("formula%d", i)
	}
	return roots
}

func nonCoreRoots(count int) []string {
	roots := make([]string, count)
	for i := range roots {
		roots[i] = fmt.Sprintf("owner%d/tap%d/formula", i, i)
	}
	return roots
}

func runtimeRootsSpec(roots []string) string {
	var result strings.Builder
	result.WriteString("dependencies:\n  runtime:\n")
	for _, root := range roots {
		fmt.Fprintf(&result, "    %s: {}\n", root)
	}
	result.WriteString("image:\n  entrypoint: runtime\n")
	return result.String()
}

func forwardedSpec(frontendRef, cmdline string) string {
	var result strings.Builder
	result.WriteString("dependencies:\n  runtime:\n    hello: {}\n")
	result.WriteString("image:\n  entrypoint: hello\n")
	result.WriteString("targets:\n  homebrew:\n    frontend:\n")
	fmt.Fprintf(&result, "      image: %s\n", frontendRef)
	if cmdline != "" {
		fmt.Fprintf(&result, "      cmdline: %s\n", cmdline)
	}
	result.WriteString("    dependencies:\n      runtime:\n        hello: {}\n")
	return result.String()
}

func TestRootVolumeOverlapsProtectedPaths(t *testing.T) {
	if err := ValidateImage("global", &dalec.ImageConfig{Volumes: map[string]struct{}{"/": {}}}); err == nil {
		t.Fatal("root volume accepted")
	}
}
