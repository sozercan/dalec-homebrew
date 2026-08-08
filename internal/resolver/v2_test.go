package resolver

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestCoreGeneratedAtSource(t *testing.T) {
	base := time.Unix(1_800_000_000, 0).UTC()
	tests := []struct {
		name       string
		formula    metadata.DocumentInfo
		migrations metadata.DocumentInfo
		generated  time.Time
		want       string
	}{
		{
			name:       "older HTTP observation",
			formula:    metadata.DocumentInfo{GeneratedAt: base, GeneratedAtSource: metadata.GeneratedAtLastModified},
			migrations: metadata.DocumentInfo{GeneratedAt: base.Add(time.Minute), GeneratedAtSource: metadata.GeneratedAtSignedPayload},
			generated:  base,
			want:       resolution.CoreGeneratedAtLastModified,
		},
		{
			name:       "later HTTP observation still makes aggregate transport-dependent",
			formula:    metadata.DocumentInfo{GeneratedAt: base, GeneratedAtSource: metadata.GeneratedAtSignedPayload},
			migrations: metadata.DocumentInfo{GeneratedAt: base.Add(time.Minute), GeneratedAtSource: metadata.GeneratedAtLastModified},
			generated:  base,
			want:       resolution.CoreGeneratedAtLastModified,
		},
		{
			name:       "equal mixed timestamps use HTTP trust",
			formula:    metadata.DocumentInfo{GeneratedAt: base, GeneratedAtSource: metadata.GeneratedAtLastModified},
			migrations: metadata.DocumentInfo{GeneratedAt: base, GeneratedAtSource: metadata.GeneratedAtSignedPayload},
			generated:  base,
			want:       resolution.CoreGeneratedAtLastModified,
		},
		{
			name:       "both timestamps signed",
			formula:    metadata.DocumentInfo{GeneratedAt: base, GeneratedAtSource: metadata.GeneratedAtSignedPayload},
			migrations: metadata.DocumentInfo{GeneratedAt: base.Add(time.Minute), GeneratedAtSource: metadata.GeneratedAtSignedPayload},
			generated:  base,
			want:       resolution.CoreGeneratedAtSignedPayload,
		},
		{
			name:       "missing source",
			formula:    metadata.DocumentInfo{GeneratedAt: base},
			migrations: metadata.DocumentInfo{GeneratedAt: base.Add(time.Minute), GeneratedAtSource: metadata.GeneratedAtSignedPayload},
			generated:  base,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coreGeneratedAtSource(metadata.SnapshotInfo{
				GeneratedAt: tt.generated,
				Formula:     tt.formula,
				Migrations:  tt.migrations,
			})
			if got != tt.want {
				t.Fatalf("coreGeneratedAtSource() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConvertCatalogPrebuiltDerivationV2CopiesEverySignedField(t *testing.T) {
	digest := func(ch string) string { return "sha256:" + strings.Repeat(ch, 64) }
	input := catalog.PrebuiltDerivation{
		PolicyVersion: "prebuilt-derived-bottle-v1",
		PolicyDigest:  digest("1"),
		Source: catalog.PrebuiltSourceArtifact{
			Filename: "a365_0.3.3_linux_amd64.tar.gz", Size: 11, SHA256: digest("2"), Format: "tar+gzip",
			Transport: catalog.Transport{HTTPS: &catalog.HTTPSTransport{URL: "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz", ExpectedSize: 11, SHA256: digest("2"), Filename: "a365_0.3.3_linux_amd64.tar.gz", AllowedRedirectHosts: []string{"release-assets.githubusercontent.com", "github.com"}, FetchPolicyVersion: resolution.HTTPSFetchPolicyVersionV1}},
		},
		SourceInventory: catalog.PrebuiltSourceInventory{InventoryDigest: digest("3"), EntryCount: 3, ExpandedSize: 20},
		Payload:         catalog.PrebuiltPayloadEvidence{SourcePath: "a365", DestinationPath: "bin/a365", SHA256: digest("4"), Size: 12, ArchiveMode: 0o755, DerivedMode: 0o555},
		ELF:             catalog.PrebuiltELFEvidence{Format: "elf64", Machine: "x86_64", StaticallyLinked: true, NeededLibraries: []string{}, RPaths: []string{}},
		FormulaSource: catalog.PrebuiltFormulaSourceEvidence{
			Transport: catalog.TapFormulaSourceTransport{Tap: catalog.TapSource{ID: "sozercan/repo", Repository: "https://github.com/sozercan/homebrew-repo", Commit: strings.Repeat("a", 40), TreeDigest: digest("5"), ArchiveDigest: digest("6")}, Path: "Formula/a365.rb"},
			SHA256:    digest("7"), Size: 123,
		},
		RecipeDigest:  digest("8"),
		DerivedBottle: catalog.PrebuiltDerivedBottleRelation{Tag: "x86_64_linux", Filename: "a365--0.3.3.x86_64_linux.bottle.tar.gz", SHA256: digest("9"), Size: 13, Verification: catalog.BottleVerification{PolicyVersion: resolution.BottleVerificationPolicyV1, InventoryDigest: "sha256:" + strings.Repeat("a", 64), EntryCount: 2, ExpandedSize: 14}, FormulaSourceDigest: digest("7")},
	}
	want := &resolution.PrebuiltDerivationV2{
		PolicyVersion: input.PolicyVersion, PolicyDigest: input.PolicyDigest,
		Source:          resolution.PrebuiltSourceArtifactV2{Filename: input.Source.Filename, Size: input.Source.Size, SHA256: input.Source.SHA256, Format: input.Source.Format, Transport: resolution.BottleTransport{HTTPS: &resolution.HTTPSTransport{URL: input.Source.Transport.HTTPS.URL, ExpectedSize: input.Source.Transport.HTTPS.ExpectedSize, SHA256: input.Source.Transport.HTTPS.SHA256, Filename: input.Source.Transport.HTTPS.Filename, AllowedRedirectHosts: []string{"release-assets.githubusercontent.com", "github.com"}, FetchPolicyVersion: input.Source.Transport.HTTPS.FetchPolicyVersion}}},
		SourceInventory: resolution.PrebuiltSourceInventoryV2{InventoryDigest: input.SourceInventory.InventoryDigest, EntryCount: input.SourceInventory.EntryCount, ExpandedSize: input.SourceInventory.ExpandedSize},
		Payload:         resolution.PrebuiltPayloadEvidenceV2{SourcePath: input.Payload.SourcePath, DestinationPath: input.Payload.DestinationPath, SHA256: input.Payload.SHA256, Size: input.Payload.Size, ArchiveMode: input.Payload.ArchiveMode, DerivedMode: input.Payload.DerivedMode},
		ELF:             resolution.PrebuiltELFEvidenceV2{Format: input.ELF.Format, Machine: input.ELF.Machine, StaticallyLinked: true, NeededLibraries: []string{}, RPaths: []string{}},
		FormulaSource:   resolution.PrebuiltFormulaSourceEvidenceV2{Transport: resolution.TapFormulaSourceTransportV2{Tap: resolution.TapSourceV2{ID: "sozercan/repo", Repository: input.FormulaSource.Transport.Tap.Repository, Commit: input.FormulaSource.Transport.Tap.Commit, TreeDigest: input.FormulaSource.Transport.Tap.TreeDigest, ArchiveDigest: input.FormulaSource.Transport.Tap.ArchiveDigest}, Path: input.FormulaSource.Transport.Path}, SHA256: input.FormulaSource.SHA256, Size: input.FormulaSource.Size},
		RecipeDigest:    input.RecipeDigest,
		DerivedBottle:   resolution.PrebuiltDerivedBottleRelationV2{Tag: input.DerivedBottle.Tag, Filename: input.DerivedBottle.Filename, SHA256: input.DerivedBottle.SHA256, Size: input.DerivedBottle.Size, Verification: resolution.BottleVerificationV2{PolicyVersion: input.DerivedBottle.Verification.PolicyVersion, InventoryDigest: input.DerivedBottle.Verification.InventoryDigest, EntryCount: input.DerivedBottle.Verification.EntryCount, ExpandedSize: input.DerivedBottle.Verification.ExpandedSize}, FormulaSourceDigest: input.DerivedBottle.FormulaSourceDigest},
	}
	got := convertCatalogPrebuiltDerivationV2(input)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("converted derivation = %#v, want %#v", got, want)
	}
	input.Source.Transport.HTTPS.AllowedRedirectHosts[0] = "mutated.example.com"
	if got.Source.Transport.HTTPS.AllowedRedirectHosts[0] == "mutated.example.com" {
		t.Fatal("conversion retained mutable catalog slice storage")
	}
}

func TestConvertCatalogNodeV2UsesDedicatedPrebuiltWaiver(t *testing.T) {
	node := catalog.Node{ID: "sozercan/repo/a365", Tap: "sozercan/repo", Name: "a365", HomebrewFullName: "sozercan/repo/a365", FormulaVersion: "0.3.3", PkgVersion: "0.3.3"}
	artifact := catalog.BottleArtifact{ID: node.ID, PrebuiltDerivation: &catalog.PrebuiltDerivation{}, Transport: catalog.Transport{Local: &catalog.LocalTransport{PolicyVersion: catalog.BuildLocalArtifactPolicyVersion, SHA256: "sha256:" + strings.Repeat("a", 64), Size: 1, Filename: "a365.tgz"}}, Provenance: catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: catalog.PrebuiltProvenanceWaiver}}}
	got, err := convertCatalogNodeV2(node, artifact, map[catalog.FormulaID]catalog.Node{node.ID: node})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance.Waiver == nil || got.Provenance.Waiver.Policy != resolution.PrebuiltProvenanceWaiverPolicyV1 {
		t.Fatalf("prebuilt waiver = %+v", got.Provenance)
	}
	if got.Bottle.PrebuiltDerivation == nil {
		t.Fatal("prebuilt derivation was dropped")
	}
	if got.Bottle.Transport.Local == nil || got.Bottle.Transport.Local.PolicyVersion != catalog.BuildLocalArtifactPolicyVersion {
		t.Fatalf("build-local transport was dropped: %+v", got.Bottle.Transport)
	}
}
