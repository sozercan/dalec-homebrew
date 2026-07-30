package buildfiles

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

func TestDockerfileParses(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	f, err := os.Open(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := parser.Parse(f); err != nil {
		t.Fatal(err)
	}
}

func TestFrontendAdvertisesInputsCapability(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	data, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `moby.buildkit.frontend.caps="moby.buildkit.frontend.inputs"`) {
		t.Fatal("frontend image must advertise the BuildKit frontend inputs capability")
	}
}
