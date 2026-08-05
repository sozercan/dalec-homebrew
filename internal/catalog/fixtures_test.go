package catalog

import (
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
)

func testDigest(char byte) string {
	return "sha256:" + strings.Repeat(string(char), 64)
}

func testTime() time.Time {
	return time.Date(2026, time.August, 3, 12, 0, 0, 123456789, time.FixedZone("fixture", -7*60*60))
}

func validRequest() *Request {
	return &Request{
		SchemaVersion: RequestSchemaVersion,
		Targets: []PlatformRequest{{
			Platform:      Platform{OS: "linux", Architecture: "amd64"},
			ExternalRoots: []FormulaID{"acme/tools/widget"},
		}},
		HomebrewCommit:     strings.Repeat("a", 40),
		CoreSnapshotDigest: testDigest('a'),
	}
}

func validTapCatalog() *TapCatalog {
	tap := TapSource{
		ID:            "acme/tools",
		Repository:    "https://github.com/acme/homebrew-tools",
		Commit:        strings.Repeat("b", 40),
		TreeDigest:    testDigest('b'),
		ArchiveDigest: testDigest('c'),
	}
	bottle := func(name string, char byte) *BottleDeclaration {
		return &BottleDeclaration{
			RootURL: "https://bottles.example.com/" + name,
			Rebuild: 1,
			Files: []BottleFile{
				{Tag: "x86_64_linux", URL: "https://bottles.example.com/" + name + "-x86.tar.gz", SHA256: testDigest(char), Cellar: "/home/linuxbrew/.linuxbrew/Cellar"},
				{Tag: "arm64_linux", URL: "https://bottles.example.com/" + name + "-arm64.tar.gz", SHA256: testDigest(char + 1), Cellar: "any_skip_relocation"},
			},
		}
	}
	return &TapCatalog{
		SchemaVersion: TapCatalogSchemaVersion,
		Tap:           tap,
		PublishedAt:   testTime(),
		Sequence:      7,
		Formulae: []Formula{
			{
				ID:               "acme/tools/widget@1",
				Name:             "widget@1",
				HomebrewFullName: "acme/tools/widget@1",
				SourcePath:       "Formula/widget@1.rb",
				SourceDigest:     testDigest('f'),
				StableVersion:    "1.9",
				Bottle:           bottle("widget-at-1", '7'),
			},
			{
				ID:               "acme/tools/helper",
				Name:             "helper",
				HomebrewFullName: "acme/tools/helper",
				SourcePath:       "Formula/helper.rb",
				SourceDigest:     testDigest('e'),
				StableVersion:    "3.0",
				Bottle:           bottle("helper", '5'),
			},
			{
				ID:               "acme/tools/widget",
				Name:             "widget",
				HomebrewFullName: "acme/tools/widget",
				SourcePath:       "Formula/widget.rb",
				SourceDigest:     testDigest('d'),
				StableVersion:    "2.0",
				Revision:         1,
				VersionScheme:    2,
				License:          "MIT",
				Dependencies: []Dependency{
					{Raw: "helper", ID: "acme/tools/helper"},
					{Raw: "zlib", ID: "homebrew/core/zlib"},
				},
				Variations: []FormulaVariation{
					{Tag: "x86_64_linux", Dependencies: []Dependency{{Raw: "zlib", ID: "homebrew/core/zlib"}}, OverridesDependencies: true, KegOnly: true, OverridesKegOnly: true},
					{Tag: "arm64_linux", OverridesDependencies: true},
				},
				VersionedFormulae: []FormulaID{"acme/tools/widget@1"},
				Bottle:            bottle("widget", '3'),
			},
		},
		Aliases: []ScopedMapping{
			{From: "acme/tools/widget-current", To: "acme/tools/widget"},
			{From: "acme/tools/widget-latest", To: "acme/tools/widget"},
		},
		Renames: []ScopedMapping{
			{From: "acme/tools/old-widget", To: "acme/tools/widget"},
			{From: "acme/tools/old-helper", To: "acme/tools/helper"},
		},
		Migrations: []Migration{
			{From: "acme/tools/legacy", RawTarget: "other/utils/new-widget", To: "other/utils/new-widget"},
			{From: "acme/tools/legacy-helper", RawTarget: "homebrew/core/zlib", To: "homebrew/core/zlib"},
		},
	}
}

func validClosure() ClosureResult {
	return ClosureResult{
		Requested: []FormulaID{"acme/tools/widget"},
		Nodes: []Node{
			{
				ID:               "acme/tools/widget",
				Tap:              "acme/tools",
				Name:             "widget",
				HomebrewFullName: "acme/tools/widget",
				FormulaVersion:   "2.0",
				FormulaRevision:  1,
				PkgVersion:       "2.0_1",
				VersionScheme:    2,
				BottleRebuild:    1,
				License:          "MIT",
				Dependencies: []Requirement{{
					Raw:                  "zlib",
					ID:                   "homebrew/core/zlib",
					MinimumPkgVersion:    "1.3.1",
					MinimumRevision:      0,
					MinimumBottleRebuild: 0,
					DeclaredDirectly:     true,
				}},
			},
			{
				ID:               "homebrew/core/zlib",
				Tap:              "homebrew/core",
				Name:             "zlib",
				HomebrewFullName: "homebrew/core/zlib",
				FormulaVersion:   "1.3.1",
				PkgVersion:       "1.3.1",
				License:          "Zlib",
			},
		},
		InstallOrder: []FormulaID{"homebrew/core/zlib", "acme/tools/widget"},
	}
}

func validHTTPSArtifact() BottleArtifact {
	platform := Platform{OS: "linux", Architecture: "amd64"}
	return BottleArtifact{
		ID:                         "acme/tools/widget",
		Platform:                   platform,
		Tag:                        "x86_64_linux",
		Filename:                   "widget--2.0_1.x86_64_linux.bottle.tar.gz",
		SHA256:                     testDigest('8'),
		Size:                       100,
		Cellar:                     "/home/linuxbrew/.linuxbrew/Cellar",
		Tab:                        BottleTab{Arch: "x86_64"},
		CurrentFormulaSourceDigest: testDigest('d'),
		BottleFormulaSourceDigest:  testDigest('9'),
		BottleSourceWaiver:         HTTPSBottleSourceWaiver,
		Transport: Transport{HTTPS: &HTTPSTransport{
			URL:                  "https://bottles.example.com/widget.tar.gz?download=1",
			ExpectedSize:         100,
			SHA256:               testDigest('8'),
			Filename:             "widget--2.0_1.x86_64_linux.bottle.tar.gz",
			AllowedRedirectHosts: []string{"objects.example.com", "bottles.example.com"},
			FetchPolicyVersion:   HTTPSFetchPolicyVersion,
		}},
		Verification: BottleVerification{
			PolicyVersion:   BottleVerificationPolicy,
			InventoryDigest: testDigest('1'),
			EntryCount:      8,
			ExpandedSize:    4096,
		},
		Provenance: Provenance{Waiver: &ProvenanceWaiver{Policy: ChecksumProvenanceWaiver}},
	}
}

func validOCIArtifact() BottleArtifact {
	platform := Platform{OS: "linux", Architecture: "amd64"}
	descriptor := func(char byte, size int64, mediaType string) Descriptor {
		return Descriptor{Digest: testDigest(char), Size: size, MediaType: mediaType}
	}
	manifest := descriptor('c', 40, "application/vnd.oci.image.manifest.v1+json")
	manifest.Platform = &platform
	manifest.Annotations = []Annotation{{Key: "org.opencontainers.image.source", Value: "https://github.com/homebrew/homebrew-core"}, {Key: "org.opencontainers.image.version", Value: "1.3.1"}}
	return BottleArtifact{
		ID:                         "homebrew/core/zlib",
		Platform:                   platform,
		Tag:                        "x86_64_linux",
		Filename:                   "zlib--1.3.1.x86_64_linux.bottle.tar.gz",
		SHA256:                     testDigest('f'),
		Size:                       100,
		Cellar:                     "/home/linuxbrew/.linuxbrew/Cellar",
		Tab:                        BottleTab{Arch: "x86_64"},
		CurrentFormulaSourceDigest: testDigest('2'),
		BottleFormulaSourceDigest:  testDigest('3'),
		BottleSourceRepository:     "https://github.com/homebrew/homebrew-core",
		BottleSourceCommit:         strings.Repeat("1", 40),
		BottleFormulaPath:          "Formula/z/zlib.rb",
		Transport: Transport{OCI: &OCITransport{
			Registry:   "ghcr.io",
			Repository: "homebrew/core/zlib",
			Index:      descriptor('a', 20, "application/vnd.oci.image.index.v1+json"),
			Manifest:   manifest,
			Config:     descriptor('d', 30, "application/vnd.oci.image.config.v1+json"),
			Layer:      descriptor('f', 100, "application/vnd.oci.image.layer.v1.tar+gzip"),
		}},
		Verification: BottleVerification{
			PolicyVersion:   BottleVerificationPolicy,
			InventoryDigest: testDigest('4'),
			EntryCount:      4,
			ExpandedSize:    2048,
		},
		Provenance: Provenance{Waiver: &ProvenanceWaiver{Policy: ChecksumProvenanceWaiver}},
	}
}

func validPlatformResult() PlatformResult {
	return PlatformResult{
		Platform:  Platform{OS: "linux", Architecture: "amd64"},
		Closure:   validClosure(),
		Artifacts: []BottleArtifact{validHTTPSArtifact(), validOCIArtifact()},
	}
}

func catalogReferenceFor(t *testing.T, catalog *TapCatalog) CatalogReference {
	t.Helper()
	data, err := CanonicalTapCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	d := digest.FromBytes(data).String()
	return CatalogReference{
		Tap:         catalog.Tap,
		PublishedAt: catalog.PublishedAt,
		Sequence:    catalog.Sequence,
		URL:         "https://catalog.example.com" + CatalogDocumentPathPrefix + strings.TrimPrefix(d, "sha256:"),
		Size:        int64(len(data)),
		SHA256:      d,
	}
}

func catalogReferenceForBytes(catalog *TapCatalog, data []byte) CatalogReference {
	d := digest.FromBytes(data).String()
	return CatalogReference{
		Tap:         catalog.Tap,
		PublishedAt: catalog.PublishedAt,
		Sequence:    catalog.Sequence,
		URL:         "https://catalog.example.com" + CatalogDocumentPathPrefix + strings.TrimPrefix(d, "sha256:"),
		Size:        int64(len(data)),
		SHA256:      d,
	}
}

func validPayload(t *testing.T) (*CatalogSetPayload, *Request, *TapCatalog) {
	t.Helper()
	request := validRequest()
	requestDigest, err := RequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	catalog := validTapCatalog()
	payload := &CatalogSetPayload{
		SchemaVersion:      CatalogSetSchemaVersion,
		RequestDigest:      requestDigest.String(),
		CoreSnapshotDigest: request.CoreSnapshotDigest,
		GeneratedAt:        testTime(),
		ExpiresAt:          testTime().Add(24 * time.Hour),
		CatalogService:     ComponentIdentity{Name: "catalog-service", Version: "v2.0.0", Digest: testDigest('5')},
		Extractor:          ComponentIdentity{Name: "catalog-extractor", Version: "v2.0.0", Digest: testDigest('6')},
		Catalogs:           []CatalogReference{catalogReferenceFor(t, catalog)},
		Results:            []PlatformResult{validPlatformResult()},
	}
	return payload, request, catalog
}

func validResult() *CatalogSetResult {
	return &CatalogSetResult{
		SchemaVersion: ResultSchemaVersion,
		RequestDigest: testDigest('a'),
		PayloadDigest: testDigest('b'),
		JWS:           []byte(`{"payload":"{}","protected":"e30","signature":"sig"}`),
	}
}

func cloneForTest[T any](t *testing.T, value T) T {
	t.Helper()
	clone, err := cloneValue(value)
	if err != nil {
		t.Fatal(err)
	}
	return clone
}

func validPrebuiltArchiveDeclaration() PrebuiltArchiveDeclaration {
	return PrebuiltArchiveDeclaration{Files: []PrebuiltArchiveFile{
		{
			Tag:    "x86_64_linux",
			URL:    "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz",
			SHA256: testDigest('9'),
			Format: PrebuiltArchiveFormatTarGzip,
		},
		{
			Tag:    "arm64_linux",
			URL:    "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_arm64.tar.gz",
			SHA256: testDigest('a'),
			Format: PrebuiltArchiveFormatTarGzip,
		},
	}}
}

func validPrebuiltArtifact() BottleArtifact {
	platform := Platform{OS: "linux", Architecture: "amd64"}
	formulaDigest := testDigest('2')
	verification := BottleVerification{
		PolicyVersion:   BottleVerificationPolicy,
		InventoryDigest: testDigest('4'),
		EntryCount:      3,
		ExpandedSize:    12_000_000,
	}
	derivedFilename := "a365--0.3.3.x86_64_linux.bottle.tar.gz"
	derivedDigest := testDigest('8')
	derivedSize := int64(8_000_000)
	return BottleArtifact{
		ID:                         "sozercan/repo/a365",
		Platform:                   platform,
		Tag:                        "x86_64_linux",
		Filename:                   derivedFilename,
		SHA256:                     derivedDigest,
		Size:                       derivedSize,
		Cellar:                     "/home/linuxbrew/.linuxbrew/Cellar",
		Tab:                        BottleTab{Receiptless: true, Arch: "x86_64"},
		CurrentFormulaSourceDigest: formulaDigest,
		BottleFormulaSourceDigest:  formulaDigest,
		ExecutablePaths:            []string{"bin/a365"},
		Transport: Transport{HTTPS: &HTTPSTransport{
			URL:                  "https://catalog.example.com/v1/artifacts/sha256/" + strings.TrimPrefix(derivedDigest, "sha256:"),
			ExpectedSize:         derivedSize,
			SHA256:               derivedDigest,
			Filename:             derivedFilename,
			AllowedRedirectHosts: []string{"catalog.example.com"},
			FetchPolicyVersion:   HTTPSFetchPolicyVersion,
		}},
		Verification: verification,
		Provenance:   Provenance{Waiver: &ProvenanceWaiver{Policy: PrebuiltProvenanceWaiver}},
		PrebuiltDerivation: &PrebuiltDerivation{
			PolicyVersion: "single-static-elf-archive-v1",
			PolicyDigest:  testDigest('5'),
			Source: PrebuiltSourceArtifact{
				Filename: "a365_0.3.3_linux_amd64.tar.gz",
				Size:     7_000_000,
				SHA256:   testDigest('9'),
				Format:   PrebuiltArchiveFormatTarGzip,
				Transport: Transport{HTTPS: &HTTPSTransport{
					URL:                  "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz",
					ExpectedSize:         7_000_000,
					SHA256:               testDigest('9'),
					Filename:             "a365_0.3.3_linux_amd64.tar.gz",
					AllowedRedirectHosts: []string{"release-assets.githubusercontent.com", "github.com"},
					FetchPolicyVersion:   HTTPSFetchPolicyVersion,
				}},
			},
			SourceInventory: PrebuiltSourceInventory{
				InventoryDigest: testDigest('6'),
				EntryCount:      3,
				ExpandedSize:    15_000_000,
			},
			Payload: PrebuiltPayloadEvidence{
				SourcePath:      "a365",
				DestinationPath: "bin/a365",
				SHA256:          testDigest('7'),
				Size:            14_000_000,
				ArchiveMode:     0o755,
				DerivedMode:     0o555,
			},
			ELF: PrebuiltELFEvidence{
				Format:           PrebuiltELFFormatELF64,
				Machine:          PrebuiltELFMachineX8664,
				StaticallyLinked: true,
				NeededLibraries:  []string{},
				RPaths:           []string{},
			},
			FormulaSource: PrebuiltFormulaSourceEvidence{
				Transport: TapFormulaSourceTransport{
					Tap: TapSource{
						ID:            "sozercan/repo",
						Repository:    "https://github.com/sozercan/homebrew-repo",
						Commit:        strings.Repeat("c", 40),
						TreeDigest:    testDigest('c'),
						ArchiveDigest: testDigest('d'),
					},
					Path: "Formula/a365.rb",
				},
				SHA256: formulaDigest,
				Size:   4096,
			},
			RecipeDigest: testDigest('e'),
			DerivedBottle: PrebuiltDerivedBottleRelation{
				Tag:                 "x86_64_linux",
				Filename:            derivedFilename,
				SHA256:              derivedDigest,
				Size:                derivedSize,
				Verification:        verification,
				FormulaSourceDigest: formulaDigest,
			},
		},
	}
}
