package spec

import (
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

func TestPreflightRejectsTapSyntax(t *testing.T) {
	data := strings.Replace(baseSpec, "hello", "homebrew/core/hello", 1)
	if err := PreflightFormulaNames([]byte(data), ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestRootVolumeOverlapsProtectedPaths(t *testing.T) {
	if err := ValidateImage("global", &dalec.ImageConfig{Volumes: map[string]struct{}{"/": {}}}); err == nil {
		t.Fatal("root volume accepted")
	}
}
