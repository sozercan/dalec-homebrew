package metadata

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCaptureBundleFetchesOfficialEnvelopesOnceAndRoundTrips(t *testing.T) {
	one, _ := generatedTestKeys(t)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	formula := makeGeneralJWS(t, string(validFormulaPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	migrations := makeGeneralJWS(t, string(validMigrationPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	responses := map[string][]byte{
		OfficialFormulaURL:    formula,
		OfficialMigrationsURL: migrations,
	}
	requests := map[string]int{}
	client, err := NewClient(Config{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests[request.URL.String()]++
			data, ok := responses[request.URL.String()]
			if !ok {
				t.Fatalf("unexpected request URL %q", request.URL)
			}
			return metadataResponse(request, data, now.Add(-time.Hour)), nil
		})},
		Keys:  testKeySet(t),
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	bundle, err := client.CaptureBundle(t.Context(), BundleCaptureOptions{})
	if err != nil {
		t.Fatalf("CaptureBundle: %v", err)
	}
	if requests[OfficialFormulaURL] != 1 || requests[OfficialMigrationsURL] != 1 || len(requests) != 2 {
		t.Fatalf("request counts = %#v", requests)
	}
	if !bytes.Equal(bundle.Formula, formula) || !bytes.Equal(bundle.Migrations, migrations) {
		t.Fatal("captured bytes differ from HTTP response bytes")
	}
	manifestData, err := bundle.CanonicalManifest()
	if err != nil {
		t.Fatalf("CanonicalManifest: %v", err)
	}
	if got, err := bundle.Digest(); err != nil || got != digestBytes(manifestData) {
		t.Fatalf("Digest = %q, %v", got, err)
	}
	snapshot, manifest, err := LoadBundleBytes(manifestData, bundle.Formula, bundle.Migrations, BundleLoadOptions{
		Keys: testKeySet(t),
		Now:  now,
	})
	if err != nil {
		t.Fatalf("LoadBundleBytes: %v", err)
	}
	if manifest != bundle.Manifest {
		t.Fatalf("loaded manifest = %#v, want %#v", manifest, bundle.Manifest)
	}
	info := snapshot.Info()
	if info.Formula.URL != OfficialFormulaURL || info.Migrations.URL != OfficialMigrationsURL {
		t.Fatalf("snapshot URLs = %q, %q", info.Formula.URL, info.Migrations.URL)
	}
	if info.Formula.EnvelopeDigest != digestBytes(formula) || info.Migrations.EnvelopeDigest != digestBytes(migrations) {
		t.Fatalf("snapshot envelope digests = %q, %q", info.Formula.EnvelopeDigest, info.Migrations.EnvelopeDigest)
	}
}

func TestCaptureBundleRejectsNonOfficialBaseURLBeforeFetching(t *testing.T) {
	requests := 0
	client, err := NewClient(Config{
		BaseURL: "https://mirror.example.test/api/",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			return nil, errors.New("must not fetch")
		})},
		Keys: testKeySet(t),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.CaptureBundle(t.Context(), BundleCaptureOptions{}); err == nil || !strings.Contains(err.Error(), "official endpoints") {
		t.Fatalf("error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestCaptureBundleVerifiesJWSAndFreshnessBeforeSuccess(t *testing.T) {
	one, _ := generatedTestKeys(t)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	validFormula := makeGeneralJWS(t, string(validFormulaPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	validMigrations := makeGeneralJWS(t, string(validMigrationPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	tests := []struct {
		name          string
		formula       []byte
		lastModified  time.Time
		freshness     FreshnessPolicy
		wantError     error
		wantSubstring string
	}{
		{
			name:          "invalid JWS",
			formula:       []byte(`{}`),
			lastModified:  now,
			wantError:     ErrInvalidJWS,
			wantSubstring: "verify formula.jws.json",
		},
		{
			name:         "stale",
			formula:      validFormula,
			lastModified: now.Add(-2 * time.Hour),
			freshness: FreshnessPolicy{
				MaxAge:        time.Hour,
				MaxFutureSkew: time.Minute,
			},
			wantError: ErrMetadataStale,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			responses := map[string][]byte{
				OfficialFormulaURL:    tc.formula,
				OfficialMigrationsURL: validMigrations,
			}
			client, err := NewClient(Config{
				HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					return metadataResponse(request, responses[request.URL.String()], tc.lastModified), nil
				})},
				Keys:      testKeySet(t),
				Freshness: tc.freshness,
				Clock:     func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = client.CaptureBundle(t.Context(), BundleCaptureOptions{})
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("error = %v, want %v", err, tc.wantError)
			}
			if tc.wantSubstring != "" && !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantSubstring)
			}
		})
	}
}

func TestCaptureBundleHonorsExistingResponseLimits(t *testing.T) {
	one, _ := generatedTestKeys(t)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	formula := makeGeneralJWS(t, string(validFormulaPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	client, err := NewClient(Config{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return metadataResponse(request, formula, now), nil
		})},
		Keys:            testKeySet(t),
		MaxFormulaBytes: int64(len(formula) - 1),
		Clock:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.CaptureBundle(t.Context(), BundleCaptureOptions{}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want %v", err, ErrResponseTooLarge)
	}
}

func TestLoadBundleBytesRejectsManifestAndContentTampering(t *testing.T) {
	bundle, manifestData, now := validTestBundle(t)
	tests := []struct {
		name       string
		manifest   func() []byte
		formula    func() []byte
		migrations func() []byte
		want       string
	}{
		{
			name: "tampered envelope",
			formula: func() []byte {
				data := slices.Clone(bundle.Formula)
				data[len(data)-1] ^= 1
				return data
			},
			want: "envelope digest",
		},
		{
			name: "missing field",
			manifest: func() []byte {
				manifest := bundle.Manifest
				manifest.Formula.Filename = ""
				return marshalBundleManifest(t, manifest)
			},
			want: "filename",
		},
		{
			name: "extra field",
			manifest: func() []byte {
				return bytes.Replace(manifestData, []byte(`"schema_version"`), []byte(`"extra":true,"schema_version"`), 1)
			},
			want: "unknown field",
		},
		{
			name: "wrong URL",
			manifest: func() []byte {
				manifest := bundle.Manifest
				manifest.Formula.URL = "https://example.test/formula.jws.json"
				return marshalBundleManifest(t, manifest)
			},
			want: "url must be",
		},
		{
			name: "non-canonical timestamp",
			manifest: func() []byte {
				manifest := bundle.Manifest
				manifest.Formula.GeneratedAt = strings.Replace(manifest.Formula.GeneratedAt, "Z", "+00:00", 1)
				return marshalBundleManifest(t, manifest)
			},
			want: "canonical UTC RFC3339",
		},
		{
			name: "wrong size",
			manifest: func() []byte {
				manifest := bundle.Manifest
				manifest.Formula.Size++
				return marshalBundleManifest(t, manifest)
			},
			want: "manifest requires",
		},
		{
			name: "wrong digest",
			manifest: func() []byte {
				manifest := bundle.Manifest
				manifest.Formula.EnvelopeDigest = "sha256:" + strings.Repeat("0", 64)
				return marshalBundleManifest(t, manifest)
			},
			want: "manifest requires",
		},
		{
			name: "missing content",
			formula: func() []byte {
				return nil
			},
			want: "size is 0",
		},
		{
			name: "non-canonical manifest JSON",
			manifest: func() []byte {
				return append(slices.Clone(manifestData), '\n')
			},
			want: "not canonical",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest := manifestData
			if tc.manifest != nil {
				manifest = tc.manifest()
			}
			formula := bundle.Formula
			if tc.formula != nil {
				formula = tc.formula()
			}
			migrations := bundle.Migrations
			if tc.migrations != nil {
				migrations = tc.migrations()
			}
			_, _, err := LoadBundleBytes(manifest, formula, migrations, BundleLoadOptions{Keys: testKeySet(t), Now: now})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadBundleBytesDelegatesJWSAndCallerFreshnessValidation(t *testing.T) {
	bundle, _, now := validTestBundle(t)

	invalidJWS := []byte(`{}`)
	invalidManifest := bundle.Manifest
	invalidManifest.Formula.Size = int64(len(invalidJWS))
	invalidManifest.Formula.EnvelopeDigest = digestBytes(invalidJWS)
	invalidManifestData, err := CanonicalBundleManifest(&invalidManifest)
	if err != nil {
		t.Fatalf("CanonicalBundleManifest: %v", err)
	}
	if _, _, err := LoadBundleBytes(invalidManifestData, invalidJWS, bundle.Migrations, BundleLoadOptions{Keys: testKeySet(t), Now: now}); !errors.Is(err, ErrInvalidJWS) {
		t.Fatalf("invalid JWS error = %v, want %v", err, ErrInvalidJWS)
	}

	staleManifest := bundle.Manifest
	stale := now.Add(-2 * time.Hour).Format(time.RFC3339)
	staleManifest.Formula.GeneratedAt = stale
	staleManifest.Migrations.GeneratedAt = stale
	staleManifestData, err := CanonicalBundleManifest(&staleManifest)
	if err != nil {
		t.Fatalf("CanonicalBundleManifest: %v", err)
	}
	_, _, err = LoadBundleBytes(staleManifestData, bundle.Formula, bundle.Migrations, BundleLoadOptions{
		Keys: testKeySet(t),
		Freshness: FreshnessPolicy{
			MaxAge:        time.Hour,
			MaxFutureSkew: time.Minute,
		},
		Now: now,
	})
	if !errors.Is(err, ErrMetadataStale) {
		t.Fatalf("freshness error = %v, want %v", err, ErrMetadataStale)
	}
}

func TestLoadBundleBytesBoundsInputs(t *testing.T) {
	bundle, manifest, now := validTestBundle(t)
	oversizedManifest := make([]byte, DefaultMaxBundleManifestBytes+1)
	if _, _, err := LoadBundleBytes(oversizedManifest, bundle.Formula, bundle.Migrations, BundleLoadOptions{Keys: testKeySet(t), Now: now}); err == nil || !strings.Contains(err.Error(), "manifest exceeds") {
		t.Fatalf("manifest limit error = %v", err)
	}
	oversizedMigrations := make([]byte, DefaultMaxMigrationsBytes+1)
	if _, _, err := LoadBundleBytes(manifest, bundle.Formula, oversizedMigrations, BundleLoadOptions{Keys: testKeySet(t), Now: now}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("migrations limit error = %v", err)
	}
}

func validTestBundle(t *testing.T) (*Bundle, []byte, time.Time) {
	t.Helper()
	one, _ := generatedTestKeys(t)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	generatedAt := now.Add(-time.Hour)
	formula := makeGeneralJWS(t, string(validFormulaPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	migrations := makeGeneralJWS(t, string(validMigrationPayload(t)), validTestSignature(t, DefaultRequiredKeyID, one))
	bundle := &Bundle{
		Manifest: BundleManifest{
			SchemaVersion: BundleSchema,
			Formula: BundleDocument{
				Filename:       BundleFormulaFilename,
				URL:            OfficialFormulaURL,
				GeneratedAt:    generatedAt.Format(time.RFC3339),
				Size:           int64(len(formula)),
				EnvelopeDigest: digestBytes(formula),
			},
			Migrations: BundleDocument{
				Filename:       BundleMigrationsFilename,
				URL:            OfficialMigrationsURL,
				GeneratedAt:    generatedAt.Format(time.RFC3339),
				Size:           int64(len(migrations)),
				EnvelopeDigest: digestBytes(migrations),
			},
		},
		Formula:    formula,
		Migrations: migrations,
	}
	manifest, err := bundle.CanonicalManifest()
	if err != nil {
		t.Fatalf("CanonicalManifest: %v", err)
	}
	return bundle, manifest, now
}

func marshalBundleManifest(t *testing.T, manifest BundleManifest) []byte {
	t.Helper()
	data, err := jsonMarshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// jsonMarshal exists to keep mutation tests deliberately below the validated
// CanonicalBundleManifest API.
func jsonMarshal(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func metadataResponse(request *http.Request, data []byte, generatedAt time.Time) *http.Response {
	header := make(http.Header)
	header.Set("Last-Modified", generatedAt.Format(http.TimeFormat))
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        http.StatusText(http.StatusOK),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: int64(len(data)),
		Request:       request,
	}
}
