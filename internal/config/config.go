package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

const (
	DefaultAttestationWaiver = "homebrew-jws-and-verified-oci-chain-v1"
	DefaultFormulaURL        = "https://formulae.brew.sh/api/formula.jws.json"
	DefaultMigrationsURL     = "https://formulae.brew.sh/api/formula_tap_migrations.jws.json"
)

// These are populated in release frontend builds with -X. Digest-pinned build
// args remain available for local development and reproducibility tests.
var (
	RuntimeBaseRef         string
	MaterializerRef        string
	FrontendRef            string
	HomebrewCommit         string
	VerificationKeysDigest string
	PortableRubyVersion    string
)

type Config struct {
	FormulaURL             string
	MigrationsURL          string
	MetadataMaxAge         time.Duration
	MetadataNotBefore      time.Time
	RuntimeBaseRef         string
	MaterializerRef        string
	FrontendRef            string
	HomebrewCommit         string
	VerificationKeysDigest string
	PortableRubyVersion    string
	SkipTests              bool
	AllowUnattested        bool
	AttestationWaiver      string
}

func FromBuildOpts(opts map[string]string) (Config, error) {
	get := func(k, fallback string) string {
		if v := opts["build-arg:"+k]; v != "" {
			return v
		}
		return fallback
	}
	var errs []error
	bind := func(key, compiled string) string {
		supplied := opts["build-arg:"+key]
		if compiled != "" {
			if supplied != "" && supplied != compiled {
				errs = append(errs, fmt.Errorf("%s differs from the release-bound value", key))
			}
			return compiled
		}
		return supplied
	}
	frontendBinding := firstNonEmpty(FrontendRef, opts["source"])
	releaseBound := RuntimeBaseRef != "" || MaterializerRef != "" || HomebrewCommit != "" || VerificationKeysDigest != ""
	cfg := Config{
		FormulaURL: get("DALEC_HOMEBREW_METADATA_URL", DefaultFormulaURL), MigrationsURL: get("DALEC_HOMEBREW_MIGRATIONS_URL", DefaultMigrationsURL), MetadataMaxAge: 7 * 24 * time.Hour,
		RuntimeBaseRef: bind("DALEC_HOMEBREW_RUNTIME_BASE", RuntimeBaseRef), MaterializerRef: bind("DALEC_HOMEBREW_MATERIALIZER", MaterializerRef), FrontendRef: bind("DALEC_HOMEBREW_FRONTEND_REF", frontendBinding),
		HomebrewCommit: bind("DALEC_HOMEBREW_COMMIT", HomebrewCommit), VerificationKeysDigest: bind("DALEC_HOMEBREW_KEYS_DIGEST", VerificationKeysDigest), PortableRubyVersion: bind("DALEC_HOMEBREW_RUBY_VERSION", firstNonEmpty(PortableRubyVersion, "4.0.6")),
		AttestationWaiver: get("DALEC_HOMEBREW_ATTESTATION_WAIVER", DefaultAttestationWaiver),
	}
	if source := opts["source"]; source != "" && cfg.FrontendRef != source {
		errs = append(errs, errors.New("frontend binding differs from the invoking gateway source"))
	}
	if releaseBound && (cfg.FormulaURL != DefaultFormulaURL || cfg.MigrationsURL != DefaultMigrationsURL) {
		errs = append(errs, errors.New("release-bound frontends cannot override official Homebrew metadata endpoints"))
	}
	if raw := get("DALEC_HOMEBREW_METADATA_MAX_AGE", ""); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("invalid DALEC_HOMEBREW_METADATA_MAX_AGE %q", raw))
		} else if releaseBound && d > 7*24*time.Hour {
			errs = append(errs, fmt.Errorf("release-bound metadata max age %s exceeds 168h", d))
		} else {
			cfg.MetadataMaxAge = d
		}
	}
	if raw := get("DALEC_HOMEBREW_METADATA_NOT_BEFORE", ""); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid DALEC_HOMEBREW_METADATA_NOT_BEFORE %q: %w", raw, err))
		} else {
			cfg.MetadataNotBefore = t
		}
	}
	cfg.SkipTests = parseBool(get("DALEC_SKIP_TESTS", ""))
	if releaseBound && cfg.SkipTests {
		errs = append(errs, errors.New("release-bound frontends cannot skip runtime tests"))
	}
	cfg.AllowUnattested = parseBool(get("DALEC_HOMEBREW_ALLOW_UNATTESTED", "1"))
	for name, ref := range map[string]string{"runtime base": cfg.RuntimeBaseRef, "materializer": cfg.MaterializerRef} {
		if err := validatePinnedRef(ref); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if cfg.FrontendRef != "" {
		if err := validatePinnedRef(cfg.FrontendRef); err != nil {
			errs = append(errs, fmt.Errorf("frontend: %w", err))
		}
	}
	if cfg.PortableRubyVersion == "" {
		errs = append(errs, errors.New("portable Ruby version is not bound into the component tuple"))
	}
	if cfg.HomebrewCommit == "" {
		errs = append(errs, errors.New("Homebrew commit is not bound into the component tuple"))
	}
	if err := validateDigest(cfg.VerificationKeysDigest); err != nil {
		errs = append(errs, fmt.Errorf("verification key set: %w", err))
	} else if cfg.VerificationKeysDigest != metadata.DefaultKeySetDigest() {
		errs = append(errs, fmt.Errorf("verification key set digest %s does not match embedded key set %s", cfg.VerificationKeysDigest, metadata.DefaultKeySetDigest()))
	}
	if !cfg.AllowUnattested {
		errs = append(errs, errors.New("upstream attestation verification is required but no verifier is configured"))
	}
	if cfg.AttestationWaiver != DefaultAttestationWaiver {
		errs = append(errs, fmt.Errorf("unsupported V1 attestation waiver %q", cfg.AttestationWaiver))
	}
	return cfg, errors.Join(errs...)
}

func parseBool(s string) bool {
	v, _ := strconv.ParseBool(s)
	return v || s == "1"
}

func validatePinnedRef(ref string) error { return resolution.ValidatePinnedReference(ref) }

func validateDigest(value string) error {
	if value == "" {
		return errors.New("sha256 digest is required")
	}
	d, err := digest.Parse(value)
	if err != nil {
		return err
	}
	if d.Algorithm() != digest.SHA256 {
		return fmt.Errorf("only sha256 is accepted")
	}
	return d.Validate()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
