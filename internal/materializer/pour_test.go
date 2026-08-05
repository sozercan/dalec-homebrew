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

func TestPourAdapterDerivedPrebuiltSkipsFormulaHooks(t *testing.T) {
	data, err := os.ReadFile("pour.rb")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, want := range []string{
		`ARGV.fetch(3) != "derived-prebuilt-v1"`,
		"formula.bottle_specification.sha256(",
		"cellar: :any_skip_relocation",
		"skip_post_install: derived_prebuilt",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("derived prebuilt adapter is missing %q", want)
		}
	}
	if !strings.Contains(source, "formula.local_bottle_path = bottle_path") {
		t.Fatal("derived prebuilt path must remain a local bottle pour")
	}
}

func TestPourAdapterV2LoadsStagedTapFormula(t *testing.T) {
	data, err := os.ReadFile("pour.rb")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, want := range []string{
		"formula_id = ARGV.fetch(0)",
		"formula_path = Pathname(ARGV.fetch(1)).realpath",
		"formula = Formulary.factory(formula_path, force_bottle: true)",
		"formula.local_bottle_path = bottle_path",
		`"homebrew/core/#{formula.name}"`,
		`abort "staged Formula identity mismatch" unless actual_id == formula_id`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("V2 adapter is missing %q", want)
		}
	}
	legacy := strings.Index(source, `ENV["HOMEBREW_INTERNAL_ALLOW_PACKAGES_FROM_PATHS"] = "1"`)
	v2 := strings.Index(source, "formula_id = ARGV.fetch(0)")
	if legacy < 0 || v2 < 0 || legacy > v2 {
		t.Fatal("legacy path switch must remain confined to the one-argument branch")
	}
}
