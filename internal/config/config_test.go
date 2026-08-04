package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"slices"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

const (
	testDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func resetCompiledBindings(t *testing.T) {
	t.Helper()
	oldRuntimeBaseRef := RuntimeBaseRef
	oldMaterializerRef := MaterializerRef
	oldFrontendRef := FrontendRef
	oldHomebrewCommit := HomebrewCommit
	oldVerificationKeysDigest := VerificationKeysDigest
	oldPortableRubyVersion := PortableRubyVersion
	oldBottleFetcherRef := BottleFetcherRef
	oldCatalogServiceOrigin := CatalogServiceOrigin
	oldIngestionJWSKeyPolicyDigest := IngestionJWSKeyPolicyDigest
	oldIngestionJWSKeyPolicyBase64 := IngestionJWSKeyPolicyBase64
	oldTapPolicyDigest := TapPolicyDigest
	oldExecutableRuntimePolicyDigest := ExecutableRuntimePolicyDigest
	oldSupportedCatalogPolicyVersions := SupportedCatalogPolicyVersions
	oldSupportedFetchPolicyVersions := SupportedFetchPolicyVersions
	oldSupportedProvenancePolicyVersions := SupportedProvenancePolicyVersions
	t.Cleanup(func() {
		RuntimeBaseRef = oldRuntimeBaseRef
		MaterializerRef = oldMaterializerRef
		FrontendRef = oldFrontendRef
		HomebrewCommit = oldHomebrewCommit
		VerificationKeysDigest = oldVerificationKeysDigest
		PortableRubyVersion = oldPortableRubyVersion
		BottleFetcherRef = oldBottleFetcherRef
		CatalogServiceOrigin = oldCatalogServiceOrigin
		IngestionJWSKeyPolicyDigest = oldIngestionJWSKeyPolicyDigest
		IngestionJWSKeyPolicyBase64 = oldIngestionJWSKeyPolicyBase64
		TapPolicyDigest = oldTapPolicyDigest
		ExecutableRuntimePolicyDigest = oldExecutableRuntimePolicyDigest
		SupportedCatalogPolicyVersions = oldSupportedCatalogPolicyVersions
		SupportedFetchPolicyVersions = oldSupportedFetchPolicyVersions
		SupportedProvenancePolicyVersions = oldSupportedProvenancePolicyVersions
	})
	RuntimeBaseRef = ""
	MaterializerRef = ""
	FrontendRef = ""
	HomebrewCommit = ""
	VerificationKeysDigest = ""
	PortableRubyVersion = ""
	BottleFetcherRef = ""
	CatalogServiceOrigin = ""
	IngestionJWSKeyPolicyDigest = ""
	IngestionJWSKeyPolicyBase64 = ""
	TapPolicyDigest = ""
	ExecutableRuntimePolicyDigest = ""
	SupportedCatalogPolicyVersions = ""
	SupportedFetchPolicyVersions = ""
	SupportedProvenancePolicyVersions = ""
}

func localV1BuildOpts() map[string]string {
	return map[string]string{
		"build-arg:DALEC_HOMEBREW_RUNTIME_BASE": "example/base@" + testDigestA,
		"build-arg:DALEC_HOMEBREW_MATERIALIZER": "example/materializer@" + testDigestA,
		"build-arg:DALEC_HOMEBREW_FRONTEND_REF": "example/frontend@" + testDigestA,
		"build-arg:DALEC_HOMEBREW_COMMIT":       "0123456789abcdef0123456789abcdef01234567",
		"build-arg:DALEC_HOMEBREW_KEYS_DIGEST":  metadata.DefaultKeySetDigest(),
	}
}

func v2PolicyDigests(t *testing.T) (string, string) {
	t.Helper()
	tapDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDigest, err := policyv2.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return tapDigest, runtimeDigest
}

func addV2BuildOpts(t *testing.T, opts map[string]string) {
	t.Helper()
	tapDigest, runtimeDigest := v2PolicyDigests(t)
	opts["build-arg:"+BottleFetcherBuildArg] = "example/fetcher@" + testDigestA
	opts["build-arg:"+CatalogServiceOriginBuildArg] = "https://catalog.example.com"
	opts["build-arg:"+IngestionJWSKeyPolicyDigestBuildArg] = testDigestB
	opts["build-arg:"+TapPolicyDigestBuildArg] = tapDigest
	opts["build-arg:"+ExecutableRuntimePolicyDigestBuildArg] = runtimeDigest
	opts["build-arg:"+SupportedCatalogPolicyVersionsBuildArg] = CatalogPolicyVersionV1
	opts["build-arg:"+SupportedFetchPolicyVersionsBuildArg] = BottleFetchPolicyVersionV1
	opts["build-arg:"+SupportedProvenancePolicyVersionsBuildArg] = ChecksumWaiverPolicyVersionV1 + "," + CoreWaiverPolicyVersionV1 + "," + HTTPSSourceWaiverPolicyVersionV1 + "," + SigstoreProvenancePolicyVersionV1
}

func setCompiledV1() {
	RuntimeBaseRef = "example/base@" + testDigestA
	MaterializerRef = "example/materializer@" + testDigestA
	HomebrewCommit = "0123456789abcdef0123456789abcdef01234567"
	VerificationKeysDigest = metadata.DefaultKeySetDigest()
	PortableRubyVersion = "4.0.6"
}

func testCatalogKeyPolicy(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	policy := &catalogkeys.Policy{
		SchemaVersion:           catalogkeys.SchemaVersion,
		RequiredKeyID:           "catalog-1",
		Keys:                    []catalogkeys.Key{{ID: "catalog-1", Algorithm: "PS512", PublicPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}},
		CatalogServiceDigests:   []string{testDigestA},
		CatalogExtractorDigests: []string{testDigestB},
	}
	data, err := catalogkeys.Canonical(policy)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := catalogkeys.Digest(policy)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(data), digest.String()
}

func setCompiledV2(t *testing.T) {
	t.Helper()
	tapDigest, runtimeDigest := v2PolicyDigests(t)
	BottleFetcherRef = "example/fetcher@" + testDigestA
	CatalogServiceOrigin = "https://catalog.example.com"
	IngestionJWSKeyPolicyBase64, IngestionJWSKeyPolicyDigest = testCatalogKeyPolicy(t)
	TapPolicyDigest = tapDigest
	ExecutableRuntimePolicyDigest = runtimeDigest
	SupportedCatalogPolicyVersions = CatalogPolicyVersionV1
	SupportedFetchPolicyVersions = BottleFetchPolicyVersionV1
	SupportedProvenancePolicyVersions = ChecksumWaiverPolicyVersionV1 + "," + CoreWaiverPolicyVersionV1 + "," + HTTPSSourceWaiverPolicyVersionV1 + "," + SigstoreProvenancePolicyVersionV1
}

func TestRequiresPinnedComponents(t *testing.T) {
	resetCompiledBindings(t)
	_, err := FromBuildOpts(map[string]string{})
	if err == nil {
		t.Fatal("expected missing component error")
	}
}

func TestFromBuildOpts(t *testing.T) {
	resetCompiledBindings(t)
	opts := localV1BuildOpts()
	opts["build-arg:DALEC_SKIP_TESTS"] = "1"
	cfg, err := FromBuildOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SkipTests {
		t.Fatal("skip tests not parsed")
	}
	if cfg.SupportsNonCoreTaps() {
		t.Fatal("V1 local config unexpectedly supports non-core taps")
	}
}

func TestRejectsMismatchedVerificationKeyDigest(t *testing.T) {
	resetCompiledBindings(t)
	opts := localV1BuildOpts()
	opts["build-arg:DALEC_HOMEBREW_KEYS_DIGEST"] = testDigestA
	_, err := FromBuildOpts(opts)
	if err == nil {
		t.Fatal("expected embedded key digest mismatch")
	}
}

func TestReleaseBoundFrontendCannotSkipTests(t *testing.T) {
	resetCompiledBindings(t)
	setCompiledV1()
	_, err := FromBuildOpts(map[string]string{
		"source":                     "example/frontend@" + testDigestA,
		"build-arg:DALEC_SKIP_TESTS": "1",
	})
	if err == nil {
		t.Fatal("expected release-bound test bypass rejection")
	}
}

func TestReleaseBindingsCannotBeOverridden(t *testing.T) {
	resetCompiledBindings(t)
	setCompiledV1()
	_, err := FromBuildOpts(map[string]string{
		"source":                                "example/frontend@" + testDigestA,
		"build-arg:DALEC_HOMEBREW_RUNTIME_BASE": "example/other@" + testDigestB,
		"build-arg:DALEC_HOMEBREW_COMMIT":       "ffffffffffffffffffffffffffffffffffffffff",
	})
	if err == nil {
		t.Fatal("expected release binding mismatch")
	}
}

func TestGatewaySourceMustBePinned(t *testing.T) {
	resetCompiledBindings(t)
	opts := localV1BuildOpts()
	opts["source"] = "example/frontend:latest"
	delete(opts, "build-arg:DALEC_HOMEBREW_FRONTEND_REF")
	_, err := FromBuildOpts(opts)
	if err == nil {
		t.Fatal("expected mutable source rejection")
	}
}

func TestCoreOnlyReleaseRemainsValid(t *testing.T) {
	resetCompiledBindings(t)
	setCompiledV1()
	cfg, err := FromBuildOpts(map[string]string{"source": "example/frontend@" + testDigestA})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SupportsNonCoreTaps() || SupportsCompiledNonCoreTaps() {
		t.Fatal("V1 release unexpectedly advertises non-core support")
	}
}

func TestCoreOnlyReleaseCannotBeUpgradedByBuildArgs(t *testing.T) {
	resetCompiledBindings(t)
	setCompiledV1()
	opts := map[string]string{"source": "example/frontend@" + testDigestA}
	addV2BuildOpts(t, opts)
	cfg, err := FromBuildOpts(opts)
	if err == nil || !strings.Contains(err.Error(), CatalogServiceOriginBuildArg) {
		t.Fatalf("err=%v", err)
	}
	if cfg.SupportsNonCoreTaps() {
		t.Fatal("V2 build args upgraded a core-only release")
	}
}

func TestInvocationFilledV2BindingsDoNotEnableCapability(t *testing.T) {
	resetCompiledBindings(t)
	opts := localV1BuildOpts()
	addV2BuildOpts(t, opts)
	cfg, err := FromBuildOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SupportsNonCoreTaps() {
		t.Fatal("invocation-filled V2 bindings enabled a compiled capability")
	}
	if SupportsCompiledNonCoreTaps() {
		t.Fatal("invocation build opts changed the compiled capability")
	}
	if got, want := cfg.SupportedCatalogPolicyVersions, []string{CatalogPolicyVersionV1}; !slices.Equal(got, want) {
		t.Fatalf("catalog versions=%v, want %v", got, want)
	}
}

func TestCompiledV2BindingsEnableAndLockCapability(t *testing.T) {
	resetCompiledBindings(t)
	setCompiledV1()
	setCompiledV2(t)
	cfg, err := FromBuildOpts(map[string]string{"source": "example/frontend@" + testDigestA})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SupportsNonCoreTaps() || !SupportsCompiledNonCoreTaps() {
		t.Fatal("compiled V2 bindings did not enable capability")
	}
	tampered := cfg
	tampered.CatalogServiceOrigin = "https://other.example.com"
	if tampered.SupportsNonCoreTaps() {
		t.Fatal("configuration changed away from compiled bindings retained capability")
	}

	_, err = FromBuildOpts(map[string]string{
		"source": "example/frontend@" + testDigestA,
		"build-arg:" + CatalogServiceOriginBuildArg: "https://other.example.com",
	})
	if err == nil || !strings.Contains(err.Error(), CatalogServiceOriginBuildArg) {
		t.Fatalf("override err=%v", err)
	}
}

func TestPartialV2BindingsFailClosed(t *testing.T) {
	resetCompiledBindings(t)
	opts := localV1BuildOpts()
	opts["build-arg:"+CatalogServiceOriginBuildArg] = "https://catalog.example.com"
	cfg, err := FromBuildOpts(opts)
	if err == nil || !strings.Contains(err.Error(), "bottle fetcher") {
		t.Fatalf("err=%v", err)
	}
	if cfg.SupportsNonCoreTaps() {
		t.Fatal("partial V2 bindings enabled capability")
	}
}

func TestCompiledV2RejectsMismatchedEmbeddedPolicyDigests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func()
		want   string
	}{
		{name: "tap", mutate: func() { TapPolicyDigest = testDigestA }, want: "embedded V2 tap policy"},
		{name: "runtime", mutate: func() { ExecutableRuntimePolicyDigest = testDigestA }, want: "embedded V2 policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCompiledBindings(t)
			setCompiledV1()
			setCompiledV2(t)
			tt.mutate()
			cfg, err := FromBuildOpts(map[string]string{"source": "example/frontend@" + testDigestA})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v", err)
			}
			if cfg.SupportsNonCoreTaps() || SupportsCompiledNonCoreTaps() {
				t.Fatal("mismatched compiled policy digest enabled capability")
			}
		})
	}
}

func TestCompiledV2RejectsUnsupportedPolicyVersionSets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func()
		want   string
	}{
		{name: "catalog", mutate: func() { SupportedCatalogPolicyVersions = "other-catalog-v1" }, want: CatalogPolicyVersionV1},
		{name: "fetch", mutate: func() { SupportedFetchPolicyVersions = "other-fetch-v1" }, want: BottleFetchPolicyVersionV1},
		{name: "provenance", mutate: func() { SupportedProvenancePolicyVersions = ChecksumWaiverPolicyVersionV1 }, want: SigstoreProvenancePolicyVersionV1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCompiledBindings(t)
			setCompiledV1()
			setCompiledV2(t)
			tt.mutate()
			cfg, err := FromBuildOpts(map[string]string{"source": "example/frontend@" + testDigestA})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v", err)
			}
			if cfg.SupportsNonCoreTaps() || SupportsCompiledNonCoreTaps() {
				t.Fatal("unsupported compiled policy set enabled capability")
			}
		})
	}
}

func TestV2BindingsRejectInvalidOriginAndPolicyLists(t *testing.T) {
	resetCompiledBindings(t)
	opts := localV1BuildOpts()
	addV2BuildOpts(t, opts)
	opts["build-arg:"+CatalogServiceOriginBuildArg] = "https://catalog.example.com/path"
	opts["build-arg:"+SupportedFetchPolicyVersionsBuildArg] = "fetch-v1,fetch-v1"
	cfg, err := FromBuildOpts(opts)
	if err == nil || !strings.Contains(err.Error(), "catalog service origin") || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("err=%v", err)
	}
	if cfg.SupportsNonCoreTaps() {
		t.Fatal("invalid V2 bindings enabled capability")
	}
}

func TestCompiledV2RequiresCompiledCoreComponentTuple(t *testing.T) {
	resetCompiledBindings(t)
	setCompiledV2(t)
	if SupportsCompiledNonCoreTaps() {
		t.Fatal("V2-only bindings enabled capability without compiled runtime/materializer tuple")
	}
}
