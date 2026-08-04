package cataloggenerator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogextractor"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type fakeSnapshot struct {
	info     metadata.SnapshotInfo
	formulae map[string]metadata.Match
}

func (f *fakeSnapshot) Lookup(name string) (metadata.Match, error) {
	match, ok := f.formulae[name]
	if !ok {
		return metadata.Match{}, metadata.ErrFormulaNotFound
	}
	return match, nil
}
func (f *fakeSnapshot) Info() metadata.SnapshotInfo { return f.info }

type fakeCoreProvider struct{ snapshot CoreSnapshot }

func (f fakeCoreProvider) Snapshot(context.Context, *catalog.Request) (CoreSnapshot, error) {
	return f.snapshot, nil
}

type fakeExtractor struct {
	values map[catalog.TapID]*catalogextractor.ExtractedTap
	calls  []catalog.TapID
}

func (f *fakeExtractor) Extract(_ context.Context, tap catalog.TapID) (*catalogextractor.ExtractedTap, error) {
	f.calls = append(f.calls, tap)
	return f.values[tap], nil
}

type fakeArtifacts struct{}

func (fakeArtifacts) Build(_ context.Context, _ *catalog.Request, _ CoreSnapshot, catalogs map[catalog.TapID]*catalog.TapCatalog, node catalog.Node, platform catalog.Platform) (catalog.BottleArtifact, error) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	document := catalogs[node.ID.Tap()]
	sourceDigest := digestA
	if document != nil {
		for _, formula := range document.Formulae {
			if formula.ID == node.ID {
				sourceDigest = formula.SourceDigest
			}
		}
	}
	arch := "x86_64"
	tag := "x86_64_linux"
	if platform.Architecture == "arm64" {
		arch, tag = "arm64", "arm64_linux"
	}
	return catalog.BottleArtifact{ID: node.ID, Platform: platform, Tag: tag, Filename: node.Name + ".tgz", SHA256: digestA, Size: 1, Cellar: ":any", Tab: catalog.BottleTab{Receiptless: true, Arch: arch}, CurrentFormulaSourceDigest: sourceDigest, BottleFormulaSourceDigest: digestB, BottleSourceWaiver: catalog.HTTPSBottleSourceWaiver, Transport: catalog.Transport{HTTPS: &catalog.HTTPSTransport{URL: "https://bottles.example/" + node.Name + ".tgz", ExpectedSize: 1, SHA256: digestA, Filename: node.Name + ".tgz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: catalog.HTTPSFetchPolicyVersion}}, Verification: catalog.BottleVerification{PolicyVersion: catalog.BottleVerificationPolicy, InventoryDigest: digestB, EntryCount: 1, ExpandedSize: 1}, Provenance: catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: catalog.ChecksumProvenanceWaiver}}}, nil
}

func TestGeneratorIngestsCrossTapClosureOnDemand(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	commit := strings.Repeat("c", 40)
	acme, _ := catalog.ParseTapID("acme/tools")
	other, _ := catalog.ParseTapID("other/lib")
	makeTap := func(tap catalog.TapID, name string, dependencies []string) *catalogextractor.ExtractedTap {
		platform := func(tag string) catalogextractor.ExtractedPlatformFormula {
			return catalogextractor.ExtractedPlatformFormula{Tag: tag, Name: name, HomebrewFullName: string(tap) + "/" + name, StableVersion: "1", Dependencies: dependencies, Bottle: &catalogextractor.ExtractedBottle{RootURL: "https://bottles.example", Files: []catalog.BottleFile{{Tag: tag, URL: "https://bottles.example/" + name + ".tgz", SHA256: digest, Cellar: ":any"}}}}
		}
		return &catalogextractor.ExtractedTap{SchemaVersion: catalogextractor.ExtractedTapSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: commit, TreeDigest: digest, ArchiveDigest: digest}, Formulae: []catalogextractor.ExtractedFormula{{SourcePath: "Formula/" + name + ".rb", SourceDigest: digest, Platforms: []catalogextractor.ExtractedPlatformFormula{platform("x86_64_linux"), platform("arm64_linux")}}}}
	}
	extractor := &fakeExtractor{values: map[catalog.TapID]*catalogextractor.ExtractedTap{acme: makeTap(acme, "widget", []string{"other/lib/helper"}), other: makeTap(other, "helper", nil)}}
	snapshot := &fakeSnapshot{info: metadata.SnapshotInfo{Digest: digest}, formulae: map[string]metadata.Match{}}
	generator, err := NewGenerator(Config{Extractor: extractor, Core: fakeCoreProvider{snapshot}, Artifacts: fakeArtifacts{}})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := catalog.ParseFormulaID("acme/tools/widget")
	request := &catalog.Request{SchemaVersion: catalog.RequestSchemaVersion, Targets: []catalog.PlatformRequest{{Platform: catalog.Platform{OS: "linux", Architecture: "amd64"}, ExternalRoots: []catalog.FormulaID{root}}}, HomebrewCommit: strings.Repeat("f", 40), CoreSnapshotDigest: digest}
	generated, err := generator.Generate(t.Context(), request)
	if err != nil {
		t.Fatalf("%v (cause: %v)", err, errors.Unwrap(err))
	}
	if len(extractor.calls) != 2 || extractor.calls[0] != acme || extractor.calls[1] != other {
		t.Fatalf("extract calls=%v", extractor.calls)
	}
	if len(generated.Catalogs) != 2 || len(generated.Results) != 1 || len(generated.Results[0].Closure.Nodes) != 2 || len(generated.Results[0].Artifacts) != 2 {
		t.Fatalf("generated=%+v", generated)
	}
	if err := catalog.ValidatePlatformResult(generated.Results[0]); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialCoreProviderRequiresReleaseCommitBeforeFetch(t *testing.T) {
	provider := &officialCoreProvider{homebrewCommit: strings.Repeat("a", 40)}
	_, err := provider.Snapshot(context.Background(), &catalog.Request{HomebrewCommit: strings.Repeat("b", 40)})
	if err == nil || !strings.Contains(err.Error(), "does not match generator release") {
		t.Fatalf("err=%v", err)
	}
}
