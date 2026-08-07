package prebuilt

import (
	"archive/tar"
	"bytes"
	"encoding/hex"
	"reflect"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
)

func TestDerivePositiveBothArchitectures(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			payload := goELFFixture(t, arch)
			formula := fixtureFormula()
			source := makeSourceArchive(t, baseEntries(payload))
			profile := profileFor(source, formula, arch)

			result, err := Derive(bytes.NewReader(source), formula, profile)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Bottle) == 0 {
				t.Fatal("derived bottle is empty")
			}
			evidence := result.Evidence
			if evidence.SchemaVersion != EvidenceSchemaVersion || evidence.PolicyVersion != profile.PolicyVersion {
				t.Fatalf("unexpected evidence identity: %+v", evidence)
			}
			canonicalProfile, err := CanonicalProfile(profile)
			if err != nil {
				t.Fatal(err)
			}
			if evidence.ProfileSHA256 != digestBytes(canonicalProfile) {
				t.Fatalf("profile digest %s does not match canonical profile", evidence.ProfileSHA256)
			}
			if evidence.Source.SHA256 != profile.Source.SHA256 || evidence.Source.Size != profile.Source.Size {
				t.Fatalf("source evidence mismatch: %+v", evidence.Source)
			}
			if got, want := evidence.Source.PayloadSHA256, digestBytes(payload); got != want {
				t.Fatalf("payload digest = %s, want %s", got, want)
			}
			if evidence.ELF.Class != "ELFCLASS64" || evidence.ELF.Data != "ELFDATA2LSB" {
				t.Fatalf("unexpected ELF evidence: %+v", evidence.ELF)
			}
			if evidence.ELF.Interpreter != "" || len(evidence.ELF.ImportedLibraries) != 0 || evidence.ELF.WritableExecutableSegments != 0 {
				t.Fatalf("payload is not recorded as static: %+v", evidence.ELF)
			}
			if arch == "amd64" && evidence.ELF.Machine != "EM_X86_64" {
				t.Fatalf("amd64 machine = %q", evidence.ELF.Machine)
			}
			if arch == "arm64" && evidence.ELF.Machine != "EM_AARCH64" {
				t.Fatalf("arm64 machine = %q", evidence.ELF.Machine)
			}
			if evidence.GoBuild.ModulePath != fixtureModule || evidence.GoBuild.GOOS != "linux" || evidence.GoBuild.GOARCH != arch || evidence.GoBuild.CGOEnabled {
				t.Fatalf("unexpected Go build evidence: %+v", evidence.GoBuild)
			}
			if !evidence.Derivation.Receiptless || evidence.Derivation.PolicyVersion != DerivationPolicyVersion {
				t.Fatalf("unexpected derivation policy: %+v", evidence.Derivation)
			}
			if evidence.Derivation.SHA256 != digestBytes(result.Bottle) || evidence.Derivation.Size != int64(len(result.Bottle)) {
				t.Fatalf("derived bottle identity mismatch: %+v", evidence.Derivation)
			}

			headers, contents, gzipHeader := inspectDerivedBottle(t, result.Bottle)
			if got, want := gzipHeader.ModTime.Unix(), profile.SourceDateEpoch; got != want {
				t.Fatalf("gzip mtime = %d, want %d", got, want)
			}
			if gzipHeader.OS != 255 || gzipHeader.Name != "" || gzipHeader.Comment != "" || len(gzipHeader.Extra) != 0 {
				t.Fatalf("non-canonical gzip header: %+v", gzipHeader)
			}
			wantPaths := []string{"a365/0.3.3/.brew/a365.rb", "a365/0.3.3/bin/a365"}
			if len(headers) != len(wantPaths) {
				t.Fatalf("derived entry count = %d, want %d", len(headers), len(wantPaths))
			}
			for index, header := range headers {
				if header.Name != wantPaths[index] {
					t.Fatalf("derived path %d = %q, want %q", index, header.Name, wantPaths[index])
				}
				if header.Format != tar.FormatUSTAR || header.Typeflag != tar.TypeReg {
					t.Fatalf("derived header is not canonical USTAR regular file: %+v", header)
				}
				if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
					t.Fatalf("derived ownership is not canonical: %+v", header)
				}
				if header.ModTime.Unix() != profile.SourceDateEpoch {
					t.Fatalf("derived mtime = %d", header.ModTime.Unix())
				}
			}
			if headers[0].Mode != 0o444 || headers[1].Mode != 0o555 {
				t.Fatalf("derived modes = %#o, %#o", headers[0].Mode, headers[1].Mode)
			}
			if !bytes.Equal(contents[wantPaths[0]], formula) || !bytes.Equal(contents[wantPaths[1]], payload) {
				t.Fatal("derived contents differ from verified inputs")
			}
			canonicalEvidence, err := CanonicalEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			canonicalEvidenceAgain, err := CanonicalEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonicalEvidence, canonicalEvidenceAgain) {
				t.Fatal("canonical evidence is unstable")
			}
		})
	}
}

func TestDeriveIsDeterministicAcrossSourceArchiveMetadataAndOrder(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	firstEntries := baseEntries(payload)
	secondEntries := []testArchiveEntry{
		{name: "a365", mode: 0o755, data: payload, uid: 501, gid: 20, modTime: time.Unix(42, 0)},
		{name: "README.md", mode: 0o644, data: []byte("fixture readme\n"), uid: 12, gid: 13, modTime: time.Unix(43, 0)},
		{name: "LICENSE", mode: 0o644, data: []byte("fixture license\n"), uid: 99, gid: 98, modTime: time.Unix(44, 0)},
	}
	firstSource := makeSourceArchive(t, firstEntries)
	secondSource := makeSourceArchive(t, secondEntries)
	firstProfile := profileFor(firstSource, formula, "amd64")
	secondProfile := profileFor(secondSource, formula, "amd64")

	first, err := Derive(bytes.NewReader(firstSource), formula, firstProfile)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Derive(bytes.NewReader(secondSource), formula, secondProfile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstSource, secondSource) {
		t.Fatal("test sources unexpectedly match")
	}
	if !bytes.Equal(first.Bottle, second.Bottle) {
		t.Fatal("derived bottle depends on source entry order, ownership, or timestamps")
	}
	if first.Evidence.Derivation.SHA256 != second.Evidence.Derivation.SHA256 || first.Evidence.Derivation.InventorySHA256 != second.Evidence.Derivation.InventorySHA256 {
		t.Fatal("derivation evidence is not stable")
	}
	if first.Evidence.Source.InventorySHA256 != second.Evidence.Source.InventorySHA256 {
		t.Fatal("canonical source inventory depends on source order or metadata")
	}
	if first.Evidence.Source.SHA256 == second.Evidence.Source.SHA256 || first.Evidence.ProfileSHA256 == second.Evidence.ProfileSHA256 {
		t.Fatal("source-specific evidence did not distinguish different source archives")
	}

	repeated, err := Derive(bytes.NewReader(firstSource), formula, firstProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bottle, repeated.Bottle) || !reflect.DeepEqual(first.Evidence, repeated.Evidence) {
		t.Fatal("repeated derivation is not byte-for-byte deterministic")
	}
}

func TestDerivedBottlePassesIndependentBottleVerifier(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	source := makeSourceArchive(t, baseEntries(payload))
	result, err := Derive(bytes.NewReader(source), formula, profileFor(source, formula, "amd64"))
	if err != nil {
		t.Fatal(err)
	}
	hexDigest := result.Evidence.Derivation.SHA256[len("sha256:"):]
	if _, err := hex.DecodeString(hexDigest); err != nil {
		t.Fatal(err)
	}
	verified, err := bottle.Verify(bytes.NewReader(result.Bottle), bottle.Expectation{
		Name:             "a365",
		FullName:         "sozercan/repo/a365",
		FormulaVersion:   "0.3.3",
		PkgVersion:       "0.3.3",
		BottleTag:        "x86_64_linux",
		CompressedSHA256: result.Evidence.Derivation.SHA256,
		CompressedSize:   int64(len(result.Bottle)),
		HomebrewSHA256:   hexDigest,
		HomebrewVersion:  "4.6.0",
		Arch:             "x86_64",
		Compiler:         "go",
		ExpectedTap:      "sozercan/repo",
		FormulaIdentity:  "sozercan/repo/a365",
	}, bottle.Options{})
	if err != nil {
		t.Fatalf("independent bottle verifier rejected derived bottle: %v", err)
	}
	if verified.Receipt != nil {
		t.Fatal("derived bottle unexpectedly contains a receipt")
	}
	if got, want := verified.Formula.SHA256, digestBytes(formula); got != want {
		t.Fatalf("embedded Formula digest = %s, want %s", got, want)
	}
}

func TestCanonicalProfileAndInventoryIgnoreInputOrder(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	source := makeSourceArchive(t, baseEntries(payload))
	first := profileFor(source, formula, "amd64")
	second := first
	second.Entries = []EntryProfile{first.Entries[2], first.Entries[0], first.Entries[1]}
	firstBytes, err := CanonicalProfile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := CanonicalProfile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("canonical profile depends on entry order")
	}
	entries := []InventoryEntry{
		{Path: "b", Mode: 0o444, Size: 1, SHA256: digestBytes([]byte("b"))},
		{Path: "a", Mode: 0o555, Size: 1, SHA256: digestBytes([]byte("a"))},
	}
	ordered, err := CanonicalInventory(entries)
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := CanonicalInventory([]InventoryEntry{entries[1], entries[0]})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ordered, reversed) {
		t.Fatal("canonical inventory depends on entry order")
	}
}
