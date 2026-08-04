package resolver

import (
	"context"
	"fmt"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/oci"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type fakeCatalog map[string]metadata.Match

func (f fakeCatalog) Lookup(name string) (metadata.Match, error) {
	m, ok := f[name]
	if !ok {
		return metadata.Match{}, fmt.Errorf("missing %s", name)
	}
	return m, nil
}

type fakeBottles struct {
	deps map[string][]resolution.RuntimeDependency
}

func (f fakeBottles) Resolve(_ context.Context, formula oci.Formula, target ocispec.Platform) (*oci.Result, error) {
	ref, err := oci.ResolveFormulaReference(formula)
	if err != nil {
		return nil, err
	}
	d := digest.FromString(formula.Name)
	desc := ocispec.Descriptor{Digest: d, Size: 1, MediaType: ocispec.MediaTypeImageManifest}
	checksum := formula.BottleFiles[oci.BottleTagX8664Linux].SHA256
	layer := ocispec.Descriptor{Digest: digest.Digest("sha256:" + checksum), Size: 1, MediaType: ocispec.MediaTypeImageLayerGzip}
	return &oci.Result{Formula: formula, Reference: ref, Target: target, SelectedBottleTag: oci.BottleTagX8664Linux, SelectedChildTag: ref.PkgVersion + ".x86_64_linux", Filename: formula.Name + "--" + ref.PkgVersion + ".x86_64_linux.bottle.tar.gz", Cellar: "/home/linuxbrew/.linuxbrew/Cellar", HomebrewSHA256: checksum, Index: desc, Manifest: func() ocispec.Descriptor { value := desc; value.Platform = &target; return value }(), Config: ocispec.Descriptor{Digest: d, Size: 1, MediaType: ocispec.MediaTypeImageConfig}, Layer: layer, Tab: resolution.BottleTab{HomebrewVersion: "6", Arch: "x86_64", Dependencies: f.deps[formula.Name]}}, nil
}

func formula(name, version string, deps ...string) metadata.Formula {
	checksum := digest.FromString("bottle-" + name).Encoded()
	return metadata.Formula{Name: name, FullName: "homebrew/core/" + name, Tap: "homebrew/core", StableVersion: version, License: "MIT", Dependencies: deps, Bottle: &metadata.Bottle{RootURL: "https://ghcr.io/v2/homebrew/core", Files: []metadata.BottleFile{{Tag: oci.BottleTagX8664Linux, Cellar: "/home/linuxbrew/.linuxbrew/Cellar", SHA256: checksum}}}}
}

func options() Options {
	tm := time.Unix(1_800_000_000, 0).UTC()
	d := digest.FromString("metadata").String()
	return Options{SpecDigest: digest.FromString("spec").String(), Now: tm, Metadata: metadata.SnapshotInfo{Digest: d, FormulaDigest: d, MigrationDigest: d, GeneratedAt: tm, FetchedAt: tm, Formula: metadata.DocumentInfo{URL: "https://example/formula", EnvelopeDigest: d, GeneratedAtSource: metadata.GeneratedAtSignedPayload, Signatures: []metadata.SignatureInfo{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}}}, Migrations: metadata.DocumentInfo{URL: "https://example/migrations", EnvelopeDigest: d, GeneratedAtSource: metadata.GeneratedAtSignedPayload, Signatures: []metadata.SignatureInfo{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}}}}, Runtime: resolution.RuntimePolicy{User: "linuxbrew", UID: 1000, GID: 1000, CPUBaseline: "core2"}, Attestation: resolution.AttestationPolicy{Waiver: "test"}}
}

func TestResolveClosureAndAlias(t *testing.T) {
	hello, jq := formula("hello", "2", "jq"), formula("jq", "1")
	cat := fakeCatalog{
		"hi":    {Requested: "hi", Canonical: "hello", Kind: metadata.MatchAlias, Formula: hello},
		"hello": {Requested: "hello", Canonical: "hello", Kind: metadata.MatchCanonical, Formula: hello},
		"jq":    {Requested: "jq", Canonical: "jq", Kind: metadata.MatchCanonical, Formula: jq},
	}
	b := fakeBottles{deps: map[string][]resolution.RuntimeDependency{"hello": {{FullName: "jq", Version: "1", PkgVersion: "1", DeclaredDirectly: true}}}}
	r, err := Resolve(context.Background(), cat, b, []string{"hi"}, ocispec.Platform{OS: "linux", Architecture: "amd64"}, options())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Nodes) != 2 || len(r.InstallOrder) != 2 || r.InstallOrder[0] != "jq" || r.InstallOrder[1] != "hello" {
		t.Fatalf("record=%+v", r)
	}
	if r.Requested[0].Canonical != "hello" {
		t.Fatalf("requested=%+v", r.Requested)
	}
}

func TestRejectUnsatisfiedMinimum(t *testing.T) {
	a, bFormula := formula("a", "1", "b"), formula("b", "1")
	cat := fakeCatalog{"a": {Requested: "a", Canonical: "a", Kind: metadata.MatchCanonical, Formula: a}, "b": {Requested: "b", Canonical: "b", Kind: metadata.MatchCanonical, Formula: bFormula}}
	b := fakeBottles{deps: map[string][]resolution.RuntimeDependency{"a": {{FullName: "b", Version: "2", PkgVersion: "2"}}}}
	if _, err := Resolve(context.Background(), cat, b, []string{"a"}, ocispec.Platform{OS: "linux", Architecture: "amd64"}, options()); err == nil {
		t.Fatal("expected error")
	}
}

func TestCycleDiagnostic(t *testing.T) {
	a, bFormula := formula("a", "1", "b"), formula("b", "1", "a")
	cat := fakeCatalog{"a": {Requested: "a", Canonical: "a", Kind: metadata.MatchCanonical, Formula: a}, "b": {Requested: "b", Canonical: "b", Kind: metadata.MatchCanonical, Formula: bFormula}}
	b := fakeBottles{deps: map[string][]resolution.RuntimeDependency{"a": {{FullName: "b", Version: "1", PkgVersion: "1"}}, "b": {{FullName: "a", Version: "1", PkgVersion: "1"}}}}
	if _, err := Resolve(context.Background(), cat, b, []string{"a"}, ocispec.Platform{OS: "linux", Architecture: "amd64"}, options()); err == nil {
		t.Fatal("expected cycle")
	}
}

func TestReleaseBoundRecordPassesIndependentVerifier(t *testing.T) {
	hello := formula("hello", "2")
	cat := fakeCatalog{"hello": {Requested: "hello", Canonical: "hello", Kind: metadata.MatchCanonical, Formula: hello}}
	opts := options()
	opts.Attestation = resolution.AttestationPolicy{Waiver: "homebrew-jws-and-verified-oci-chain-v1"}
	d := digest.FromString("component").String()
	opts.Components = resolution.Components{FrontendRef: "ghcr.io/example/frontend@" + d, RuntimeBaseRef: "ghcr.io/example/base@" + d, MaterializerRef: "ghcr.io/example/materializer@" + d, HomebrewCommit: "0123456789abcdef0123456789abcdef01234567", RubyRuntime: "portable-ruby-4.0.6", VerificationKeys: d, DalecModule: "v0.21.5", BuildKitModule: "v0.31.2"}
	r, err := Resolve(context.Background(), cat, fakeBottles{deps: map[string][]resolution.RuntimeDependency{}}, []string{"hello"}, ocispec.Platform{OS: "linux", Architecture: "amd64"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.BindRuntimePolicy(r); err != nil {
		t.Fatal(err)
	}
	if err := resolution.ValidateForMaterialization(r); err != nil {
		t.Fatal(err)
	}
}

func TestBottleTabCannotAddUnsignedDependency(t *testing.T) {
	a, bFormula := formula("a", "1"), formula("b", "1")
	cat := fakeCatalog{"a": {Requested: "a", Canonical: "a", Kind: metadata.MatchCanonical, Formula: a}, "b": {Requested: "b", Canonical: "b", Kind: metadata.MatchCanonical, Formula: bFormula}}
	b := fakeBottles{deps: map[string][]resolution.RuntimeDependency{"a": {{FullName: "b", Version: "1", PkgVersion: "1"}}}}
	r, err := Resolve(context.Background(), cat, b, []string{"a"}, ocispec.Platform{OS: "linux", Architecture: "amd64"}, options())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Nodes) != 1 || r.Nodes[0].Name != "a" {
		t.Fatalf("tab added unsigned dependency: %+v", r.Nodes)
	}
}

func TestRuntimeCompatibilityValidatesArchAndGlibc(t *testing.T) {
	if err := validateRuntimeCompatibility(resolution.BottleTab{Arch: "x86_64"}, ocispec.Platform{OS: "linux", Architecture: "arm64"}); err == nil {
		t.Fatal("wrong tab arch accepted")
	}
	if err := validateRuntimeCompatibility(resolution.BottleTab{BuiltOn: resolution.BuiltOn{GlibcVersion: "2.40"}}, ocispec.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatal("too-new all-bottle glibc accepted")
	}
}

func TestResolverRejectsPlatformVariant(t *testing.T) {
	if _, err := Resolve(context.Background(), fakeCatalog{}, fakeBottles{}, []string{"x"}, ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v9"}, options()); err == nil {
		t.Fatal("variant accepted")
	}
}

func TestCoreResolutionUpgradesToV2WithoutCatalogService(t *testing.T) {
	hello := formula("hello", "1")
	cat := fakeCatalog{"hello": {Requested: "hello", Canonical: "hello", Kind: metadata.MatchCanonical, Formula: hello}}
	opts := options()
	d := digest.FromString("component").String()
	opts.Components = resolution.Components{FrontendRef: "ghcr.io/example/frontend@" + d, RuntimeBaseRef: "ghcr.io/example/base@" + d, MaterializerRef: "ghcr.io/example/materializer@" + d, HomebrewCommit: "0123456789abcdef0123456789abcdef01234567", RubyRuntime: "4.0.6", VerificationKeys: d, DalecModule: "dalec@v1", BuildKitModule: "buildkit@v1"}
	legacy, err := Resolve(context.Background(), cat, fakeBottles{deps: map[string][]resolution.RuntimeDependency{}}, []string{"hello"}, ocispec.Platform{OS: "linux", Architecture: "amd64"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	components := resolution.ComponentsV2{FrontendIndexRef: "ghcr.io/example/frontend@" + d, FrontendRef: "ghcr.io/example/frontend@" + d, RuntimeBaseRef: "ghcr.io/example/base@" + d, MaterializerRef: "ghcr.io/example/materializer@" + d, BottleFetcherRef: "ghcr.io/example/fetcher@" + d, CatalogServiceOrigin: "https://catalog.example.com", IngestionJWSKeyPolicyDigest: d, TapPolicyDigest: d, ExecutableRuntimePolicyDigest: d, HomebrewCommit: opts.Components.HomebrewCommit, RubyRuntime: "4.0.6", VerificationKeys: d, DalecModule: "dalec@v1", BuildKitModule: "buildkit@v1", SupportedCatalogPolicyVersions: []string{catalog.TapCatalogPolicyVersion}, SupportedFetchPolicyVersions: []string{resolution.HTTPSFetchPolicyVersionV1}, SupportedProvenancePolicyVersions: []string{resolution.CoreProvenanceWaiverPolicyV1, resolution.HTTPSBottleSourceWaiverPolicyV1, resolution.ProvenanceWaiverPolicyV1, resolution.VerifiedProvenancePolicyV1}}
	record, err := RecordV2FromCore(legacy, components, opts.Metadata, 0)
	if err != nil {
		t.Fatal(err)
	}
	if record.Nodes[0].Bottle.Verification.PolicyVersion != resolution.CoreBottleVerificationDeferredV1 || record.Nodes[0].Provenance.Waiver.Policy != resolution.CoreProvenanceWaiverPolicyV1 {
		t.Fatalf("record=%+v", record)
	}
}
