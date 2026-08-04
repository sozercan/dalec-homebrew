package catalogextractor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type fakeCore map[string]metadata.Match

func (f fakeCore) Lookup(name string) (metadata.Match, error) {
	value, ok := f[name]
	if !ok {
		return metadata.Match{}, metadata.ErrFormulaNotFound
	}
	return value, nil
}

func TestToCatalogNormalizesCoreFirstAndTapFallback(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	bottleDigest := "sha256:" + strings.Repeat("b", 64)
	platform := func(tag string, dependencies []string) ExtractedPlatformFormula {
		return ExtractedPlatformFormula{Tag: tag, Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1.2.3", License: "MIT", Dependencies: dependencies, Bottle: &ExtractedBottle{RootURL: "https://bottles.example", Files: []catalog.BottleFile{{Tag: tag, URL: "https://bottles.example/widget.tgz", SHA256: bottleDigest, Cellar: ":any"}}}}
	}
	extracted := &ExtractedTap{SchemaVersion: ExtractedTapSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("c", 40), TreeDigest: digest, ArchiveDigest: digest}, Formulae: []ExtractedFormula{
		{SourcePath: "Formula/widget.rb", SourceDigest: digest, Platforms: []ExtractedPlatformFormula{platform("x86_64_linux", []string{"openssl@3", "helper"}), platform("arm64_linux", []string{"openssl@3", "helper"})}},
		{SourcePath: "Formula/helper.rb", SourceDigest: digest, Platforms: []ExtractedPlatformFormula{{Tag: "x86_64_linux", Name: "helper", HomebrewFullName: "acme/tools/helper", StableVersion: "1"}, {Tag: "arm64_linux", Name: "helper", HomebrewFullName: "acme/tools/helper", StableVersion: "1"}}},
	}}
	core := fakeCore{"openssl@3": {Canonical: "homebrew/core/openssl@3", Formula: metadata.Formula{Name: "openssl@3", FullName: "homebrew/core/openssl@3"}}}
	document, err := ToCatalog(extracted, core)
	if err != nil {
		t.Fatal(err)
	}
	if !document.PublishedAt.Equal(catalogValidationTime) || document.Sequence != 1 {
		t.Fatalf("deterministic service placeholders are missing: %+v", document)
	}
	var widget catalog.Formula
	for _, formula := range document.Formulae {
		if formula.Name == "widget" {
			widget = formula
		}
	}
	if len(widget.Dependencies) != 2 || widget.Dependencies[0].ID != "acme/tools/helper" || widget.Dependencies[1].ID != "homebrew/core/openssl@3" {
		t.Fatalf("dependencies=%+v", widget.Dependencies)
	}
}

func TestToCatalogRejectsBareDependencyOutsideCoreAndTap(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	extracted := &ExtractedTap{SchemaVersion: ExtractedTapSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("c", 40), TreeDigest: digest, ArchiveDigest: digest}, Formulae: []ExtractedFormula{{SourcePath: "Formula/widget.rb", SourceDigest: digest, Platforms: []ExtractedPlatformFormula{{Tag: "x86_64_linux", Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1", Dependencies: []string{"missing"}}, {Tag: "arm64_linux", Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1", Dependencies: []string{"missing"}}}}}}
	_, err := ToCatalog(extracted, fakeCore{})
	if err == nil || !strings.Contains(err.Error(), "absent from core and tap") {
		t.Fatalf("err=%v", err)
	}
}

func TestDecodeExtractedTapRejectsDuplicateMembers(t *testing.T) {
	_, err := DecodeExtractedTap([]byte(`{"schema_version":"x","schema_version":"y"}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON member") {
		t.Fatalf("err=%v", err)
	}
}

func TestToCatalogRetainsPlatformUnavailability(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	extracted := &ExtractedTap{SchemaVersion: ExtractedTapSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("c", 40), TreeDigest: digest, ArchiveDigest: digest}, Formulae: []ExtractedFormula{{SourcePath: "Formula/widget.rb", SourceDigest: digest, Platforms: []ExtractedPlatformFormula{{Tag: "x86_64_linux", Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1"}}}}}
	document, err := ToCatalog(extracted, fakeCore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Formulae[0].Variations) != 1 || document.Formulae[0].Variations[0].Tag != "arm64_linux" || !document.Formulae[0].Variations[0].Unavailable {
		t.Fatalf("variations=%+v", document.Formulae[0].Variations)
	}
}

func TestValidateExtractedFileBindsAuthenticatedSource(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	extracted := &ExtractedTap{SchemaVersion: ExtractedTapSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("c", 40), TreeDigest: digest, ArchiveDigest: digest}, Formulae: []ExtractedFormula{{SourcePath: "Formula/widget.rb", SourceDigest: digest, Platforms: []ExtractedPlatformFormula{{Tag: "x86_64_linux", Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1"}}}}}
	inputData, err := jsonMarshalExtracted(extracted)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "input.json")
	source := filepath.Join(t.TempDir(), "source.json")
	output := filepath.Join(t.TempDir(), "output.json")
	if err := os.WriteFile(input, inputData, 0o444); err != nil {
		t.Fatal(err)
	}
	wrong := SourceMetadata{SchemaVersion: SourceMetadataSchemaVersion, Tap: extracted.Tap}
	wrong.Tap.Commit = strings.Repeat("d", 40)
	sourceData, _ := json.Marshal(wrong)
	if err := os.WriteFile(source, sourceData, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExtractedFile(input, source, t.TempDir(), output); err == nil || !strings.Contains(err.Error(), "changed authenticated tap source") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateExtractedFilePublishesVerifiedSourceDigest(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	tapRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tapRoot, "Formula"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceBytes := []byte("class Widget < Formula\nend\n")
	sum := sha256.Sum256(sourceBytes)
	formulaDigest := "sha256:" + hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(tapRoot, "Formula", "widget.rb"), sourceBytes, 0o444); err != nil {
		t.Fatal(err)
	}
	extracted := &ExtractedTap{SchemaVersion: ExtractedTapSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("c", 40), TreeDigest: digest, ArchiveDigest: digest}, Formulae: []ExtractedFormula{{SourcePath: "Formula/widget.rb", SourceDigest: formulaDigest, Platforms: []ExtractedPlatformFormula{{Tag: "x86_64_linux", Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1"}}}}}
	inputData, _ := jsonMarshalExtracted(extracted)
	sourceData, _ := json.Marshal(SourceMetadata{SchemaVersion: SourceMetadataSchemaVersion, Tap: extracted.Tap})
	input := filepath.Join(t.TempDir(), "input.json")
	source := filepath.Join(t.TempDir(), "source.json")
	output := filepath.Join(t.TempDir(), "output.json")
	if err := os.WriteFile(input, inputData, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, sourceData, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExtractedFile(input, source, tapRoot, output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
}
