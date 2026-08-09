package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

const (
	SchemaVersionV1 = "dalec-homebrew-components/v1"
	SchemaVersionV2 = "dalec-homebrew-components/v2"

	// SchemaVersion is retained as the V1 name for source compatibility.
	SchemaVersion = SchemaVersionV1

	RuntimePolicyVersionV2            = policyv2.ResolverPolicyVersion
	CatalogPolicyVersionV1            = "tap-catalog-v1"
	BottleFetchPolicyVersionV1        = policyv2.FetchPolicyVersion
	SigstoreProvenancePolicyVersionV1 = "sigstore-in-toto-v1"
	ChecksumWaiverPolicyVersionV1     = policyv2.NonCoreProvenanceWaiver
	HTTPSSourceWaiverPolicyVersionV1  = catalog.HTTPSBottleSourceWaiver
	PrebuiltWaiverPolicyVersionV1     = resolution.PrebuiltProvenanceWaiverPolicyV1
	CoreWaiverPolicyVersionV1         = "homebrew-jws-and-verified-oci-chain-v1"
)

type Manifest struct {
	SchemaVersion          string     `json:"schema_version"`
	PolicyVersion          string     `json:"policy_version"`
	Frontend               Component  `json:"frontend"`
	RuntimeBase            Component  `json:"runtime_base"`
	Materializer           Component  `json:"materializer"`
	BottleFetcher          *Component `json:"bottle_fetcher,omitempty"`
	CatalogExtractor       *Component `json:"catalog_extractor,omitempty"`
	HomebrewCommit         string     `json:"homebrew_commit"`
	PortableRubyVersion    string     `json:"portable_ruby_version"`
	VerificationKeysDigest string     `json:"verification_keys_digest"`
	MetadataBundleDigest   string     `json:"metadata_bundle_digest,omitempty"`
	DalecModule            string     `json:"dalec_module"`
	BuildKitModule         string     `json:"buildkit_module"`

	CatalogServiceOrigin              string   `json:"catalog_service_origin,omitempty"`
	IngestionJWSKeyPolicyDigest       string   `json:"ingestion_jws_key_policy_digest,omitempty"`
	TapPolicyDigest                   string   `json:"tap_policy_digest,omitempty"`
	ExecutableRuntimePolicyDigest     string   `json:"executable_runtime_policy_digest,omitempty"`
	SupportedCatalogPolicyVersions    []string `json:"supported_catalog_policy_versions,omitempty"`
	SupportedFetchPolicyVersions      []string `json:"supported_fetch_policy_versions,omitempty"`
	SupportedProvenancePolicyVersions []string `json:"supported_provenance_policy_versions,omitempty"`
}

type Component struct {
	Index     string        `json:"index"`
	Platforms []PlatformRef `json:"platforms"`
}

type PlatformRef struct {
	Platform resolution.Platform `json:"platform"`
	Ref      string              `json:"ref"`
}

// Bindings is the platform-specific component and policy tuple selected from a
// manifest. Components contains the V1-compatible subset; V2 callers must also
// retain every additional field in this type when constructing replay records.
type Bindings struct {
	SchemaVersion string
	PolicyVersion string
	Components    resolution.Components
	ComponentsV2  resolution.ComponentsV2

	BottleFetcherRef                  string
	CatalogExtractorRef               string
	MetadataBundleDigest              string
	CatalogServiceOrigin              string
	IngestionJWSKeyPolicyDigest       string
	TapPolicyDigest                   string
	ExecutableRuntimePolicyDigest     string
	SupportedCatalogPolicyVersions    []string
	SupportedFetchPolicyVersions      []string
	SupportedProvenancePolicyVersions []string
}

func Decode(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, 4<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("component manifest exceeds 4 MiB")
	}
	if err := validateUniqueJSON(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	canonicalize(&m)
	if err := Validate(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func Canonical(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, errors.New("nil component manifest")
	}
	c := cloneManifest(*m)
	canonicalize(&c)
	if err := Validate(&c); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func Digest(m *Manifest) (digest.Digest, error) {
	b, err := Canonical(m)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(b), nil
}

func Validate(m *Manifest) error {
	if m == nil {
		return errors.New("nil component manifest")
	}
	var errs []error
	switch m.SchemaVersion {
	case SchemaVersionV1:
		if m.PolicyVersion != resolution.PolicyVersion {
			errs = append(errs, fmt.Errorf("unsupported V1 policy %q", m.PolicyVersion))
		}
		if hasV2Bindings(m) {
			errs = append(errs, errors.New("V1 component manifest contains V2-only bindings"))
		}
	case SchemaVersionV2:
		if m.PolicyVersion != RuntimePolicyVersionV2 {
			errs = append(errs, fmt.Errorf("unsupported V2 policy %q", m.PolicyVersion))
		}
		if err := validateV2Bindings(m); err != nil {
			errs = append(errs, err)
		}
	default:
		errs = append(errs, fmt.Errorf("unsupported schema %q", m.SchemaVersion))
	}
	for _, entry := range []struct {
		name      string
		component Component
	}{
		{name: "frontend", component: m.Frontend},
		{name: "runtime_base", component: m.RuntimeBase},
		{name: "materializer", component: m.Materializer},
	} {
		if err := validateComponent(entry.name, entry.component); err != nil {
			errs = append(errs, err)
		}
	}
	if len(m.HomebrewCommit) != 40 || !lowerHex(m.HomebrewCommit) {
		errs = append(errs, errors.New("invalid Homebrew commit"))
	}
	if m.PortableRubyVersion == "" {
		errs = append(errs, errors.New("portable Ruby version is required"))
	}
	if err := validateDigest(m.VerificationKeysDigest); err != nil {
		errs = append(errs, fmt.Errorf("verification keys: %w", err))
	}
	if m.DalecModule == "" || m.BuildKitModule == "" {
		errs = append(errs, errors.New("Dalec and BuildKit module versions are required"))
	}
	return errors.Join(errs...)
}

// BindingsFor selects the complete platform binding set. V2 integrations must
// use this method rather than projecting the manifest through ComponentsFor.
func (m *Manifest) BindingsFor(platform resolution.Platform) (Bindings, error) {
	if err := Validate(m); err != nil {
		return Bindings{}, err
	}
	f, err := componentRefFor(m.Frontend, platform, m.SchemaVersion == SchemaVersionV2)
	if err != nil {
		return Bindings{}, fmt.Errorf("frontend: %w", err)
	}
	b, err := componentRefFor(m.RuntimeBase, platform, m.SchemaVersion == SchemaVersionV2)
	if err != nil {
		return Bindings{}, fmt.Errorf("runtime base: %w", err)
	}
	mat, err := componentRefFor(m.Materializer, platform, m.SchemaVersion == SchemaVersionV2)
	if err != nil {
		return Bindings{}, fmt.Errorf("materializer: %w", err)
	}
	bindings := Bindings{
		SchemaVersion: m.SchemaVersion,
		PolicyVersion: m.PolicyVersion,
		Components: resolution.Components{
			FrontendRef:      f,
			RuntimeBaseRef:   b,
			MaterializerRef:  mat,
			HomebrewCommit:   m.HomebrewCommit,
			RubyRuntime:      m.PortableRubyVersion,
			VerificationKeys: m.VerificationKeysDigest,
			DalecModule:      m.DalecModule,
			BuildKitModule:   m.BuildKitModule,
		},
		CatalogServiceOrigin:              m.CatalogServiceOrigin,
		MetadataBundleDigest:              m.MetadataBundleDigest,
		IngestionJWSKeyPolicyDigest:       m.IngestionJWSKeyPolicyDigest,
		TapPolicyDigest:                   m.TapPolicyDigest,
		ExecutableRuntimePolicyDigest:     m.ExecutableRuntimePolicyDigest,
		SupportedCatalogPolicyVersions:    append([]string(nil), m.SupportedCatalogPolicyVersions...),
		SupportedFetchPolicyVersions:      append([]string(nil), m.SupportedFetchPolicyVersions...),
		SupportedProvenancePolicyVersions: append([]string(nil), m.SupportedProvenancePolicyVersions...),
	}
	if m.SchemaVersion == SchemaVersionV2 {
		if _, err := componentRefFor(*m.BottleFetcher, platform, true); err != nil {
			return Bindings{}, fmt.Errorf("bottle fetcher: %w", err)
		}
		// The frontend compiles the multi-platform helper indexes and resolves
		// their platform children at execution time. Preserve those exact index
		// identities in replay bindings while still requiring the manifest to
		// contain a child for the selected platform.
		fetcher := m.BottleFetcher.Index
		extractor := ""
		if m.CatalogExtractor != nil {
			if _, err := componentRefFor(*m.CatalogExtractor, platform, true); err != nil {
				return Bindings{}, fmt.Errorf("catalog extractor: %w", err)
			}
			extractor = m.CatalogExtractor.Index
		}
		bindings.BottleFetcherRef = fetcher
		bindings.CatalogExtractorRef = extractor
		bindings.ComponentsV2 = resolution.ComponentsV2{
			FrontendIndexRef: m.Frontend.Index, FrontendRef: f, RuntimeBaseRef: b, MaterializerRef: mat, BottleFetcherRef: fetcher, CatalogExtractorRef: extractor,
			CatalogServiceOrigin:          m.CatalogServiceOrigin,
			IngestionJWSKeyPolicyDigest:   m.IngestionJWSKeyPolicyDigest,
			TapPolicyDigest:               m.TapPolicyDigest,
			ExecutableRuntimePolicyDigest: m.ExecutableRuntimePolicyDigest,
			HomebrewCommit:                m.HomebrewCommit, RubyRuntime: m.PortableRubyVersion,
			VerificationKeys: m.VerificationKeysDigest, DalecModule: m.DalecModule, BuildKitModule: m.BuildKitModule,
			SupportedCatalogPolicyVersions:    append([]string(nil), m.SupportedCatalogPolicyVersions...),
			SupportedFetchPolicyVersions:      append([]string(nil), m.SupportedFetchPolicyVersions...),
			SupportedProvenancePolicyVersions: append([]string(nil), m.SupportedProvenancePolicyVersions...),
		}
	}
	return bindings, nil
}

// ComponentsFor retains the V1 projection used by existing resolution records.
// It rejects V2 manifests so callers cannot silently drop V2 security bindings.
func (m *Manifest) ComponentsFor(platform resolution.Platform) (resolution.Components, error) {
	if m == nil {
		return resolution.Components{}, errors.New("nil component manifest")
	}
	if m.SchemaVersion != SchemaVersionV1 {
		return resolution.Components{}, fmt.Errorf("schema %q requires BindingsFor; V2 bindings cannot be represented by resolution.Components", m.SchemaVersion)
	}
	bindings, err := m.BindingsFor(platform)
	if err != nil {
		return resolution.Components{}, err
	}
	return bindings.Components, nil
}

// SupportsNonCoreTaps reports whether the manifest is a complete, valid V2
// tuple. V1 manifests intentionally report false.
func (m *Manifest) SupportsNonCoreTaps() bool {
	return m != nil && m.SchemaVersion == SchemaVersionV2 && Validate(m) == nil
}

func validateV2Bindings(m *Manifest) error {
	var errs []error
	if err := validateDigest(m.MetadataBundleDigest); err != nil {
		errs = append(errs, fmt.Errorf("metadata bundle: %w", err))
	}
	if m.BottleFetcher == nil {
		errs = append(errs, errors.New("V2 bottle fetcher component is required"))
	} else if err := validateComponent("bottle_fetcher", *m.BottleFetcher); err != nil {
		errs = append(errs, err)
	}
	localMode := m.CatalogExtractor != nil
	serviceMode := m.CatalogServiceOrigin != "" || m.IngestionJWSKeyPolicyDigest != ""
	if localMode == serviceMode {
		errs = append(errs, errors.New("V2 manifest must select exactly one of build-local catalog extractor or hosted catalog service"))
	}
	if localMode {
		if err := validateComponent("catalog_extractor", *m.CatalogExtractor); err != nil {
			errs = append(errs, err)
		}
	} else {
		if err := validateHTTPSOrigin(m.CatalogServiceOrigin); err != nil {
			errs = append(errs, fmt.Errorf("catalog service origin: %w", err))
		}
		if err := validateDigest(m.IngestionJWSKeyPolicyDigest); err != nil {
			errs = append(errs, fmt.Errorf("ingestion JWS key policy: %w", err))
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "tap policy", value: m.TapPolicyDigest},
		{name: "executable runtime policy", value: m.ExecutableRuntimePolicyDigest},
	} {
		if err := validateDigest(field.value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field.name, err))
		}
	}
	for _, field := range []struct {
		name   string
		values []string
	}{
		{name: "catalog", values: m.SupportedCatalogPolicyVersions},
		{name: "fetch", values: m.SupportedFetchPolicyVersions},
		{name: "provenance", values: m.SupportedProvenancePolicyVersions},
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
		{name: "catalog", values: m.SupportedCatalogPolicyVersions, want: v2CatalogPolicyVersions()},
		{name: "fetch", values: m.SupportedFetchPolicyVersions, want: v2FetchPolicyVersions()},
		{name: "provenance", values: m.SupportedProvenancePolicyVersions, want: v2ProvenancePolicyVersions()},
	} {
		if err := validateExactPolicyVersions(field.name, field.values, field.want); err != nil {
			errs = append(errs, err)
		}
	}
	tapDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		errs = append(errs, fmt.Errorf("load embedded tap policy digest: %w", err))
	} else if m.TapPolicyDigest != tapDigest {
		errs = append(errs, fmt.Errorf("tap policy digest %q does not match embedded V2 tap policy %q", m.TapPolicyDigest, tapDigest))
	}
	runtimeDigest, err := policyv2.Digest()
	if err != nil {
		errs = append(errs, fmt.Errorf("load embedded executable runtime policy digest: %w", err))
	} else if m.ExecutableRuntimePolicyDigest != runtimeDigest {
		errs = append(errs, fmt.Errorf("executable runtime policy digest %q does not match embedded V2 policy %q", m.ExecutableRuntimePolicyDigest, runtimeDigest))
	}
	return errors.Join(errs...)
}

func hasV2Bindings(m *Manifest) bool {
	return m.BottleFetcher != nil ||
		m.CatalogExtractor != nil ||
		m.MetadataBundleDigest != "" ||
		m.CatalogServiceOrigin != "" ||
		m.IngestionJWSKeyPolicyDigest != "" ||
		m.TapPolicyDigest != "" ||
		m.ExecutableRuntimePolicyDigest != "" ||
		len(m.SupportedCatalogPolicyVersions) != 0 ||
		len(m.SupportedFetchPolicyVersions) != 0 ||
		len(m.SupportedProvenancePolicyVersions) != 0
}

func componentRefFor(c Component, platform resolution.Platform, matchVariant bool) (string, error) {
	for _, p := range c.Platforms {
		if p.Platform.OS == platform.OS && p.Platform.Architecture == platform.Architecture && (!matchVariant || p.Platform.Variant == platform.Variant) {
			return p.Ref, nil
		}
	}
	return "", fmt.Errorf("component has no %s/%s child", platform.OS, platform.Architecture)
}

func validateComponent(name string, c Component) error {
	if err := validateRef(c.Index); err != nil {
		return fmt.Errorf("%s index: %w", name, err)
	}
	repo := strings.Split(c.Index, "@")[0]
	seen := map[string]struct{}{}
	for _, p := range c.Platforms {
		key := p.Platform.OS + "/" + p.Platform.Architecture
		if p.Platform.OS != "linux" || (p.Platform.Architecture != "amd64" && p.Platform.Architecture != "arm64") || p.Platform.Variant != "" {
			return fmt.Errorf("%s unsupported platform %s", name, key)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%s duplicate platform %s", name, key)
		}
		seen[key] = struct{}{}
		if err := validateRef(p.Ref); err != nil {
			return fmt.Errorf("%s %s: %w", name, key, err)
		}
		if strings.Split(p.Ref, "@")[0] != repo {
			return fmt.Errorf("%s %s child uses a different repository", name, key)
		}
	}
	for _, key := range []string{"linux/amd64", "linux/arm64"} {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("%s misses %s", name, key)
		}
	}
	return nil
}

func validateRef(ref string) error { return resolution.ValidatePinnedReference(ref) }

func validateDigest(v string) error {
	d, err := digest.Parse(v)
	if err != nil {
		return err
	}
	if d.Algorithm() != digest.SHA256 {
		return errors.New("only sha256 is accepted")
	}
	return d.Validate()
}

func validateHTTPSOrigin(raw string) error {
	return catalog.ValidateServiceOrigin(raw)
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

func canonicalize(m *Manifest) {
	components := []*Component{&m.Frontend, &m.RuntimeBase, &m.Materializer}
	if m.BottleFetcher != nil {
		components = append(components, m.BottleFetcher)
	}
	if m.CatalogExtractor != nil {
		components = append(components, m.CatalogExtractor)
	}
	for _, c := range components {
		slices.SortFunc(c.Platforms, func(a, b PlatformRef) int {
			if x := strings.Compare(a.Platform.OS, b.Platform.OS); x != 0 {
				return x
			}
			if x := strings.Compare(a.Platform.Architecture, b.Platform.Architecture); x != 0 {
				return x
			}
			return strings.Compare(a.Platform.Variant, b.Platform.Variant)
		})
	}
	slices.Sort(m.SupportedCatalogPolicyVersions)
	slices.Sort(m.SupportedFetchPolicyVersions)
	slices.Sort(m.SupportedProvenancePolicyVersions)
}

func cloneManifest(m Manifest) Manifest {
	m.Frontend = cloneComponent(m.Frontend)
	m.RuntimeBase = cloneComponent(m.RuntimeBase)
	m.Materializer = cloneComponent(m.Materializer)
	if m.BottleFetcher != nil {
		fetcher := cloneComponent(*m.BottleFetcher)
		m.BottleFetcher = &fetcher
	}
	if m.CatalogExtractor != nil {
		extractor := cloneComponent(*m.CatalogExtractor)
		m.CatalogExtractor = &extractor
	}
	m.SupportedCatalogPolicyVersions = append([]string(nil), m.SupportedCatalogPolicyVersions...)
	m.SupportedFetchPolicyVersions = append([]string(nil), m.SupportedFetchPolicyVersions...)
	m.SupportedProvenancePolicyVersions = append([]string(nil), m.SupportedProvenancePolicyVersions...)
	return m
}

func cloneComponent(c Component) Component {
	c.Platforms = append([]PlatformRef(nil), c.Platforms...)
	return c
}

func lowerHex(v string) bool {
	for _, r := range v {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validateUniqueJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if err := walkUniqueJSON(dec, token); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func walkUniqueJSON(dec *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, value); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for dec.More() {
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, value); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}
