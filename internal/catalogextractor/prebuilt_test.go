package catalogextractor

import (
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

func TestToCatalogPublishesPerPlatformStableSourcesWithoutBottle(t *testing.T) {
	t.Parallel()
	tap, _ := catalog.ParseTapID("sozercan/repo")
	digest := "sha256:" + strings.Repeat("a", 64)
	extracted := &ExtractedTap{
		SchemaVersion: ExtractedTapSchemaVersion,
		Tap: catalog.TapSource{
			ID:            tap,
			Repository:    tap.DefaultGitHubRepository(),
			Commit:        strings.Repeat("c", 40),
			TreeDigest:    digest,
			ArchiveDigest: digest,
		},
		Formulae: []ExtractedFormula{{
			SourcePath:   "Formula/a365.rb",
			SourceDigest: digest,
			Platforms: []ExtractedPlatformFormula{
				{
					Tag:              "x86_64_linux",
					Name:             "a365",
					HomebrewFullName: "sozercan/repo/a365",
					StableVersion:    "0.3.3",
					License:          "MIT",
					StableSource: &ExtractedStableSource{
						URL:           "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz",
						SHA256:        "sha256:" + strings.Repeat("1", 64),
						ArchiveFormat: catalog.PrebuiltArchiveFormatTarGzip,
					},
				},
				{
					Tag:              "arm64_linux",
					Name:             "a365",
					HomebrewFullName: "sozercan/repo/a365",
					StableVersion:    "0.3.3",
					License:          "MIT",
					StableSource: &ExtractedStableSource{
						URL:           "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_arm64.tar.gz",
						SHA256:        "sha256:" + strings.Repeat("2", 64),
						ArchiveFormat: catalog.PrebuiltArchiveFormatTarGzip,
					},
				},
			},
		}},
	}

	document, err := ToCatalog(extracted, fakeCore{})
	if err != nil {
		t.Fatal(err)
	}
	formula := document.Formulae[0]
	if formula.Bottle != nil {
		t.Fatalf("unexpected native bottle: %+v", formula.Bottle)
	}
	if formula.PrebuiltArchive == nil || len(formula.PrebuiltArchive.Files) != 2 {
		t.Fatalf("prebuilt declaration = %+v", formula.PrebuiltArchive)
	}
	if got := formula.PrebuiltArchive.Files; got[0].Tag != "x86_64_linux" || got[1].Tag != "arm64_linux" || got[0].Format != catalog.PrebuiltArchiveFormatTarGzip {
		t.Fatalf("prebuilt files = %+v", got)
	}
}

func TestToCatalogPrefersNativeBottleOverStableSource(t *testing.T) {
	t.Parallel()
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	bottleDigest := "sha256:" + strings.Repeat("b", 64)
	platform := func(tag, arch string) ExtractedPlatformFormula {
		return ExtractedPlatformFormula{
			Tag:              tag,
			Name:             "widget",
			HomebrewFullName: "acme/tools/widget",
			StableVersion:    "1.2.3",
			Bottle: &ExtractedBottle{
				RootURL: "https://bottles.example.com/widget",
				Files: []catalog.BottleFile{{
					Tag:    tag,
					URL:    "https://bottles.example.com/widget-" + arch + ".tar.gz",
					SHA256: bottleDigest,
					Cellar: ":any",
				}},
			},
			StableSource: &ExtractedStableSource{
				URL:           "https://github.com/acme/widget/releases/download/v1.2.3/widget-" + arch + ".tar.gz",
				SHA256:        digest,
				ArchiveFormat: catalog.PrebuiltArchiveFormatTarGzip,
			},
		}
	}
	extracted := &ExtractedTap{
		SchemaVersion: ExtractedTapSchemaVersion,
		Tap: catalog.TapSource{
			ID:            tap,
			Repository:    tap.DefaultGitHubRepository(),
			Commit:        strings.Repeat("c", 40),
			TreeDigest:    digest,
			ArchiveDigest: digest,
		},
		Formulae: []ExtractedFormula{{
			SourcePath:   "Formula/widget.rb",
			SourceDigest: digest,
			Platforms: []ExtractedPlatformFormula{
				platform("x86_64_linux", "amd64"),
				platform("arm64_linux", "arm64"),
			},
		}},
	}

	document, err := ToCatalog(extracted, fakeCore{})
	if err != nil {
		t.Fatal(err)
	}
	formula := document.Formulae[0]
	if formula.Bottle == nil || formula.PrebuiltArchive != nil {
		t.Fatalf("native bottle preference was not preserved: bottle=%+v prebuilt=%+v", formula.Bottle, formula.PrebuiltArchive)
	}
}

func TestToCatalogUsesPrebuiltOnlyForTagsWithoutNativeBottle(t *testing.T) {
	t.Parallel()
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	x86Source := &ExtractedStableSource{URL: "https://downloads.example.com/widget-amd64.tar.gz", SHA256: digest, ArchiveFormat: catalog.PrebuiltArchiveFormatTarGzip}
	armSource := &ExtractedStableSource{URL: "https://downloads.example.com/widget-arm64.tar.gz", SHA256: "sha256:" + strings.Repeat("b", 64), ArchiveFormat: catalog.PrebuiltArchiveFormatTarGzip}
	bottle := &ExtractedBottle{
		RootURL: "https://bottles.example.com/widget",
		Files: []catalog.BottleFile{{
			Tag:    "x86_64_linux",
			URL:    "https://bottles.example.com/widget-amd64.tar.gz",
			SHA256: "sha256:" + strings.Repeat("c", 64),
			Cellar: ":any",
		}},
	}
	extracted := &ExtractedTap{
		SchemaVersion: ExtractedTapSchemaVersion,
		Tap:           catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: strings.Repeat("c", 40), TreeDigest: digest, ArchiveDigest: digest},
		Formulae: []ExtractedFormula{{
			SourcePath:   "Formula/widget.rb",
			SourceDigest: digest,
			Platforms: []ExtractedPlatformFormula{
				{Tag: "x86_64_linux", Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1", Bottle: bottle, StableSource: x86Source},
				{Tag: "arm64_linux", Name: "widget", HomebrewFullName: "acme/tools/widget", StableVersion: "1", Bottle: bottle, StableSource: armSource},
			},
		}},
	}
	document, err := ToCatalog(extracted, fakeCore{})
	if err != nil {
		t.Fatal(err)
	}
	formula := document.Formulae[0]
	if formula.Bottle == nil || formula.PrebuiltArchive == nil {
		t.Fatalf("expected non-overlapping native and fallback declarations: %+v", formula)
	}
	if len(formula.PrebuiltArchive.Files) != 1 || formula.PrebuiltArchive.Files[0].Tag != "arm64_linux" {
		t.Fatalf("prebuilt fallback files = %+v", formula.PrebuiltArchive.Files)
	}
}

func TestToCatalogAllowsFormulaWithoutSupportedArtifact(t *testing.T) {
	t.Parallel()
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	extracted := &ExtractedTap{
		SchemaVersion: ExtractedTapSchemaVersion,
		Tap: catalog.TapSource{
			ID:            tap,
			Repository:    tap.DefaultGitHubRepository(),
			Commit:        strings.Repeat("c", 40),
			TreeDigest:    digest,
			ArchiveDigest: digest,
		},
		Formulae: []ExtractedFormula{{
			SourcePath:   "Formula/widget.rb",
			SourceDigest: digest,
			Platforms: []ExtractedPlatformFormula{{
				Tag:              "x86_64_linux",
				Name:             "widget",
				HomebrewFullName: "acme/tools/widget",
				StableVersion:    "1",
			}},
		}},
	}
	document, err := ToCatalog(extracted, fakeCore{})
	if err != nil {
		t.Fatal(err)
	}
	if document.Formulae[0].Bottle != nil || document.Formulae[0].PrebuiltArchive != nil {
		t.Fatalf("unsupported unselected Formula gained an artifact: %+v", document.Formulae[0])
	}
}

func TestToCatalogRejectsInvalidExtractedStableSource(t *testing.T) {
	t.Parallel()
	tap, _ := catalog.ParseTapID("acme/tools")
	digest := "sha256:" + strings.Repeat("a", 64)
	extracted := &ExtractedTap{
		SchemaVersion: ExtractedTapSchemaVersion,
		Tap: catalog.TapSource{
			ID:            tap,
			Repository:    tap.DefaultGitHubRepository(),
			Commit:        strings.Repeat("c", 40),
			TreeDigest:    digest,
			ArchiveDigest: digest,
		},
		Formulae: []ExtractedFormula{{
			SourcePath:   "Formula/widget.rb",
			SourceDigest: digest,
			Platforms: []ExtractedPlatformFormula{{
				Tag:              "x86_64_linux",
				Name:             "widget",
				HomebrewFullName: "acme/tools/widget",
				StableVersion:    "1",
				StableSource: &ExtractedStableSource{
					URL:           "https://downloads.example.com/widget.zip",
					SHA256:        digest,
					ArchiveFormat: catalog.PrebuiltArchiveFormatTarGzip,
				},
			}},
		}},
	}
	if _, err := ToCatalog(extracted, fakeCore{}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeExtractedTapRetainsStableSource(t *testing.T) {
	t.Parallel()
	data := []byte(`{
		"schema_version":"dalec-homebrew-extracted-tap/v1",
		"tap":{
			"id":"sozercan/repo",
			"repository":"https://github.com/sozercan/homebrew-repo",
			"commit":"cccccccccccccccccccccccccccccccccccccccc",
			"tree_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"archive_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		"formulae":[{
			"source_path":"Formula/a365.rb",
			"source_digest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			"platforms":[{
				"tag":"x86_64_linux",
				"name":"a365",
				"homebrew_full_name":"sozercan/repo/a365",
				"stable_version":"0.3.3",
				"stable_source":{
					"url":"https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz",
					"sha256":"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
					"archive_format":"tar+gzip"
				}
			}]
		}]
	}`)
	extracted, err := DecodeExtractedTap(data)
	if err != nil {
		t.Fatal(err)
	}
	stable := extracted.Formulae[0].Platforms[0].StableSource
	if stable == nil || stable.ArchiveFormat != catalog.PrebuiltArchiveFormatTarGzip || !strings.Contains(stable.URL, "linux_amd64.tar.gz") {
		t.Fatalf("stable source = %+v", stable)
	}
}
