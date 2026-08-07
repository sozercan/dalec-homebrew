package catalogextractor

import (
	"os"
	"strings"
	"testing"
)

func readExtractorScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("extract.rb")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRubyExtractorKeepsRuntimeDependenciesAndSourceContained(t *testing.T) {
	script := readExtractorScript(t)
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
		`hash.fetch("urls", {}).fetch("stable", nil)`,
		`stable.fetch("checksum", "").to_s`,
		`return "tar+gzip" if uri.path.end_with?(".tar.gz", ".tgz")`,
		`"stable_source" => stable_source_from_hash(hash)`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("Ruby extractor is missing %q", required)
		}
	}
}

func TestRubyExtractorRejectsOversizedFormulaBeforeReading(t *testing.T) {
	script := readExtractorScript(t)
	constant := strings.Index(script, `MAX_FORMULA_BYTES = 4 * 1024 * 1024`)
	limit := strings.Index(script, `if stat.size > MAX_FORMULA_BYTES`)
	read := strings.Index(script, `source = formula_path.binread`)
	if constant < 0 || limit < 0 || read < 0 || limit > read {
		t.Fatalf("Formula size limit must be declared and checked before binread: constant=%d limit=%d read=%d", constant, limit, read)
	}
}

func TestRubyExtractorPreservesAllBottleDeclarations(t *testing.T) {
	script := readExtractorScript(t)
	for _, required := range []string{
		`PLATFORM_TAGS = %w[x86_64_linux arm64_linux].freeze`,
		`SUPPORTED_TAGS = (PLATFORM_TAGS + %w[all]).freeze`,
		`next unless SUPPORTED_TAGS.include?(tag_name)`,
		`platforms = PLATFORM_TAGS.filter_map { |tag| platform_formula(formula_path, tag) }`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("Ruby extractor is missing %q", required)
		}
	}
}
