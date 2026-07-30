package materializer

import (
	"os"
	"strings"
	"testing"
)

func TestPourAdapterPropagatesDeferredHomebrewFailure(t *testing.T) {
	data, err := os.ReadFile("pour.rb")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if !strings.Contains(source, `ENV["HOMEBREW_INTERNAL_ALLOW_PACKAGES_FROM_PATHS"] = "1"`) {
		t.Fatal("pour adapter must explicitly permit its verified local bottle path")
	}
	if strings.Contains(source, `ENV["HOMEBREW_DEVELOPER"]`) {
		t.Fatal("pour adapter must not enable Homebrew developer mode")
	}
	if !strings.Contains(source, "installer.finish") || !strings.Contains(source, "exit 1 if Homebrew.failed?") {
		t.Fatal("pour adapter must propagate Homebrew's deferred failure status")
	}
}
