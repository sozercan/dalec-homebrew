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
    - hello
    - jq
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

func TestListShorthandAndOrder(t *testing.T) {
	order, err := RuntimeDependencyOrder([]byte(baseSpec), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "hello,jq" {
		t.Fatalf("order=%v", order)
	}
	sel, err := Validate(load(t, baseSpec), "", "amd64", order)
	if err != nil {
		t.Fatal(err)
	}
	if got := sel.Roots[0].Name; got != "hello" {
		t.Fatalf("first root %q", got)
	}
}

func TestBareDependencyOnlySpec(t *testing.T) {
	data := `{"dependencies":{"runtime":{"hello":{}}},"image":{"entrypoint":"/home/linuxbrew/.linuxbrew/bin/hello"}}`
	order, err := RuntimeDependencyOrder([]byte(data), "")
	if err != nil {
		t.Fatal(err)
	}
	sel, err := Validate(load(t, data), "", "amd64", order)
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
	order, err := RuntimeDependencyOrder([]byte(data), "prod")
	if err != nil {
		t.Fatal(err)
	}
	sel, err := Validate(load(t, data), "prod", "amd64", order)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Roots) != 1 || sel.Roots[0].Name != "hello" {
		t.Fatalf("roots=%v", sel.Roots)
	}
}

func TestRejectVersionAndForbiddenFields(t *testing.T) {
	data := strings.Replace(baseSpec, "    - hello\n    - jq", "    hello:\n      version: ['>=2']\n    jq: {}", 1) + "\nsources:\n  x:\n    http:\n      url: https://example.invalid/x\n"
	_, err := Validate(load(t, data), "", "amd64", nil)
	if err == nil || !strings.Contains(err.Error(), "V2 feature") || !strings.Contains(err.Error(), "sources") {
		t.Fatalf("err=%v", err)
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
	_, err := Validate(load(t, data), "prod", "amd64", nil)
	if err == nil || !strings.Contains(err.Error(), "sysext") || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("err=%v", err)
	}
}

func TestPreflightCanonicalizesExplicitCore(t *testing.T) {
	data := strings.Replace(baseSpec, "hello", "homebrew/core/hello", 1)
	if err := PreflightFormulaNames([]byte(data), ""); err != nil {
		t.Fatal(err)
	}
	order, err := RuntimeDependencyOrder([]byte(data), "")
	if err != nil {
		t.Fatal(err)
	}
	sel, err := Validate(load(t, data), "", "amd64", order)
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
	order, err := RuntimeDependencyOrder([]byte(data), "")
	if err != nil {
		t.Fatal(err)
	}
	sel, err := Validate(load(t, data), "", "amd64", order, Capabilities{NonCoreTaps: true})
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
				order, err := RuntimeDependencyOrder([]byte(data), "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Validate(load(t, data), "", "amd64", order, caps); err == nil || !strings.Contains(err.Error(), test.want) {
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
			order, err := RuntimeDependencyOrder([]byte(data), "")
			if err != nil {
				t.Fatal(err)
			}
			selection, err := Validate(load(t, data), "", "amd64", order, caps)
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

func TestRootVolumeOverlapsProtectedPaths(t *testing.T) {
	if err := ValidateImage("global", &dalec.ImageConfig{Volumes: map[string]struct{}{"/": {}}}); err == nil {
		t.Fatal("root volume accepted")
	}
}
