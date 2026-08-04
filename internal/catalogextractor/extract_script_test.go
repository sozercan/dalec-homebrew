package catalogextractor

import (
	"os"
	"strings"
	"testing"
)

func TestRubyExtractorKeepsRuntimeDependenciesAndSourceContained(t *testing.T) {
	data, err := os.ReadFile("extract.rb")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, required := range []string{
		`hash.fetch("dependencies", []) + hash.fetch("recommended_dependencies", [])`,
		`hash.fetch("uses_from_macos", [])`,
		`stat.symlink? || !stat.file?`,
		`resolved.to_s.start_with?(tap_prefix)`,
		`Digest::SHA256.hexdigest(source)`,
		`Utils.name_from_full_name(value)`,
		`simulated_arch = { x86_64: :intel, arm64: :arm }.fetch(tag.arch)`,
		`Homebrew::SimulateSystem.with(os: tag.system, arch: simulated_arch)`,
		`next unless SUPPORTED_TAGS.include?(tag_name)`,
		`cellar = ENV.fetch("HOMEBREW_CELLAR") if cellar == "/Cellar"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("Ruby extractor is missing %q", required)
		}
	}
}
