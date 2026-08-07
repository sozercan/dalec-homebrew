package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

const (
	DefaultAttestationWaiver          = "homebrew-jws-and-verified-oci-chain-v1"
	DefaultFormulaURL                 = "https://formulae.brew.sh/api/formula.jws.json"
	DefaultMigrationsURL              = "https://formulae.brew.sh/api/formula_tap_migrations.jws.json"
	CatalogPolicyVersionV1            = "tap-catalog-v1"
	BottleFetchPolicyVersionV1        = policyv2.FetchPolicyVersion
	SigstoreProvenancePolicyVersionV1 = "sigstore-in-toto-v1"
	ChecksumWaiverPolicyVersionV1     = policyv2.NonCoreProvenanceWaiver
	HTTPSSourceWaiverPolicyVersionV1  = catalog.HTTPSBottleSourceWaiver
	PrebuiltWaiverPolicyVersionV1     = resolution.PrebuiltProvenanceWaiverPolicyV1
	CoreWaiverPolicyVersionV1         = DefaultAttestationWaiver

	BottleFetcherBuildArg                     = "DALEC_HOMEBREW_BOTTLE_FETCHER"
	CatalogExtractorBuildArg                  = "DALEC_HOMEBREW_CATALOG_EXTRACTOR"
	CatalogServiceOriginBuildArg              = "DALEC_HOMEBREW_CATALOG_SERVICE_ORIGIN"
	IngestionJWSKeyPolicyDigestBuildArg       = "DALEC_HOMEBREW_INGESTION_JWS_KEY_POLICY_DIGEST"
	TapPolicyDigestBuildArg                   = "DALEC_HOMEBREW_TAP_POLICY_DIGEST"
	ExecutableRuntimePolicyDigestBuildArg     = "DALEC_HOMEBREW_EXECUTABLE_RUNTIME_POLICY_DIGEST"
	SupportedCatalogPolicyVersionsBuildArg    = "DALEC_HOMEBREW_SUPPORTED_CATALOG_POLICY_VERSIONS"
	SupportedFetchPolicyVersionsBuildArg      = "DALEC_HOMEBREW_SUPPORTED_FETCH_POLICY_VERSIONS"
	SupportedProvenancePolicyVersionsBuildArg = "DALEC_HOMEBREW_SUPPORTED_PROVENANCE_POLICY_VERSIONS"
)

// These are populated in release frontend builds with -X. Existing V1
// bindings may still be supplied for local development and reproducibility
// tests. V2 values can be parsed from invocation options for diagnostics, but
// only a complete compiled tuple enables non-core capability. Supported policy
// version variables use comma-separated ASCII identifiers.
var dalecBuildArgs = []string{
	"DALEC_HOMEBREW_ALLOW_UNATTESTED",
	"DALEC_HOMEBREW_ATTESTATION_WAIVER",
	BottleFetcherBuildArg,
	CatalogExtractorBuildArg,
	CatalogServiceOriginBuildArg,
	"DALEC_HOMEBREW_COMMIT",
	ExecutableRuntimePolicyDigestBuildArg,
	"DALEC_HOMEBREW_FRONTEND_REF",
	IngestionJWSKeyPolicyDigestBuildArg,
	"DALEC_HOMEBREW_KEYS_DIGEST",
	"DALEC_HOMEBREW_MATERIALIZER",
	"DALEC_HOMEBREW_METADATA_MAX_AGE",
	"DALEC_HOMEBREW_METADATA_NOT_BEFORE",
	"DALEC_HOMEBREW_METADATA_URL",
	"DALEC_HOMEBREW_MIGRATIONS_URL",
	"DALEC_HOMEBREW_RUBY_VERSION",
	"DALEC_HOMEBREW_RUNTIME_BASE",
	"DALEC_SKIP_TESTS",
	SupportedCatalogPolicyVersionsBuildArg,
	SupportedFetchPolicyVersionsBuildArg,
	SupportedProvenancePolicyVersionsBuildArg,
	TapPolicyDigestBuildArg,
}

// DalecBuildArgs returns the exact frontend-owned build arguments that Dalec
// must ignore unless a spec explicitly references them. The frontend consumes
// and validates these arguments before Dalec substitutes spec-defined args.
func DalecBuildArgs() []string {
	return slices.Clone(dalecBuildArgs)
}

// BuildArgNames returns the frontend-owned build arguments that Dalec must
// permit without requiring declarations in the user spec.
func BuildArgNames() []string {
	return DalecBuildArgs()
}

var (
	RuntimeBaseRef         string
	MaterializerRef        string
	FrontendRef            string
	HomebrewCommit         string
	VerificationKeysDigest string
	PortableRubyVersion    string

	BottleFetcherRef            string
	CatalogExtractorRef         string
	CatalogServiceOrigin        string
	IngestionJWSKeyPolicyDigest string
	// IngestionJWSKeyPolicyBase64 contains the release-pinned public-key
	// policy bytes. It is compiled into capable frontends and is never accepted
	// as an invocation build option.
	IngestionJWSKeyPolicyBase64       string
	TapPolicyDigest                   string
	ExecutableRuntimePolicyDigest     string
	SupportedCatalogPolicyVersions    string
	SupportedFetchPolicyVersions      string
	SupportedProvenancePolicyVersions string
	MaterializerV2BindingsRequired    string
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

	BottleFetcherRef            string
	CatalogExtractorRef         string
	CatalogServiceOrigin        string
	IngestionJWSKeyPolicyDigest string
	// IngestionJWSKeyPolicyBase64 contains the release-pinned public-key
	// policy bytes. It is compiled into capable frontends and is never accepted
	// as an invocation build option.
	IngestionJWSKeyPolicyBase64       string
	TapPolicyDigest                   string
	ExecutableRuntimePolicyDigest     string
	SupportedCatalogPolicyVersions    []string
	SupportedFetchPolicyVersions      []string
	SupportedProvenancePolicyVersions []string

	compiledNonCoreTaps bool
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
	releaseBound := compiledReleaseBound()
	bindCapability := func(key, compiled string) string {
		supplied := opts["build-arg:"+key]
		if releaseBound {
			if supplied != "" && supplied != compiled {
				errs = append(errs, fmt.Errorf("%s differs from the release-bound value", key))
			}
			return compiled
		}
		return supplied
	}

	catalogPolicyVersionsRaw := bindCapability(SupportedCatalogPolicyVersionsBuildArg, SupportedCatalogPolicyVersions)
	fetchPolicyVersionsRaw := bindCapability(SupportedFetchPolicyVersionsBuildArg, SupportedFetchPolicyVersions)
	provenancePolicyVersionsRaw := bindCapability(SupportedProvenancePolicyVersionsBuildArg, SupportedProvenancePolicyVersions)
	catalogPolicyVersions, err := parsePolicyVersions("catalog", catalogPolicyVersionsRaw)
	if err != nil {
		errs = append(errs, err)
	}
	fetchPolicyVersions, err := parsePolicyVersions("fetch", fetchPolicyVersionsRaw)
	if err != nil {
		errs = append(errs, err)
	}
	provenancePolicyVersions, err := parsePolicyVersions("provenance", provenancePolicyVersionsRaw)
	if err != nil {
		errs = append(errs, err)
	}

	frontendBinding := firstNonEmpty(FrontendRef, opts["source"])
	cfg := Config{
		FormulaURL:             get("DALEC_HOMEBREW_METADATA_URL", DefaultFormulaURL),
		MigrationsURL:          get("DALEC_HOMEBREW_MIGRATIONS_URL", DefaultMigrationsURL),
		MetadataMaxAge:         7 * 24 * time.Hour,
		RuntimeBaseRef:         bind("DALEC_HOMEBREW_RUNTIME_BASE", RuntimeBaseRef),
		MaterializerRef:        bind("DALEC_HOMEBREW_MATERIALIZER", MaterializerRef),
		FrontendRef:            bind("DALEC_HOMEBREW_FRONTEND_REF", frontendBinding),
		HomebrewCommit:         bind("DALEC_HOMEBREW_COMMIT", HomebrewCommit),
		VerificationKeysDigest: bind("DALEC_HOMEBREW_KEYS_DIGEST", VerificationKeysDigest),
		PortableRubyVersion:    bind("DALEC_HOMEBREW_RUBY_VERSION", firstNonEmpty(PortableRubyVersion, "4.0.6")),
		AttestationWaiver:      get("DALEC_HOMEBREW_ATTESTATION_WAIVER", DefaultAttestationWaiver),

		BottleFetcherRef:                  bindCapability(BottleFetcherBuildArg, BottleFetcherRef),
		CatalogExtractorRef:               bindCapability(CatalogExtractorBuildArg, CatalogExtractorRef),
		CatalogServiceOrigin:              bindCapability(CatalogServiceOriginBuildArg, CatalogServiceOrigin),
		IngestionJWSKeyPolicyDigest:       bindCapability(IngestionJWSKeyPolicyDigestBuildArg, IngestionJWSKeyPolicyDigest),
		TapPolicyDigest:                   bindCapability(TapPolicyDigestBuildArg, TapPolicyDigest),
		ExecutableRuntimePolicyDigest:     bindCapability(ExecutableRuntimePolicyDigestBuildArg, ExecutableRuntimePolicyDigest),
		SupportedCatalogPolicyVersions:    catalogPolicyVersions,
		SupportedFetchPolicyVersions:      fetchPolicyVersions,
		SupportedProvenancePolicyVersions: provenancePolicyVersions,

		compiledNonCoreTaps: SupportsCompiledNonCoreTaps(),
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
	if err := validateNonCoreBindings(cfg); err != nil {
		errs = append(errs, err)
	}
	if hasCompiledNonCoreBindings() && CatalogExtractorRef == "" {
		if _, err := CompiledCatalogKeyPolicy(); err != nil {
			errs = append(errs, fmt.Errorf("compiled ingestion JWS key policy: %w", err))
		}
	}
	return cfg, errors.Join(errs...)
}

// SupportsNonCoreTaps reports whether this parsed configuration exactly
// retains a complete capability tuple compiled into the frontend. Invocation-
// filled values never enable the capability.
func (c Config) SupportsNonCoreTaps() bool {
	if !c.compiledNonCoreTaps || !supportsNonCoreBindings(c) {
		return false
	}
	compiled, ok := compiledNonCoreConfig()
	return ok &&
		c.RuntimeBaseRef == compiled.RuntimeBaseRef &&
		c.MaterializerRef == compiled.MaterializerRef &&
		c.HomebrewCommit == compiled.HomebrewCommit &&
		c.VerificationKeysDigest == compiled.VerificationKeysDigest &&
		c.PortableRubyVersion == compiled.PortableRubyVersion &&
		c.BottleFetcherRef == compiled.BottleFetcherRef &&
		c.CatalogExtractorRef == compiled.CatalogExtractorRef &&
		c.CatalogServiceOrigin == compiled.CatalogServiceOrigin &&
		c.IngestionJWSKeyPolicyDigest == compiled.IngestionJWSKeyPolicyDigest &&
		c.TapPolicyDigest == compiled.TapPolicyDigest &&
		c.ExecutableRuntimePolicyDigest == compiled.ExecutableRuntimePolicyDigest &&
		slices.Equal(c.SupportedCatalogPolicyVersions, compiled.SupportedCatalogPolicyVersions) &&
		slices.Equal(c.SupportedFetchPolicyVersions, compiled.SupportedFetchPolicyVersions) &&
		slices.Equal(c.SupportedProvenancePolicyVersions, compiled.SupportedProvenancePolicyVersions)
}

// SupportsCompiledNonCoreTaps reports whether the linked frontend binary has a
// complete V2 capability tuple, independent of invocation build options.
func SupportsCompiledNonCoreTaps() bool {
	_, ok := compiledNonCoreConfig()
	return ok
}

func compiledNonCoreConfig() (Config, bool) {
	catalog, err := parsePolicyVersions("catalog", SupportedCatalogPolicyVersions)
	if err != nil {
		return Config{}, false
	}
	fetch, err := parsePolicyVersions("fetch", SupportedFetchPolicyVersions)
	if err != nil {
		return Config{}, false
	}
	provenance, err := parsePolicyVersions("provenance", SupportedProvenancePolicyVersions)
	if err != nil {
		return Config{}, false
	}
	if CatalogExtractorRef == "" {
		if _, err := CompiledCatalogKeyPolicy(); err != nil {
			return Config{}, false
		}
	}
	if RuntimeBaseRef == "" || MaterializerRef == "" || HomebrewCommit == "" || VerificationKeysDigest == "" || PortableRubyVersion == "" {
		return Config{}, false
	}
	cfg := Config{
		RuntimeBaseRef: RuntimeBaseRef, MaterializerRef: MaterializerRef, HomebrewCommit: HomebrewCommit,
		VerificationKeysDigest: VerificationKeysDigest, PortableRubyVersion: PortableRubyVersion,
		BottleFetcherRef:                  BottleFetcherRef,
		CatalogExtractorRef:               CatalogExtractorRef,
		CatalogServiceOrigin:              CatalogServiceOrigin,
		IngestionJWSKeyPolicyDigest:       IngestionJWSKeyPolicyDigest,
		TapPolicyDigest:                   TapPolicyDigest,
		ExecutableRuntimePolicyDigest:     ExecutableRuntimePolicyDigest,
		SupportedCatalogPolicyVersions:    catalog,
		SupportedFetchPolicyVersions:      fetch,
		SupportedProvenancePolicyVersions: provenance,
	}
	return cfg, supportsNonCoreBindings(cfg)
}

func supportsNonCoreBindings(cfg Config) bool {
	return hasNonCoreBindings(cfg) && validateNonCoreBindings(cfg) == nil
}

func validateNonCoreBindings(cfg Config) error {
	if !hasNonCoreBindings(cfg) {
		return nil
	}
	var errs []error
	if err := validatePinnedRef(cfg.BottleFetcherRef); err != nil {
		errs = append(errs, fmt.Errorf("bottle fetcher: %w", err))
	}
	localMode := cfg.CatalogExtractorRef != ""
	serviceMode := cfg.CatalogServiceOrigin != "" || cfg.IngestionJWSKeyPolicyDigest != ""
	if localMode == serviceMode {
		errs = append(errs, errors.New("non-core bindings must select exactly one of build-local extractor or hosted catalog service"))
	}
	if localMode {
		if err := validatePinnedRef(cfg.CatalogExtractorRef); err != nil {
			errs = append(errs, fmt.Errorf("catalog extractor: %w", err))
		}
		if cfg.CatalogServiceOrigin != "" || cfg.IngestionJWSKeyPolicyDigest != "" || cfg.IngestionJWSKeyPolicyBase64 != "" {
			errs = append(errs, errors.New("build-local extraction cannot include hosted catalog-service bindings"))
		}
	} else {
		if err := validateHTTPSOrigin(cfg.CatalogServiceOrigin); err != nil {
			errs = append(errs, fmt.Errorf("catalog service origin: %w", err))
		}
		if err := validateDigest(cfg.IngestionJWSKeyPolicyDigest); err != nil {
			errs = append(errs, fmt.Errorf("ingestion JWS key policy: %w", err))
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "tap policy", value: cfg.TapPolicyDigest},
		{name: "executable runtime policy", value: cfg.ExecutableRuntimePolicyDigest},
	} {
		if err := validateDigest(field.value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field.name, err))
		}
	}
	for _, field := range []struct {
		name   string
		values []string
	}{
		{name: "catalog", values: cfg.SupportedCatalogPolicyVersions},
		{name: "fetch", values: cfg.SupportedFetchPolicyVersions},
		{name: "provenance", values: cfg.SupportedProvenancePolicyVersions},
	} {
		if err := validatePolicyVersions(field.name, field.values); err != nil {
			errs = append(errs, err)
		}
	}
	for _, field := range []struct {
		name   string
		values []string
		want   []string
	}{
		{name: "catalog", values: cfg.SupportedCatalogPolicyVersions, want: []string{CatalogPolicyVersionV1}},
		{name: "fetch", values: cfg.SupportedFetchPolicyVersions, want: []string{BottleFetchPolicyVersionV1}},
		{name: "provenance", values: cfg.SupportedProvenancePolicyVersions, want: []string{SigstoreProvenancePolicyVersionV1, ChecksumWaiverPolicyVersionV1, HTTPSSourceWaiverPolicyVersionV1, PrebuiltWaiverPolicyVersionV1, CoreWaiverPolicyVersionV1}},
	} {
		if err := validateExactPolicyVersions(field.name, field.values, field.want); err != nil {
			errs = append(errs, err)
		}
	}
	tapDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		errs = append(errs, fmt.Errorf("load embedded tap policy digest: %w", err))
	} else if cfg.TapPolicyDigest != tapDigest {
		errs = append(errs, fmt.Errorf("tap policy digest %q does not match embedded V2 tap policy %q", cfg.TapPolicyDigest, tapDigest))
	}
	runtimeDigest, err := policyv2.Digest()
	if err != nil {
		errs = append(errs, fmt.Errorf("load embedded executable runtime policy digest: %w", err))
	} else if cfg.ExecutableRuntimePolicyDigest != runtimeDigest {
		errs = append(errs, fmt.Errorf("executable runtime policy digest %q does not match embedded V2 policy %q", cfg.ExecutableRuntimePolicyDigest, runtimeDigest))
	}
	return errors.Join(errs...)
}

func hasNonCoreBindings(cfg Config) bool {
	return cfg.BottleFetcherRef != "" ||
		cfg.CatalogExtractorRef != "" ||
		cfg.CatalogServiceOrigin != "" ||
		cfg.IngestionJWSKeyPolicyDigest != "" ||
		cfg.TapPolicyDigest != "" ||
		cfg.ExecutableRuntimePolicyDigest != "" ||
		len(cfg.SupportedCatalogPolicyVersions) != 0 ||
		len(cfg.SupportedFetchPolicyVersions) != 0 ||
		len(cfg.SupportedProvenancePolicyVersions) != 0
}

func compiledReleaseBound() bool {
	return RuntimeBaseRef != "" ||
		MaterializerRef != "" ||
		HomebrewCommit != "" ||
		VerificationKeysDigest != "" ||
		BottleFetcherRef != "" ||
		CatalogExtractorRef != "" ||
		CatalogServiceOrigin != "" ||
		IngestionJWSKeyPolicyDigest != "" ||
		IngestionJWSKeyPolicyBase64 != "" ||
		TapPolicyDigest != "" ||
		ExecutableRuntimePolicyDigest != "" ||
		SupportedCatalogPolicyVersions != "" ||
		SupportedFetchPolicyVersions != "" ||
		SupportedProvenancePolicyVersions != ""
}

// CompiledCatalogKeyPolicy returns the concrete release-pinned public-key
// policy after verifying its canonical digest against the compiled tuple.
func CompiledCatalogKeyPolicy() (*catalogkeys.Policy, error) {
	if IngestionJWSKeyPolicyBase64 == "" {
		return nil, errors.New("embedded catalog key policy is absent")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(IngestionJWSKeyPolicyBase64)
	if err != nil {
		return nil, fmt.Errorf("decode embedded catalog key policy: %w", err)
	}
	policy, err := catalogkeys.Decode(data)
	if err != nil {
		return nil, err
	}
	digest, err := catalogkeys.Digest(policy)
	if err != nil {
		return nil, err
	}
	if IngestionJWSKeyPolicyDigest == "" || digest.String() != IngestionJWSKeyPolicyDigest {
		return nil, fmt.Errorf("embedded catalog key policy digest %s does not match compiled %s", digest, IngestionJWSKeyPolicyDigest)
	}
	return policy, nil
}

func hasCompiledNonCoreBindings() bool {
	return BottleFetcherRef != "" || CatalogExtractorRef != "" || CatalogServiceOrigin != "" || IngestionJWSKeyPolicyDigest != "" ||
		IngestionJWSKeyPolicyBase64 != "" || TapPolicyDigest != "" || ExecutableRuntimePolicyDigest != "" ||
		SupportedCatalogPolicyVersions != "" || SupportedFetchPolicyVersions != "" || SupportedProvenancePolicyVersions != ""
}

func parsePolicyVersions(name, raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	values := strings.Split(raw, ",")
	if err := validatePolicyVersions(name, values); err != nil {
		return nil, err
	}
	slices.Sort(values)
	return values, nil
}

func validateExactPolicyVersions(name string, values, want []string) error {
	got := append([]string(nil), values...)
	expected := append([]string(nil), want...)
	slices.Sort(got)
	slices.Sort(expected)
	if !slices.Equal(got, expected) {
		return fmt.Errorf("supported %s policy versions must be exactly %v", name, expected)
	}
	return nil
}

func validatePolicyVersions(name string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("supported %s policy versions are required", name)
	}
	if len(values) > 32 {
		return fmt.Errorf("supported %s policy versions exceed 32 entries", name)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || len(value) > 128 {
			return fmt.Errorf("supported %s policy version %q has invalid length", name, value)
		}
		for _, b := range []byte(value) {
			if b < 0x21 || b > 0x7e || b == ',' {
				return fmt.Errorf("supported %s policy version %q is not a printable comma-free ASCII identifier", name, value)
			}
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("supported %s policy version %q is duplicated", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateHTTPSOrigin(raw string) error {
	return catalog.ValidateServiceOrigin(raw)
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
