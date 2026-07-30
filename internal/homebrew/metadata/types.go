// Package metadata fetches, authenticates, and indexes the signed Homebrew
// Formula metadata used by the V1 resolver.
package metadata

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is Homebrew's public, signed metadata API.
	DefaultBaseURL = "https://formulae.brew.sh/api/"

	FormulaEndpoint    = "formula.jws.json"
	MigrationsEndpoint = "formula_tap_migrations.jws.json"

	DefaultRequiredKeyID = "homebrew-1"
	JWSAlgorithmPS512    = "PS512"

	DefaultMaxFormulaBytes    int64 = 64 << 20
	DefaultMaxMigrationsBytes int64 = 2 << 20

	SnapshotSchema = "homebrew-metadata-snapshot/v1"
)

var (
	ErrInvalidJWS                  = errors.New("invalid JWS")
	ErrUnknownSigner               = errors.New("unknown JWS signer")
	ErrSignatureMismatch           = errors.New("JWS signature mismatch")
	ErrRequiredSignatureMissing    = errors.New("required JWS signature missing")
	ErrResponseTooLarge            = errors.New("metadata response exceeds configured limit")
	ErrGeneratedDateMissing        = errors.New("metadata generated date is missing")
	ErrMetadataStale               = errors.New("metadata is stale")
	ErrMetadataFromFuture          = errors.New("metadata generated date is in the future")
	ErrMetadataRollback            = errors.New("metadata generated date is below rollback floor")
	ErrInvalidCatalog              = errors.New("invalid Homebrew metadata catalog")
	ErrInvalidFormulaName          = errors.New("invalid Formula name")
	ErrFormulaNotFound             = errors.New("Formula not found")
	ErrVersionedFormulaNotExplicit = errors.New("versioned Formula must be an explicit canonical name")
	ErrOutOfCore                   = errors.New("Formula identity is outside homebrew/core")
	ErrBottleUnavailable           = errors.New("current stable bottle metadata is unavailable")
	ErrFormulaDisabled             = errors.New("Formula is disabled")
)

// SignatureInfo is stable, non-secret evidence for a successfully verified
// signature. Digests use the sha256:<hex> form.
type SignatureInfo struct {
	KeyID           string `json:"key_id"`
	Algorithm       string `json:"algorithm"`
	Verified        bool   `json:"verified"`
	ProtectedDigest string `json:"protected_digest"`
	SignatureDigest string `json:"signature_digest"`
}

// GeneratedAtSource records whether freshness came from an authenticated
// wrapper field or from the HTTP Last-Modified metadata used by the current
// aggregate Homebrew endpoints.
type GeneratedAtSource string

const (
	GeneratedAtSignedPayload GeneratedAtSource = "signed-payload"
	GeneratedAtLastModified  GeneratedAtSource = "http-last-modified"
)

// DocumentInfo describes one signed metadata document.
type DocumentInfo struct {
	URL               string            `json:"url"`
	Size              int64             `json:"size"`
	EnvelopeDigest    string            `json:"envelope_digest"`
	PayloadDigest     string            `json:"payload_digest"`
	GeneratedAt       time.Time         `json:"generated_at"`
	GeneratedAtSource GeneratedAtSource `json:"generated_at_source"`
	Signatures        []SignatureInfo   `json:"signatures"`
}

// SnapshotInfo is the immutable identity and freshness evidence for a pair of
// Formula and migration documents. Digest is computed from the two signed
// payload digests, not from transport headers or randomized RSA-PSS bytes.
type SnapshotInfo struct {
	SchemaVersion   string       `json:"schema_version"`
	Digest          string       `json:"digest"`
	FormulaDigest   string       `json:"formula_digest"`
	MigrationDigest string       `json:"migration_digest"`
	GeneratedAt     time.Time    `json:"generated_at"`
	FetchedAt       time.Time    `json:"fetched_at"`
	Formula         DocumentInfo `json:"formula"`
	Migrations      DocumentInfo `json:"migrations"`
}

// BottleFile is one authenticated bottle entry from formula.json.
type BottleFile struct {
	Tag    string `json:"tag"`
	Cellar string `json:"cellar"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Bottle is the current stable bottle metadata for a Formula.
type Bottle struct {
	Rebuild int          `json:"rebuild"`
	RootURL string       `json:"root_url"`
	Files   []BottleFile `json:"files"`
}

// File returns an exact bottle tag, falling back to Homebrew's portable "all"
// tag when present.
func (b Bottle) File(tag string) (BottleFile, bool) {
	for _, file := range b.Files {
		if file.Tag == tag {
			return file, true
		}
	}
	if tag != "all" {
		for _, file := range b.Files {
			if file.Tag == "all" {
				return file, true
			}
		}
	}
	return BottleFile{}, false
}

// Variation contains the V1-relevant fields that Homebrew shallowly overlays
// for a target bottle tag.
type Variation struct {
	Tag                   string   `json:"tag"`
	Dependencies          []string `json:"dependencies,omitempty"`
	OverridesDependencies bool     `json:"overrides_dependencies,omitempty"`
	KegOnly               bool     `json:"keg_only,omitempty"`
	OverridesKegOnly      bool     `json:"overrides_keg_only,omitempty"`
}

// Formula is the current stable metadata needed by the V1 resolver. FullName
// is normalized to homebrew/core/<name> even though Homebrew's aggregate API
// currently emits an unqualified full_name for core Formulae.
type Formula struct {
	Name              string      `json:"name"`
	FullName          string      `json:"full_name"`
	Tap               string      `json:"tap"`
	OldNames          []string    `json:"oldnames,omitempty"`
	Aliases           []string    `json:"aliases,omitempty"`
	VersionedFormulae []string    `json:"versioned_formulae,omitempty"`
	Description       string      `json:"description,omitempty"`
	License           string      `json:"license,omitempty"`
	Homepage          string      `json:"homepage,omitempty"`
	StableVersion     string      `json:"stable_version"`
	Revision          int         `json:"revision"`
	VersionScheme     int         `json:"version_scheme"`
	KegOnly           bool        `json:"keg_only,omitempty"`
	Disabled          bool        `json:"disabled,omitempty"`
	Dependencies      []string    `json:"dependencies,omitempty"`
	Variations        []Variation `json:"variations,omitempty"`
	Bottle            *Bottle     `json:"bottle,omitempty"`
}

// PkgVersion returns Homebrew's stable package version with its Formula
// revision suffix.
func (f Formula) PkgVersion() string {
	if f.Revision > 0 {
		return fmt.Sprintf("%s_%d", f.StableVersion, f.Revision)
	}
	return f.StableVersion
}

// DependenciesFor applies Homebrew's shallow variation semantics for a bottle
// tag. The returned slice is always caller-owned.
func (f Formula) DependenciesFor(tag string) []string {
	for _, variation := range f.Variations {
		if variation.Tag == tag && variation.OverridesDependencies {
			return slices.Clone(variation.Dependencies)
		}
	}
	return slices.Clone(f.Dependencies)
}

// KegOnlyFor applies a target-specific keg_only override when present.
func (f Formula) KegOnlyFor(tag string) bool {
	for _, variation := range f.Variations {
		if variation.Tag == tag && variation.OverridesKegOnly {
			return variation.KegOnly
		}
	}
	return f.KegOnly
}

// BottleFor selects an exact bottle tag with Homebrew's "all" fallback.
func (f Formula) BottleFor(tag string) (BottleFile, error) {
	if f.Bottle == nil {
		return BottleFile{}, fmt.Errorf("%w for %q", ErrBottleUnavailable, f.Name)
	}
	file, ok := f.Bottle.File(tag)
	if !ok {
		return BottleFile{}, fmt.Errorf("%w for %q and tag %q", ErrBottleUnavailable, f.Name, tag)
	}
	return file, nil
}

// Migration describes one authenticated formula tap migration. InCore is
// false for cask and third-party targets; V1 retains these entries so lookup
// can reject them explicitly instead of treating them as unknown names.
type Migration struct {
	Name       string `json:"name"`
	Target     string `json:"target"`
	TargetName string `json:"target_name,omitempty"`
	InCore     bool   `json:"in_core"`
}

// MatchKind identifies how a requested name reached a canonical Formula.
type MatchKind string

const (
	MatchCanonical MatchKind = "canonical"
	MatchAlias     MatchKind = "alias"
	MatchOldName   MatchKind = "oldname"
	MatchMigration MatchKind = "migration"
)

// Match is the V1 lookup result.
type Match struct {
	Requested string    `json:"requested"`
	Canonical string    `json:"canonical"`
	Kind      MatchKind `json:"kind"`
	Formula   Formula   `json:"formula"`
}

// LookupError adds the requested and optional target identity while preserving
// errors.Is support through Unwrap.
type LookupError struct {
	Name   string
	Target string
	Err    error
}

func (e *LookupError) Error() string {
	if e.Target != "" {
		return fmt.Sprintf("Formula %q targets %q: %v", e.Name, e.Target, e.Err)
	}
	return fmt.Sprintf("Formula %q: %v", e.Name, e.Err)
}

func (e *LookupError) Unwrap() error { return e.Err }

func cloneSignatureInfo(in []SignatureInfo) []SignatureInfo {
	return slices.Clone(in)
}

func cloneDocumentInfo(in DocumentInfo) DocumentInfo {
	in.Signatures = cloneSignatureInfo(in.Signatures)
	return in
}

func cloneSnapshotInfo(in SnapshotInfo) SnapshotInfo {
	in.Formula = cloneDocumentInfo(in.Formula)
	in.Migrations = cloneDocumentInfo(in.Migrations)
	return in
}

func cloneFormula(in Formula) Formula {
	in.OldNames = slices.Clone(in.OldNames)
	in.Aliases = slices.Clone(in.Aliases)
	in.VersionedFormulae = slices.Clone(in.VersionedFormulae)
	in.Dependencies = slices.Clone(in.Dependencies)
	in.Variations = slices.Clone(in.Variations)
	for i := range in.Variations {
		in.Variations[i].Dependencies = slices.Clone(in.Variations[i].Dependencies)
	}
	if in.Bottle != nil {
		bottle := *in.Bottle
		bottle.Files = slices.Clone(bottle.Files)
		in.Bottle = &bottle
	}
	return in
}

func validateSimpleName(name string) error {
	if !formulaNamePattern.MatchString(name) || strings.ContainsAny(name, `/\\`) {
		return fmt.Errorf("%w %q", ErrInvalidFormulaName, name)
	}
	return nil
}
