package frontend

import (
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	hboci "github.com/sozercan/dalec-homebrew/internal/homebrew/oci"
)

func TestResolveNonCoreArtifactsBindsPrebuiltDeclaration(t *testing.T) {
	digest := func(value string) string { return "sha256:" + strings.Repeat(value, 64) }
	platform := catalog.Platform{OS: "linux", Architecture: "amd64"}
	declaration := catalog.PrebuiltArchiveDeclaration{Files: []catalog.PrebuiltArchiveFile{{Tag: "x86_64_linux", URL: "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz", SHA256: digest("9"), Format: catalog.PrebuiltArchiveFormatTarGzip}}}
	verification := catalog.BottleVerification{PolicyVersion: catalog.BottleVerificationPolicy, InventoryDigest: digest("4"), EntryCount: 2, ExpandedSize: 42}
	derivedDigest := digest("8")
	formulaDigest := digest("2")
	artifact := catalog.BottleArtifact{
		ID: "sozercan/repo/a365", Platform: platform, Tag: "x86_64_linux", Filename: "a365--0.3.3.x86_64_linux.bottle.tar.gz", SHA256: derivedDigest, Size: 40, Cellar: "/home/linuxbrew/.linuxbrew/Cellar",
		Tab: catalog.BottleTab{Receiptless: true, Arch: "x86_64"}, CurrentFormulaSourceDigest: formulaDigest, BottleFormulaSourceDigest: formulaDigest, ExecutablePaths: []string{"bin/a365"},
		Transport:    catalog.Transport{HTTPS: &catalog.HTTPSTransport{URL: "https://catalog.example.com/v1/artifacts/sha256/" + strings.TrimPrefix(derivedDigest, "sha256:"), ExpectedSize: 40, SHA256: derivedDigest, Filename: "a365--0.3.3.x86_64_linux.bottle.tar.gz", AllowedRedirectHosts: []string{"catalog.example.com"}, FetchPolicyVersion: catalog.HTTPSFetchPolicyVersion}},
		Verification: verification, Provenance: catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: catalog.PrebuiltProvenanceWaiver}},
		PrebuiltDerivation: &catalog.PrebuiltDerivation{
			PolicyVersion: "prebuilt-derived-bottle-v1", PolicyDigest: digest("5"),
			Source:          catalog.PrebuiltSourceArtifact{Filename: "a365_0.3.3_linux_amd64.tar.gz", Size: 30, SHA256: digest("9"), Format: catalog.PrebuiltArchiveFormatTarGzip, Transport: catalog.Transport{HTTPS: &catalog.HTTPSTransport{URL: declaration.Files[0].URL, ExpectedSize: 30, SHA256: digest("9"), Filename: "a365_0.3.3_linux_amd64.tar.gz", AllowedRedirectHosts: []string{"github.com", "release-assets.githubusercontent.com"}, FetchPolicyVersion: catalog.HTTPSFetchPolicyVersion}}},
			SourceInventory: catalog.PrebuiltSourceInventory{InventoryDigest: digest("6"), EntryCount: 3, ExpandedSize: 35},
			Payload:         catalog.PrebuiltPayloadEvidence{SourcePath: "a365", DestinationPath: "bin/a365", SHA256: digest("7"), Size: 25, ArchiveMode: 0o755, DerivedMode: 0o555},
			ELF:             catalog.PrebuiltELFEvidence{Format: catalog.PrebuiltELFFormatELF64, Machine: catalog.PrebuiltELFMachineX8664, StaticallyLinked: true, NeededLibraries: []string{}, RPaths: []string{}},
			FormulaSource:   catalog.PrebuiltFormulaSourceEvidence{Transport: catalog.TapFormulaSourceTransport{Tap: catalog.TapSource{ID: "sozercan/repo", Repository: "https://github.com/sozercan/homebrew-repo", Commit: strings.Repeat("c", 40), TreeDigest: digest("c"), ArchiveDigest: digest("d")}, Path: "Formula/a365.rb"}, SHA256: formulaDigest, Size: 20},
			RecipeDigest:    digest("e"), DerivedBottle: catalog.PrebuiltDerivedBottleRelation{Tag: "x86_64_linux", Filename: "a365--0.3.3.x86_64_linux.bottle.tar.gz", SHA256: derivedDigest, Size: 40, Verification: verification, FormulaSourceDigest: formulaDigest},
		},
	}
	client, err := hboci.NewClient("https://ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	catalogs := map[catalog.TapID]*catalog.TapCatalog{"sozercan/repo": {Formulae: []catalog.Formula{{ID: artifact.ID, PrebuiltArchive: &declaration}}}}
	result := catalog.PlatformResult{Platform: platform, Artifacts: []catalog.BottleArtifact{artifact}}
	if _, err := ResolveNonCoreOCIArtifacts(t.Context(), client, nil, result, catalogs); err != nil {
		t.Fatal(err)
	}

	tampered := result
	tampered.Artifacts = append([]catalog.BottleArtifact(nil), result.Artifacts...)
	copyArtifact := tampered.Artifacts[0]
	copyDerivation := *copyArtifact.PrebuiltDerivation
	copyDerivation.Source.SHA256 = digest("1")
	copyArtifact.PrebuiltDerivation = &copyDerivation
	tampered.Artifacts[0] = copyArtifact
	if _, err := ResolveNonCoreOCIArtifacts(t.Context(), client, nil, tampered, catalogs); err == nil {
		t.Fatal("prebuilt source substitution accepted")
	}
}
