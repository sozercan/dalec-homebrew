package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	// The go command runs tests from the package directory. Avoid runtime.Caller:
	// -trimpath rewrites caller filenames to module paths, not filesystem paths.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func TestRuntimeDependencyGraphExcludesBuildStack(t *testing.T) {
	root := repositoryRoot(t)
	cmd := exec.Command("go", "list", "-mod=readonly", "-deps", "-f", "{{.ImportPath}}", "./cmd/test-runner")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list test-runner dependencies: %v\n%s", err, output)
	}

	forbidden := []string{
		"github.com/project-dalec/dalec",
		"github.com/moby/buildkit",
		"google.golang.org/grpc",
		"google.golang.org/protobuf",
	}
	for _, dependency := range strings.Fields(string(output)) {
		for _, prefix := range forbidden {
			if dependency == prefix || strings.HasPrefix(dependency, prefix+"/") {
				t.Errorf("runtime test runner depends on build-only package %q", dependency)
			}
		}
	}
}
