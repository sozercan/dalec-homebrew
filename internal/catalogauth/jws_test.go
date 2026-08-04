package catalogauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

func testPayload(t *testing.T, now time.Time) *catalog.CatalogSetPayload {
	t.Helper()
	id, err := catalog.ParseTapID("acme/tools")
	if err != nil {
		t.Fatal(err)
	}
	root, err := catalog.ParseFormulaID("acme/tools/widget")
	if err != nil {
		t.Fatal(err)
	}
	d := "sha256:" + strings.Repeat("a", 64)
	archive := "sha256:" + strings.Repeat("b", 64)
	return &catalog.CatalogSetPayload{
		SchemaVersion: catalog.CatalogSetSchemaVersion, RequestDigest: d, CoreSnapshotDigest: archive,
		GeneratedAt: now, ExpiresAt: now.Add(time.Hour),
		CatalogService: catalog.ComponentIdentity{Name: "catalog-service", Version: "test", Digest: d},
		Extractor:      catalog.ComponentIdentity{Name: "catalog-extractor", Version: "test", Digest: archive},
		Catalogs:       []catalog.CatalogReference{{Tap: catalog.TapSource{ID: id, Repository: "https://github.com/acme/homebrew-tools", Commit: strings.Repeat("c", 40), TreeDigest: d, ArchiveDigest: archive}, PublishedAt: now, Sequence: 1, URL: "https://catalog.example/v1/catalogs/sha256/" + strings.Repeat("d", 64), Size: 1, SHA256: "sha256:" + strings.Repeat("d", 64)}},
		Results:        []catalog.PlatformResult{{Platform: catalog.Platform{OS: "linux", Architecture: "amd64"}, Closure: catalog.ClosureResult{Requested: []catalog.FormulaID{root}, Nodes: []catalog.Node{{ID: root, Tap: id, Name: "widget", HomebrewFullName: "acme/tools/widget", FormulaVersion: "1", PkgVersion: "1"}}, InstallOrder: []catalog.FormulaID{root}}, Artifacts: []catalog.BottleArtifact{{ID: root, Platform: catalog.Platform{OS: "linux", Architecture: "amd64"}, Tag: "x86_64_linux", Filename: "widget--1.x86_64_linux.bottle.tar.gz", SHA256: "sha256:" + strings.Repeat("e", 64), Size: 1, Cellar: ":any", Tab: catalog.BottleTab{Arch: "x86_64"}, CurrentFormulaSourceDigest: d, BottleFormulaSourceDigest: archive, BottleSourceWaiver: catalog.HTTPSBottleSourceWaiver, Transport: catalog.Transport{HTTPS: &catalog.HTTPSTransport{URL: "https://bottles.example/widget.tgz", ExpectedSize: 1, SHA256: "sha256:" + strings.Repeat("e", 64), Filename: "widget--1.x86_64_linux.bottle.tar.gz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: catalog.HTTPSFetchPolicyVersion}}, Verification: catalog.BottleVerification{PolicyVersion: catalog.BottleVerificationPolicy, InventoryDigest: d, EntryCount: 1, ExpandedSize: 1}, Provenance: catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: catalog.ChecksumProvenanceWaiver}}}}}},
	}
}

func TestSignAndVerifyBindings(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := metadata.NewKeySet(map[string]*rsa.PublicKey{"catalog-1": &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	payload := testPayload(t, now)
	envelope, err := Sign(payload, "catalog-1", key)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Verify(envelope, keys, "catalog-1", payload.RequestDigest, payload.CoreSnapshotDigest, now.Add(time.Minute), false)
	if err != nil {
		t.Fatal(err)
	}
	if set.Payload.RequestDigest != payload.RequestDigest || len(set.Signatures) != 1 {
		t.Fatalf("verified=%+v", set)
	}
	if _, err := Verify(envelope, keys, "catalog-1", "sha256:"+strings.Repeat("f", 64), payload.CoreSnapshotDigest, now.Add(time.Minute), false); err == nil {
		t.Fatal("request substitution accepted")
	}
	if _, err := Verify(envelope, keys, "catalog-1", payload.RequestDigest, payload.CoreSnapshotDigest, payload.ExpiresAt, false); err == nil {
		t.Fatal("expired set accepted")
	}
}

func TestVerifyAcceptedSupportsOverlapSignerRotation(t *testing.T) {
	oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := metadata.NewKeySet(map[string]*rsa.PublicKey{"catalog-old": &oldKey.PublicKey, "catalog-new": &newKey.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	payload := testPayload(t, now)
	envelope, err := Sign(payload, "catalog-new", newKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(envelope, keys, "catalog-old", payload.RequestDigest, payload.CoreSnapshotDigest, now.Add(time.Minute), false); err == nil {
		t.Fatal("new signer satisfied old-only policy")
	}
	verified, err := VerifyAccepted(envelope, keys, []string{"catalog-old", "catalog-new"}, payload.RequestDigest, payload.CoreSnapshotDigest, now.Add(time.Minute), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified.Signatures) != 1 || verified.Signatures[0].KeyID != "catalog-new" {
		t.Fatalf("signatures = %+v", verified.Signatures)
	}
}

func TestLoadSigningKeyRejectsPermissions(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	filename := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigningKey(filename); err == nil {
		t.Fatal("insecure key permissions accepted")
	}
	if err := os.Chmod(filename, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigningKey(filename); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRequiresRequestAndCoreBindings(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := metadata.NewKeySet(map[string]*rsa.PublicKey{"catalog-1": &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	payload := testPayload(t, now)
	envelope, err := Sign(payload, "catalog-1", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(envelope, keys, "catalog-1", "", payload.CoreSnapshotDigest, now.Add(time.Minute), false); err == nil {
		t.Fatal("empty request binding accepted")
	}
}

func TestSignRejectsUndersizedPS512Key(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sign(testPayload(t, time.Unix(1_800_000_000, 0).UTC()), "catalog-1", key); err == nil {
		t.Fatal("undersized PS512 signer accepted")
	}
}
