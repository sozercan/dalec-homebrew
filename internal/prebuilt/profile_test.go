package prebuilt

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestProfileValidationRejectsUnsafeOrAmbiguousPolicy(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	source := makeSourceArchive(t, baseEntries(payload))
	base := profileFor(source, formula, "amd64")
	tests := []struct {
		name   string
		mutate func(*Profile)
	}{
		{name: "policy empty", mutate: func(profile *Profile) { profile.PolicyVersion = "" }},
		{name: "policy uppercase", mutate: func(profile *Profile) { profile.PolicyVersion = "Policy/V1" }},
		{name: "Formula uppercase", mutate: func(profile *Profile) { profile.Name = "A365" }},
		{name: "package traversal", mutate: func(profile *Profile) { profile.PkgVersion = ".." }},
		{name: "package slash", mutate: func(profile *Profile) { profile.PkgVersion = "0/3" }},
		{name: "target OS", mutate: func(profile *Profile) { profile.Target.OS = "darwin" }},
		{name: "target architecture", mutate: func(profile *Profile) { profile.Target.Arch = "386" }},
		{name: "source size zero", mutate: func(profile *Profile) { profile.Source.Size = 0 }},
		{name: "source digest algorithm", mutate: func(profile *Profile) { profile.Source.SHA256 = "sha512:" + strings.Repeat("0", 64) }},
		{name: "source digest uppercase", mutate: func(profile *Profile) { profile.Source.SHA256 = "sha256:" + strings.Repeat("A", 64) }},
		{name: "Formula digest", mutate: func(profile *Profile) { profile.FormulaSHA256 = "sha256:bad" }},
		{name: "negative epoch", mutate: func(profile *Profile) { profile.SourceDateEpoch = -1 }},
		{name: "overflow epoch", mutate: func(profile *Profile) { profile.SourceDateEpoch = math.MaxUint32 + 1 }},
		{name: "no entries", mutate: func(profile *Profile) { profile.Entries = nil }},
		{name: "duplicate entry", mutate: func(profile *Profile) { profile.Entries = append(profile.Entries, profile.Entries[0]) }},
		{name: "case-fold collision", mutate: func(profile *Profile) {
			profile.Entries = append(profile.Entries, EntryProfile{Path: "license", Mode: 0o644})
		}},
		{name: "entry traversal", mutate: func(profile *Profile) { profile.Entries[0].Path = "../a365" }},
		{name: "entry backslash", mutate: func(profile *Profile) { profile.Entries[0].Path = `dir\a365` }},
		{name: "entry space", mutate: func(profile *Profile) { profile.Entries[0].Path = "a 365" }},
		{name: "entry non-ASCII", mutate: func(profile *Profile) { profile.Entries[0].Path = "å365" }},
		{name: "entry zero mode", mutate: func(profile *Profile) { profile.Entries[0].Mode = 0 }},
		{name: "entry setid mode", mutate: func(profile *Profile) { profile.Entries[0].Mode = 0o4755 }},
		{name: "payload absent", mutate: func(profile *Profile) { profile.PayloadPath = "other" }},
		{name: "payload non-executable", mutate: func(profile *Profile) { setEntryMode(profile, "a365", 0o644) }},
		{name: "module empty", mutate: func(profile *Profile) { profile.GoBuild.ModulePath = "" }},
		{name: "module traversal", mutate: func(profile *Profile) { profile.GoBuild.ModulePath = "example.com/../tool" }},
		{name: "module colon", mutate: func(profile *Profile) { profile.GoBuild.ModulePath = "https://example.com/tool" }},
		{name: "negative limit", mutate: func(profile *Profile) { profile.Limits.MaxExpandedBytes = -1 }},
		{name: "file exceeds expanded", mutate: func(profile *Profile) { profile.Limits.MaxExpandedBytes = 1024; profile.Limits.MaxFileBytes = 2048 }},
		{name: "tar padding too small", mutate: func(profile *Profile) { profile.Limits.MaxTarPaddingBytes = 512 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := base
			profile.Entries = append([]EntryProfile(nil), base.Entries...)
			test.mutate(&profile)
			_, err := CanonicalProfile(profile)
			requireErrorCode(t, err, CodeInvalidProfile)
		})
	}
}

func TestCanonicalInventoryRejectsMalformedOrDuplicateEntries(t *testing.T) {
	valid := InventoryEntry{Path: "a365", Mode: 0o555, Size: 1, SHA256: digestBytes([]byte("x"))}
	tests := []struct {
		name    string
		entries []InventoryEntry
	}{
		{name: "duplicate", entries: []InventoryEntry{valid, valid}},
		{name: "path", entries: []InventoryEntry{{Path: "../a365", Mode: valid.Mode, Size: valid.Size, SHA256: valid.SHA256}}},
		{name: "mode", entries: []InventoryEntry{{Path: valid.Path, Mode: 0o4755, Size: valid.Size, SHA256: valid.SHA256}}},
		{name: "size", entries: []InventoryEntry{{Path: valid.Path, Mode: valid.Mode, Size: -1, SHA256: valid.SHA256}}},
		{name: "digest", entries: []InventoryEntry{{Path: valid.Path, Mode: valid.Mode, Size: valid.Size, SHA256: "sha256:bad"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CanonicalInventory(test.entries); err == nil {
				t.Fatal("expected canonical inventory error")
			}
		})
	}
}

func TestDeriveRejectsNilReaderAndBoundedOutput(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	source := makeSourceArchive(t, baseEntries(payload))
	profile := profileFor(source, formula, "amd64")

	_, err := Derive(nil, formula, profile)
	requireErrorCode(t, err, CodeInvalidGzip)

	profile.Limits.MaxBottleBytes = 1
	_, err = Derive(bytes.NewReader(source), formula, profile)
	requireErrorCode(t, err, CodeDerivation)
}

func TestCanonicalEvidenceRejectsUnknownSchemaAndDuplicateInventory(t *testing.T) {
	evidence := Evidence{SchemaVersion: "unknown"}
	if _, err := CanonicalEvidence(evidence); err == nil {
		t.Fatal("expected unknown schema rejection")
	}
	evidence = Evidence{
		SchemaVersion: EvidenceSchemaVersion,
		Source: SourceEvidence{Inventory: []InventoryEntry{
			{Path: "a", Mode: 0o444, Size: 1, SHA256: digestBytes([]byte("a"))},
			{Path: "a", Mode: 0o444, Size: 1, SHA256: digestBytes([]byte("a"))},
		}},
		Derivation: DerivationEvidence{Inventory: []InventoryEntry{}},
	}
	if _, err := CanonicalEvidence(evidence); err == nil {
		t.Fatal("expected duplicate source inventory rejection")
	}
}

func setEntryMode(profile *Profile, entryPath string, mode uint32) {
	for index := range profile.Entries {
		if profile.Entries[index].Path == entryPath {
			profile.Entries[index].Mode = mode
			return
		}
	}
}
