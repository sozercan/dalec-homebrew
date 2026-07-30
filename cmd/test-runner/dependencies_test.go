package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeDependencyGraphExcludesBuildStack(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
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
