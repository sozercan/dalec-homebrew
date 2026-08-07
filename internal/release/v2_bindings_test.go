package release

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func TestGenerateV2Bindings(t *testing.T) {
	input := testV2BindingsInput(t)
	bindings, keyPolicyData, err := GenerateV2Bindings(input)
	if err != nil {
		t.Fatal(err)
	}

	if bindings.SchemaVersion != V2BindingsSchemaVersion {
		t.Fatalf("schema version = %q", bindings.SchemaVersion)
	}
	if got, want := bindings.SupportedCatalogPolicyVersions, "tap-catalog-v1"; got != want {
		t.Fatalf("catalog policy versions = %q, want %q", got, want)
	}
	if got, want := bindings.SupportedFetchPolicyVersions, "homebrew-bottle-fetch-v1"; got != want {
		t.Fatalf("fetch policy versions = %q, want %q", got, want)
	}
	if got, want := bindings.SupportedProvenancePolicyVersions, "homebrew-jws-and-verified-oci-chain-v1,https-bottle-embedded-formula-digest-only-v1,prebuilt-archive-buildkit-and-verified-checksum-v1,sigstore-in-toto-v1,tap-catalog-buildkit-and-verified-checksum-v1"; got != want {
		t.Fatalf("provenance policy versions = %q, want %q", got, want)
	}

	tapDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	if bindings.TapPolicyDigest != tapDigest {
		t.Fatalf("tap policy digest = %q, want %q", bindings.TapPolicyDigest, tapDigest)
	}
	runtimeDigest, err := policyv2.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if bindings.ExecutableRuntimePolicyDigest != runtimeDigest {
		t.Fatalf("runtime policy digest = %q, want %q", bindings.ExecutableRuntimePolicyDigest, runtimeDigest)
	}

	decodedPolicyData, err := base64.StdEncoding.Strict().DecodeString(bindings.IngestionJWSKeyPolicyBase64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decodedPolicyData, keyPolicyData) {
		t.Fatal("base64 key policy differs from returned canonical policy")
	}
	if got := digest.FromBytes(keyPolicyData).String(); got != bindings.IngestionJWSKeyPolicyDigest {
		t.Fatalf("key policy digest = %q, want %q", bindings.IngestionJWSKeyPolicyDigest, got)
	}
	keyPolicy, err := catalogkeys.Decode(keyPolicyData)
	if err != nil {
		t.Fatalf("decode generated key policy: %v", err)
	}
	if keyPolicy.RequiredKeyID != input.KeyID || len(keyPolicy.Keys) != 1 || keyPolicy.Keys[0].ID != input.KeyID || keyPolicy.Keys[0].Algorithm != catalog.CatalogSetJWSAlgorithm {
		t.Fatalf("unexpected key policy identity: %+v", keyPolicy)
	}
	if len(keyPolicy.CatalogServiceDigests) != 1 || keyPolicy.CatalogServiceDigests[0] != input.CatalogServiceDigest {
		t.Fatalf("service digests = %v", keyPolicy.CatalogServiceDigests)
	}
	if len(keyPolicy.CatalogExtractorDigests) != 1 || keyPolicy.CatalogExtractorDigests[0] != input.CatalogExtractorDigest {
		t.Fatalf("extractor digests = %v", keyPolicy.CatalogExtractorDigests)
	}

	first, err := CanonicalV2Bindings(bindings)
	if err != nil {
		t.Fatal(err)
	}
	secondBindings, secondPolicy, err := GenerateV2Bindings(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalV2Bindings(secondBindings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || !bytes.Equal(keyPolicyData, secondPolicy) {
		t.Fatal("V2 binding generation is not deterministic for identical inputs")
	}
	if bytes.HasSuffix(first, []byte("\n")) {
		t.Fatal("canonical V2 bindings have a trailing newline")
	}
	var roundTrip V2Bindings
	if err := json.Unmarshal(first, &roundTrip); err != nil {
		t.Fatal(err)
	}
	roundTripData, err := CanonicalV2Bindings(&roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, roundTripData) {
		t.Fatal("canonical V2 bindings changed after JSON round trip")
	}
}

func TestGenerateV2BindingsRejectsInvalidInputs(t *testing.T) {
	valid := testV2BindingsInput(t)
	tests := []struct {
		name   string
		mutate func(*V2BindingsInput)
		want   string
	}{
		{name: "unsafe key ID", mutate: func(input *V2BindingsInput) { input.KeyID = "catalog key" }, want: "required_key_id"},
		{name: "invalid public key", mutate: func(input *V2BindingsInput) { input.PublicKeyPEM = []byte("not PEM") }, want: "PEM"},
		{name: "invalid service digest", mutate: func(input *V2BindingsInput) { input.CatalogServiceDigest = "sha256:not-a-digest" }, want: "catalog_service_digests"},
		{name: "invalid extractor digest", mutate: func(input *V2BindingsInput) { input.CatalogExtractorDigest = "sha512:" + strings.Repeat("a", 128) }, want: "catalog_extractor_digests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := valid
			input.PublicKeyPEM = bytes.Clone(valid.PublicKeyPEM)
			tt.mutate(&input)
			if _, _, err := GenerateV2Bindings(input); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateV2BindingsRejectsTampering(t *testing.T) {
	bindings, _, err := GenerateV2Bindings(testV2BindingsInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateV2Bindings(nil); err == nil {
		t.Fatal("nil bindings accepted")
	}

	tests := []struct {
		name   string
		mutate func(*V2Bindings)
		want   string
	}{
		{name: "schema", mutate: func(value *V2Bindings) { value.SchemaVersion = "other" }, want: "schema"},
		{name: "key policy digest", mutate: func(value *V2Bindings) { value.IngestionJWSKeyPolicyDigest = "sha256:" + strings.Repeat("f", 64) }, want: "does not match"},
		{name: "key policy base64", mutate: func(value *V2Bindings) { value.IngestionJWSKeyPolicyBase64 += "\n" }, want: "base64"},
		{name: "tap policy", mutate: func(value *V2Bindings) { value.TapPolicyDigest = "sha256:" + strings.Repeat("f", 64) }, want: "embedded policy"},
		{name: "runtime policy", mutate: func(value *V2Bindings) { value.ExecutableRuntimePolicyDigest = "sha256:" + strings.Repeat("f", 64) }, want: "embedded policy"},
		{name: "catalog versions", mutate: func(value *V2Bindings) { value.SupportedCatalogPolicyVersions = "other" }, want: "catalog"},
		{name: "fetch versions", mutate: func(value *V2Bindings) { value.SupportedFetchPolicyVersions = "other" }, want: "fetch"},
		{name: "provenance versions", mutate: func(value *V2Bindings) { value.SupportedProvenancePolicyVersions = "other" }, want: "provenance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := *bindings
			tt.mutate(&value)
			if err := ValidateV2Bindings(&value); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if _, err := CanonicalV2Bindings(&value); err == nil {
				t.Fatal("canonicalization accepted tampered bindings")
			}
		})
	}
}

func testV2BindingsInput(t *testing.T) V2BindingsInput {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return V2BindingsInput{
		KeyID:                  "catalog-e2e",
		PublicKeyPEM:           pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKey}),
		CatalogServiceDigest:   "sha256:" + strings.Repeat("a", 64),
		CatalogExtractorDigest: "sha256:" + strings.Repeat("b", 64),
	}
}

func TestGenerateBuildLocalV2Bindings(t *testing.T) {
	ref := "ghcr.io/example/catalog-extractor@sha256:" + strings.Repeat("c", 64)
	bindings, err := GenerateBuildLocalV2Bindings(ref)
	if err != nil {
		t.Fatal(err)
	}
	if bindings.CatalogExtractorRef != ref || bindings.IngestionJWSKeyPolicyDigest != "" || bindings.IngestionJWSKeyPolicyBase64 != "" {
		t.Fatalf("build-local bindings = %+v", bindings)
	}
	if err := ValidateV2Bindings(bindings); err != nil {
		t.Fatal(err)
	}
	mutated := *bindings
	mutated.CatalogExtractorRef = ""
	if err := ValidateV2Bindings(&mutated); err == nil {
		t.Fatal("missing build-local extractor accepted")
	}
}
