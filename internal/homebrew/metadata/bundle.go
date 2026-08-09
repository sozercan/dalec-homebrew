package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const (
	// BundleSchema is the canonical on-disk metadata bundle contract.
	BundleSchema = "dalec-homebrew-metadata-bundle/v1"

	BundleManifestFilename   = "manifest.json"
	BundleFormulaFilename    = FormulaEndpoint
	BundleMigrationsFilename = MigrationsEndpoint

	OfficialFormulaURL    = DefaultBaseURL + FormulaEndpoint
	OfficialMigrationsURL = DefaultBaseURL + MigrationsEndpoint

	// DefaultMaxBundleManifestBytes bounds an untrusted bundle manifest before
	// JSON decoding. The JWS members retain their existing package limits.
	DefaultMaxBundleManifestBytes int64 = 64 << 10
)

// BundleDocument binds one exact, signed HTTP response into a bundle manifest.
// GeneratedAt is the canonical UTC representation of the authenticated
// document's effective freshness timestamp: signed payload date when present,
// otherwise the parsed Last-Modified header.
type BundleDocument struct {
	Filename       string `json:"filename"`
	URL            string `json:"url"`
	GeneratedAt    string `json:"generated_at"`
	Size           int64  `json:"size"`
	EnvelopeDigest string `json:"envelope_digest"`
}

// BundleManifest binds the two official Homebrew metadata envelopes.
type BundleManifest struct {
	SchemaVersion string         `json:"schema_version"`
	Formula       BundleDocument `json:"formula"`
	Migrations    BundleDocument `json:"migrations"`
}

// Bundle contains the canonical manifest and the exact fetched JWS response
// bytes. Callers receive ownership of all byte slices.
type Bundle struct {
	Manifest   BundleManifest
	Formula    []byte
	Migrations []byte
}

// BundleCaptureOptions optionally overrides the Client verification time and
// policies for one capture. Zero fields inherit the frozen Client settings.
type BundleCaptureOptions struct {
	Keys         KeySet
	Verification VerificationPolicy
	Freshness    FreshnessPolicy
	Now          time.Time
}

// BundleLoadOptions configures authentication and freshness checks for an
// already-read bundle. The JWS byte limits remain fixed package invariants.
type BundleLoadOptions struct {
	Keys         KeySet
	Verification VerificationPolicy
	Freshness    FreshnessPolicy
	Now          time.Time
}

// CaptureBundle downloads each official JWS endpoint exactly once, verifies
// both documents, and returns their canonical immutable bundle.
func (c *Client) CaptureBundle(ctx context.Context, options BundleCaptureOptions) (*Bundle, error) {
	if c == nil {
		return nil, errors.New("metadata client is nil")
	}
	formulaURL := c.resolveEndpoint(FormulaEndpoint)
	migrationsURL := c.resolveEndpoint(MigrationsEndpoint)
	if formulaURL != OfficialFormulaURL || migrationsURL != OfficialMigrationsURL {
		return nil, fmt.Errorf("metadata bundle capture requires official endpoints %q and %q", OfficialFormulaURL, OfficialMigrationsURL)
	}

	formulaLimit := min(c.maxFormulaBytes, DefaultMaxFormulaBytes)
	migrationsLimit := min(c.maxMigrationsBytes, DefaultMaxMigrationsBytes)
	formula, err := c.fetchSource(ctx, formulaURL, formulaLimit)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", FormulaEndpoint, err)
	}
	migrations, err := c.fetchSource(ctx, migrationsURL, migrationsLimit)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", MigrationsEndpoint, err)
	}

	loadOptions := c.bundleLoadOptions(options)
	snapshot, err := LoadSnapshot(formula, migrations, loadOptions)
	if err != nil {
		return nil, err
	}
	info := snapshot.Info()
	bundle := &Bundle{
		Manifest: BundleManifest{
			SchemaVersion: BundleSchema,
			Formula: bundleDocument(
				BundleFormulaFilename,
				OfficialFormulaURL,
				formula,
				info.Formula,
			),
			Migrations: bundleDocument(
				BundleMigrationsFilename,
				OfficialMigrationsURL,
				migrations,
				info.Migrations,
			),
		},
		Formula:    slices.Clone(formula.Data),
		Migrations: slices.Clone(migrations.Data),
	}
	if _, err := bundle.CanonicalManifest(); err != nil {
		return nil, fmt.Errorf("build metadata bundle manifest: %w", err)
	}
	return bundle, nil
}

func (c *Client) bundleLoadOptions(options BundleCaptureOptions) LoadOptions {
	keys := options.Keys
	if keys.empty() {
		keys = c.keys
	}
	verification := options.Verification
	if verification == (VerificationPolicy{}) {
		verification = c.verification
	}
	freshness := options.Freshness
	if freshness == (FreshnessPolicy{}) {
		freshness = c.freshness
	}
	now := options.Now
	if now.IsZero() {
		now = c.clock()
	}
	return LoadOptions{
		Keys:         keys,
		Verification: verification,
		Freshness:    freshness,
		Now:          now.UTC().Round(0),
	}
}

func bundleDocument(filename, sourceURL string, source SignedSource, info DocumentInfo) BundleDocument {
	generatedAt := ""
	if !info.GeneratedAt.IsZero() {
		generatedAt = info.GeneratedAt.UTC().Round(0).Format(time.RFC3339)
	}
	return BundleDocument{
		Filename:       filename,
		URL:            sourceURL,
		GeneratedAt:    generatedAt,
		Size:           int64(len(source.Data)),
		EnvelopeDigest: info.EnvelopeDigest,
	}
}

// CanonicalManifest returns the exact manifest bytes used as the bundle
// identity. No newline is appended.
func (b *Bundle) CanonicalManifest() ([]byte, error) {
	if b == nil {
		return nil, errors.New("metadata bundle is nil")
	}
	if err := validateBundleContent(b.Manifest.Formula, b.Formula); err != nil {
		return nil, fmt.Errorf("formula bundle member: %w", err)
	}
	if err := validateBundleContent(b.Manifest.Migrations, b.Migrations); err != nil {
		return nil, fmt.Errorf("migrations bundle member: %w", err)
	}
	return CanonicalBundleManifest(&b.Manifest)
}

// Digest returns the sha256 digest of CanonicalManifest.
func (b *Bundle) Digest() (string, error) {
	manifest, err := b.CanonicalManifest()
	if err != nil {
		return "", err
	}
	return digestBytes(manifest), nil
}

// CanonicalBundleManifest validates and canonically encodes one manifest. No
// newline is appended.
func CanonicalBundleManifest(manifest *BundleManifest) ([]byte, error) {
	if manifest == nil {
		return nil, errors.New("metadata bundle manifest is nil")
	}
	if err := validateBundleManifest(*manifest); err != nil {
		return nil, err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode metadata bundle manifest: %w", err)
	}
	return data, nil
}

// LoadBundleBytes strictly validates the canonical manifest and all content
// bindings before delegating JWS, catalog, and freshness verification to
// LoadSnapshot.
func LoadBundleBytes(manifestData, formulaData, migrationsData []byte, options BundleLoadOptions) (*Snapshot, BundleManifest, error) {
	if int64(len(manifestData)) > DefaultMaxBundleManifestBytes {
		return nil, BundleManifest{}, fmt.Errorf("metadata bundle manifest exceeds %d bytes", DefaultMaxBundleManifestBytes)
	}
	if int64(len(formulaData)) > DefaultMaxFormulaBytes {
		return nil, BundleManifest{}, fmt.Errorf("%s: %w: read more than %d bytes", BundleFormulaFilename, ErrResponseTooLarge, DefaultMaxFormulaBytes)
	}
	if int64(len(migrationsData)) > DefaultMaxMigrationsBytes {
		return nil, BundleManifest{}, fmt.Errorf("%s: %w: read more than %d bytes", BundleMigrationsFilename, ErrResponseTooLarge, DefaultMaxMigrationsBytes)
	}

	manifest, err := decodeBundleManifest(manifestData)
	if err != nil {
		return nil, BundleManifest{}, err
	}
	if err := validateBundleContent(manifest.Formula, formulaData); err != nil {
		return nil, BundleManifest{}, fmt.Errorf("formula bundle member: %w", err)
	}
	if err := validateBundleContent(manifest.Migrations, migrationsData); err != nil {
		return nil, BundleManifest{}, fmt.Errorf("migrations bundle member: %w", err)
	}

	formulaGeneratedAt, _ := time.Parse(time.RFC3339, manifest.Formula.GeneratedAt)
	migrationsGeneratedAt, _ := time.Parse(time.RFC3339, manifest.Migrations.GeneratedAt)
	snapshot, err := LoadSnapshot(
		SignedSource{Data: formulaData, URL: manifest.Formula.URL, GeneratedAt: formulaGeneratedAt},
		SignedSource{Data: migrationsData, URL: manifest.Migrations.URL, GeneratedAt: migrationsGeneratedAt},
		LoadOptions{
			Keys:         options.Keys,
			Verification: options.Verification,
			Freshness:    options.Freshness,
			Now:          options.Now,
		},
	)
	if err != nil {
		return nil, BundleManifest{}, fmt.Errorf("load metadata bundle snapshot: %w", err)
	}
	return snapshot, manifest, nil
}

func decodeBundleManifest(data []byte) (BundleManifest, error) {
	if len(data) == 0 {
		return BundleManifest{}, errors.New("metadata bundle manifest is empty")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return BundleManifest{}, fmt.Errorf("decode metadata bundle manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest BundleManifest
	if err := decoder.Decode(&manifest); err != nil {
		return BundleManifest{}, fmt.Errorf("decode metadata bundle manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return BundleManifest{}, errors.New("decode metadata bundle manifest: trailing JSON value")
		}
		return BundleManifest{}, fmt.Errorf("decode metadata bundle manifest trailing data: %w", err)
	}
	canonical, err := CanonicalBundleManifest(&manifest)
	if err != nil {
		return BundleManifest{}, fmt.Errorf("validate metadata bundle manifest: %w", err)
	}
	if !bytes.Equal(data, canonical) {
		return BundleManifest{}, errors.New("metadata bundle manifest is not canonical JSON")
	}
	return manifest, nil
}

func validateBundleManifest(manifest BundleManifest) error {
	if manifest.SchemaVersion != BundleSchema {
		return fmt.Errorf("schema_version must be %q", BundleSchema)
	}
	if err := validateBundleDocument(manifest.Formula, BundleFormulaFilename, OfficialFormulaURL, DefaultMaxFormulaBytes); err != nil {
		return fmt.Errorf("formula: %w", err)
	}
	if err := validateBundleDocument(manifest.Migrations, BundleMigrationsFilename, OfficialMigrationsURL, DefaultMaxMigrationsBytes); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	return nil
}

func validateBundleDocument(document BundleDocument, filename, sourceURL string, limit int64) error {
	if document.Filename != filename {
		return fmt.Errorf("filename must be %q", filename)
	}
	if document.URL != sourceURL {
		return fmt.Errorf("url must be %q", sourceURL)
	}
	generatedAt, err := time.Parse(time.RFC3339, document.GeneratedAt)
	if err != nil || document.GeneratedAt != generatedAt.UTC().Format(time.RFC3339) || generatedAt.Nanosecond() != 0 || generatedAt.IsZero() {
		return fmt.Errorf("generated_at must be a non-zero canonical UTC RFC3339 timestamp")
	}
	if document.Size <= 0 || document.Size > limit {
		return fmt.Errorf("size must be between 1 and %d", limit)
	}
	if !validSHA256Digest(document.EnvelopeDigest) {
		return errors.New("envelope_digest must be a canonical sha256 digest")
	}
	return nil
}

func validateBundleContent(document BundleDocument, data []byte) error {
	if int64(len(data)) != document.Size {
		return fmt.Errorf("size is %d, manifest requires %d", len(data), document.Size)
	}
	actual := digestBytes(data)
	if actual != document.EnvelopeDigest {
		return fmt.Errorf("envelope digest is %q, manifest requires %q", actual, document.EnvelopeDigest)
	}
	return nil
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
