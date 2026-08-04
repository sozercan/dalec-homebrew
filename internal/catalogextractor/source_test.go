package catalogextractor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSourceMetadataBindsExactGitCommit(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "tap")
	if err := os.MkdirAll(filepath.Join(repository, "Formula"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "Formula", "widget.rb"), []byte("class Widget < Formula\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.invalid"}, {"add", "Formula/widget.rb"}, {"commit", "-q", "-m", "fixture"}} {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	output := filepath.Join(t.TempDir(), "source.json")
	if err := WriteSourceMetadata(t.Context(), "acme/tools", "https://github.com/acme/homebrew-tools", repository, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := DecodeSourceMetadata(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Tap.Commit) != 40 || !strings.HasPrefix(metadata.Tap.TreeDigest, "sha256:") || !strings.HasPrefix(metadata.Tap.ArchiveDigest, "sha256:") {
		t.Fatalf("metadata=%+v", metadata)
	}
}
