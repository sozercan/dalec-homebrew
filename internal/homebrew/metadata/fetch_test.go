package metadata

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientFetchBuildsAuthenticatedImmutableSnapshot(t *testing.T) {
	one, _ := generatedTestKeys(t)
	now := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	generated := now.Add(-30 * time.Minute)
	formulaJWS := makeGeneralJWS(t, string(validFormulaPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	migrationJWS := makeGeneralJWS(t, string(validMigrationPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))

	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		paths = append(paths, request.URL.Path)
		mu.Unlock()
		if request.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", request.Header.Get("Accept"))
		}
		response.Header().Set("Last-Modified", generated.Format(http.TimeFormat))
		switch request.URL.Path {
		case "/api/" + FormulaEndpoint:
			_, _ = response.Write(formulaJWS)
		case "/api/" + MigrationsEndpoint:
			_, _ = response.Write(migrationJWS)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{
		BaseURL:    server.URL + "/api/",
		HTTPClient: server.Client(),
		Keys:       testKeySet(t),
		Freshness: FreshnessPolicy{
			MaxAge:        2 * time.Hour,
			MaxFutureSkew: time.Minute,
		},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	snapshot, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if want := []string{"/api/" + FormulaEndpoint, "/api/" + MigrationsEndpoint}; !reflect.DeepEqual(gotPaths, want) {
		t.Fatalf("paths = %v, want %v", gotPaths, want)
	}

	info := snapshot.Info()
	if info.SchemaVersion != SnapshotSchema || info.Digest == "" || info.FormulaDigest == "" || info.MigrationDigest == "" {
		t.Fatalf("incomplete snapshot info: %#v", info)
	}
	if info.GeneratedAt != generated || info.FetchedAt != now {
		t.Fatalf("times = generated %s fetched %s", info.GeneratedAt, info.FetchedAt)
	}
	if info.Formula.GeneratedAtSource != GeneratedAtLastModified || info.Migrations.GeneratedAtSource != GeneratedAtLastModified {
		t.Fatalf("date sources = %q / %q", info.Formula.GeneratedAtSource, info.Migrations.GeneratedAtSource)
	}
	if info.Formula.URL != server.URL+"/api/"+FormulaEndpoint || info.Migrations.URL != server.URL+"/api/"+MigrationsEndpoint {
		t.Fatalf("document URLs = %q / %q", info.Formula.URL, info.Migrations.URL)
	}
	if info.Formula.EnvelopeDigest != digestBytes(formulaJWS) || info.Formula.PayloadDigest != digestBytes(validFormulaPayload(t)) {
		t.Fatalf("formula digest evidence = %#v", info.Formula)
	}
	if got := snapshot.Signatures(); len(got) != 2 || got[0].KeyID != DefaultRequiredKeyID || got[1].KeyID != DefaultRequiredKeyID {
		t.Fatalf("signatures = %#v", got)
	}
	match, err := snapshot.Lookup("legacy")
	if err != nil || match.Canonical != "tool" {
		t.Fatalf("snapshot lookup = %#v, %v", match, err)
	}

	// SnapshotInfo and signature accessors must be caller-owned.
	info.Formula.Signatures[0].KeyID = "mutated"
	if snapshot.Info().Formula.Signatures[0].KeyID != DefaultRequiredKeyID {
		t.Fatal("Snapshot.Info exposed signature backing storage")
	}
	signatures := snapshot.Signatures()
	signatures[0].KeyID = "mutated"
	if snapshot.Signatures()[0].KeyID != DefaultRequiredKeyID {
		t.Fatal("Snapshot.Signatures exposed backing storage")
	}
}

func TestLoadSnapshotStableDigestIgnoresRandomizedSignatures(t *testing.T) {
	one, _ := generatedTestKeys(t)
	keys := testKeySet(t)
	now := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	formulaPayload := string(validFormulaPayload(t))
	migrationPayload := string(validMigrationPayload(t))

	load := func() *Snapshot {
		t.Helper()
		snapshot, err := LoadSnapshot(
			SignedSource{Data: makeGeneralJWS(t, formulaPayload, validTestSignature(t, DefaultRequiredKeyID, one)), URL: "formula", GeneratedAt: now},
			SignedSource{Data: makeGeneralJWS(t, migrationPayload, validTestSignature(t, DefaultRequiredKeyID, one)), URL: "migrations", GeneratedAt: now},
			LoadOptions{Keys: keys, Now: now},
		)
		if err != nil {
			t.Fatalf("LoadSnapshot: %v", err)
		}
		return snapshot
	}
	first := load()
	second := load()
	if first.Digest() != second.Digest() || first.FormulaDigest() != second.FormulaDigest() || first.MigrationDigest() != second.MigrationDigest() {
		t.Fatalf("stable digests differ: %#v vs %#v", first.Info(), second.Info())
	}
	if first.Info().Formula.EnvelopeDigest == second.Info().Formula.EnvelopeDigest {
		t.Fatal("test did not produce distinct randomized RSA-PSS envelopes")
	}
	if first.Digest() != snapshotDigest(digestBytes([]byte(formulaPayload)), digestBytes([]byte(migrationPayload))) {
		t.Fatalf("snapshot digest = %q", first.Digest())
	}
}

func TestLoadSnapshotFreshnessFutureAndRollbackPolicy(t *testing.T) {
	one, _ := generatedTestKeys(t)
	keys := testKeySet(t)
	now := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	formulaJWS := makeGeneralJWS(t, string(validFormulaPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	migrationJWS := makeGeneralJWS(t, string(validMigrationPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))

	tests := []struct {
		name      string
		generated time.Time
		policy    FreshnessPolicy
		want      error
	}{
		{
			name:      "stale",
			generated: now.Add(-2 * time.Hour),
			policy:    FreshnessPolicy{MaxAge: time.Hour, MaxFutureSkew: time.Minute},
			want:      ErrMetadataStale,
		},
		{
			name:      "future",
			generated: now.Add(10 * time.Minute),
			policy:    FreshnessPolicy{MaxAge: time.Hour, MaxFutureSkew: 5 * time.Minute},
			want:      ErrMetadataFromFuture,
		},
		{
			name:      "rollback floor",
			generated: now.Add(-time.Hour),
			policy: FreshnessPolicy{
				MaxAge:        2 * time.Hour,
				MaxFutureSkew: time.Minute,
				RollbackFloor: now.Add(-30 * time.Minute),
			},
			want: ErrMetadataRollback,
		},
		{
			name:      "missing generated date",
			generated: time.Time{},
			policy:    FreshnessPolicy{MaxAge: time.Hour, MaxFutureSkew: time.Minute},
			want:      ErrGeneratedDateMissing,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSnapshot(
				SignedSource{Data: formulaJWS, GeneratedAt: tc.generated},
				SignedSource{Data: migrationJWS, GeneratedAt: tc.generated},
				LoadOptions{Keys: keys, Freshness: tc.policy, Now: now},
			)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSignedGeneratedDateOverridesTransportDate(t *testing.T) {
	one, _ := generatedTestKeys(t)
	now := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	formulaWrapper := string(mustJSON(t, map[string]any{
		"generated_date": now.Add(-10 * time.Minute).Format(time.RFC3339),
		"formulae":       jsonRaw(validFormulaPayload(t)),
	}))
	migrationWrapper := string(mustJSON(t, map[string]any{
		"generated_date": now.Add(-20 * time.Minute).Format(time.RFC3339),
		"migrations":     jsonRaw(validMigrationPayload(t)),
	}))
	snapshot, err := LoadSnapshot(
		SignedSource{Data: makeGeneralJWS(t, formulaWrapper, validTestSignature(t, DefaultRequiredKeyID, one)), GeneratedAt: now.Add(-30 * 24 * time.Hour)},
		SignedSource{Data: makeGeneralJWS(t, migrationWrapper, validTestSignature(t, DefaultRequiredKeyID, one)), GeneratedAt: now.Add(-30 * 24 * time.Hour)},
		LoadOptions{
			Keys: testKeySet(t),
			Freshness: FreshnessPolicy{
				MaxAge:        time.Hour,
				MaxFutureSkew: time.Minute,
			},
			Now: now,
		},
	)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	info := snapshot.Info()
	if info.Formula.GeneratedAtSource != GeneratedAtSignedPayload || info.Migrations.GeneratedAtSource != GeneratedAtSignedPayload {
		t.Fatalf("generated date sources = %q, %q", info.Formula.GeneratedAtSource, info.Migrations.GeneratedAtSource)
	}
	if info.GeneratedAt != now.Add(-20*time.Minute) {
		t.Fatalf("snapshot generated at = %s", info.GeneratedAt)
	}
}

func TestClientBoundsEveryHTTPRead(t *testing.T) {
	one, _ := generatedTestKeys(t)
	now := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	validFormula := makeGeneralJWS(t, string(validFormulaPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	validMigrations := makeGeneralJWS(t, string(validMigrationPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))

	tests := []struct {
		name       string
		formula    func(http.ResponseWriter)
		migration  func(http.ResponseWriter)
		formulaMax int64
		migMax     int64
		want       error
	}{
		{
			name: "formula Content-Length",
			formula: func(w http.ResponseWriter) {
				w.Header().Set("Content-Length", "1000")
			},
			migration:  func(w http.ResponseWriter) { _, _ = w.Write(validMigrations) },
			formulaMax: 100,
			migMax:     int64(len(validMigrations) + 1),
			want:       ErrResponseTooLarge,
		},
		{
			name: "formula streaming overflow",
			formula: func(w http.ResponseWriter) {
				w.(http.Flusher).Flush()
				_, _ = w.Write([]byte(strings.Repeat("x", 101)))
			},
			migration:  func(w http.ResponseWriter) { _, _ = w.Write(validMigrations) },
			formulaMax: 100,
			migMax:     int64(len(validMigrations) + 1),
			want:       ErrResponseTooLarge,
		},
		{
			name:    "migration streaming overflow",
			formula: func(w http.ResponseWriter) { _, _ = w.Write(validFormula) },
			migration: func(w http.ResponseWriter) {
				w.(http.Flusher).Flush()
				_, _ = w.Write([]byte(strings.Repeat("x", 101)))
			},
			formulaMax: int64(len(validFormula) + 1),
			migMax:     100,
			want:       ErrResponseTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Last-Modified", now.Format(http.TimeFormat))
				switch request.URL.Path {
				case "/" + FormulaEndpoint:
					tc.formula(response)
				case "/" + MigrationsEndpoint:
					tc.migration(response)
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			client, err := NewClient(Config{
				BaseURL:            server.URL + "/",
				HTTPClient:         server.Client(),
				Keys:               testKeySet(t),
				MaxFormulaBytes:    tc.formulaMax,
				MaxMigrationsBytes: tc.migMax,
				Clock:              func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Fetch(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestClientRejectsHTTPAndHeaderFailures(t *testing.T) {
	one, _ := generatedTestKeys(t)
	now := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	formula := makeGeneralJWS(t, string(validFormulaPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name: "HTTP status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
			want: "unexpected HTTP status 503",
		},
		{
			name: "malformed Last-Modified",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Last-Modified", "not-a-date")
				_, _ = w.Write(formula)
			},
			want: "invalid Last-Modified",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			client, err := NewClient(Config{
				BaseURL:    server.URL + "/",
				HTTPClient: server.Client(),
				Keys:       testKeySet(t),
				Clock:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Fetch(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestNewClientValidatesConfiguration(t *testing.T) {
	_, two := generatedTestKeys(t)
	missingRequired, err := NewKeySet(map[string]*rsa.PublicKey{"homebrew-2": &two.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		config Config
	}{
		{name: "relative URL", config: Config{BaseURL: "/api", Keys: testKeySet(t)}},
		{name: "negative formula limit", config: Config{Keys: testKeySet(t), MaxFormulaBytes: -1}},
		{name: "required key absent", config: Config{Keys: missingRequired}},
		{name: "invalid unknown signature policy", config: Config{Keys: testKeySet(t), Verification: VerificationPolicy{UnknownSignatures: 99}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.config); err == nil {
				t.Fatal("NewClient unexpectedly succeeded")
			}
		})
	}
}

// jsonRaw preserves nested fixtures as JSON values rather than base64 strings.
type jsonRaw []byte

func (r jsonRaw) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return nil, fmt.Errorf("empty raw JSON")
	}
	return r, nil
}
