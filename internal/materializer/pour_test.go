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
	if !strings.Contains(source, "rescue FormulaUnavailableError") || !strings.Contains(source, "LinkageChecker.prepend(DalecHomebrewOfflineLinkage)") {
		t.Fatal("pour adapter must bound the empty-tap workaround to unavailable linkage Formula metadata")
	}
	if strings.Contains(source, "rescue Exception") || strings.Contains(source, "rescue StandardError") {
		t.Fatal("pour adapter must not hide genuine installer or linkage failures")
	}
	install := strings.Index(source, "installer.install")
	patch := strings.Index(source, "LinkageChecker.prepend(DalecHomebrewOfflineLinkage)")
	finish := strings.Index(source, "installer.finish")
	if install < 0 || patch < install || finish < patch {
		t.Fatal("pour adapter must apply the bounded linkage workaround only after pour and before finish")
	}
	for _, forbidden := range []string{
		"bottle_tab_runtime_dependencies",
		"Tab.define_singleton_method",
		"installed_tab.runtime_dependencies",
	} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("pour adapter must leave receipt dependency evidence to Homebrew and the Go verifier: found %q", forbidden)
		}
	}
}
