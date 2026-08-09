package release

import (
	"slices"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func testComponent(name string) Component {
	d := "sha256:" + strings.Repeat("a", 64)
	repo := "ghcr.io/x/" + name
	return Component{
		Index: repo + "@" + d,
		Platforms: []PlatformRef{
			{Platform: resolution.Platform{OS: "linux", Architecture: "arm64"}, Ref: repo + "@" + d},
			{Platform: resolution.Platform{OS: "linux", Architecture: "amd64"}, Ref: repo + "@" + d},
		},
	}
}

func validV1() *Manifest {
	d := "sha256:" + strings.Repeat("a", 64)
	return &Manifest{
		SchemaVersion:          SchemaVersionV1,
		PolicyVersion:          resolution.PolicyVersion,
		Frontend:               testComponent("frontend"),
		RuntimeBase:            testComponent("base"),
		Materializer:           testComponent("materializer"),
		HomebrewCommit:         strings.Repeat("a", 40),
		PortableRubyVersion:    "4.0.6",
		VerificationKeysDigest: d,
		DalecModule:            "v1",
		BuildKitModule:         "v1",
	}
}

func validV2(t *testing.T) *Manifest {
	t.Helper()
	m := validV1()
	d := "sha256:" + strings.Repeat("b", 64)
	tapDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDigest, err := policyv2.Digest()
	if err != nil {
		t.Fatal(err)
	}
	fetcher := testComponent("fetcher")
	m.SchemaVersion = SchemaVersionV2
	m.PolicyVersion = RuntimePolicyVersionV2
	m.MetadataBundleDigest = "sha256:" + strings.Repeat("c", 64)
	m.BottleFetcher = &fetcher
	m.CatalogServiceOrigin = "https://catalog.example.com"
	m.IngestionJWSKeyPolicyDigest = d
	m.TapPolicyDigest = tapDigest
	m.ExecutableRuntimePolicyDigest = runtimeDigest
	m.SupportedCatalogPolicyVersions = []string{CatalogPolicyVersionV1}
	m.SupportedFetchPolicyVersions = []string{BottleFetchPolicyVersionV1}
	m.SupportedProvenancePolicyVersions = []string{ChecksumWaiverPolicyVersionV1, CoreWaiverPolicyVersionV1, HTTPSSourceWaiverPolicyVersionV1, PrebuiltWaiverPolicyVersionV1, SigstoreProvenancePolicyVersionV1}
	return m
}

func TestCanonicalAndPlatformV1(t *testing.T) {
	m := validV1()
	a, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical(m)
	if err != nil || string(a) != string(b) {
		t.Fatal("unstable")
	}
	if strings.Contains(string(a), "bottle_fetcher") || strings.Contains(string(a), "catalog_service_origin") {
		t.Fatalf("V1 canonical manifest contains V2 fields: %s", a)
	}
	c, err := m.ComponentsFor(resolution.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || c.RuntimeBaseRef == "" {
		t.Fatalf("%+v %v", c, err)
	}
	if m.SupportsNonCoreTaps() {
		t.Fatal("V1 manifest unexpectedly advertises non-core support")
	}
	if SchemaVersion != SchemaVersionV1 {
		t.Fatalf("compatibility SchemaVersion=%q", SchemaVersion)
	}
}

func TestV1PlatformLookupRemainsVariantInsensitive(t *testing.T) {
	m := validV1()
	components, err := m.ComponentsFor(resolution.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"})
	if err != nil {
		t.Fatal(err)
	}
	if components.RuntimeBaseRef == "" {
		t.Fatal("V1 variant-insensitive lookup returned an empty runtime base")
	}
}

func TestV2CanonicalBindingsAndCapability(t *testing.T) {
	m := validV2(t)
	originalProvenanceOrder := append([]string(nil), m.SupportedProvenancePolicyVersions...)
	canonical, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.SupportedProvenancePolicyVersions, originalProvenanceOrder) {
		t.Fatal("Canonical mutated the caller's policy version order")
	}
	decoded, err := Decode(strings.NewReader(string(canonical)))
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.SupportsNonCoreTaps() {
		t.Fatal("valid V2 manifest did not advertise non-core support")
	}
	bindings, err := decoded.BindingsFor(resolution.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if bindings.BottleFetcherRef == "" || bindings.CatalogServiceOrigin != m.CatalogServiceOrigin {
		t.Fatalf("incomplete V2 bindings: %+v", bindings)
	}
	if got, want := bindings.SupportedCatalogPolicyVersions, []string{CatalogPolicyVersionV1}; !slices.Equal(got, want) {
		t.Fatalf("catalog policy versions=%v, want %v", got, want)
	}
	bindings.SupportedCatalogPolicyVersions[0] = "mutated"
	if decoded.SupportedCatalogPolicyVersions[0] == "mutated" {
		t.Fatal("BindingsFor returned an aliased policy version slice")
	}
	if _, err := decoded.ComponentsFor(resolution.Platform{OS: "linux", Architecture: "amd64"}); err == nil || !strings.Contains(err.Error(), "BindingsFor") {
		t.Fatalf("ComponentsFor V2 error=%v", err)
	}
}

func TestV2RequiresCompleteBindings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{name: "fetcher", mutate: func(m *Manifest) { m.BottleFetcher = nil }, want: "bottle fetcher"},
		{name: "metadata bundle", mutate: func(m *Manifest) { m.MetadataBundleDigest = "" }, want: "metadata bundle"},
		{name: "invalid metadata bundle", mutate: func(m *Manifest) { m.MetadataBundleDigest = "sha256:nope" }, want: "metadata bundle"},
		{name: "origin", mutate: func(m *Manifest) { m.CatalogServiceOrigin = "" }, want: "catalog service origin"},
		{name: "ingestion policy", mutate: func(m *Manifest) { m.IngestionJWSKeyPolicyDigest = "" }, want: "ingestion JWS key policy"},
		{name: "tap policy", mutate: func(m *Manifest) { m.TapPolicyDigest = "" }, want: "tap policy"},
		{name: "runtime policy", mutate: func(m *Manifest) { m.ExecutableRuntimePolicyDigest = "" }, want: "executable runtime policy"},
		{name: "catalog versions", mutate: func(m *Manifest) { m.SupportedCatalogPolicyVersions = nil }, want: "catalog policy versions"},
		{name: "fetch versions", mutate: func(m *Manifest) { m.SupportedFetchPolicyVersions = nil }, want: "fetch policy versions"},
		{name: "provenance versions", mutate: func(m *Manifest) { m.SupportedProvenancePolicyVersions = nil }, want: "provenance policy versions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validV2(t)
			tt.mutate(m)
			if err := Validate(m); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v, want %q", err, tt.want)
			}
			if m.SupportsNonCoreTaps() {
				t.Fatal("incomplete V2 manifest advertised non-core support")
			}
		})
	}
}

func TestV1RejectsV2Bindings(t *testing.T) {
	m := validV1()
	fetcher := testComponent("fetcher")
	m.BottleFetcher = &fetcher
	if err := Validate(m); err == nil || !strings.Contains(err.Error(), "V2-only") {
		t.Fatalf("err=%v", err)
	}
}

func TestV2RejectsInvalidCatalogOrigins(t *testing.T) {
	for _, origin := range []string{
		"http://catalog.example.com",
		"https://user@catalog.example.com",
		"https://catalog.example.com/",
		"https://catalog.example.com/path",
		"https://catalog.example.com?token=secret",
		"https://catalog.example.com#fragment",
		" catalog.example.com ",
	} {
		t.Run(origin, func(t *testing.T) {
			m := validV2(t)
			m.CatalogServiceOrigin = origin
			if err := Validate(m); err == nil || !strings.Contains(err.Error(), "catalog service origin") {
				t.Fatalf("origin %q err=%v", origin, err)
			}
		})
	}
}

func TestV2RejectsMismatchedEmbeddedPolicyDigests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{name: "tap", mutate: func(m *Manifest) { m.TapPolicyDigest = "sha256:" + strings.Repeat("c", 64) }, want: "embedded V2 tap policy"},
		{name: "runtime", mutate: func(m *Manifest) { m.ExecutableRuntimePolicyDigest = "sha256:" + strings.Repeat("c", 64) }, want: "embedded V2 policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validV2(t)
			tt.mutate(m)
			if err := Validate(m); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v", err)
			}
			if m.SupportsNonCoreTaps() {
				t.Fatal("mismatched policy digest advertised non-core support")
			}
		})
	}
}

func TestV2RejectsUnsupportedPolicyVersionSets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{name: "catalog", mutate: func(m *Manifest) { m.SupportedCatalogPolicyVersions = []string{"other-catalog-v1"} }, want: CatalogPolicyVersionV1},
		{name: "provenance", mutate: func(m *Manifest) { m.SupportedProvenancePolicyVersions = []string{ChecksumWaiverPolicyVersionV1} }, want: SigstoreProvenancePolicyVersionV1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validV2(t)
			tt.mutate(m)
			if err := Validate(m); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestV2RejectsUnsupportedFetchPolicyVersion(t *testing.T) {
	m := validV2(t)
	m.SupportedFetchPolicyVersions = []string{"other-fetch-v1"}
	if err := Validate(m); err == nil || !strings.Contains(err.Error(), BottleFetchPolicyVersionV1) {
		t.Fatalf("err=%v", err)
	}
}

func TestV2RejectsDuplicatePolicyVersions(t *testing.T) {
	m := validV2(t)
	m.SupportedCatalogPolicyVersions = []string{"catalog-v1", "catalog-v1"}
	if err := Validate(m); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("err=%v", err)
	}
}

func TestMixedRepositoryFails(t *testing.T) {
	m := validV1()
	m.Materializer.Platforms[0].Ref = "ghcr.io/other/m@" + strings.Split(m.Materializer.Index, "@")[1]
	if err := Validate(m); err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeRejectsDuplicateMembers(t *testing.T) {
	_, err := Decode(strings.NewReader(`{"schema_version":"dalec-homebrew-components/v1","schema_version":"other"}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err=%v", err)
	}
}

func TestBindingsForV2PreservesCompleteResolutionTuple(t *testing.T) {
	manifest := validV2(t)
	manifest.BottleFetcher.Index = "ghcr.io/x/fetcher@sha256:" + strings.Repeat("c", 64)
	bindings, err := manifest.BindingsFor(resolution.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	components := bindings.ComponentsV2
	if components.FrontendIndexRef != manifest.Frontend.Index || components.FrontendRef == "" || components.BottleFetcherRef != manifest.BottleFetcher.Index || bindings.BottleFetcherRef != manifest.BottleFetcher.Index || bindings.MetadataBundleDigest != manifest.MetadataBundleDigest || components.CatalogServiceOrigin != manifest.CatalogServiceOrigin || components.IngestionJWSKeyPolicyDigest != manifest.IngestionJWSKeyPolicyDigest || components.TapPolicyDigest != manifest.TapPolicyDigest || components.ExecutableRuntimePolicyDigest != manifest.ExecutableRuntimePolicyDigest {
		t.Fatalf("V2 components dropped bindings: %+v", components)
	}
	if len(components.SupportedCatalogPolicyVersions) == 0 || len(components.SupportedFetchPolicyVersions) == 0 || len(components.SupportedProvenancePolicyVersions) == 0 {
		t.Fatalf("V2 components dropped policy versions: %+v", components)
	}
}

func TestV2BuildLocalManifestBindings(t *testing.T) {
	m := validV2(t)
	extractor := testComponent("catalog-extractor")
	extractor.Index = "ghcr.io/x/catalog-extractor@sha256:" + strings.Repeat("c", 64)
	m.CatalogExtractor = &extractor
	m.CatalogServiceOrigin = ""
	m.IngestionJWSKeyPolicyDigest = ""
	if err := Validate(m); err != nil {
		t.Fatal(err)
	}
	bindings, err := m.BindingsFor(resolution.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if bindings.CatalogExtractorRef != extractor.Index || bindings.ComponentsV2.CatalogExtractorRef != extractor.Index {
		t.Fatalf("build-local bindings = %+v", bindings)
	}
	if bindings.ComponentsV2.CatalogServiceOrigin != "" || bindings.ComponentsV2.IngestionJWSKeyPolicyDigest != "" {
		t.Fatalf("build-local tuple retained hosted service fields: %+v", bindings.ComponentsV2)
	}
}
