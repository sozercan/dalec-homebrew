package cataloggenerator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	"github.com/sozercan/dalec-homebrew/internal/prebuilt"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

type fakeArtifactFetcher struct {
	bodies map[string][]byte
	hosts  map[string][]string
}

func (f *fakeArtifactFetcher) Probe(_ context.Context, rawURL string) (fetcher.ProbeResult, error) {
	body, ok := f.bodies[rawURL]
	if !ok {
		return fetcher.ProbeResult{}, fmt.Errorf("unexpected probe %s", rawURL)
	}
	hosts := slices.Clone(f.hosts[rawURL])
	if len(hosts) == 0 {
		parsed, _ := url.Parse(rawURL)
		hosts = []string{parsed.Hostname()}
	}
	return fetcher.ProbeResult{FinalURL: rawURL, Size: int64(len(body)), RedirectHostSequence: hosts}, nil
}

func (f *fakeArtifactFetcher) ProbeOptional(ctx context.Context, rawURL string) (fetcher.ProbeResult, bool, error) {
	result, err := f.Probe(ctx, rawURL)
	return result, err == nil, err
}

func (f *fakeArtifactFetcher) Fetch(_ context.Context, request fetcher.Request, destination io.Writer) (fetcher.Evidence, error) {
	body, ok := f.bodies[request.URL]
	if !ok {
		return fetcher.Evidence{}, fmt.Errorf("unexpected fetch %s", request.URL)
	}
	if int64(len(body)) != request.ExpectedSize {
		return fetcher.Evidence{}, fmt.Errorf("fake body size mismatch")
	}
	if _, err := destination.Write(body); err != nil {
		return fetcher.Evidence{}, err
	}
	return fetcher.Evidence{
		SchemaVersion:        fetcher.EvidenceSchemaVersion,
		FetchPolicyVersion:   request.FetchPolicyVersion,
		ArtifactID:           request.ArtifactID,
		Filename:             request.Filename,
		Size:                 int64(len(body)),
		SHA256:               request.SHA256,
		RedactedHostSequence: slices.Clone(request.AllowedRedirectHosts),
	}, nil
}

func (f *fakeArtifactFetcher) FetchObserved(ctx context.Context, rawURL string, expectedSize int64, filename string, allowedRedirectHosts []string, destination io.Writer) (fetcher.Evidence, error) {
	return f.Fetch(ctx, fetcher.Request{SchemaVersion: fetcher.RequestSchemaVersion, FetchPolicyVersion: fetcher.FetchPolicyVersion, ArtifactID: filename, URL: rawURL, ExpectedSize: expectedSize, SHA256: strings.Repeat("0", 64), Filename: filename, AllowedRedirectHosts: allowedRedirectHosts}, destination)
}

type fakeGeneratedArtifactStore struct {
	values map[digest.Digest][]byte
}

func (s *fakeGeneratedArtifactStore) Put(expected digest.Digest, size int64, source io.Reader) error {
	data, err := io.ReadAll(source)
	if err != nil {
		return err
	}
	if int64(len(data)) != size || digest.FromBytes(data) != expected {
		return fmt.Errorf("stored derived bottle does not match expected identity")
	}
	if s.values == nil {
		s.values = map[digest.Digest][]byte{}
	}
	s.values[expected] = slices.Clone(data)
	return nil
}

func (s *fakeGeneratedArtifactStore) Verify(expected digest.Digest, size int64) error {
	data, ok := s.values[expected]
	if !ok || int64(len(data)) != size {
		return fmt.Errorf("generated artifact is unavailable")
	}
	return nil
}

func TestProductionArtifactBuilderGeneratesPolicyAuthorizedPrebuiltBottle(t *testing.T) {
	formulaSource := []byte("class Widget < Formula\n  def install\n    bin.install \"widget\"\n  end\nend\n")
	sourceArchive := []byte("bounded source fixture")
	formulaDigest := digestString(formulaSource)
	sourceDigest := digestString(sourceArchive)
	id := catalog.FormulaID("acme/tools/widget")
	tap := catalog.TapID("acme/tools")
	platform := catalog.Platform{OS: "linux", Architecture: "amd64"}
	sourceURL := "https://github.com/acme/widget/releases/download/v1.0.0/widget_1.0.0_linux_amd64.tar.gz"
	commit := strings.Repeat("a", 40)
	rawFormulaURL := "https://raw.githubusercontent.com/acme/homebrew-tools/" + commit + "/Formula/widget.rb"
	falseValue := false
	policy := policyv2.PrebuiltArchivePolicy{
		FormulaID: string(id), PolicyVersion: policyv2.PrebuiltDerivedBottlePolicyVersion, Version: "1.0.0", FormulaSourceDigest: formulaDigest,
		RootOnly: true, RequireNoBottle: true, Dependencies: []string{},
		Platforms: []policyv2.PrebuiltArchivePlatformPolicy{{Platform: "linux/amd64", URL: sourceURL, SHA256: sourceDigest}},
		Archive: policyv2.PrebuiltArchiveConstraints{
			Format: "tar+gzip", SingleGzipMember: true, MaxCompressedBytes: 1024, MaxExpandedBytes: 4096, MaxExpansionRatio: 8, MaxEntries: 3, MaxFileBytes: 2048, MaxDepth: 1, MaxPathBytes: 255,
			Members: []policyv2.PrebuiltArchiveMemberPolicy{{Path: "LICENSE", Type: "regular", Mode: "0644"}, {Path: "README.md", Type: "regular", Mode: "0644"}, {Path: "widget", Type: "regular", Mode: "0755"}},
		},
		Install: policyv2.PrebuiltArchiveInstallPolicy{Source: "widget", Destination: "bin/widget", Mode: "0555"},
		Binary:  policyv2.PrebuiltBinaryPolicy{GoModule: "example.com/widget", CGOEnabled: &falseValue},
	}
	store := &fakeGeneratedArtifactStore{}
	builder := &ProductionArtifactBuilder{
		fetcher: &fakeArtifactFetcher{
			bodies: map[string][]byte{sourceURL: sourceArchive, rawFormulaURL: formulaSource},
			hosts:  map[string][]string{sourceURL: {"github.com", "release-assets.githubusercontent.com"}, rawFormulaURL: {"raw.githubusercontent.com"}},
		},
		serviceOrigin:   "https://catalog.example.test",
		artifactStore:   store,
		tapPolicy:       &policyv2.TapPolicy{PrebuiltArchives: []policyv2.PrebuiltArchivePolicy{policy}},
		tapPolicyDigest: "sha256:" + strings.Repeat("b", 64),
		derivePrebuilt:  fakePrebuiltDeriver(t),
		inspectBottle:   bottle.InspectForCatalog,
	}
	formula := catalog.Formula{
		ID: id, Name: "widget", HomebrewFullName: string(id), SourcePath: "Formula/widget.rb", SourceDigest: formulaDigest,
		StableVersion: "1.0.0", PrebuiltArchive: &catalog.PrebuiltArchiveDeclaration{Files: []catalog.PrebuiltArchiveFile{{Tag: "x86_64_linux", URL: sourceURL, SHA256: sourceDigest, Format: catalog.PrebuiltArchiveFormatTarGzip}}},
	}
	document := &catalog.TapCatalog{
		SchemaVersion: catalog.TapCatalogSchemaVersion,
		Tap:           catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: commit, TreeDigest: "sha256:" + strings.Repeat("c", 64), ArchiveDigest: "sha256:" + strings.Repeat("d", 64)},
		PublishedAt:   time.Unix(1, 0).UTC(), Sequence: 1, Formulae: []catalog.Formula{formula},
	}
	node := catalog.Node{ID: id, Tap: tap, Name: "widget", HomebrewFullName: string(id), FormulaVersion: "1.0.0", PkgVersion: "1.0.0"}
	request := &catalog.Request{SchemaVersion: catalog.RequestSchemaVersion, Targets: []catalog.PlatformRequest{{Platform: platform, ExternalRoots: []catalog.FormulaID{id}}}, HomebrewCommit: strings.Repeat("e", 40), CoreSnapshotDigest: "sha256:" + strings.Repeat("f", 64)}
	core := &fakeSnapshot{info: metadata.SnapshotInfo{Digest: request.CoreSnapshotDigest}}

	artifact, err := builder.Build(t.Context(), request, core, map[catalog.TapID]*catalog.TapCatalog{tap: document}, node, platform)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateBottleArtifact(artifact); err != nil {
		t.Fatalf("generated artifact is invalid: %v", err)
	}
	if artifact.PrebuiltDerivation == nil || artifact.PrebuiltDerivation.PolicyDigest != builder.tapPolicyDigest || artifact.PrebuiltDerivation.RecipeDigest == "" {
		t.Fatalf("missing prebuilt policy/profile evidence: %+v", artifact.PrebuiltDerivation)
	}
	if got, want := artifact.Transport.HTTPS.URL, builder.derivedArtifactURL(artifact.SHA256); got != want {
		t.Fatalf("derived URL=%q, want %q", got, want)
	}
	if got := artifact.PrebuiltDerivation.Source.Transport.HTTPS.AllowedRedirectHosts; !slices.Equal(got, []string{"github.com", "release-assets.githubusercontent.com"}) {
		t.Fatalf("source redirect evidence=%v", got)
	}
	if artifact.PrebuiltDerivation.FormulaSource.Transport.Tap != document.Tap || artifact.PrebuiltDerivation.FormulaSource.Transport.Path != formula.SourcePath {
		t.Fatalf("Formula source transport=%+v", artifact.PrebuiltDerivation.FormulaSource.Transport)
	}
	storedDigest := digest.Digest(artifact.SHA256)
	if err := store.Verify(storedDigest, artifact.Size); err != nil {
		t.Fatal(err)
	}
}

func TestProductionArtifactBuilderPrefersSupportedNativeBottle(t *testing.T) {
	id := catalog.FormulaID("acme/tools/widget")
	tap := id.Tap()
	platform := catalog.Platform{OS: "linux", Architecture: "amd64"}
	formula := catalog.Formula{ID: id, Name: "widget", HomebrewFullName: string(id), StableVersion: "1", Bottle: &catalog.BottleDeclaration{RootURL: "https://ghcr.io/v2/acme/tools", Files: []catalog.BottleFile{{Tag: "x86_64_linux", SHA256: "sha256:" + strings.Repeat("a", 64)}}}, PrebuiltArchive: &catalog.PrebuiltArchiveDeclaration{Files: []catalog.PrebuiltArchiveFile{{Tag: "x86_64_linux", URL: "https://github.com/acme/widget/archive.tar.gz", SHA256: "sha256:" + strings.Repeat("b", 64), Format: catalog.PrebuiltArchiveFormatTarGzip}}}}
	builder := &ProductionArtifactBuilder{fetcher: &fakeArtifactFetcher{}, tapPolicy: &policyv2.TapPolicy{}}
	request := &catalog.Request{}
	core := &fakeSnapshot{}
	_, err := builder.Build(t.Context(), request, core, map[catalog.TapID]*catalog.TapCatalog{tap: {Formulae: []catalog.Formula{formula}}}, catalog.Node{ID: id}, platform)
	if err == nil || !strings.Contains(err.Error(), "OCI artifact resolver is unavailable") {
		t.Fatalf("expected native OCI path, got %v", err)
	}
}

func TestProductionArtifactCacheBindingIncludesServiceOrigin(t *testing.T) {
	base := &ProductionArtifactBuilder{serviceOrigin: "https://catalog-a.example.test", tapPolicyDigest: "sha256:" + strings.Repeat("a", 64)}
	other := &ProductionArtifactBuilder{serviceOrigin: "https://catalog-b.example.test", tapPolicyDigest: base.tapPolicyDigest}
	left, err := base.artifactCacheBinding()
	if err != nil {
		t.Fatal(err)
	}
	right, err := other.artifactCacheBinding()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(left, right) {
		t.Fatal("artifact cache binding ignored the catalog service origin")
	}
}

func fakePrebuiltDeriver(t *testing.T) prebuiltDeriver {
	t.Helper()
	return func(source io.Reader, formula []byte, profile prebuilt.Profile) (*prebuilt.Result, error) {
		if _, err := io.ReadAll(source); err != nil {
			return nil, err
		}
		bottleBytes := makeReceiptlessBottle(t, profile.Name, profile.PkgVersion, formula, []byte("fake executable"), profile.SourceDateEpoch)
		profileBytes, err := prebuilt.CanonicalProfile(profile)
		if err != nil {
			return nil, err
		}
		payloadDigest := digestString([]byte("fake executable"))
		return &prebuilt.Result{Bottle: bottleBytes, Evidence: prebuilt.Evidence{
			SchemaVersion: prebuilt.EvidenceSchemaVersion, PolicyVersion: profile.PolicyVersion, ProfileSHA256: digestString(profileBytes),
			Source:     prebuilt.SourceEvidence{SHA256: profile.Source.SHA256, Size: profile.Source.Size, ExpandedSHA256: digestString([]byte("expanded")), ExpandedSize: 100, InventorySHA256: digestString([]byte("inventory")), Inventory: []prebuilt.InventoryEntry{{Path: "LICENSE", Mode: 0o644, Size: 1, SHA256: digestString([]byte("l"))}, {Path: "README.md", Mode: 0o644, Size: 1, SHA256: digestString([]byte("r"))}, {Path: profile.PayloadPath, Mode: 0o755, Size: 15, SHA256: payloadDigest}}, PayloadPath: profile.PayloadPath, PayloadSHA256: payloadDigest, PayloadSize: 15},
			Formula:    prebuilt.FormulaEvidence{SHA256: profile.FormulaSHA256, Size: int64(len(formula))},
			ELF:        prebuilt.ELFEvidence{Class: "ELFCLASS64", Machine: "EM_X86_64", ImportedLibraries: []string{}},
			Derivation: prebuilt.DerivationEvidence{PolicyVersion: prebuilt.DerivationPolicyVersion, Receiptless: true, ExecutablePath: "bin/" + profile.Name, SHA256: digestString(bottleBytes), Size: int64(len(bottleBytes))},
		}}, nil
	}
}

func makeReceiptlessBottle(t *testing.T, name, version string, formula, executable []byte, epoch int64) []byte {
	t.Helper()
	var tarBytes bytes.Buffer
	tw := tar.NewWriter(&tarBytes)
	for _, file := range []struct {
		name string
		mode int64
		data []byte
	}{{name + "/" + version + "/.brew/" + name + ".rb", 0o444, formula}, {name + "/" + version + "/bin/" + name, 0o555, executable}} {
		header := &tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.data)), Typeflag: tar.TypeReg, ModTime: time.Unix(epoch, 0).UTC(), Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	gw.Header.ModTime = time.Unix(epoch, 0).UTC()
	if _, err := gw.Write(tarBytes.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func digestString(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
