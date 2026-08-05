package catalogservice

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogartifactstore"
	"github.com/sozercan/dalec-homebrew/internal/catalogauth"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type lockedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lockedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lockedClock) Add(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func testDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}

func testServiceArtifact(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write([]byte("service-hosted generated artifact")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func testRequest(t *testing.T) *catalog.Request {
	t.Helper()
	root, err := catalog.ParseFormulaID("acme/tools/widget")
	if err != nil {
		t.Fatal(err)
	}
	return &catalog.Request{
		SchemaVersion:      catalog.RequestSchemaVersion,
		ExternalRoots:      []catalog.FormulaID{root},
		Platforms:          []catalog.Platform{{OS: "linux", Architecture: "amd64"}},
		HomebrewCommit:     strings.Repeat("f", 40),
		CoreSnapshotDigest: testDigest('a'),
	}
}

func testGeneratedSet(t *testing.T) *GeneratedSet {
	t.Helper()
	tap, err := catalog.ParseTapID("acme/tools")
	if err != nil {
		t.Fatal(err)
	}
	root, err := catalog.ParseFormulaID("acme/tools/widget")
	if err != nil {
		t.Fatal(err)
	}
	platform := catalog.Platform{OS: "linux", Architecture: "amd64"}
	bottleDigest := testDigest('e')
	source := catalog.TapSource{
		ID:            tap,
		Repository:    "https://github.com/acme/homebrew-tools",
		Commit:        strings.Repeat("c", 40),
		TreeDigest:    testDigest('b'),
		ArchiveDigest: testDigest('c'),
	}
	return &GeneratedSet{
		Catalogs: []catalog.TapCatalog{{
			SchemaVersion: catalog.TapCatalogSchemaVersion,
			Tap:           source,
			Formulae: []catalog.Formula{{
				ID:               root,
				Name:             "widget",
				HomebrewFullName: "acme/tools/widget",
				SourcePath:       "Formula/widget.rb",
				SourceDigest:     testDigest('d'),
				StableVersion:    "1.0",
				License:          "MIT",
				Bottle: &catalog.BottleDeclaration{
					RootURL: "https://bottles.example/widget",
					Files: []catalog.BottleFile{{
						Tag: "x86_64_linux", URL: "https://bottles.example/widget.tar.gz", SHA256: bottleDigest, Cellar: ":any",
					}},
				},
			}},
		}},
		Results: []catalog.PlatformResult{{
			Platform: platform,
			Closure: catalog.ClosureResult{
				Requested: []catalog.FormulaID{root},
				Nodes: []catalog.Node{{
					ID: root, Tap: tap, Name: "widget", HomebrewFullName: "acme/tools/widget", FormulaVersion: "1.0", PkgVersion: "1.0", License: "MIT",
				}},
				InstallOrder: []catalog.FormulaID{root},
			},
			Artifacts: []catalog.BottleArtifact{{
				ID: root, Platform: platform, Tag: "x86_64_linux", Filename: "widget--1.0.x86_64_linux.bottle.tar.gz",
				SHA256: bottleDigest, Size: 1, Cellar: ":any", Tab: catalog.BottleTab{Arch: "x86_64"}, CurrentFormulaSourceDigest: testDigest('d'), BottleFormulaSourceDigest: testDigest('c'), BottleSourceWaiver: catalog.HTTPSBottleSourceWaiver,
				Transport: catalog.Transport{HTTPS: &catalog.HTTPSTransport{
					URL: "https://bottles.example/widget.tar.gz", ExpectedSize: 1, SHA256: bottleDigest,
					Filename: "widget--1.0.x86_64_linux.bottle.tar.gz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: catalog.HTTPSFetchPolicyVersion,
				}},
				Verification: catalog.BottleVerification{PolicyVersion: catalog.BottleVerificationPolicy, InventoryDigest: testDigest('b'), EntryCount: 1, ExpandedSize: 1},
				Provenance:   catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: catalog.ChecksumProvenanceWaiver}},
			}},
		}},
	}
}

func writeSigningKey(t *testing.T, directory string) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	filename := filepath.Join(directory, "signing-key.pem")
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return filename, key
}

func testConfig(storeDir, keyPath string, generator Generator, clock *lockedClock) Config {
	return Config{
		StoreDir:           storeDir,
		Origin:             "https://catalog.example",
		Generator:          generator,
		SigningKeyPath:     keyPath,
		SigningKeyID:       "catalog-test-1",
		CatalogService:     catalog.ComponentIdentity{Name: "catalog-service", Version: "test", Digest: testDigest('1')},
		Extractor:          catalog.ComponentIdentity{Name: "catalog-extractor", Version: "test", Digest: testDigest('2')},
		CatalogSetLifetime: time.Hour,
		GenerationTimeout:  5 * time.Second,
		RetryAfter:         time.Second,
		Now:                clock.Now,
	}
}

func postCatalogSet(t *testing.T, service http.Handler, request *catalog.Request) *httptest.ResponseRecorder {
	t.Helper()
	data, err := catalog.CanonicalRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "https://service.test/v1/catalog-sets", bytes.NewReader(data))
	httpRequest.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, httpRequest)
	return response
}

func getOperation(t *testing.T, service http.Handler, id string) (*catalog.Operation, *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://service.test/v1/operations/"+id, nil)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET operation status=%d body=%s", response.Code, response.Body.String())
	}
	operation, err := catalog.DecodeOperation(response.Body.Bytes())
	if err != nil {
		t.Fatalf("decode operation: %v\n%s", err, response.Body.String())
	}
	return operation, response
}

func waitOperation(t *testing.T, service http.Handler, id string, want catalog.OperationStatus) *catalog.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		operation, _ := getOperation(t, service, id)
		if operation.Status == want {
			return operation
		}
		if operation.Status != catalog.OperationPending {
			t.Fatalf("operation status=%s, want %s; failure=%+v", operation.Status, want, operation.Failure)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for operation %s", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestArtifactHTTPGetHeadPersistsAcrossRestartAndFailsClosedOnCorruption(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)}
	artifactStore, err := catalogartifactstore.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	data := testServiceArtifact(t)
	expected := digest.FromBytes(data)
	if err := artifactStore.Put(expected, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	generator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) {
		t.Fatal("artifact requests must not invoke the catalog generator")
		return nil, nil
	})
	config := testConfig(storeDir, keyPath, generator, clock)
	config.ArtifactStore = artifactStore
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}

	path := catalogartifactstore.HTTPPathPrefix + expected.Encoded()
	assertArtifact := func(t *testing.T, handler http.Handler, method string) {
		t.Helper()
		request := httptest.NewRequest(method, "https://service.test"+path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s artifact status=%d body=%s", method, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Length"); got != fmt.Sprint(len(data)) {
			t.Fatalf("%s content-length=%q", method, got)
		}
		if got := response.Header().Get("Content-Type"); got != "application/gzip" {
			t.Fatalf("%s content-type=%q", method, got)
		}
		if got := response.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("%s cache-control=%q", method, got)
		}
		if got := response.Header().Get("ETag"); got != `"`+expected.String()+`"` {
			t.Fatalf("%s etag=%q", method, got)
		}
		if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s nosniff=%q", method, got)
		}
		if method == http.MethodHead {
			if response.Body.Len() != 0 {
				t.Fatalf("HEAD returned %d body bytes", response.Body.Len())
			}
		} else if !bytes.Equal(response.Body.Bytes(), data) {
			t.Fatal("GET artifact bytes differ")
		}
	}
	assertArtifact(t, service, http.MethodGet)
	assertArtifact(t, service, http.MethodHead)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	restartedStore, err := catalogartifactstore.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	restartedConfig := testConfig(storeDir, keyPath, generator, clock)
	restartedConfig.ArtifactStore = restartedStore
	restarted, err := New(restartedConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	assertArtifact(t, restarted, http.MethodGet)

	artifactPath := filepath.Join(restartedStore.Directory(), expected.Encoded())
	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-1] ^= 0xff
	if err := os.Chmod(artifactPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(artifactPath, 0o400); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://service.test"+path, nil)
	response := httptest.NewRecorder()
	restarted.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt artifact status=%d body=%s", response.Code, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), data) {
		t.Fatal("corrupt artifact bytes were served")
	}
}

func TestArtifactHTTPRejectsMethodsQueriesRangesAndMalformedDigests(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)}
	artifactStore, err := catalogartifactstore.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	data := testServiceArtifact(t)
	expected := digest.FromBytes(data)
	if err := artifactStore.Put(expected, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	generator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) { return nil, errors.New("unused") })
	config := testConfig(storeDir, keyPath, generator, clock)
	config.ArtifactStore = artifactStore
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	path := catalogartifactstore.HTTPPathPrefix + expected.Encoded()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		t.Run("method "+method, func(t *testing.T) {
			request := httptest.NewRequest(method, "https://service.test"+path, nil)
			response := httptest.NewRecorder()
			service.ServeHTTP(response, request)
			if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("status=%d allow=%q body=%s", response.Code, response.Header().Get("Allow"), response.Body.String())
			}
		})
	}

	for name, mutate := range map[string]func(*http.Request){
		"query":       func(request *http.Request) { request.URL.RawQuery = "download=1" },
		"empty query": func(request *http.Request) { request.URL.ForceQuery = true },
		"range":       func(request *http.Request) { request.Header.Set("Range", "bytes=0-1") },
		"empty range": func(request *http.Request) {
			request.Header["Range"] = []string{""}
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://service.test"+path, nil)
			mutate(request)
			response := httptest.NewRecorder()
			service.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	percentEncoded := catalogartifactstore.HTTPPathPrefix + fmt.Sprintf("%%%02x", expected.Encoded()[0]) + expected.Encoded()[1:]
	malformed := []string{
		catalogartifactstore.HTTPPathPrefix,
		percentEncoded,
		catalogartifactstore.HTTPPathPrefix + strings.Repeat("a", 63),
		catalogartifactstore.HTTPPathPrefix + strings.Repeat("A", 64),
		catalogartifactstore.HTTPPathPrefix + strings.Repeat("g", 64),
		catalogartifactstore.HTTPPathPrefix + expected.Encoded() + "/extra",
		catalogartifactstore.HTTPPathPrefix + expected.String(),
	}
	for _, requestPath := range malformed {
		t.Run("malformed "+requestPath, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://service.test"+requestPath, nil)
			response := httptest.NewRecorder()
			service.ServeHTTP(response, request)
			if response.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	missing := httptest.NewRequest(http.MethodGet, "https://service.test"+catalogartifactstore.HTTPPathPrefix+strings.Repeat("f", 64), nil)
	missingResponse := httptest.NewRecorder()
	service.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", missingResponse.Code, missingResponse.Body.String())
	}
}

func TestNewRejectsArtifactStoreOutsideCatalogStoreRoot(t *testing.T) {
	storeDir := t.TempDir()
	artifactStore, err := catalogartifactstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)}
	generator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) { return nil, errors.New("unused") })
	config := testConfig(storeDir, keyPath, generator, clock)
	config.ArtifactStore = artifactStore
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "rooted below") {
		t.Fatalf("mismatched artifact store error=%v", err)
	}
}

func TestHTTPPendingCompletedImmutableCatalogAndCacheHit(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, key := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	generated := testGeneratedSet(t)
	gate := make(chan struct{})
	var calls atomic.Int32
	generator := GeneratorFunc(func(ctx context.Context, request *catalog.Request) (*GeneratedSet, error) {
		calls.Add(1)
		select {
		case <-gate:
			return generated, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	service, err := New(testConfig(storeDir, keyPath, generator, clock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	request := testRequest(t)

	response := postCatalogSet(t, service, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("POST status=%d retry=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	pending, err := catalog.DecodeOperation(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	wantID, err := OperationID(request)
	if err != nil {
		t.Fatal(err)
	}
	if pending.ID != wantID || pending.Status != catalog.OperationPending {
		t.Fatalf("pending=%+v want ID %s", pending, wantID)
	}
	close(gate)
	completed := waitOperation(t, service, pending.ID, catalog.OperationCompleted)
	if completed.Result == nil {
		t.Fatal("completed operation has no result")
	}

	keys, err := metadata.NewKeySet(map[string]*rsa.PublicKey{"catalog-test-1": &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := catalogauth.Verify(completed.Result.JWS, keys, "catalog-test-1", completed.Result.RequestDigest, request.CoreSnapshotDigest, clock.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified.Payload.Catalogs) != 1 {
		t.Fatalf("catalog references=%d", len(verified.Payload.Catalogs))
	}
	reference := verified.Payload.Catalogs[0]
	catalogRequest := httptest.NewRequest(http.MethodGet, "https://service.test"+catalog.CatalogDocumentPathPrefix+strings.TrimPrefix(reference.SHA256, "sha256:"), nil)
	catalogResponse := httptest.NewRecorder()
	service.ServeHTTP(catalogResponse, catalogRequest)
	if catalogResponse.Code != http.StatusOK {
		t.Fatalf("GET catalog status=%d body=%s", catalogResponse.Code, catalogResponse.Body.String())
	}
	if catalogResponse.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("cache-control=%q", catalogResponse.Header().Get("Cache-Control"))
	}
	if _, err := catalog.DecodeReferencedTapCatalog(reference, catalogResponse.Body.Bytes()); err != nil {
		t.Fatal(err)
	}

	cacheHit := postCatalogSet(t, service, request)
	if cacheHit.Code != http.StatusOK {
		t.Fatalf("cache-hit POST status=%d body=%s", cacheHit.Code, cacheHit.Body.String())
	}
	if _, err := catalog.DecodeCatalogSetResult(cacheHit.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("generator calls=%d, want 1", calls.Load())
	}
}

func TestRestartUsesPersistedCompletedCache(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	request := testRequest(t)
	generated := testGeneratedSet(t)
	var calls atomic.Int32
	generator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) {
		calls.Add(1)
		return generated, nil
	})
	service, err := New(testConfig(storeDir, keyPath, generator, clock))
	if err != nil {
		t.Fatal(err)
	}
	first := postCatalogSet(t, service, request)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	operation, err := catalog.DecodeOperation(first.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service, operation.ID, catalog.OperationCompleted)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(testConfig(storeDir, keyPath, generator, clock))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	response := postCatalogSet(t, restarted, request)
	if response.Code != http.StatusOK {
		t.Fatalf("restart cache status=%d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("generator calls after restart=%d, want 1", calls.Load())
	}
}

func TestExpiredResultIsNeverReturnedAsFallback(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	request := testRequest(t)
	generated := testGeneratedSet(t)
	var calls atomic.Int32
	generator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) {
		if calls.Add(1) == 1 {
			return generated, nil
		}
		return nil, NewFailureError(catalog.FailureMissingBottle, "selected bottle is unavailable", nil)
	})
	config := testConfig(storeDir, keyPath, generator, clock)
	config.CatalogSetLifetime = time.Minute
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	first := postCatalogSet(t, service, request)
	operation, err := catalog.DecodeOperation(first.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service, operation.ID, catalog.OperationCompleted)

	clock.Add(2 * time.Minute)
	stale := postCatalogSet(t, service, request)
	if stale.Code != http.StatusAccepted {
		t.Fatalf("expired POST status=%d, want 202; body=%s", stale.Code, stale.Body.String())
	}
	pending, err := catalog.DecodeOperation(stale.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	failed := waitOperation(t, service, pending.ID, catalog.OperationFailed)
	if failed.Failure == nil || failed.Failure.Code != catalog.FailureMissingBottle {
		t.Fatalf("failure=%+v", failed.Failure)
	}
	if calls.Load() != 2 {
		t.Fatalf("generator calls=%d, want 2", calls.Load())
	}
}

func TestRestartResumesPendingOperation(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	request := testRequest(t)
	blocked := GeneratorFunc(func(ctx context.Context, _ *catalog.Request) (*GeneratedSet, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	service, err := New(testConfig(storeDir, keyPath, blocked, clock))
	if err != nil {
		t.Fatal(err)
	}
	first := postCatalogSet(t, service, request)
	operation, err := catalog.DecodeOperation(first.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	generated := testGeneratedSet(t)
	var calls atomic.Int32
	resumedGenerator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) {
		calls.Add(1)
		return generated, nil
	})
	restarted, err := New(testConfig(storeDir, keyPath, resumedGenerator, clock))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	waitOperation(t, restarted, operation.ID, catalog.OperationCompleted)
	if calls.Load() != 1 {
		t.Fatalf("resumed generator calls=%d, want 1", calls.Load())
	}
}

func TestMissingCachedCatalogForcesRegeneration(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, key := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	request := testRequest(t)
	generated := testGeneratedSet(t)
	var calls atomic.Int32
	generator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) {
		calls.Add(1)
		return generated, nil
	})
	service, err := New(testConfig(storeDir, keyPath, generator, clock))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	first := postCatalogSet(t, service, request)
	operation, err := catalog.DecodeOperation(first.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	completed := waitOperation(t, service, operation.ID, catalog.OperationCompleted)
	keys, err := metadata.NewKeySet(map[string]*rsa.PublicKey{"catalog-test-1": &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := catalogauth.Verify(completed.Result.JWS, keys, "catalog-test-1", completed.Result.RequestDigest, request.CoreSnapshotDigest, clock.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	cachedDigest := strings.TrimPrefix(verified.Payload.Catalogs[0].SHA256, "sha256:")
	if err := os.Remove(filepath.Join(storeDir, "catalogs", "sha256", cachedDigest)); err != nil {
		t.Fatal(err)
	}

	response := postCatalogSet(t, service, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("missing-catalog POST status=%d body=%s", response.Code, response.Body.String())
	}
	pending, err := catalog.DecodeOperation(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service, pending.ID, catalog.OperationCompleted)
	if calls.Load() != 2 {
		t.Fatalf("generator calls=%d, want 2", calls.Load())
	}
}

func TestStrictBoundedRequestHandling(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	generated := testGeneratedSet(t)
	generator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) {
		return generated, nil
	})
	service, err := New(testConfig(storeDir, keyPath, generator, clock))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	for name, body := range map[string]string{
		"duplicate": `{"schema_version":"dalec-homebrew-catalog-request/v1","schema_version":"dalec-homebrew-catalog-request/v1"}`,
		"unknown":   `{"unknown":true}`,
		"trailing":  `{}` + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://service.test/v1/catalog-sets", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			service.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var failure catalog.Failure
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || !failure.Code.Valid() {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
		})
	}

	oversized := bytes.Repeat([]byte(" "), int(catalog.MaxRequestBytes)+1)
	requestHTTP := httptest.NewRequest(http.MethodPost, "https://service.test/v1/catalog-sets", bytes.NewReader(oversized))
	requestHTTP.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, requestHTTP)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDeterministicOperationIDUsesCanonicalRequest(t *testing.T) {
	left := testRequest(t)
	secondRoot, err := catalog.ParseFormulaID("other/tap/tool")
	if err != nil {
		t.Fatal(err)
	}
	left.ExternalRoots = append(left.ExternalRoots, secondRoot)
	left.Platforms = append(left.Platforms, catalog.Platform{OS: "linux", Architecture: "arm64"})
	right := *left
	right.ExternalRoots = []catalog.FormulaID{secondRoot, left.ExternalRoots[0]}
	right.Platforms = []catalog.Platform{left.Platforms[1], left.Platforms[0]}
	leftID, err := OperationID(left)
	if err != nil {
		t.Fatal(err)
	}
	rightID, err := OperationID(&right)
	if err != nil {
		t.Fatal(err)
	}
	if leftID != rightID || !validOperationID(leftID) {
		t.Fatalf("operation IDs differ: %q != %q", leftID, rightID)
	}
}

func TestConcurrentDuplicatePostsLaunchOneGenerator(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	generated := testGeneratedSet(t)
	gate := make(chan struct{})
	var calls atomic.Int32
	generator := GeneratorFunc(func(ctx context.Context, _ *catalog.Request) (*GeneratedSet, error) {
		calls.Add(1)
		select {
		case <-gate:
			return generated, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	service, err := New(testConfig(storeDir, keyPath, generator, clock))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	request := testRequest(t)

	const clients = 24
	start := make(chan struct{})
	var wait sync.WaitGroup
	errorsChannel := make(chan error, clients)
	for range clients {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			response := postCatalogSetConcurrent(service, request)
			if response.Code != http.StatusAccepted {
				errorsChannel <- fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
	close(gate)
	id, _ := OperationID(request)
	waitOperation(t, service, id, catalog.OperationCompleted)
	if calls.Load() != 1 {
		t.Fatalf("generator calls=%d, want 1", calls.Load())
	}
}

func TestGenerationAdmissionBoundRejectsDistinctRequestsBeforePersistence(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	gate := make(chan struct{})
	generator := GeneratorFunc(func(ctx context.Context, _ *catalog.Request) (*GeneratedSet, error) {
		select {
		case <-gate:
			return nil, NewFailureError(catalog.FailureUnavailable, "test catalog generator is unavailable", nil)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	config := testConfig(storeDir, keyPath, generator, clock)
	config.MaxConcurrentGenerations = 1
	config.MaxPendingGenerations = 2
	config.MaxStoredOperations = 2
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	requests := make([]*catalog.Request, 0, 3)
	for _, value := range []string{"acme/tools/widget", "acme/tools/gadget", "acme/tools/utility"} {
		formula, err := catalog.ParseFormulaID(value)
		if err != nil {
			t.Fatal(err)
		}
		request := *testRequest(t)
		request.ExternalRoots = []catalog.FormulaID{formula}
		requests = append(requests, &request)
	}

	responses := make([]*httptest.ResponseRecorder, len(requests))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index, request := range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			responses[index] = postCatalogSetConcurrent(service, request)
		}()
	}
	close(start)
	wait.Wait()

	accepted := make([]int, 0, config.MaxPendingGenerations)
	rejected := -1
	for index, response := range responses {
		switch response.Code {
		case http.StatusAccepted:
			accepted = append(accepted, index)
		case http.StatusServiceUnavailable:
			if rejected != -1 {
				t.Fatalf("multiple requests rejected: %d and %d", rejected, index)
			}
			rejected = index
			var failure catalog.Failure
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if failure.Code != catalog.FailureUnavailable || failure.Message != generationAdmissionFailureMessage {
				t.Fatalf("failure=%+v", failure)
			}
		default:
			t.Fatalf("request %d status=%d body=%s", index, response.Code, response.Body.String())
		}
	}
	if len(accepted) != config.MaxPendingGenerations || rejected == -1 {
		t.Fatalf("accepted=%v rejected=%d", accepted, rejected)
	}

	service.mu.Lock()
	activeCount := len(service.active)
	queuedCount := len(service.queued)
	service.mu.Unlock()
	if activeCount != 1 || queuedCount != 1 {
		t.Fatalf("active=%d queued=%d, want 1 each", activeCount, queuedCount)
	}
	operationIDs, err := service.store.listOperations()
	if err != nil {
		t.Fatal(err)
	}
	if len(operationIDs) != config.MaxPendingGenerations {
		t.Fatalf("persisted operations=%v", operationIDs)
	}
	for _, index := range accepted {
		id, err := OperationID(requests[index])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.store.loadOperation(id); err != nil {
			t.Fatalf("accepted operation %s was evicted: %v", id, err)
		}
	}
	rejectedID, err := OperationID(requests[rejected])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.loadOperation(rejectedID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected operation was persisted: %v", err)
	}
	if _, _, err := service.store.loadRequest(rejectedID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected request was persisted: %v", err)
	}

	retry := postCatalogSet(t, service, requests[rejected])
	if retry.Code != http.StatusServiceUnavailable || retry.Body.String() != responses[rejected].Body.String() {
		t.Fatalf("retry status=%d body=%s, want stable %d %s", retry.Code, retry.Body.String(), responses[rejected].Code, responses[rejected].Body.String())
	}
	duplicate := postCatalogSet(t, service, requests[accepted[0]])
	if duplicate.Code != http.StatusAccepted {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	close(gate)
	for _, index := range accepted {
		id, err := OperationID(requests[index])
		if err != nil {
			t.Fatal(err)
		}
		waitOperation(t, service, id, catalog.OperationFailed)
	}
	afterCapacityFrees := postCatalogSet(t, service, requests[rejected])
	if afterCapacityFrees.Code != http.StatusAccepted {
		t.Fatalf("post-capacity status=%d body=%s", afterCapacityFrees.Code, afterCapacityFrees.Body.String())
	}
	waitOperation(t, service, rejectedID, catalog.OperationFailed)
}

func TestServiceCloseRemovesCommandGeneratorPinnedDirectory(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewCommandGenerator(CommandGeneratorConfig{Path: executable})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = generator.Close() })
	directory := generator.cleanupDir
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	service, err := New(testConfig(t.TempDir(), keyPath, generator, clock))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned command directory remains after service shutdown: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("second service Close: %v", err)
	}
}

func postCatalogSetConcurrent(service http.Handler, request *catalog.Request) *httptest.ResponseRecorder {
	data, _ := catalog.CanonicalRequest(request)
	httpRequest := httptest.NewRequest(http.MethodPost, "https://service.test/v1/catalog-sets", bytes.NewReader(data))
	httpRequest.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, httpRequest)
	return response
}

func TestCompletedCacheRefreshesAfterOneHour(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	var calls atomic.Int32
	generator := GeneratorFunc(func(context.Context, *catalog.Request) (*GeneratedSet, error) {
		calls.Add(1)
		return testGeneratedSet(t), nil
	})
	service, err := New(testConfig(storeDir, keyPath, generator, clock))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	request := testRequest(t)
	first := postCatalogSet(t, service, request)
	operation, err := catalog.DecodeOperation(first.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	_ = waitOperation(t, service, operation.ID, catalog.OperationCompleted)
	clock.Add(DefaultCatalogRefresh)
	refreshed := postCatalogSet(t, service, request)
	if refreshed.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", refreshed.Code, refreshed.Body.String())
	}
	_ = waitOperation(t, service, operation.ID, catalog.OperationCompleted)
	if calls.Load() != 2 {
		t.Fatalf("generator calls=%d, want 2", calls.Load())
	}
}
