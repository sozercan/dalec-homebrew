package catalogextractor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBrewRubyCommandUsesPinnedHomebrewEntrypoint(t *testing.T) {
	repository := t.TempDir()
	brew := filepath.Join(repository, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMEBREW_REPOSITORY", repository)

	command, err := brewRubyCommand("/extract.rb", "svt/avtools", "https://github.com/svt/homebrew-avtools")
	if err != nil {
		t.Fatal(err)
	}
	if command.Path != brew {
		t.Fatalf("path=%q want=%q", command.Path, brew)
	}
	wantArgs := []string{brew, "ruby", "/extract.rb", "svt/avtools", "https://github.com/svt/homebrew-avtools"}
	if !slices.Equal(command.Args, wantArgs) {
		t.Fatalf("args=%q want=%q", command.Args, wantArgs)
	}
	for _, want := range []string{
		"HOME=/home/linuxbrew",
		"USER=linuxbrew",
		"LOGNAME=linuxbrew",
		"HOMEBREW_DEVELOPER=1",
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_NO_ANALYTICS=1",
		"HOMEBREW_NO_INSTALL_FROM_API=1",
		"HOMEBREW_NO_ENV_HINTS=1",
	} {
		if !slices.Contains(command.Env, want) {
			t.Fatalf("environment is missing %q", want)
		}
	}
}

func TestBrewRubyCommandRejectsSymlinkEntrypoint(t *testing.T) {
	repository := t.TempDir()
	bin := filepath.Join(repository, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repository, "brew-real")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bin, "brew")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMEBREW_REPOSITORY", repository)
	if _, err := brewRubyCommand("/extract.rb"); err == nil || !strings.Contains(err.Error(), "non-symlink executable") {
		t.Fatalf("err=%v", err)
	}
}
