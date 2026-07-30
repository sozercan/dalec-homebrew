package metadata

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	DefaultHTTPTimeout   = 30 * time.Second
	DefaultMaxAge        = 7 * 24 * time.Hour
	DefaultMaxFutureSkew = 15 * time.Minute
)

// FreshnessPolicy validates each component's generated date. Negative MaxAge
// or MaxFutureSkew disables that individual bound. Zero values are replaced by
// the package defaults when a Client or LoadSnapshot normalizes the policy.
type FreshnessPolicy struct {
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	RollbackFloor time.Time
}

// DefaultFreshnessPolicy returns the fail-closed default policy.
func DefaultFreshnessPolicy() FreshnessPolicy {
	return FreshnessPolicy{
		MaxAge:        DefaultMaxAge,
		MaxFutureSkew: DefaultMaxFutureSkew,
	}
}

func (p FreshnessPolicy) normalized() FreshnessPolicy {
	if p.MaxAge == 0 {
		p.MaxAge = DefaultMaxAge
	}
	if p.MaxFutureSkew == 0 {
		p.MaxFutureSkew = DefaultMaxFutureSkew
	}
	p.RollbackFloor = p.RollbackFloor.UTC().Round(0)
	return p
}

// ValidateGeneratedAt enforces freshness, future skew, and rollback floor.
func ValidateGeneratedAt(generatedAt, now time.Time, policy FreshnessPolicy) error {
	if generatedAt.IsZero() {
		return ErrGeneratedDateMissing
	}
	generatedAt = generatedAt.UTC().Round(0)
	now = now.UTC().Round(0)
	policy = policy.normalized()
	if policy.MaxFutureSkew >= 0 && generatedAt.After(now.Add(policy.MaxFutureSkew)) {
		return fmt.Errorf("%w: generated %s, now %s, maximum future skew %s", ErrMetadataFromFuture, generatedAt.Format(time.RFC3339), now.Format(time.RFC3339), policy.MaxFutureSkew)
	}
	if !policy.RollbackFloor.IsZero() && generatedAt.Before(policy.RollbackFloor) {
		return fmt.Errorf("%w: generated %s, floor %s", ErrMetadataRollback, generatedAt.Format(time.RFC3339), policy.RollbackFloor.Format(time.RFC3339))
	}
	if policy.MaxAge >= 0 && generatedAt.Before(now.Add(-policy.MaxAge)) {
		return fmt.Errorf("%w: generated %s, now %s, maximum age %s", ErrMetadataStale, generatedAt.Format(time.RFC3339), now.Format(time.RFC3339), policy.MaxAge)
	}
	return nil
}

// Config configures a metadata Client. Zero limits and an empty base URL use
// package defaults. A zero KeySet uses the embedded pinned Homebrew key.
type Config struct {
	BaseURL            string
	HTTPClient         *http.Client
	Keys               KeySet
	Verification       VerificationPolicy
	Freshness          FreshnessPolicy
	MaxFormulaBytes    int64
	MaxMigrationsBytes int64
	Clock              func() time.Time
}

// Fetch creates a Client from config and returns one authenticated snapshot.
func Fetch(ctx context.Context, config Config) (*Snapshot, error) {
	client, err := NewClient(config)
	if err != nil {
		return nil, err
	}
	return client.Fetch(ctx)
}

// Client fetches one pair of signed Homebrew metadata documents.
type Client struct {
	baseURL            *url.URL
	httpClient         *http.Client
	keys               KeySet
	verification       VerificationPolicy
	freshness          FreshnessPolicy
	maxFormulaBytes    int64
	maxMigrationsBytes int64
	clock              func() time.Time
}

// NewClient validates and freezes configuration.
func NewClient(config Config) (*Client, error) {
	base := config.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	parsedBase, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse metadata base URL: %w", err)
	}
	if (parsedBase.Scheme != "https" && parsedBase.Scheme != "http") || parsedBase.Host == "" || parsedBase.User != nil || parsedBase.RawQuery != "" || parsedBase.Fragment != "" {
		return nil, fmt.Errorf("metadata base URL must be an absolute http(s) URL without userinfo, query, or fragment")
	}
	if !strings.HasSuffix(parsedBase.Path, "/") {
		parsedBase.Path += "/"
	}

	keys := config.Keys
	if keys.empty() {
		keys, err = DefaultKeySet()
		if err != nil {
			return nil, err
		}
	}
	verification, err := config.Verification.normalized()
	if err != nil {
		return nil, err
	}
	if _, ok := keys.get(verification.RequiredKeyID); !ok {
		return nil, fmt.Errorf("required JWS kid %q is absent from verification key set", verification.RequiredKeyID)
	}

	maxFormulaBytes := config.MaxFormulaBytes
	if maxFormulaBytes == 0 {
		maxFormulaBytes = DefaultMaxFormulaBytes
	}
	maxMigrationsBytes := config.MaxMigrationsBytes
	if maxMigrationsBytes == 0 {
		maxMigrationsBytes = DefaultMaxMigrationsBytes
	}
	if maxFormulaBytes < 0 || maxMigrationsBytes < 0 {
		return nil, fmt.Errorf("metadata byte limits must be positive")
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Client{
		baseURL:            parsedBase,
		httpClient:         httpClient,
		keys:               keys,
		verification:       verification,
		freshness:          config.Freshness.normalized(),
		maxFormulaBytes:    maxFormulaBytes,
		maxMigrationsBytes: maxMigrationsBytes,
		clock:              clock,
	}, nil
}

// Fetch downloads, bounds, verifies, freshness-checks, and indexes both signed
// Homebrew documents into one immutable Snapshot.
func (c *Client) Fetch(ctx context.Context) (*Snapshot, error) {
	if c == nil {
		return nil, fmt.Errorf("metadata client is nil")
	}
	formulaURL := c.resolveEndpoint(FormulaEndpoint)
	migrationURL := c.resolveEndpoint(MigrationsEndpoint)
	formula, err := c.fetchSource(ctx, formulaURL, c.maxFormulaBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", FormulaEndpoint, err)
	}
	migrations, err := c.fetchSource(ctx, migrationURL, c.maxMigrationsBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", MigrationsEndpoint, err)
	}
	return LoadSnapshot(formula, migrations, LoadOptions{
		Keys:         c.keys,
		Verification: c.verification,
		Freshness:    c.freshness,
		Now:          c.clock().UTC().Round(0),
	})
}

func (c *Client) resolveEndpoint(endpoint string) string {
	reference := &url.URL{Path: path.Base(endpoint)}
	return c.baseURL.ResolveReference(reference).String()
}

func (c *Client) fetchSource(ctx context.Context, endpoint string, limit int64) (SignedSource, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SignedSource{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "dalec-homebrew-metadata/1")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return SignedSource{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return SignedSource{}, fmt.Errorf("unexpected HTTP status %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	if response.ContentLength > limit {
		return SignedSource{}, fmt.Errorf("%w: Content-Length %d is greater than %d", ErrResponseTooLarge, response.ContentLength, limit)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return SignedSource{}, err
	}
	if int64(len(data)) > limit {
		return SignedSource{}, fmt.Errorf("%w: read more than %d bytes", ErrResponseTooLarge, limit)
	}
	generatedAt := time.Time{}
	if value := response.Header.Get("Last-Modified"); value != "" {
		generatedAt, err = http.ParseTime(value)
		if err != nil {
			return SignedSource{}, fmt.Errorf("invalid Last-Modified header %q: %w", value, err)
		}
	}
	return SignedSource{
		Data:        data,
		URL:         endpoint,
		GeneratedAt: generatedAt.UTC().Round(0),
	}, nil
}

// SignedSource is a fetched or cached JWS document. GeneratedAt is normally
// the parsed HTTP Last-Modified value for the current aggregate endpoints; an
// authenticated generated_date wrapper field takes precedence when present.
type SignedSource struct {
	Data        []byte
	URL         string
	GeneratedAt time.Time
}

// LoadOptions configures offline verification of two SignedSource values.
type LoadOptions struct {
	Keys         KeySet
	Verification VerificationPolicy
	Freshness    FreshnessPolicy
	Now          time.Time
}

// LoadSnapshot verifies and indexes two already-read bounded documents.
func LoadSnapshot(formulaSource, migrationSource SignedSource, options LoadOptions) (*Snapshot, error) {
	keys := options.Keys
	var err error
	if keys.empty() {
		keys, err = DefaultKeySet()
		if err != nil {
			return nil, err
		}
	}
	verification, err := options.Verification.normalized()
	if err != nil {
		return nil, err
	}
	if _, ok := keys.get(verification.RequiredKeyID); !ok {
		return nil, fmt.Errorf("required JWS kid %q is absent from verification key set", verification.RequiredKeyID)
	}
	freshness := options.Freshness.normalized()
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Round(0)

	formulaJWS, err := VerifyJWS(formulaSource.Data, keys, verification)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", FormulaEndpoint, err)
	}
	migrationJWS, err := VerifyJWS(migrationSource.Data, keys, verification)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", MigrationsEndpoint, err)
	}

	formulaBody, formulaDateString, err := unwrapFormulaPayload(formulaJWS.Payload())
	if err != nil {
		return nil, fmt.Errorf("parse %s payload: %w", FormulaEndpoint, err)
	}
	migrationBody, migrationDateString, err := unwrapMigrationPayload(migrationJWS.Payload())
	if err != nil {
		return nil, fmt.Errorf("parse %s payload: %w", MigrationsEndpoint, err)
	}
	formulaGeneratedAt, formulaDateSource, err := chooseGeneratedAt(formulaDateString, formulaSource.GeneratedAt)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FormulaEndpoint, err)
	}
	migrationGeneratedAt, migrationDateSource, err := chooseGeneratedAt(migrationDateString, migrationSource.GeneratedAt)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MigrationsEndpoint, err)
	}
	if err := ValidateGeneratedAt(formulaGeneratedAt, now, freshness); err != nil {
		return nil, fmt.Errorf("%s freshness: %w", FormulaEndpoint, err)
	}
	if err := ValidateGeneratedAt(migrationGeneratedAt, now, freshness); err != nil {
		return nil, fmt.Errorf("%s freshness: %w", MigrationsEndpoint, err)
	}

	catalog, err := ParseCatalog(formulaBody, migrationBody)
	if err != nil {
		return nil, err
	}
	formulaInfo := DocumentInfo{
		URL:               formulaSource.URL,
		Size:              int64(len(formulaSource.Data)),
		EnvelopeDigest:    formulaJWS.EnvelopeDigest(),
		PayloadDigest:     formulaJWS.PayloadDigest(),
		GeneratedAt:       formulaGeneratedAt,
		GeneratedAtSource: formulaDateSource,
		Signatures:        formulaJWS.Signatures(),
	}
	migrationInfo := DocumentInfo{
		URL:               migrationSource.URL,
		Size:              int64(len(migrationSource.Data)),
		EnvelopeDigest:    migrationJWS.EnvelopeDigest(),
		PayloadDigest:     migrationJWS.PayloadDigest(),
		GeneratedAt:       migrationGeneratedAt,
		GeneratedAtSource: migrationDateSource,
		Signatures:        migrationJWS.Signatures(),
	}
	generatedAt := formulaGeneratedAt
	if migrationGeneratedAt.Before(generatedAt) {
		generatedAt = migrationGeneratedAt
	}
	info := SnapshotInfo{
		SchemaVersion:   SnapshotSchema,
		Digest:          snapshotDigest(formulaJWS.PayloadDigest(), migrationJWS.PayloadDigest()),
		FormulaDigest:   formulaJWS.PayloadDigest(),
		MigrationDigest: migrationJWS.PayloadDigest(),
		GeneratedAt:     generatedAt,
		FetchedAt:       now,
		Formula:         formulaInfo,
		Migrations:      migrationInfo,
	}
	return &Snapshot{catalog: catalog, info: info}, nil
}

func chooseGeneratedAt(signedDate string, fallback time.Time) (time.Time, GeneratedAtSource, error) {
	if signedDate != "" {
		parsed, err := parseGeneratedDate(signedDate)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("invalid signed generated_date %q: %w", signedDate, err)
		}
		return parsed.UTC().Round(0), GeneratedAtSignedPayload, nil
	}
	if fallback.IsZero() {
		return time.Time{}, "", ErrGeneratedDateMissing
	}
	return fallback.UTC().Round(0), GeneratedAtLastModified, nil
}

func parseGeneratedDate(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	if parsed, err := http.ParseTime(value); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unsupported date format")
}

// Snapshot binds one immutable Catalog to stable digest and signature evidence.
type Snapshot struct {
	catalog *Catalog
	info    SnapshotInfo
}

// Catalog returns the immutable catalog. Its exported accessors return copies.
func (s *Snapshot) Catalog() *Catalog {
	if s == nil {
		return nil
	}
	return s.catalog
}

// Info returns caller-owned snapshot metadata.
func (s *Snapshot) Info() SnapshotInfo {
	if s == nil {
		return SnapshotInfo{}
	}
	return cloneSnapshotInfo(s.info)
}

// Digest returns the stable combined signed-payload digest.
func (s *Snapshot) Digest() string {
	if s == nil {
		return ""
	}
	return s.info.Digest
}

// FormulaDigest returns the signed Formula payload digest.
func (s *Snapshot) FormulaDigest() string {
	if s == nil {
		return ""
	}
	return s.info.FormulaDigest
}

// MigrationDigest returns the signed migration payload digest.
func (s *Snapshot) MigrationDigest() string {
	if s == nil {
		return ""
	}
	return s.info.MigrationDigest
}

// GeneratedAt returns the older generated date of the two components.
func (s *Snapshot) GeneratedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.info.GeneratedAt
}

// Signatures returns sorted caller-owned verification evidence for both documents.
func (s *Snapshot) Signatures() []SignatureInfo {
	if s == nil {
		return nil
	}
	out := append(cloneSignatureInfo(s.info.Formula.Signatures), s.info.Migrations.Signatures...)
	slices.SortFunc(out, func(a, b SignatureInfo) int {
		if c := strings.Compare(a.KeyID, b.KeyID); c != 0 {
			return c
		}
		if c := strings.Compare(a.Algorithm, b.Algorithm); c != 0 {
			return c
		}
		return strings.Compare(a.SignatureDigest, b.SignatureDigest)
	})
	return out
}

// Lookup delegates to the immutable catalog.
func (s *Snapshot) Lookup(name string) (Match, error) {
	if s == nil || s.catalog == nil {
		return Match{}, &LookupError{Name: name, Err: ErrFormulaNotFound}
	}
	return s.catalog.Lookup(name)
}
