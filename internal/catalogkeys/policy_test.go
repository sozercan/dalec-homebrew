package catalogkeys

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

func testPolicy(t *testing.T) *Policy {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	d1 := "sha256:" + strings.Repeat("a", 64)
	d2 := "sha256:" + strings.Repeat("b", 64)
	return &Policy{SchemaVersion: SchemaVersion, RequiredKeyID: "catalog-1", Keys: []Key{{ID: "catalog-1", Algorithm: "PS512", PublicPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}))}}, CatalogServiceDigests: []string{d1}, CatalogExtractorDigests: []string{d2}}
}

func TestPolicyCanonicalDigestAndKeySet(t *testing.T) {
	policy := testPolicy(t)
	first, err := Canonical(policy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Canonical(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("canonical policy changed after decode")
	}
	if _, err := decoded.KeySet(); err != nil {
		t.Fatal(err)
	}
	if value, err := Digest(decoded); err != nil || value == "" {
		t.Fatalf("digest=%q err=%v", value, err)
	}
}

func TestOverlapKeysSupportSignerRotation(t *testing.T) {
	policy := testPolicy(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	policy.Keys = append(policy.Keys, Key{ID: "catalog-2", Algorithm: "PS512", PublicPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}))})
	policy.OverlapKeyIDs = []string{"catalog-2"}
	data, err := Canonical(policy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	accepted := decoded.AcceptedKeyIDs()
	if len(accepted) != 2 || accepted[0] != "catalog-1" || accepted[1] != "catalog-2" {
		t.Fatalf("accepted key IDs = %v", accepted)
	}

	policy.OverlapKeyIDs = []string{"catalog-1"}
	if err := Validate(policy); err == nil {
		t.Fatal("required key accepted as an overlap key")
	}
	policy.OverlapKeyIDs = []string{"catalog-missing"}
	if err := Validate(policy); err == nil {
		t.Fatal("missing overlap key accepted")
	}
}

func TestAuthorizePayloadBindsComponentIdentities(t *testing.T) {
	policy := testPolicy(t)
	payload := &catalog.CatalogSetPayload{CatalogService: catalog.ComponentIdentity{Digest: policy.CatalogServiceDigests[0]}, Extractor: catalog.ComponentIdentity{Digest: policy.CatalogExtractorDigests[0]}}
	if err := policy.AuthorizePayload(payload); err != nil {
		t.Fatal(err)
	}
	payload.Extractor.Digest = "sha256:" + strings.Repeat("c", 64)
	if err := policy.AuthorizePayload(payload); err == nil {
		t.Fatal("unauthorized extractor accepted")
	}
}

func TestDecodeRejectsDuplicateMembers(t *testing.T) {
	if _, err := Decode([]byte(`{"schema_version":"x","schema_version":"y"}`)); err == nil {
		t.Fatal("duplicate member accepted")
	}
}

func TestSequenceFloorsAreCanonicalAndTapScoped(t *testing.T) {
	policy := testPolicy(t)
	policy.SequenceFloors = []SequenceFloor{{Tap: "other/lib", Minimum: 4}, {Tap: "acme/tools", Minimum: 2}}
	data, err := Canonical(policy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	floors := decoded.MinimumSequences()
	if floors["acme/tools"] != 2 || floors["other/lib"] != 4 {
		t.Fatalf("floors=%v", floors)
	}
	policy.SequenceFloors = []SequenceFloor{{Tap: "homebrew/core", Minimum: 1}}
	if err := Validate(policy); err == nil {
		t.Fatal("core sequence floor accepted")
	}
}

func TestRejectsUndersizedPS512Key(t *testing.T) {
	policy := testPolicy(t)
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	policy.Keys[0].PublicPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if err := Validate(policy); err == nil {
		t.Fatal("undersized PS512 key accepted")
	}
}

func TestRejectsDuplicatePublicKeyMaterialAcrossRoles(t *testing.T) {
	policy := testPolicy(t)
	duplicate := policy.Keys[0]
	duplicate.ID = "catalog-old"
	policy.Keys = append(policy.Keys, duplicate)
	if err := Validate(policy); err == nil {
		t.Fatal("duplicate public-key material accepted under another key ID")
	}
}

func TestDecodeRejectsCaseFoldedPolicyFields(t *testing.T) {
	data, err := Canonical(testPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	aliased := strings.Replace(string(data), `"required_key_id":`, `"REQUIRED_KEY_ID":`, 1)
	if _, err := Decode([]byte(aliased)); err == nil {
		t.Fatal("case-folded policy field accepted")
	}
}
