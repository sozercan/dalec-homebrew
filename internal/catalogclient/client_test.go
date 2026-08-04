package catalogclient

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogauth"
	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type rewriteTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (r rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host
	return r.base.RoundTrip(clone)
}

func fixture(t *testing.T, origin string, now time.Time, key *rsa.PrivateKey) (*catalog.Request, []byte, []byte) {
	t.Helper()
	tap, err := catalog.ParseTapID("acme/tools")
	if err != nil {
		t.Fatal(err)
	}
	root, err := catalog.ParseFormulaID("acme/tools/widget")
	if err != nil {
		t.Fatal(err)
	}
	d := "sha256:" + strings.Repeat("a", 64)
	archive := "sha256:" + strings.Repeat("b", 64)
	bottleDigest := "sha256:" + strings.Repeat("e", 64)
	source := catalog.TapSource{ID: tap, Repository: "https://github.com/acme/homebrew-tools", Commit: strings.Repeat("c", 40), TreeDigest: d, ArchiveDigest: archive}
	document := &catalog.TapCatalog{SchemaVersion: catalog.TapCatalogSchemaVersion, Tap: source, PublishedAt: now, Sequence: 1, Formulae: []catalog.Formula{{ID: root, Name: "widget", HomebrewFullName: "acme/tools/widget", SourcePath: "Formula/widget.rb", SourceDigest: d, StableVersion: "1", License: "MIT", Bottle: &catalog.BottleDeclaration{RootURL: "https://bottles.example", Files: []catalog.BottleFile{{Tag: "x86_64_linux", URL: "https://bottles.example/widget.tgz", SHA256: bottleDigest, Cellar: ":any"}}}}}}
	documentBytes, err := catalog.CanonicalTapCatalog(document)
	if err != nil {
		t.Fatal(err)
	}
	docSum := sha256.Sum256(documentBytes)
	docDigest := "sha256:" + hex.EncodeToString(docSum[:])
	request := &catalog.Request{SchemaVersion: catalog.RequestSchemaVersion, ExternalRoots: []catalog.FormulaID{root}, Platforms: []catalog.Platform{{OS: "linux", Architecture: "amd64"}}, HomebrewCommit: strings.Repeat("f", 40), CoreSnapshotDigest: archive}
	requestDigest, err := catalog.RequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	payload := &catalog.CatalogSetPayload{
		SchemaVersion: catalog.CatalogSetSchemaVersion, RequestDigest: requestDigest.String(), CoreSnapshotDigest: request.CoreSnapshotDigest, GeneratedAt: now, ExpiresAt: now.Add(time.Hour),
		CatalogService: catalog.ComponentIdentity{Name: "catalog-service", Version: "test", Digest: d}, Extractor: catalog.ComponentIdentity{Name: "catalog-extractor", Version: "test", Digest: archive},
		Catalogs: []catalog.CatalogReference{{Tap: source, PublishedAt: now, Sequence: 1, URL: origin + "/v1/catalogs/sha256/" + strings.TrimPrefix(docDigest, "sha256:"), Size: int64(len(documentBytes)), SHA256: docDigest}},
		Results:  []catalog.PlatformResult{{Platform: request.Platforms[0], Closure: catalog.ClosureResult{Requested: []catalog.FormulaID{root}, Nodes: []catalog.Node{{ID: root, Tap: tap, Name: "widget", HomebrewFullName: "acme/tools/widget", FormulaVersion: "1", PkgVersion: "1", License: "MIT"}}, InstallOrder: []catalog.FormulaID{root}}, Artifacts: []catalog.BottleArtifact{{ID: root, Platform: request.Platforms[0], Tag: "x86_64_linux", Filename: "widget--1.x86_64_linux.bottle.tar.gz", SHA256: bottleDigest, Size: 1, Cellar: ":any", Tab: catalog.BottleTab{Arch: "x86_64"}, CurrentFormulaSourceDigest: d, BottleFormulaSourceDigest: archive, BottleSourceWaiver: catalog.HTTPSBottleSourceWaiver, Transport: catalog.Transport{HTTPS: &catalog.HTTPSTransport{URL: "https://bottles.example/widget.tgz", ExpectedSize: 1, SHA256: bottleDigest, Filename: "widget--1.x86_64_linux.bottle.tar.gz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: catalog.HTTPSFetchPolicyVersion}}, Verification: catalog.BottleVerification{PolicyVersion: catalog.BottleVerificationPolicy, InventoryDigest: d, EntryCount: 1, ExpandedSize: 1}, Provenance: catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: catalog.ChecksumProvenanceWaiver}}}}}},
	}
	envelope, err := catalogauth.Sign(payload, "catalog-1", key)
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest, err := catalog.CatalogSetPayloadDigest(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := catalog.CatalogSetResult{SchemaVersion: catalog.ResultSchemaVersion, RequestDigest: requestDigest.String(), PayloadDigest: payloadDigest.String(), JWS: envelope}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return request, documentBytes, resultBytes
}

func TestResolveCompletedResultAndCatalog(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	var request *catalog.Request
	var document, result []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/catalog-sets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(result)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/catalogs/sha256/"):
			w.Header().Set("Content-Length", strconv.Itoa(len(document)))
			_, _ = w.Write(document)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	request, document, result = fixture(t, "https://catalog.example", now, key)
	keys, err := metadata.NewKeySet(map[string]*rsa.PublicKey{"catalog-1": &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := server.Client()
	httpClient.Transport = rewriteTransport{base: httpClient.Transport, target: target}
	client, err := New(Config{Origin: "https://catalog.example", HTTPClient: httpClient, Keys: keys, RequiredKeyID: "catalog-1", Now: func() time.Time { return now.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := client.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Catalogs) != 1 || resolved.Payload.RequestDigest == "" {
		t.Fatalf("resolved=%+v", resolved)
	}
}

func TestClientKeyPolicyAcceptsOverlapSigner(t *testing.T) {
	oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encodePublic := func(key *rsa.PublicKey) string {
		der, err := x509.MarshalPKIXPublicKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	}
	policy := &catalogkeys.Policy{
		SchemaVersion: catalogkeys.SchemaVersion,
		RequiredKeyID: "catalog-old",
		OverlapKeyIDs: []string{"catalog-new"},
		Keys: []catalogkeys.Key{
			{ID: "catalog-old", Algorithm: "PS512", PublicPEM: encodePublic(&oldKey.PublicKey)},
			{ID: "catalog-new", Algorithm: "PS512", PublicPEM: encodePublic(&newKey.PublicKey)},
		},
		CatalogServiceDigests:   []string{"sha256:" + strings.Repeat("a", 64)},
		CatalogExtractorDigests: []string{"sha256:" + strings.Repeat("b", 64)},
	}
	client, err := New(Config{Origin: "https://catalog.example", KeyPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.acceptedKeyIDs) != 2 || client.acceptedKeyIDs[0] != "catalog-old" || client.acceptedKeyIDs[1] != "catalog-new" {
		t.Fatalf("accepted key IDs = %v", client.acceptedKeyIDs)
	}
}

func TestResolveFailedOperation(t *testing.T) {
	operation := catalog.Operation{SchemaVersion: catalog.OperationSchemaVersion, ID: "op-1", Status: catalog.OperationFailed, Failure: &catalog.Failure{Code: catalog.FailureInvalidTap, Message: "tap missing"}}
	data, _ := json.Marshal(operation)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(data)
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := server.Client()
	httpClient.Transport = rewriteTransport{base: httpClient.Transport, target: target}
	client, err := New(Config{Origin: "https://catalog.example", HTTPClient: httpClient, RequiredKeyID: "catalog-1"})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := catalog.ParseFormulaID("acme/tools/widget")
	request := &catalog.Request{SchemaVersion: catalog.RequestSchemaVersion, ExternalRoots: []catalog.FormulaID{root}, Platforms: []catalog.Platform{{OS: "linux", Architecture: "amd64"}}, HomebrewCommit: strings.Repeat("f", 40), CoreSnapshotDigest: "sha256:" + strings.Repeat("a", 64)}
	_, err = client.Resolve(t.Context(), request)
	operationErr, ok := err.(*OperationError)
	if !ok || operationErr.Code != catalog.FailureInvalidTap {
		t.Fatalf("err=%T %v", err, err)
	}
}

func TestDefaultPollingDeadline(t *testing.T) {
	t.Parallel()
	if DefaultPollingDeadline != 30*time.Minute {
		t.Fatalf("DefaultPollingDeadline = %s, want 30m", DefaultPollingDeadline)
	}
	client, err := New(Config{Origin: "https://catalog.example", RequiredKeyID: "catalog-1"})
	if err != nil {
		t.Fatal(err)
	}
	if client.pollingDeadline != 30*time.Minute {
		t.Fatalf("polling deadline = %s, want 30m", client.pollingDeadline)
	}
}

func TestOriginMustBeHTTPS(t *testing.T) {
	if _, err := New(Config{Origin: "http://catalog.example", RequiredKeyID: "catalog-1"}); err == nil {
		t.Fatal("HTTP origin accepted")
	}
}

func TestClientRejectsCatalogRedirects(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.example/v1/catalog-sets", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	httpClient := server.Client()
	httpClient.Transport = rewriteTransport{base: httpClient.Transport, target: target}
	client, err := New(Config{Origin: "https://catalog.example", HTTPClient: httpClient, RequiredKeyID: "catalog-1"})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := catalog.ParseFormulaID("acme/tools/widget")
	request := &catalog.Request{SchemaVersion: catalog.RequestSchemaVersion, Targets: []catalog.PlatformRequest{{Platform: catalog.Platform{OS: "linux", Architecture: "amd64"}, ExternalRoots: []catalog.FormulaID{root}}}, HomebrewCommit: strings.Repeat("f", 40), CoreSnapshotDigest: "sha256:" + strings.Repeat("a", 64)}
	if _, err := client.Resolve(t.Context(), request); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("err=%v", err)
	}
}
