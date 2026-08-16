package resolution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

const (
	MetadataRollbackPolicyV1         = "monotonic-sequence-v1"
	BuildLocalExtractionPolicyV1     = "build-local-tap-extraction-v1"
	BuildLocalRollbackPolicyV1       = "build-local-exact-commit-no-cross-build-rollback-v1"
	CoreMetadataRollbackPolicyV1     = "homebrew-core-generated-at-v1"
	CoreGeneratedAtSignedPayload     = "signed-payload"
	CoreGeneratedAtLastModified      = "http-last-modified"
	HTTPSFetchPolicyVersionV1        = "homebrew-bottle-fetch-v1"
	BuildLocalArtifactPolicyV1       = "build-local-artifact-v1"
	VerifiedProvenancePolicyV1       = "sigstore-in-toto-v1"
	ProvenanceWaiverPolicyV1         = "tap-catalog-buildkit-and-verified-checksum-v1"
	PrebuiltProvenanceWaiverPolicyV1 = "prebuilt-archive-buildkit-and-verified-checksum-v1"
	PrebuiltDerivedBottlePolicyV1    = "prebuilt-derived-bottle-v1"
	HTTPSBottleSourceWaiverPolicyV1  = "https-bottle-embedded-formula-digest-only-v1"
	BottleVerificationPolicyV1       = "homebrew-bottle-static-v1"
	CoreBottleVerificationDeferredV1 = "homebrew-core-static-verify-before-exec-v1"
	CoreProvenanceWaiverPolicyV1     = "homebrew-jws-and-verified-oci-chain-v1"
	MaxResolutionV2Nodes             = 256
	MaxResolutionV2Sources           = 17
	MaxResolutionV2BottleBytes       = int64(1 << 30)
	MaxResolutionV2Redirects         = 5
	maxResolutionV2Bytes             = 64 << 20
)

// FormulaID is the canonical owner/tap/formula graph identity stored in V2
// records. Values are validated with the shared Homebrew Formula identity
// parser; bare names are never accepted in a canonical V2 record.
type FormulaID string

func (id FormulaID) String() string { return string(id) }

// TapID is the canonical owner/tap identity stored in V2 metadata and nodes.
type TapID string

func (id TapID) String() string { return string(id) }

// RecordV2 separates graph identity from the short filesystem rack identity.
// It is intentionally separate from Record so existing V1 resolver and
// materializer callers cannot accidentally opt in to V2.
type RecordV2 struct {
	SchemaVersion       string            `json:"schema_version"`
	PolicyVersion       string            `json:"policy_version"`
	Input               Input             `json:"input"`
	MetadataSources     []MetadataSource  `json:"metadata_sources"`
	ResolvedAt          time.Time         `json:"resolved_at"`
	SourceDateEpoch     int64             `json:"source_date_epoch"`
	Requested           []RequestedRootV2 `json:"requested"`
	Nodes               []NodeV2          `json:"nodes"`
	InstallOrder        []FormulaID       `json:"install_order"`
	Components          ComponentsV2      `json:"components"`
	Runtime             RuntimePolicy     `json:"runtime"`
	PruningPolicyDigest string            `json:"pruning_policy_digest,omitempty"`
}

// ComponentsV2 is the complete release-bound component, service, key-policy,
// and executable-policy tuple required by a V2 replay record. It deliberately
// does not embed the V1 Components type so V2-only bindings cannot be dropped
// by projecting through a V1 value.
type ComponentsV2 struct {
	FrontendIndexRef    string `json:"frontend_index_ref"`
	FrontendRef         string `json:"frontend_ref"`
	RuntimeBaseRef      string `json:"runtime_base_ref"`
	MaterializerRef     string `json:"materializer_ref"`
	BottleFetcherRef    string `json:"bottle_fetcher_ref"`
	CatalogExtractorRef string `json:"catalog_extractor_ref,omitempty"`

	CatalogServiceOrigin          string `json:"catalog_service_origin"`
	IngestionJWSKeyPolicyDigest   string `json:"ingestion_jws_key_policy_digest"`
	TapPolicyDigest               string `json:"tap_policy_digest"`
	ExecutableRuntimePolicyDigest string `json:"executable_runtime_policy_digest"`

	HomebrewCommit   string `json:"homebrew_commit"`
	RubyRuntime      string `json:"ruby_runtime"`
	VerificationKeys string `json:"verification_keys_digest"`
	DalecModule      string `json:"dalec_module"`
	BuildKitModule   string `json:"buildkit_module"`

	SupportedCatalogPolicyVersions    []string `json:"supported_catalog_policy_versions"`
	SupportedFetchPolicyVersions      []string `json:"supported_fetch_policy_versions"`
	SupportedProvenancePolicyVersions []string `json:"supported_provenance_policy_versions"`
}

// MetadataSource identifies one authenticated core or tap metadata source.
// Sequence and Rollback bind the accepted source to persisted monotonic state.
type MetadataSource struct {
	Tap                  TapID              `json:"tap"`
	Commit               string             `json:"commit"`
	CatalogPolicyVersion string             `json:"catalog_policy_version,omitempty"`
	Signer               Signature          `json:"signer"`
	Extraction           *TapExtractionV2   `json:"extraction,omitempty"`
	Documents            []MetadataDocument `json:"documents"`
	GeneratedAt          time.Time          `json:"generated_at"`
	GeneratedAtSource    string             `json:"generated_at_source,omitempty"`
	FetchedAt            time.Time          `json:"fetched_at"`
	Sequence             uint64             `json:"sequence"`
	Rollback             RollbackEvidence   `json:"rollback"`
}

// TapExtractionV2 binds a build-local catalog to the exact release-pinned
// extractor and immutable Git source observed by the current BuildKit solve.
type TapExtractionV2 struct {
	PolicyVersion string `json:"policy_version"`
	ExtractorRef  string `json:"extractor_ref"`
	Repository    string `json:"repository"`
	TreeDigest    string `json:"tree_digest"`
	ArchiveDigest string `json:"archive_digest"`
	CatalogDigest string `json:"catalog_digest"`
}

// MetadataDocument binds one canonical metadata payload and, when present,
// the authenticated envelope carrying it.
type MetadataDocument struct {
	Name           string `json:"name"`
	Digest         string `json:"digest"`
	EnvelopeDigest string `json:"envelope_digest,omitempty"`
}

// RollbackEvidence records the monotonic floor and persisted state used when a
// metadata source was accepted.
type RollbackEvidence struct {
	Policy        string `json:"policy"`
	SequenceFloor uint64 `json:"sequence_floor"`
	StateDigest   string `json:"state_digest"`
}

type RequestedRootV2 struct {
	Requested string    `json:"requested"`
	ID        FormulaID `json:"id"`
	KegOnly   bool      `json:"keg_only,omitempty"`
}

// NodeV2 carries a canonical graph ID and separately records the short rack
// name used under Cellar/opt and the exact Homebrew receipt identity.
type NodeV2 struct {
	ID                FormulaID       `json:"id"`
	Tap               TapID           `json:"tap"`
	Name              string          `json:"name"`
	HomebrewFullName  FormulaID       `json:"homebrew_full_name"`
	FormulaVersion    string          `json:"formula_version"`
	FormulaRevision   int             `json:"formula_revision"`
	PkgVersion        string          `json:"pkg_version"`
	VersionScheme     int             `json:"version_scheme"`
	BottleRebuild     int             `json:"bottle_rebuild"`
	License           string          `json:"license,omitempty"`
	KegOnly           bool            `json:"keg_only,omitempty"`
	Dependencies      []RequirementV2 `json:"dependencies,omitempty"`
	Bottle            BottleV2        `json:"bottle"`
	Provenance        Provenance      `json:"provenance"`
	ExecutablePaths   []string        `json:"executable_paths,omitempty"`
	UpstreamFormulaID FormulaID       `json:"upstream_formula_id,omitempty"`
}

type RequirementV2 struct {
	ID            FormulaID `json:"id"`
	Minimum       string    `json:"minimum_pkg_version"`
	Revision      int       `json:"minimum_revision"`
	BottleRebuild int       `json:"minimum_bottle_rebuild"`
	Direct        bool      `json:"declared_directly,omitempty"`
}

// BottleV2 contains metadata common to both supported immutable transports.
// Transport-specific identities are represented by the strict Transport union.
type BottleV2 struct {
	Tag                        string                `json:"tag"`
	Filename                   string                `json:"filename"`
	Size                       int64                 `json:"size"`
	SHA256                     string                `json:"sha256"`
	Cellar                     string                `json:"cellar"`
	Tab                        BottleTabV2           `json:"tab"`
	SelectedAnnotations        []KV                  `json:"selected_annotations,omitempty"`
	CurrentFormulaSourceDigest string                `json:"current_formula_source_digest"`
	BottleFormulaSourceDigest  string                `json:"bottle_formula_source_digest"`
	BottleSourceRepository     string                `json:"bottle_source_repository"`
	BottleSourceCommit         string                `json:"bottle_source_commit"`
	BottleFormulaPath          string                `json:"bottle_formula_path"`
	BottleSourceWaiver         string                `json:"bottle_source_waiver,omitempty"`
	Verification               BottleVerificationV2  `json:"verification"`
	Transport                  BottleTransport       `json:"transport"`
	PrebuiltDerivation         *PrebuiltDerivationV2 `json:"prebuilt_derivation,omitempty"`
}

// PrebuiltDerivationV2 binds the authenticated upstream archive and every
// signed input to the deterministic derived bottle selected by the outer
// BottleV2. The outer bottle digest remains the fetched service artifact.
type PrebuiltDerivationV2 struct {
	PolicyVersion   string                          `json:"policy_version"`
	PolicyDigest    string                          `json:"policy_digest"`
	Source          PrebuiltSourceArtifactV2        `json:"source"`
	SourceInventory PrebuiltSourceInventoryV2       `json:"source_inventory"`
	Payload         PrebuiltPayloadEvidenceV2       `json:"payload"`
	ELF             PrebuiltELFEvidenceV2           `json:"elf"`
	FormulaSource   PrebuiltFormulaSourceEvidenceV2 `json:"formula_source"`
	RecipeDigest    string                          `json:"recipe_digest"`
	DerivedBottle   PrebuiltDerivedBottleRelationV2 `json:"derived_bottle"`
}

type PrebuiltSourceArtifactV2 struct {
	Filename  string          `json:"filename"`
	Size      int64           `json:"size"`
	SHA256    string          `json:"sha256"`
	Format    string          `json:"format"`
	Transport BottleTransport `json:"transport"`
}

type PrebuiltSourceInventoryV2 struct {
	InventoryDigest string `json:"inventory_digest"`
	EntryCount      int    `json:"entry_count"`
	ExpandedSize    int64  `json:"expanded_size"`
}

type PrebuiltPayloadEvidenceV2 struct {
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size"`
	ArchiveMode     uint32 `json:"archive_mode"`
	DerivedMode     uint32 `json:"derived_mode"`
}

type PrebuiltELFEvidenceV2 struct {
	Format                     string   `json:"format"`
	Machine                    string   `json:"machine"`
	StaticallyLinked           bool     `json:"statically_linked"`
	Interpreter                string   `json:"interpreter"`
	NeededLibraries            []string `json:"needed_libraries"`
	RPaths                     []string `json:"rpaths"`
	WritableExecutableSegments bool     `json:"writable_executable_segments"`
}

type PrebuiltFormulaSourceEvidenceV2 struct {
	Transport TapFormulaSourceTransportV2 `json:"transport"`
	SHA256    string                      `json:"sha256"`
	Size      int64                       `json:"size"`
}

type TapFormulaSourceTransportV2 struct {
	Tap  TapSourceV2 `json:"tap"`
	Path string      `json:"path"`
}

type TapSourceV2 struct {
	ID            TapID  `json:"id"`
	Repository    string `json:"repository"`
	Commit        string `json:"commit"`
	TreeDigest    string `json:"tree_digest"`
	ArchiveDigest string `json:"archive_digest"`
}

type PrebuiltDerivedBottleRelationV2 struct {
	Tag                 string               `json:"tag"`
	Filename            string               `json:"filename"`
	SHA256              string               `json:"sha256"`
	Size                int64                `json:"size"`
	Verification        BottleVerificationV2 `json:"verification"`
	FormulaSourceDigest string               `json:"formula_source_digest"`
}

// BottleVerificationV2 binds the catalog service's bounded static archive
// verification summary to the exact selected bottle bytes.
type BottleVerificationV2 struct {
	PolicyVersion   string `json:"policy_version"`
	InventoryDigest string `json:"inventory_digest"`
	EntryCount      int    `json:"entry_count"`
	ExpandedSize    int64  `json:"expanded_size"`
}

// BottleTabV2 retains bottle build evidence while representing every runtime
// dependency with a canonical Formula ID rather than a short rack name.
type BottleTabV2 struct {
	Receiptless     bool                  `json:"receiptless,omitempty"`
	HomebrewVersion string                `json:"homebrew_version"`
	Arch            string                `json:"arch,omitempty"`
	Compiler        string                `json:"compiler,omitempty"`
	ChangedFiles    []string              `json:"changed_files,omitempty"`
	BuiltOn         BuiltOn               `json:"built_on,omitempty"`
	Dependencies    []RuntimeDependencyV2 `json:"runtime_dependencies,omitempty"`
}

type RuntimeDependencyV2 struct {
	ID               FormulaID `json:"id"`
	HomebrewFullName FormulaID `json:"homebrew_full_name"`
	Version          string    `json:"version"`
	Revision         int       `json:"revision"`
	BottleRebuild    int       `json:"bottle_rebuild"`
	PkgVersion       string    `json:"pkg_version"`
	DeclaredDirectly bool      `json:"declared_directly,omitempty"`
}

// BottleTransport is a strict union: exactly one of OCI or HTTPS must be set.
type BottleTransport struct {
	OCI   *OCITransport   `json:"oci,omitempty"`
	HTTPS *HTTPSTransport `json:"https,omitempty"`
	Local *LocalTransport `json:"local,omitempty"`
}

// OCITransport binds the complete descriptor chain used to replay one bottle.
type OCITransport struct {
	Registry   string     `json:"registry"`
	Repository string     `json:"repository"`
	Index      Descriptor `json:"index"`
	Manifest   Descriptor `json:"manifest"`
	Config     Descriptor `json:"config"`
	Layer      Descriptor `json:"layer"`
}

// HTTPSTransport binds a public HTTPS artifact to exact size, checksum,
// redirect-host allowlist, and bounded fetch policy.
type HTTPSTransport struct {
	URL                  string   `json:"url"`
	ExpectedSize         int64    `json:"expected_size"`
	SHA256               string   `json:"sha256"`
	Filename             string   `json:"filename"`
	AllowedRedirectHosts []string `json:"allowed_redirect_hosts"`
	FetchPolicyVersion   string   `json:"fetch_policy_version"`
}

// LocalTransport binds generated bottle bytes supplied directly by the
// current content-addressed BuildKit solve.
type LocalTransport struct {
	PolicyVersion string `json:"policy_version"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	Filename      string `json:"filename"`
}

// Provenance is a strict union: every node has either verified evidence or the
// single explicit V2 waiver.
type Provenance struct {
	Verified *VerifiedProvenance `json:"verified,omitempty"`
	Waiver   *ProvenanceWaiver   `json:"waiver,omitempty"`
}

// VerifiedProvenance binds a Sigstore or in-toto evidence object to the exact
// bottle digest selected by the transport.
type VerifiedProvenance struct {
	PolicyVersion   string `json:"policy_version"`
	SubjectDigest   string `json:"subject_digest"`
	StatementDigest string `json:"statement_digest"`
	BundleDigest    string `json:"bundle_digest"`
	SignerIdentity  string `json:"signer_identity"`
	Issuer          string `json:"issuer"`
}

type ProvenanceWaiver struct {
	Policy string `json:"policy"`
}

// CanonicalV2 returns stable JSON for V2 hashing and transport. Requested roots
// and install order remain ordered; metadata sources, nodes, and set-like
// evidence fields are sorted on a defensive copy.
func CanonicalV2(r *RecordV2) ([]byte, error) {
	if r == nil {
		return nil, errors.New("nil V2 resolution record")
	}
	c := cloneRecordV2(*r)
	canonicalizeV2(&c)
	if err := ValidateV2(&c); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("encode canonical V2 resolution: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func DigestV2(r *RecordV2) (digest.Digest, error) {
	data, err := CanonicalV2(r)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

// DecodeV2 strictly decodes and validates one V2 resolution object. V1 remains
// available through Decode and is deliberately not accepted here.
func DecodeV2(data []byte) (*RecordV2, error) {
	if len(data) > maxResolutionV2Bytes {
		return nil, fmt.Errorf("V2 resolution exceeds %d bytes", maxResolutionV2Bytes)
	}
	if err := validateUniqueJSONV2(data); err != nil {
		return nil, fmt.Errorf("decode V2 resolution: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r RecordV2
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("decode V2 resolution: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, fmt.Errorf("decode V2 resolution: %w", err)
	}
	canonicalizeV2(&r)
	if err := ValidateV2(&r); err != nil {
		return nil, err
	}
	canonical, err := CanonicalV2(&r)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("V2 resolution JSON is not canonical")
	}
	return &r, nil
}

// SchemaVersionOf reads only the schema discriminator. Callers must still use
// Decode or DecodeV2 for strict decoding and validation.
func SchemaVersionOf(data []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := dec.Decode(&header); err != nil {
		return "", fmt.Errorf("decode resolution schema version: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return "", fmt.Errorf("decode resolution schema version: %w", err)
	}
	if header.SchemaVersion == "" {
		return "", errors.New("resolution schema_version is required")
	}
	return header.SchemaVersion, nil
}

// ValidateV2 independently verifies all graph, identity, metadata, transport,
// provenance, and deterministic-time invariants required by the V2 record.
func ValidateV2(r *RecordV2) error {
	if r == nil {
		return errors.New("nil V2 resolution record")
	}
	var errs []error
	if r.SchemaVersion != SchemaVersionV2 {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q", r.SchemaVersion))
	}
	if r.PolicyVersion != PolicyVersionV2 {
		errs = append(errs, fmt.Errorf("unsupported policy_version %q", r.PolicyVersion))
	}
	if err := validateV2Platform(r.Input.Platform); err != nil {
		errs = append(errs, err)
	}
	if err := validateDigest(r.Input.DalecSpecDigest); err != nil {
		errs = append(errs, fmt.Errorf("input.dalec_spec_digest: %w", err))
	}
	if r.ResolvedAt.IsZero() {
		errs = append(errs, errors.New("resolved_at must be set"))
	}
	if r.SourceDateEpoch <= 0 {
		errs = append(errs, errors.New("source_date_epoch must be positive"))
	}
	if len(r.MetadataSources) == 0 {
		errs = append(errs, errors.New("resolution has no metadata sources"))
	}
	if len(r.MetadataSources) > MaxResolutionV2Sources {
		errs = append(errs, fmt.Errorf("resolution has %d metadata sources; maximum is %d", len(r.MetadataSources), MaxResolutionV2Sources))
	}
	if len(r.Requested) == 0 {
		errs = append(errs, errors.New("resolution has no requested roots"))
	}
	if len(r.Nodes) == 0 {
		errs = append(errs, errors.New("resolution has no closure nodes"))
	}
	if len(r.Nodes) > MaxResolutionV2Nodes {
		errs = append(errs, fmt.Errorf("resolution has %d closure nodes; maximum is %d", len(r.Nodes), MaxResolutionV2Nodes))
	}
	if err := validateComponentsV2(r.Components); err != nil {
		errs = append(errs, err)
	}
	supportedCatalogPolicies := stringSet(r.Components.SupportedCatalogPolicyVersions)
	supportedFetchPolicies := stringSet(r.Components.SupportedFetchPolicyVersions)
	supportedProvenancePolicies := stringSet(r.Components.SupportedProvenancePolicyVersions)

	sources := make(map[TapID]MetadataSource, len(r.MetadataSources))
	var earliestEpoch int64
	for i, source := range r.MetadataSources {
		label := fmt.Sprintf("metadata_sources[%d]", i)
		tap, tapErr := parseCanonicalTapID(source.Tap)
		if tapErr != nil {
			errs = append(errs, fmt.Errorf("%s.tap: %w", label, tapErr))
		} else {
			canonicalTap := TapID(tap.String())
			if _, exists := sources[canonicalTap]; exists {
				errs = append(errs, fmt.Errorf("duplicate metadata source for tap %q", canonicalTap))
			}
			sources[canonicalTap] = source
			if tap != formulaid.CoreTap() {
				if source.CatalogPolicyVersion == "" {
					errs = append(errs, fmt.Errorf("%s.catalog_policy_version is required for non-core tap %q", label, canonicalTap))
				} else if _, ok := supportedCatalogPolicies[source.CatalogPolicyVersion]; !ok {
					errs = append(errs, fmt.Errorf("%s catalog policy %q is absent from the release binding", label, source.CatalogPolicyVersion))
				}
			}
		}
		if len(source.Commit) != 40 || !isLowerHex(source.Commit) {
			errs = append(errs, fmt.Errorf("%s.commit is not a lowercase 40-character Git commit", label))
		}
		localExtraction := source.Extraction != nil
		if localExtraction {
			if tapErr != nil || tap == formulaid.CoreTap() {
				errs = append(errs, fmt.Errorf("%s build-local extraction is valid only for non-core taps", label))
			}
			if source.Signer != (Signature{}) {
				errs = append(errs, fmt.Errorf("%s build-local extraction cannot claim a JWS signer", label))
			}
			if err := validateTapExtractionV2(*source.Extraction, source, r.Components); err != nil {
				errs = append(errs, fmt.Errorf("%s.extraction: %w", label, err))
			}
		} else {
			if strings.TrimSpace(source.Signer.KeyID) == "" {
				errs = append(errs, fmt.Errorf("%s.signer.key_id is required", label))
			}
			if source.Signer.Algorithm != "PS512" {
				errs = append(errs, fmt.Errorf("%s.signer.algorithm %q is unsupported", label, source.Signer.Algorithm))
			}
			if !source.Signer.Verified {
				errs = append(errs, fmt.Errorf("%s.signer is not verified", label))
			}
		}
		if len(source.Documents) == 0 {
			errs = append(errs, fmt.Errorf("%s has no authenticated documents", label))
		}
		seenDocuments := make(map[string]struct{}, len(source.Documents))
		for j, document := range source.Documents {
			docLabel := fmt.Sprintf("%s.documents[%d]", label, j)
			if !validV2Token(document.Name) {
				errs = append(errs, fmt.Errorf("%s.name %q is invalid", docLabel, document.Name))
			}
			if _, exists := seenDocuments[document.Name]; exists {
				errs = append(errs, fmt.Errorf("%s has duplicate document name %q", label, document.Name))
			}
			seenDocuments[document.Name] = struct{}{}
			if err := validateDigest(document.Digest); err != nil {
				errs = append(errs, fmt.Errorf("%s.digest: %w", docLabel, err))
			}
			if document.EnvelopeDigest != "" {
				if err := validateDigest(document.EnvelopeDigest); err != nil {
					errs = append(errs, fmt.Errorf("%s.envelope_digest: %w", docLabel, err))
				}
			}
		}
		if tapErr == nil {
			if err := validateMetadataDocumentsV2(label, tap == formulaid.CoreTap(), localExtraction, source.Documents); err != nil {
				errs = append(errs, err)
			}
		}
		isCore := tapErr == nil && tap == formulaid.CoreTap()
		if isCore {
			// Empty is retained only for replay compatibility with V2 records
			// created before the timestamp trust marker was added. New resolvers
			// always emit an explicit value, and release signing requires it.
			if source.GeneratedAtSource != "" && source.GeneratedAtSource != CoreGeneratedAtSignedPayload && source.GeneratedAtSource != CoreGeneratedAtLastModified {
				errs = append(errs, fmt.Errorf("%s.generated_at_source %q is unsupported", label, source.GeneratedAtSource))
			}
		} else if source.GeneratedAtSource != "" {
			errs = append(errs, fmt.Errorf("%s.generated_at_source is valid only for homebrew/core", label))
		}
		if source.GeneratedAt.IsZero() || source.FetchedAt.IsZero() {
			errs = append(errs, fmt.Errorf("%s timestamps must be set", label))
		} else {
			generatedEpoch := source.GeneratedAt.Unix()
			if generatedEpoch <= 0 {
				errs = append(errs, fmt.Errorf("%s.generated_at must be after the Unix epoch", label))
			}
			if earliestEpoch == 0 || generatedEpoch < earliestEpoch {
				earliestEpoch = generatedEpoch
			}
			if source.FetchedAt.Before(source.GeneratedAt) {
				errs = append(errs, fmt.Errorf("%s.fetched_at precedes generated_at", label))
			}
			if !r.ResolvedAt.IsZero() && r.ResolvedAt.Before(source.FetchedAt) {
				errs = append(errs, fmt.Errorf("%s.fetched_at is after resolved_at", label))
			}
		}
		if source.Sequence == 0 {
			errs = append(errs, fmt.Errorf("%s.sequence must be positive", label))
		}
		expectedRollbackPolicy := MetadataRollbackPolicyV1
		if isCore {
			expectedRollbackPolicy = CoreMetadataRollbackPolicyV1
			if !source.GeneratedAt.IsZero() && source.Sequence != uint64(source.GeneratedAt.Unix()) {
				errs = append(errs, fmt.Errorf("%s.sequence must equal authenticated core generated_at", label))
			}
		} else if localExtraction {
			expectedRollbackPolicy = BuildLocalRollbackPolicyV1
			if source.Sequence != 1 || source.Rollback.SequenceFloor != 0 {
				errs = append(errs, fmt.Errorf("%s build-local rollback evidence must use sequence 1 and floor 0", label))
			}
		}
		if source.Rollback.Policy != expectedRollbackPolicy {
			errs = append(errs, fmt.Errorf("%s.rollback.policy %q is unsupported; expected %q", label, source.Rollback.Policy, expectedRollbackPolicy))
		}
		if source.Rollback.SequenceFloor > source.Sequence {
			errs = append(errs, fmt.Errorf("%s.rollback.sequence_floor %d exceeds accepted sequence %d", label, source.Rollback.SequenceFloor, source.Sequence))
		}
		if err := validateDigest(source.Rollback.StateDigest); err != nil {
			errs = append(errs, fmt.Errorf("%s.rollback.state_digest: %w", label, err))
		}
	}
	coreTap := TapID(formulaid.CoreTap().String())
	if coreSource, ok := sources[coreTap]; !ok {
		errs = append(errs, errors.New("metadata sources omit homebrew/core"))
	} else if coreSource.Commit != r.Components.HomebrewCommit {
		errs = append(errs, fmt.Errorf("homebrew/core metadata commit %q does not match component Homebrew commit %q", coreSource.Commit, r.Components.HomebrewCommit))
	}
	if earliestEpoch > 0 && r.SourceDateEpoch != earliestEpoch {
		errs = append(errs, fmt.Errorf("source_date_epoch %d does not equal earliest authenticated generation time %d", r.SourceDateEpoch, earliestEpoch))
	}

	nodes := make(map[FormulaID]NodeV2, len(r.Nodes))
	racks := make(map[string]FormulaID, len(r.Nodes))
	for i, node := range r.Nodes {
		label := fmt.Sprintf("nodes[%d]", i)
		id, err := parseCanonicalFormulaID(node.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.id: %w", label, err))
			continue
		}
		canonicalID := FormulaID(id.String())
		if _, exists := nodes[canonicalID]; exists {
			errs = append(errs, fmt.Errorf("duplicate closure node %q", canonicalID))
		}
		nodes[canonicalID] = node
		if node.Tap != TapID(id.Tap().String()) {
			errs = append(errs, fmt.Errorf("node %q tap %q does not match Formula ID tap %q", canonicalID, node.Tap, id.Tap().String()))
		}
		if node.Name != id.Name() {
			errs = append(errs, fmt.Errorf("node %q rack name %q does not match Formula name %q", canonicalID, node.Name, id.Name()))
		}
		if previous, exists := racks[node.Name]; exists && previous != canonicalID {
			errs = append(errs, fmt.Errorf("rack name collision: Formula IDs %q and %q both use rack %q", previous, canonicalID, node.Name))
		} else if node.Name != "" {
			racks[node.Name] = canonicalID
		}
		fullName, fullNameErr := parseCanonicalFormulaID(node.HomebrewFullName)
		if fullNameErr != nil {
			errs = append(errs, fmt.Errorf("node %q homebrew_full_name: %w", canonicalID, fullNameErr))
		} else if fullName.String() != id.String() {
			errs = append(errs, fmt.Errorf("node %q homebrew_full_name %q identifies a different Formula", canonicalID, node.HomebrewFullName))
		}
		if _, ok := sources[TapID(id.Tap().String())]; !ok {
			errs = append(errs, fmt.Errorf("node %q has no metadata source for tap %q", canonicalID, id.Tap().String()))
		}
		if node.FormulaVersion == "" || node.PkgVersion == "" {
			errs = append(errs, fmt.Errorf("node %q has an empty version", canonicalID))
		}
		if node.FormulaRevision < 0 || node.VersionScheme < 0 || node.BottleRebuild < 0 {
			errs = append(errs, fmt.Errorf("node %q has a negative version or bottle revision", canonicalID))
		}
		metadataSource := sources[TapID(id.Tap().String())]
		artifactDigest, transportErr := validateBottleV2(r.Input.Platform, canonicalID, node.Bottle, node.ExecutablePaths, r.Components, metadataSource.Commit)
		if transportErr != nil {
			errs = append(errs, transportErr)
		}
		if transport := node.Bottle.Transport.HTTPS; transport != nil {
			if _, ok := supportedFetchPolicies[transport.FetchPolicyVersion]; !ok {
				errs = append(errs, fmt.Errorf("node %q HTTPS fetch policy %q is absent from the release binding", canonicalID, transport.FetchPolicyVersion))
			}
			if node.Bottle.PrebuiltDerivation == nil {
				if _, ok := supportedProvenancePolicies[node.Bottle.BottleSourceWaiver]; !ok {
					errs = append(errs, fmt.Errorf("node %q HTTPS source waiver policy %q is absent from the release binding", canonicalID, node.Bottle.BottleSourceWaiver))
				}
			}
		}
		if derivation := node.Bottle.PrebuiltDerivation; derivation != nil && derivation.Source.Transport.HTTPS != nil {
			if _, ok := supportedFetchPolicies[derivation.Source.Transport.HTTPS.FetchPolicyVersion]; !ok {
				errs = append(errs, fmt.Errorf("node %q prebuilt source fetch policy %q is absent from the release binding", canonicalID, derivation.Source.Transport.HTTPS.FetchPolicyVersion))
			}
		}
		provenanceSubjectDigest := artifactDigest
		prebuilt := node.Bottle.PrebuiltDerivation != nil
		if prebuilt {
			provenanceSubjectDigest = node.Bottle.PrebuiltDerivation.Source.SHA256
		}
		if provenanceErr := validateProvenanceV2(canonicalID, provenanceSubjectDigest, prebuilt, node.Provenance); provenanceErr != nil {
			errs = append(errs, provenanceErr)
		}
		if node.Provenance.Verified != nil {
			if _, ok := supportedProvenancePolicies[node.Provenance.Verified.PolicyVersion]; !ok {
				errs = append(errs, fmt.Errorf("node %q verified provenance policy %q is absent from the release binding", canonicalID, node.Provenance.Verified.PolicyVersion))
			}
		}
		if node.Provenance.Waiver != nil {
			if _, ok := supportedProvenancePolicies[node.Provenance.Waiver.Policy]; !ok {
				errs = append(errs, fmt.Errorf("node %q provenance waiver policy %q is absent from the release binding", canonicalID, node.Provenance.Waiver.Policy))
			}
		}
		if node.UpstreamFormulaID != "" {
			if _, err := parseCanonicalFormulaID(node.UpstreamFormulaID); err != nil {
				errs = append(errs, fmt.Errorf("node %q upstream_formula_id: %w", canonicalID, err))
			}
		}
		seenDependencies := make(map[FormulaID]struct{}, len(node.Dependencies))
		for j, requirement := range node.Dependencies {
			dependencyID, err := parseCanonicalFormulaID(requirement.ID)
			if err != nil {
				errs = append(errs, fmt.Errorf("node %q dependencies[%d].id: %w", canonicalID, j, err))
				continue
			}
			canonicalDependency := FormulaID(dependencyID.String())
			if _, exists := seenDependencies[canonicalDependency]; exists {
				errs = append(errs, fmt.Errorf("node %q has duplicate dependency %q", canonicalID, canonicalDependency))
			}
			seenDependencies[canonicalDependency] = struct{}{}
			if requirement.Revision < 0 || requirement.BottleRebuild < 0 {
				errs = append(errs, fmt.Errorf("node %q dependency %q has a negative revision", canonicalID, canonicalDependency))
			}
		}
	}

	seenRoots := make(map[FormulaID]struct{}, len(r.Requested))
	for i, root := range r.Requested {
		if strings.TrimSpace(root.Requested) == "" {
			errs = append(errs, fmt.Errorf("requested[%d].requested is empty", i))
		}
		id, err := parseCanonicalFormulaID(root.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("requested[%d].id: %w", i, err))
			continue
		}
		canonicalID := FormulaID(id.String())
		if _, exists := seenRoots[canonicalID]; exists {
			errs = append(errs, fmt.Errorf("duplicate requested Formula ID %q", canonicalID))
		}
		seenRoots[canonicalID] = struct{}{}
		node, ok := nodes[canonicalID]
		if !ok {
			errs = append(errs, fmt.Errorf("requested root %q resolves to missing node %q", root.Requested, canonicalID))
		} else if root.KegOnly != node.KegOnly {
			errs = append(errs, fmt.Errorf("requested root %q keg_only does not match node %q", root.Requested, canonicalID))
		}
	}
	for id, node := range nodes {
		for _, requirement := range node.Dependencies {
			if _, ok := nodes[requirement.ID]; !ok {
				errs = append(errs, fmt.Errorf("node %q references missing dependency %q", id, requirement.ID))
			}
		}
	}
	if len(r.InstallOrder) != len(nodes) {
		errs = append(errs, fmt.Errorf("install_order has %d entries for %d nodes", len(r.InstallOrder), len(nodes)))
	} else {
		positions := make(map[FormulaID]int, len(r.InstallOrder))
		for i, rawID := range r.InstallOrder {
			id, err := parseCanonicalFormulaID(rawID)
			if err != nil {
				errs = append(errs, fmt.Errorf("install_order[%d]: %w", i, err))
				continue
			}
			canonicalID := FormulaID(id.String())
			if _, ok := nodes[canonicalID]; !ok {
				errs = append(errs, fmt.Errorf("install_order references unknown node %q", canonicalID))
			}
			if _, exists := positions[canonicalID]; exists {
				errs = append(errs, fmt.Errorf("duplicate install_order entry %q", canonicalID))
			}
			positions[canonicalID] = i
		}
		for id, node := range nodes {
			position, ok := positions[id]
			if !ok {
				errs = append(errs, fmt.Errorf("install_order omits node %q", id))
				continue
			}
			for _, requirement := range node.Dependencies {
				dependencyPosition, dependencyOK := positions[requirement.ID]
				if !dependencyOK || dependencyPosition >= position {
					errs = append(errs, fmt.Errorf("install_order places %q before dependency %q", id, requirement.ID))
				}
			}
		}
	}

	reachable := make(map[FormulaID]struct{}, len(nodes))
	var walk func(FormulaID)
	walk = func(id FormulaID) {
		if _, ok := reachable[id]; ok {
			return
		}
		reachable[id] = struct{}{}
		node, ok := nodes[id]
		if !ok {
			return
		}
		for _, requirement := range node.Dependencies {
			walk(requirement.ID)
		}
	}
	for _, root := range r.Requested {
		walk(root.ID)
	}
	for id := range nodes {
		if _, ok := reachable[id]; !ok {
			errs = append(errs, fmt.Errorf("closure node %q is unreachable from requested roots", id))
		}
	}
	if err := validateRuntimeIdentity(r.Runtime); err != nil {
		errs = append(errs, err)
	}
	switch r.Runtime.Profile {
	case "", RuntimeProfileMinimalV1:
	default:
		errs = append(errs, fmt.Errorf("unsupported V2 runtime profile %q", r.Runtime.Profile))
	}
	if r.PruningPolicyDigest != "" {
		if err := validateDigest(r.PruningPolicyDigest); err != nil {
			errs = append(errs, fmt.Errorf("pruning policy: %w", err))
		}
	}
	return errors.Join(errs...)
}

func validateV2Platform(platform Platform) error {
	if platform.OS != "linux" || (platform.Architecture != "amd64" && platform.Architecture != "arm64") || platform.Variant != "" {
		return fmt.Errorf("unsupported platform %s/%s", platform.OS, platform.Architecture)
	}
	return nil
}

func validateComponentsV2(components ComponentsV2) error {
	var errs []error
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "frontend index", value: components.FrontendIndexRef},
		{name: "frontend", value: components.FrontendRef},
		{name: "runtime base", value: components.RuntimeBaseRef},
		{name: "materializer", value: components.MaterializerRef},
		{name: "bottle fetcher", value: components.BottleFetcherRef},
	} {
		if err := validatePinnedReference(field.value); err != nil {
			errs = append(errs, fmt.Errorf("V2 %s component: %w", field.name, err))
		}
	}
	if !SameReferenceRepository(components.FrontendIndexRef, components.FrontendRef) {
		errs = append(errs, errors.New("V2 frontend index and child use different repositories"))
	}
	localMode := components.CatalogExtractorRef != ""
	serviceMode := components.CatalogServiceOrigin != "" || components.IngestionJWSKeyPolicyDigest != ""
	if localMode == serviceMode {
		errs = append(errs, errors.New("V2 component tuple must select exactly one of build-local extractor or hosted catalog service"))
	}
	if localMode {
		if err := validatePinnedReference(components.CatalogExtractorRef); err != nil {
			errs = append(errs, fmt.Errorf("V2 catalog extractor component: %w", err))
		}
		if components.CatalogServiceOrigin != "" || components.IngestionJWSKeyPolicyDigest != "" {
			errs = append(errs, errors.New("V2 build-local tuple contains hosted catalog-service fields"))
		}
	} else {
		if err := validateHTTPSOriginV2(components.CatalogServiceOrigin); err != nil {
			errs = append(errs, fmt.Errorf("V2 catalog service origin: %w", err))
		}
		if err := validateDigest(components.IngestionJWSKeyPolicyDigest); err != nil {
			errs = append(errs, fmt.Errorf("V2 ingestion JWS key policy: %w", err))
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "tap policy", value: components.TapPolicyDigest},
		{name: "executable runtime policy", value: components.ExecutableRuntimePolicyDigest},
		{name: "verification key set", value: components.VerificationKeys},
	} {
		if err := validateDigest(field.value); err != nil {
			errs = append(errs, fmt.Errorf("V2 %s: %w", field.name, err))
		}
	}
	if len(components.HomebrewCommit) != 40 || !isLowerHex(components.HomebrewCommit) {
		errs = append(errs, errors.New("V2 component tuple has an invalid pinned Homebrew commit"))
	}
	if strings.TrimSpace(components.RubyRuntime) == "" {
		errs = append(errs, errors.New("V2 component tuple is missing the portable Ruby identity"))
	}
	if components.DalecModule == "" || components.DalecModule == "unknown" || components.BuildKitModule == "" || components.BuildKitModule == "unknown" {
		errs = append(errs, errors.New("V2 component tuple is missing Dalec or BuildKit module versions"))
	}
	for _, field := range []struct {
		name   string
		values []string
	}{
		{name: "catalog", values: components.SupportedCatalogPolicyVersions},
		{name: "fetch", values: components.SupportedFetchPolicyVersions},
		{name: "provenance", values: components.SupportedProvenancePolicyVersions},
	} {
		if err := validatePolicyVersionsV2(field.name, field.values); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateHTTPSOriginV2(raw string) error {
	if raw == "" {
		return errors.New("HTTPS origin is required")
	}
	if raw != strings.TrimSpace(raw) {
		return errors.New("origin must not contain surrounding whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return errors.New("origin must use HTTPS and include a host")
	}
	if u.Opaque != "" || u.User != nil {
		return errors.New("origin must not contain opaque data or user information")
	}
	if u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("origin must not contain a path, query, or fragment")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return errors.New("origin has an invalid port")
		}
	}
	return nil
}

func validatePolicyVersionsV2(name string, values []string) error {
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
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("supported %s policy version %q is duplicated", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func validateTapExtractionV2(extraction TapExtractionV2, source MetadataSource, components ComponentsV2) error {
	var errs []error
	if extraction.PolicyVersion != BuildLocalExtractionPolicyV1 {
		errs = append(errs, fmt.Errorf("unsupported policy_version %q", extraction.PolicyVersion))
	}
	if err := validatePinnedReference(extraction.ExtractorRef); err != nil {
		errs = append(errs, fmt.Errorf("extractor_ref: %w", err))
	} else if extraction.ExtractorRef != components.CatalogExtractorRef {
		errs = append(errs, errors.New("extractor_ref does not match component tuple"))
	}
	tap, err := parseCanonicalTapID(source.Tap)
	if err == nil {
		want := "https://github.com/" + tap.Owner() + "/homebrew-" + tap.Name()
		if extraction.Repository != want {
			errs = append(errs, fmt.Errorf("repository %q does not match %q", extraction.Repository, want))
		}
	}
	for name, value := range map[string]string{"tree_digest": extraction.TreeDigest, "archive_digest": extraction.ArchiveDigest, "catalog_digest": extraction.CatalogDigest} {
		if err := validateDigest(value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if len(source.Documents) == 1 && source.Documents[0].Name == "catalog" && source.Documents[0].Digest != extraction.CatalogDigest {
		errs = append(errs, errors.New("catalog_digest does not match metadata document"))
	}
	return errors.Join(errs...)
}

func validateMetadataDocumentsV2(label string, core, local bool, documents []MetadataDocument) error {
	type requirement struct {
		name             string
		requiresEnvelope bool
	}
	requirements := []requirement{
		{name: "catalog", requiresEnvelope: false},
		{name: "set", requiresEnvelope: true},
	}
	description := "catalog and set"
	if local {
		requirements = []requirement{{name: "catalog", requiresEnvelope: false}}
		description = "catalog"
	}
	if core {
		requirements = []requirement{
			{name: "formula", requiresEnvelope: true},
			{name: "migrations", requiresEnvelope: true},
		}
		description = "formula and migrations"
	}

	var errs []error
	if len(documents) != len(requirements) {
		errs = append(errs, fmt.Errorf("%s documents must contain exactly %s", label, description))
	}
	byName := make(map[string]MetadataDocument, len(documents))
	allowed := make(map[string]struct{}, len(requirements))
	for _, required := range requirements {
		allowed[required.name] = struct{}{}
	}
	for _, document := range documents {
		if _, ok := allowed[document.Name]; !ok {
			errs = append(errs, fmt.Errorf("%s has unexpected authenticated document %q; expected exactly %s", label, document.Name, description))
			continue
		}
		byName[document.Name] = document
	}
	for _, required := range requirements {
		document, ok := byName[required.name]
		if !ok {
			errs = append(errs, fmt.Errorf("%s is missing required authenticated document %q", label, required.name))
			continue
		}
		if required.requiresEnvelope && document.EnvelopeDigest == "" {
			errs = append(errs, fmt.Errorf("%s document %q requires an authenticated envelope digest", label, required.name))
		}
		if !required.requiresEnvelope && document.EnvelopeDigest != "" {
			errs = append(errs, fmt.Errorf("%s document %q must not carry a separate envelope digest", label, required.name))
		}
	}
	return errors.Join(errs...)
}

func parseCanonicalFormulaID(raw FormulaID) (formulaid.FormulaID, error) {
	parsed, err := formulaid.Parse(string(raw))
	if err != nil {
		return formulaid.FormulaID{}, err
	}
	if parsed.String() != string(raw) {
		return formulaid.FormulaID{}, fmt.Errorf("Formula ID %q is not canonical; use %q", raw, parsed.String())
	}
	return parsed, nil
}

func parseCanonicalTapID(raw TapID) (formulaid.Tap, error) {
	parsed, err := formulaid.ParseTap(string(raw))
	if err != nil {
		return formulaid.Tap{}, err
	}
	if parsed.String() != string(raw) {
		return formulaid.Tap{}, fmt.Errorf("tap ID %q is not canonical; use %q", raw, parsed.String())
	}
	return parsed, nil
}

func validateBottleV2(platform Platform, id FormulaID, bottle BottleV2, executablePaths []string, components ComponentsV2, metadataCommit string) (string, error) {
	var errs []error
	if bottle.Tag == "" {
		errs = append(errs, fmt.Errorf("node %q bottle tag is empty", id))
	}
	if bottle.Cellar == "" {
		errs = append(errs, fmt.Errorf("node %q bottle Cellar policy is empty", id))
	}
	if err := validateBottleFilename(bottle.Filename); err != nil {
		errs = append(errs, fmt.Errorf("node %q bottle filename: %w", id, err))
	}
	if bottle.Size <= 0 || bottle.Size > MaxResolutionV2BottleBytes {
		errs = append(errs, fmt.Errorf("node %q bottle size %d is outside 1..%d", id, bottle.Size, MaxResolutionV2BottleBytes))
	}
	if err := validateDigest(bottle.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("node %q authenticated bottle digest: %w", id, err))
	}
	deferredCore := strings.HasPrefix(id.String(), "homebrew/core/") && bottle.Verification.PolicyVersion == CoreBottleVerificationDeferredV1
	if deferredCore {
		if err := validateDigest(bottle.CurrentFormulaSourceDigest); err != nil {
			errs = append(errs, fmt.Errorf("node %q current Formula source digest: %w", id, err))
		}
		if bottle.BottleFormulaSourceDigest != "" || bottle.BottleSourceRepository != "" || bottle.BottleSourceCommit != "" || bottle.BottleFormulaPath != "" || bottle.BottleSourceWaiver != "" || bottle.Verification.InventoryDigest != "" || bottle.Verification.EntryCount != 0 || bottle.Verification.ExpandedSize != 0 {
			errs = append(errs, fmt.Errorf("node %q deferred core verification contains premature bottle evidence", id))
		}
	} else {
		for label, value := range map[string]string{"current Formula source": bottle.CurrentFormulaSourceDigest, "bottle Formula source": bottle.BottleFormulaSourceDigest, "inventory": bottle.Verification.InventoryDigest} {
			if err := validateDigest(value); err != nil {
				errs = append(errs, fmt.Errorf("node %q %s digest: %w", id, label, err))
			}
		}
		if bottle.PrebuiltDerivation != nil {
			if bottle.Transport.HTTPS == nil && bottle.Transport.Local == nil {
				errs = append(errs, fmt.Errorf("node %q prebuilt-derived bottle requires HTTPS or build-local transport", id))
			}
			if bottle.BottleSourceWaiver != "" || bottle.BottleSourceRepository != "" || bottle.BottleSourceCommit != "" || bottle.BottleFormulaPath != "" {
				errs = append(errs, fmt.Errorf("node %q prebuilt-derived bottle conflicts with native bottle source evidence", id))
			}
		} else if bottle.Transport.HTTPS != nil {
			if bottle.BottleSourceWaiver != HTTPSBottleSourceWaiverPolicyV1 {
				errs = append(errs, fmt.Errorf("node %q HTTPS bottle requires source waiver %q", id, HTTPSBottleSourceWaiverPolicyV1))
			}
			if bottle.BottleSourceRepository != "" || bottle.BottleSourceCommit != "" || bottle.BottleFormulaPath != "" {
				errs = append(errs, fmt.Errorf("node %q HTTPS source waiver conflicts with asserted historical source", id))
			}
		} else if bottle.Transport.Local != nil {
			errs = append(errs, fmt.Errorf("node %q build-local transport is limited to prebuilt-derived bottles", id))
		} else {
			if bottle.BottleSourceWaiver != "" {
				errs = append(errs, fmt.Errorf("node %q OCI bottle cannot use an HTTPS source waiver", id))
			}
			parsedID, parsedErr := parseCanonicalFormulaID(id)
			if parsedErr == nil {
				expectedSourceRepository := "https://github.com/" + parsedID.Tap().Owner() + "/homebrew-" + parsedID.Tap().Name()
				if bottle.BottleSourceRepository != expectedSourceRepository {
					errs = append(errs, fmt.Errorf("node %q bottle source repository %q does not match %q", id, bottle.BottleSourceRepository, expectedSourceRepository))
				}
			}
			if len(bottle.BottleSourceCommit) != 40 || !isLowerHex(bottle.BottleSourceCommit) {
				errs = append(errs, fmt.Errorf("node %q bottle source commit is invalid", id))
			}
			if bottle.BottleFormulaPath == "" || path.IsAbs(bottle.BottleFormulaPath) || path.Clean(bottle.BottleFormulaPath) != bottle.BottleFormulaPath || strings.Contains(bottle.BottleFormulaPath, "\\") {
				errs = append(errs, fmt.Errorf("node %q bottle Formula path is invalid", id))
			}
		}
		if bottle.Verification.PolicyVersion != BottleVerificationPolicyV1 {
			errs = append(errs, fmt.Errorf("node %q bottle verification policy %q is unsupported", id, bottle.Verification.PolicyVersion))
		}
		if bottle.Verification.EntryCount <= 0 || bottle.Verification.EntryCount > 250000 {
			errs = append(errs, fmt.Errorf("node %q bottle verification entry count %d is invalid", id, bottle.Verification.EntryCount))
		}
		if bottle.Verification.ExpandedSize <= 0 || bottle.Verification.ExpandedSize > 8<<30 {
			errs = append(errs, fmt.Errorf("node %q bottle verification expanded size %d is invalid", id, bottle.Verification.ExpandedSize))
		}
	}
	for i, annotation := range bottle.SelectedAnnotations {
		if annotation.Key == "" {
			errs = append(errs, fmt.Errorf("node %q selected_annotations[%d] has an empty key", id, i))
		}
		for j := 0; j < i; j++ {
			if bottle.SelectedAnnotations[j].Key == annotation.Key {
				errs = append(errs, fmt.Errorf("node %q has duplicate selected annotation %q", id, annotation.Key))
				break
			}
		}
	}
	if bottle.Tab.Receiptless && (bottle.Tab.HomebrewVersion != "" || bottle.Tab.Compiler != "" || len(bottle.Tab.ChangedFiles) != 0 || bottle.Tab.BuiltOn != (BuiltOn{}) || len(bottle.Tab.Dependencies) != 0) {
		errs = append(errs, fmt.Errorf("node %q receiptless bottle tab claims receipt-only metadata", id))
	}
	seenTabDependencies := make(map[FormulaID]struct{}, len(bottle.Tab.Dependencies))
	for i, dependency := range bottle.Tab.Dependencies {
		dependencyID, err := parseCanonicalFormulaID(dependency.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %q bottle tab runtime_dependencies[%d].id: %w", id, i, err))
			continue
		}
		canonicalID := FormulaID(dependencyID.String())
		if _, duplicate := seenTabDependencies[canonicalID]; duplicate {
			errs = append(errs, fmt.Errorf("node %q bottle tab has duplicate runtime dependency %q", id, canonicalID))
		}
		seenTabDependencies[canonicalID] = struct{}{}
		fullName, err := parseCanonicalFormulaID(dependency.HomebrewFullName)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %q bottle tab runtime dependency %q homebrew_full_name: %w", id, canonicalID, err))
		} else if fullName.String() != dependencyID.String() {
			errs = append(errs, fmt.Errorf("node %q bottle tab runtime dependency %q has mismatched homebrew_full_name %q", id, canonicalID, dependency.HomebrewFullName))
		}
		if dependency.Revision < 0 || dependency.BottleRebuild < 0 {
			errs = append(errs, fmt.Errorf("node %q bottle tab runtime dependency %q has a negative revision", id, canonicalID))
		}
	}

	members := 0
	if bottle.Transport.OCI != nil {
		members++
	}
	if bottle.Transport.HTTPS != nil {
		members++
	}
	if bottle.Transport.Local != nil {
		members++
	}
	if members != 1 {
		errs = append(errs, fmt.Errorf("node %q bottle transport must set exactly one of oci, https, or local", id))
		return "", errors.Join(errs...)
	}
	if bottle.Tab.Receiptless && bottle.Transport.HTTPS == nil && bottle.Transport.Local == nil {
		errs = append(errs, fmt.Errorf("node %q receiptless tab marker is supported only for HTTPS or build-local bottles", id))
	}

	var artifactDigest string
	if transport := bottle.Transport.OCI; transport != nil {
		expectedRepository, expectedErr := expectedOCIRepository(id)
		if expectedErr != nil {
			errs = append(errs, fmt.Errorf("node %q OCI identity: %w", id, expectedErr))
		} else if transport.Registry != "ghcr.io" || transport.Repository != expectedRepository {
			errs = append(errs, fmt.Errorf("node %q OCI transport %s/%s does not match canonical ghcr.io/%s", id, transport.Registry, transport.Repository, expectedRepository))
		}
		if err := validateRegistry(transport.Registry); err != nil {
			errs = append(errs, fmt.Errorf("node %q OCI registry: %w", id, err))
		}
		if err := validateRepository(transport.Registry, transport.Repository); err != nil {
			errs = append(errs, fmt.Errorf("node %q OCI repository: %w", id, err))
		}
		for _, descriptor := range []struct {
			name  string
			value Descriptor
		}{{"index", transport.Index}, {"manifest", transport.Manifest}, {"config", transport.Config}, {"layer", transport.Layer}} {
			if err := validateDescriptor(descriptor.value); err != nil {
				errs = append(errs, fmt.Errorf("node %q OCI %s descriptor: %w", id, descriptor.name, err))
			}
		}
		artifactDigest = transport.Layer.Digest
		if transport.Layer.Size != bottle.Size {
			errs = append(errs, fmt.Errorf("node %q OCI layer size %d does not match bottle size %d", id, transport.Layer.Size, bottle.Size))
		}
		if transport.Layer.Size > MaxResolutionV2BottleBytes {
			errs = append(errs, fmt.Errorf("node %q OCI layer size %d exceeds %d", id, transport.Layer.Size, MaxResolutionV2BottleBytes))
		}
		if artifactDigest != bottle.SHA256 {
			errs = append(errs, fmt.Errorf("node %q OCI layer digest does not match authenticated Homebrew checksum", id))
		}
		if err := validateBottleTarget(platform, id, bottle.Tag, bottle.Tab.Arch, &transport.Manifest); err != nil {
			errs = append(errs, err)
		}
	}
	if transport := bottle.Transport.HTTPS; transport != nil {
		parsedURL, err := validateHTTPSURL(transport.URL)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %q HTTPS URL: %w", id, err))
		}
		if transport.ExpectedSize <= 0 || transport.ExpectedSize > MaxResolutionV2BottleBytes {
			errs = append(errs, fmt.Errorf("node %q HTTPS expected_size %d is outside 1..%d", id, transport.ExpectedSize, MaxResolutionV2BottleBytes))
		}
		if transport.ExpectedSize != bottle.Size {
			errs = append(errs, fmt.Errorf("node %q HTTPS expected_size %d does not match bottle size %d", id, transport.ExpectedSize, bottle.Size))
		}
		if err := validateDigest(transport.SHA256); err != nil {
			errs = append(errs, fmt.Errorf("node %q HTTPS sha256: %w", id, err))
		}
		artifactDigest = transport.SHA256
		if transport.SHA256 != bottle.SHA256 {
			errs = append(errs, fmt.Errorf("node %q HTTPS checksum does not match authenticated Homebrew checksum", id))
		}
		if err := validateBottleFilename(transport.Filename); err != nil {
			errs = append(errs, fmt.Errorf("node %q HTTPS filename: %w", id, err))
		} else if transport.Filename != bottle.Filename {
			errs = append(errs, fmt.Errorf("node %q HTTPS filename %q does not match bottle filename %q", id, transport.Filename, bottle.Filename))
		}
		if transport.FetchPolicyVersion != HTTPSFetchPolicyVersionV1 {
			errs = append(errs, fmt.Errorf("node %q HTTPS fetch policy %q is unsupported", id, transport.FetchPolicyVersion))
		}
		if len(transport.AllowedRedirectHosts) == 0 {
			errs = append(errs, fmt.Errorf("node %q HTTPS redirect host allowlist is empty", id))
		}
		if len(transport.AllowedRedirectHosts) > MaxResolutionV2Redirects+1 {
			errs = append(errs, fmt.Errorf("node %q HTTPS redirect host allowlist has %d entries; maximum is %d", id, len(transport.AllowedRedirectHosts), MaxResolutionV2Redirects+1))
		}
		seenHosts := make(map[string]struct{}, len(transport.AllowedRedirectHosts))
		for _, host := range transport.AllowedRedirectHosts {
			if err := validatePublicHostname(host); err != nil {
				errs = append(errs, fmt.Errorf("node %q HTTPS redirect host %q: %w", id, host, err))
			}
			if _, exists := seenHosts[host]; exists {
				errs = append(errs, fmt.Errorf("node %q HTTPS redirect host %q is duplicated", id, host))
			}
			seenHosts[host] = struct{}{}
		}
		if parsedURL != nil {
			if _, ok := seenHosts[parsedURL.Hostname()]; !ok {
				errs = append(errs, fmt.Errorf("node %q HTTPS redirect host allowlist omits origin host %q", id, parsedURL.Hostname()))
			}
		}
		fetchRequest := fetcher.Request{SchemaVersion: fetcher.RequestSchemaVersion, FetchPolicyVersion: transport.FetchPolicyVersion, ArtifactID: id.String(), URL: transport.URL, ExpectedSize: transport.ExpectedSize, SHA256: strings.TrimPrefix(transport.SHA256, "sha256:"), Filename: transport.Filename, AllowedRedirectHosts: slices.Clone(transport.AllowedRedirectHosts)}
		if err := fetcher.ValidateRequest(fetchRequest); err != nil {
			errs = append(errs, fmt.Errorf("node %q HTTPS fetch contract: %w", id, err))
		}
		if err := validateBottleTarget(platform, id, bottle.Tag, bottle.Tab.Arch, nil); err != nil {
			errs = append(errs, err)
		}
	}
	if transport := bottle.Transport.Local; transport != nil {
		artifactDigest = transport.SHA256
		if transport.PolicyVersion != BuildLocalArtifactPolicyV1 {
			errs = append(errs, fmt.Errorf("node %q build-local policy %q is unsupported", id, transport.PolicyVersion))
		}
		if transport.SHA256 != bottle.SHA256 || transport.Size != bottle.Size || transport.Filename != bottle.Filename {
			errs = append(errs, fmt.Errorf("node %q build-local transport does not match bottle identity", id))
		}
		if err := validateDigest(transport.SHA256); err != nil {
			errs = append(errs, fmt.Errorf("node %q build-local digest: %w", id, err))
		}
		if err := validateBottleTarget(platform, id, bottle.Tag, bottle.Tab.Arch, nil); err != nil {
			errs = append(errs, err)
		}
	}
	if bottle.PrebuiltDerivation != nil {
		if err := validatePrebuiltDerivationV2(platform, id, bottle, executablePaths, components.CatalogServiceOrigin, components.TapPolicyDigest, metadataCommit); err != nil {
			errs = append(errs, fmt.Errorf("node %q prebuilt derivation: %w", id, err))
		}
	}
	return artifactDigest, errors.Join(errs...)
}

func validatePrebuiltDerivationV2(platform Platform, id FormulaID, bottle BottleV2, executablePaths []string, catalogServiceOrigin, tapPolicyDigest, metadataCommit string) error {
	const (
		maxSourceEntries       = 4096
		maxSourceExpandedBytes = int64(4 << 30)
		maxFormulaSourceBytes  = int64(4 << 20)
	)
	derivation := bottle.PrebuiltDerivation
	if derivation == nil {
		return nil
	}
	var errs []error
	parsedID, idErr := parseCanonicalFormulaID(id)
	if idErr != nil {
		errs = append(errs, idErr)
	} else if parsedID.Tap() == formulaid.CoreTap() {
		errs = append(errs, errors.New("prebuilt derivation is not supported for homebrew/core"))
	}
	if derivation.PolicyVersion != PrebuiltDerivedBottlePolicyV1 {
		errs = append(errs, fmt.Errorf("unsupported policy version %q", derivation.PolicyVersion))
	}
	if err := validateDigest(derivation.PolicyDigest); err != nil {
		errs = append(errs, fmt.Errorf("policy digest: %w", err))
	} else if derivation.PolicyDigest != tapPolicyDigest {
		errs = append(errs, fmt.Errorf("policy digest %q does not match release tap policy %q", derivation.PolicyDigest, tapPolicyDigest))
	}
	if derivation.Source.Format != "tar+gzip" {
		errs = append(errs, fmt.Errorf("unsupported source format %q", derivation.Source.Format))
	}
	if err := validateBottleFilename(derivation.Source.Filename); err != nil {
		errs = append(errs, fmt.Errorf("source filename: %w", err))
	}
	if derivation.Source.Size <= 0 || derivation.Source.Size > MaxResolutionV2BottleBytes {
		errs = append(errs, fmt.Errorf("source size %d is outside 1..%d", derivation.Source.Size, MaxResolutionV2BottleBytes))
	}
	if err := validateDigest(derivation.Source.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("source digest: %w", err))
	}
	if derivation.Source.SHA256 == bottle.SHA256 && derivation.Source.SHA256 != "" {
		errs = append(errs, errors.New("source archive and derived bottle must have distinct digests"))
	}
	if derivation.Source.Transport.OCI != nil || derivation.Source.Transport.HTTPS == nil {
		errs = append(errs, errors.New("source transport must contain HTTPS only"))
	} else if err := validatePrebuiltHTTPSTransportV2(id, "source", *derivation.Source.Transport.HTTPS, derivation.Source.Filename, derivation.Source.Size, derivation.Source.SHA256); err != nil {
		errs = append(errs, err)
	}
	if err := validateDigest(derivation.SourceInventory.InventoryDigest); err != nil {
		errs = append(errs, fmt.Errorf("source inventory digest: %w", err))
	}
	if derivation.SourceInventory.EntryCount <= 0 || derivation.SourceInventory.EntryCount > maxSourceEntries {
		errs = append(errs, fmt.Errorf("source inventory entry count %d is outside 1..%d", derivation.SourceInventory.EntryCount, maxSourceEntries))
	}
	if derivation.SourceInventory.ExpandedSize <= 0 || derivation.SourceInventory.ExpandedSize > maxSourceExpandedBytes {
		errs = append(errs, fmt.Errorf("source inventory expanded size %d is outside 1..%d", derivation.SourceInventory.ExpandedSize, maxSourceExpandedBytes))
	}
	if err := validateContainedPrebuiltPathV2(derivation.Payload.SourcePath); err != nil {
		errs = append(errs, fmt.Errorf("payload source path: %w", err))
	}
	if err := validateContainedPrebuiltPathV2(derivation.Payload.DestinationPath); err != nil {
		errs = append(errs, fmt.Errorf("payload destination path: %w", err))
	}
	if idErr == nil && derivation.Payload.DestinationPath != "bin/"+parsedID.Name() {
		errs = append(errs, fmt.Errorf("payload destination %q does not match Formula rack %q", derivation.Payload.DestinationPath, parsedID.Name()))
	}
	if !slices.Contains(executablePaths, derivation.Payload.DestinationPath) {
		errs = append(errs, fmt.Errorf("payload destination %q is absent from node executable paths", derivation.Payload.DestinationPath))
	}
	if err := validateDigest(derivation.Payload.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("payload digest: %w", err))
	}
	if derivation.Payload.Size <= 0 || derivation.Payload.Size > maxSourceExpandedBytes {
		errs = append(errs, fmt.Errorf("payload size %d is outside 1..%d", derivation.Payload.Size, maxSourceExpandedBytes))
	} else if derivation.SourceInventory.ExpandedSize > 0 && derivation.Payload.Size > derivation.SourceInventory.ExpandedSize {
		errs = append(errs, errors.New("payload size exceeds verified source inventory"))
	}
	if derivation.Payload.ArchiveMode == 0 || derivation.Payload.ArchiveMode&^0o777 != 0 || derivation.Payload.ArchiveMode&0o111 == 0 || derivation.Payload.ArchiveMode&0o022 != 0 {
		errs = append(errs, fmt.Errorf("payload archive mode %#o is not a non-group-writable executable mode", derivation.Payload.ArchiveMode))
	}
	if derivation.Payload.DerivedMode != 0o555 {
		errs = append(errs, fmt.Errorf("payload derived mode %#o must be 0555", derivation.Payload.DerivedMode))
	}
	expectedMachine := "x86_64"
	if platform.Architecture == "arm64" {
		expectedMachine = "aarch64"
	}
	if derivation.ELF.Format != "elf64" || derivation.ELF.Machine != expectedMachine || !derivation.ELF.StaticallyLinked || derivation.ELF.Interpreter != "" || derivation.ELF.NeededLibraries == nil || len(derivation.ELF.NeededLibraries) != 0 || derivation.ELF.RPaths == nil || len(derivation.ELF.RPaths) != 0 || derivation.ELF.WritableExecutableSegments {
		errs = append(errs, fmt.Errorf("ELF evidence does not describe a static %s elf64 executable", expectedMachine))
	}
	formulaSource := derivation.FormulaSource
	formulaTap, tapErr := parseCanonicalTapID(formulaSource.Transport.Tap.ID)
	if tapErr != nil {
		errs = append(errs, fmt.Errorf("Formula source tap: %w", tapErr))
	} else if idErr == nil && formulaTap.String() != parsedID.Tap().String() {
		errs = append(errs, fmt.Errorf("Formula source tap %q does not match node tap %q", formulaTap, parsedID.Tap()))
	}
	if idErr == nil {
		expectedRepository := "https://github.com/" + parsedID.Tap().Owner() + "/homebrew-" + parsedID.Tap().Name()
		if formulaSource.Transport.Tap.Repository != expectedRepository {
			errs = append(errs, fmt.Errorf("Formula source repository %q does not match %q", formulaSource.Transport.Tap.Repository, expectedRepository))
		}
		if err := validateContainedPrebuiltPathV2(formulaSource.Transport.Path); err != nil || path.Base(formulaSource.Transport.Path) != parsedID.Name()+".rb" {
			errs = append(errs, fmt.Errorf("Formula source path %q does not match node %q", formulaSource.Transport.Path, id))
		}
	}
	if len(formulaSource.Transport.Tap.Commit) != 40 || !isLowerHex(formulaSource.Transport.Tap.Commit) {
		errs = append(errs, errors.New("Formula source commit is invalid"))
	} else if metadataCommit != "" && formulaSource.Transport.Tap.Commit != metadataCommit {
		errs = append(errs, fmt.Errorf("Formula source commit %q does not match metadata source commit %q", formulaSource.Transport.Tap.Commit, metadataCommit))
	}
	for label, value := range map[string]string{"Formula source tree": formulaSource.Transport.Tap.TreeDigest, "Formula source archive": formulaSource.Transport.Tap.ArchiveDigest, "Formula source": formulaSource.SHA256, "recipe": derivation.RecipeDigest} {
		if err := validateDigest(value); err != nil {
			errs = append(errs, fmt.Errorf("%s digest: %w", label, err))
		}
	}
	if formulaSource.Size <= 0 || formulaSource.Size > maxFormulaSourceBytes {
		errs = append(errs, fmt.Errorf("Formula source size %d is outside 1..%d", formulaSource.Size, maxFormulaSourceBytes))
	}
	if formulaSource.SHA256 != bottle.CurrentFormulaSourceDigest || formulaSource.SHA256 != bottle.BottleFormulaSourceDigest || formulaSource.SHA256 != derivation.DerivedBottle.FormulaSourceDigest {
		errs = append(errs, errors.New("Formula source digest does not bind current, embedded, and derived-bottle evidence"))
	}
	if !bottle.Tab.Receiptless {
		errs = append(errs, errors.New("derived bottle must be marked receiptless"))
	}
	relation := derivation.DerivedBottle
	if relation.Tag != bottle.Tag || relation.Filename != bottle.Filename || relation.SHA256 != bottle.SHA256 || relation.Size != bottle.Size || relation.Verification != bottle.Verification {
		errs = append(errs, errors.New("derived-bottle relation does not match outer bottle"))
	}
	if bottle.Transport.HTTPS != nil {
		if err := validateCatalogArtifactTransportV2(*bottle.Transport.HTTPS, catalogServiceOrigin, bottle); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validatePrebuiltHTTPSTransportV2(id FormulaID, label string, transport HTTPSTransport, filename string, size int64, checksum string) error {
	var errs []error
	parsed, err := validateHTTPSURL(transport.URL)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s HTTPS URL: %w", label, err))
	}
	if transport.ExpectedSize != size || transport.SHA256 != checksum || transport.Filename != filename {
		errs = append(errs, fmt.Errorf("%s HTTPS transport does not match signed artifact identity", label))
	}
	if transport.FetchPolicyVersion != HTTPSFetchPolicyVersionV1 {
		errs = append(errs, fmt.Errorf("%s HTTPS fetch policy %q is unsupported", label, transport.FetchPolicyVersion))
	}
	if len(transport.AllowedRedirectHosts) == 0 || len(transport.AllowedRedirectHosts) > MaxResolutionV2Redirects+1 {
		errs = append(errs, fmt.Errorf("%s HTTPS redirect host allowlist has invalid size %d", label, len(transport.AllowedRedirectHosts)))
	}
	seen := make(map[string]struct{}, len(transport.AllowedRedirectHosts))
	for _, host := range transport.AllowedRedirectHosts {
		if err := validatePublicHostname(host); err != nil {
			errs = append(errs, fmt.Errorf("%s HTTPS redirect host %q: %w", label, host, err))
		}
		if _, duplicate := seen[host]; duplicate {
			errs = append(errs, fmt.Errorf("%s HTTPS redirect host %q is duplicated", label, host))
		}
		seen[host] = struct{}{}
	}
	if parsed != nil {
		if _, ok := seen[parsed.Hostname()]; !ok {
			errs = append(errs, fmt.Errorf("%s HTTPS redirect allowlist omits origin host %q", label, parsed.Hostname()))
		}
	}
	request := fetcher.Request{SchemaVersion: fetcher.RequestSchemaVersion, FetchPolicyVersion: transport.FetchPolicyVersion, ArtifactID: id.String(), URL: transport.URL, ExpectedSize: transport.ExpectedSize, SHA256: strings.TrimPrefix(transport.SHA256, "sha256:"), Filename: transport.Filename, AllowedRedirectHosts: slices.Clone(transport.AllowedRedirectHosts)}
	if err := fetcher.ValidateRequest(request); err != nil {
		errs = append(errs, fmt.Errorf("%s HTTPS fetch contract: %w", label, err))
	}
	return errors.Join(errs...)
}

func validateCatalogArtifactTransportV2(transport HTTPSTransport, origin string, bottle BottleV2) error {
	parsedOrigin, err := url.Parse(origin)
	if err != nil || parsedOrigin.Hostname() == "" {
		return errors.New("catalog service origin is invalid")
	}
	expectedURL := origin + "/v1/artifacts/sha256/" + strings.TrimPrefix(bottle.SHA256, "sha256:")
	var errs []error
	if transport.URL != expectedURL {
		errs = append(errs, fmt.Errorf("derived bottle URL %q does not match catalog service artifact URL %q", transport.URL, expectedURL))
	}
	if len(transport.AllowedRedirectHosts) != 1 || transport.AllowedRedirectHosts[0] != parsedOrigin.Hostname() {
		errs = append(errs, fmt.Errorf("derived bottle redirect hosts must contain only catalog service host %q", parsedOrigin.Hostname()))
	}
	return errors.Join(errs...)
}

func validateContainedPrebuiltPathV2(value string) error {
	if value == "" || len(value) > 1024 || path.IsAbs(value) || strings.Contains(value, "\\") {
		return errors.New("path must be a bounded relative slash-separated path")
	}
	for i := range len(value) {
		if value[i] < 0x20 || value[i] == 0x7f {
			return errors.New("path contains a control character")
		}
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q is not clean and contained", value)
	}
	return nil
}

func validateBottleTarget(platform Platform, id FormulaID, tag, tabArch string, manifest *Descriptor) error {
	expectedTag, expectedArch := "x86_64_linux", "x86_64"
	if platform.Architecture == "arm64" {
		expectedTag, expectedArch = "arm64_linux", "arm64"
	}
	switch tag {
	case "all":
		if tabArch != "" {
			return fmt.Errorf("node %q all bottle has architecture %q", id, tabArch)
		}
		if manifest != nil && manifest.Platform != nil {
			return fmt.Errorf("node %q all bottle manifest unexpectedly has a platform", id)
		}
	case expectedTag:
		if tabArch != expectedArch {
			return fmt.Errorf("node %q bottle tab architecture %q, expected %q", id, tabArch, expectedArch)
		}
		if manifest != nil {
			p := manifest.Platform
			if p == nil || p.OS != "linux" || p.Architecture != platform.Architecture || p.Variant != "" {
				return fmt.Errorf("node %q bottle manifest platform does not match target", id)
			}
		}
	default:
		return fmt.Errorf("node %q bottle tag %q does not match target", id, tag)
	}
	return nil
}

func validateProvenanceV2(id FormulaID, subjectDigest string, prebuilt bool, provenance Provenance) error {
	members := 0
	if provenance.Verified != nil {
		members++
	}
	if provenance.Waiver != nil {
		members++
	}
	if members != 1 {
		return fmt.Errorf("node %q provenance must set exactly one of verified or waiver", id)
	}
	if verified := provenance.Verified; verified != nil {
		var errs []error
		if verified.PolicyVersion != VerifiedProvenancePolicyV1 {
			errs = append(errs, fmt.Errorf("unsupported verified provenance policy %q", verified.PolicyVersion))
		}
		if err := validateDigest(verified.SubjectDigest); err != nil {
			errs = append(errs, fmt.Errorf("subject digest: %w", err))
		} else if subjectDigest != "" && verified.SubjectDigest != subjectDigest {
			subjectKind := "bottle"
			if prebuilt {
				subjectKind = "prebuilt source archive"
			}
			errs = append(errs, fmt.Errorf("subject digest %q does not match %s digest %q", verified.SubjectDigest, subjectKind, subjectDigest))
		}
		if err := validateDigest(verified.StatementDigest); err != nil {
			errs = append(errs, fmt.Errorf("statement digest: %w", err))
		}
		if err := validateDigest(verified.BundleDigest); err != nil {
			errs = append(errs, fmt.Errorf("bundle digest: %w", err))
		}
		if strings.TrimSpace(verified.SignerIdentity) == "" {
			errs = append(errs, errors.New("signer identity is required"))
		}
		if strings.TrimSpace(verified.Issuer) == "" {
			errs = append(errs, errors.New("issuer is required"))
		}
		if err := errors.Join(errs...); err != nil {
			return fmt.Errorf("node %q verified provenance: %w", id, err)
		}
		return nil
	}
	if prebuilt {
		if provenance.Waiver.Policy != PrebuiltProvenanceWaiverPolicyV1 {
			return fmt.Errorf("node %q prebuilt provenance waiver %q is unsupported", id, provenance.Waiver.Policy)
		}
		return nil
	}
	if provenance.Waiver.Policy != ProvenanceWaiverPolicyV1 && !(strings.HasPrefix(id.String(), "homebrew/core/") && provenance.Waiver.Policy == CoreProvenanceWaiverPolicyV1) {
		return fmt.Errorf("node %q provenance waiver %q is unsupported", id, provenance.Waiver.Policy)
	}
	return nil
}

func expectedOCIRepository(id FormulaID) (string, error) {
	parsed, err := parseCanonicalFormulaID(id)
	if err != nil {
		return "", err
	}
	escaped := strings.NewReplacer("@", "/", "+", "x").Replace(parsed.Name())
	if escaped == "" || strings.HasPrefix(escaped, "/") || strings.HasSuffix(escaped, "/") {
		return "", errors.New("escaped Formula repository name is invalid")
	}
	return parsed.Tap().Owner() + "/" + parsed.Tap().Name() + "/" + escaped, nil
}

func validateRegistry(registry string) error {
	if registry == "" || registry != strings.ToLower(registry) || strings.TrimSpace(registry) != registry {
		return errors.New("registry must be a non-empty lowercase host")
	}
	if strings.ContainsAny(registry, "/\\@") || strings.Contains(registry, "://") {
		return errors.New("registry must not contain a scheme, path, or userinfo")
	}
	u, err := url.Parse("https://" + registry)
	if err != nil || u.Hostname() == "" || u.Path != "" {
		return errors.New("registry host is invalid")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("registry port %q is unsupported", port)
	}
	return validatePublicHostname(u.Hostname())
}

func validateRepository(registry, repository string) error {
	if repository == "" || repository != strings.ToLower(repository) || strings.TrimSpace(repository) != repository {
		return errors.New("repository must be a non-empty lowercase path")
	}
	if strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") || strings.Contains(repository, "\\") || strings.Contains(repository, "@") {
		return errors.New("repository path is malformed")
	}
	for _, component := range strings.Split(repository, "/") {
		if component == "" || component == "." || component == ".." {
			return errors.New("repository path contains an unsafe component")
		}
	}
	named, err := reference.ParseNormalizedNamed(registry + "/" + repository)
	if err != nil {
		return err
	}
	if _, tagged := named.(reference.Tagged); tagged {
		return errors.New("repository must not contain a tag")
	}
	if _, digested := named.(reference.Digested); digested {
		return errors.New("repository must not contain a digest")
	}
	if reference.Domain(named) != registry || reference.Path(named) != repository {
		return errors.New("repository is not canonical for the recorded registry")
	}
	return nil
}

func validateHTTPSURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 8<<10 || strings.TrimSpace(raw) != raw {
		return nil, errors.New("URL is empty, overlong, or contains surrounding whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.Host == "" || u.Opaque != "" {
		return nil, errors.New("URL must be an absolute HTTPS URL")
	}
	if u.User != nil {
		return nil, errors.New("URL userinfo is not allowed")
	}
	if u.Fragment != "" {
		return nil, errors.New("URL fragments are not allowed")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, fmt.Errorf("URL port %q is unsupported", port)
	}
	if u.Hostname() != strings.ToLower(u.Hostname()) {
		return nil, errors.New("URL hostname must be lowercase")
	}
	if err := validatePublicHostname(u.Hostname()); err != nil {
		return nil, err
	}
	return u, nil
}

func validatePublicHostname(host string) error {
	if host == "" || host != strings.ToLower(host) || strings.TrimSpace(host) != host {
		return errors.New("host must be a non-empty lowercase DNS name")
	}
	if net.ParseIP(host) != nil {
		return errors.New("IP-literal hosts are not allowed")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errors.New("local hostnames are not allowed")
	}
	if len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return errors.New("DNS hostname is malformed")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("DNS hostname label is malformed")
		}
		for i := range len(label) {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return errors.New("DNS hostname contains unsupported characters")
			}
		}
	}
	return nil
}

func validateBottleFilename(filename string) error {
	if filename == "" || len(filename) > 255 || filename == "." || filename == ".." || path.Base(filename) != filename || strings.ContainsAny(filename, "/\\") {
		return fmt.Errorf("unsafe bottle filename %q", filename)
	}
	for i := range len(filename) {
		if filename[i] < 0x20 || filename[i] == 0x7f {
			return fmt.Errorf("unsafe bottle filename %q", filename)
		}
	}
	return nil
}

func validV2Token(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func cloneRecordV2(record RecordV2) RecordV2 {
	data, _ := json.Marshal(record)
	var clone RecordV2
	_ = json.Unmarshal(data, &clone)
	clone.Runtime.Profile = record.Runtime.Profile
	return clone
}

func canonicalizeV2(r *RecordV2) {
	r.ResolvedAt = r.ResolvedAt.UTC().Round(0)
	slices.Sort(r.Components.SupportedCatalogPolicyVersions)
	slices.Sort(r.Components.SupportedFetchPolicyVersions)
	slices.Sort(r.Components.SupportedProvenancePolicyVersions)
	for i := range r.MetadataSources {
		source := &r.MetadataSources[i]
		source.GeneratedAt = source.GeneratedAt.UTC().Round(0)
		source.FetchedAt = source.FetchedAt.UTC().Round(0)
		slices.SortFunc(source.Documents, func(a, b MetadataDocument) int {
			if c := strings.Compare(a.Name, b.Name); c != 0 {
				return c
			}
			if c := strings.Compare(a.Digest, b.Digest); c != 0 {
				return c
			}
			return strings.Compare(a.EnvelopeDigest, b.EnvelopeDigest)
		})
	}
	slices.SortFunc(r.MetadataSources, func(a, b MetadataSource) int {
		if c := strings.Compare(a.Tap.String(), b.Tap.String()); c != 0 {
			return c
		}
		return strings.Compare(a.Commit, b.Commit)
	})
	slices.SortFunc(r.Nodes, func(a, b NodeV2) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	slices.Sort(r.Runtime.WritablePaths)
	if len(r.Runtime.GeneratedPATH) == 0 {
		r.Runtime.GeneratedPATH = nil
	}
	if len(r.Runtime.WritablePaths) == 0 {
		r.Runtime.WritablePaths = nil
	}
	for i := range r.Nodes {
		node := &r.Nodes[i]
		slices.SortFunc(node.Dependencies, func(a, b RequirementV2) int { return strings.Compare(a.ID.String(), b.ID.String()) })
		slices.Sort(node.ExecutablePaths)
		slices.Sort(node.Bottle.Tab.ChangedFiles)
		slices.SortFunc(node.Bottle.Tab.Dependencies, func(a, b RuntimeDependencyV2) int {
			if c := strings.Compare(a.ID.String(), b.ID.String()); c != 0 {
				return c
			}
			return strings.Compare(a.PkgVersion, b.PkgVersion)
		})
		slices.SortFunc(node.Bottle.SelectedAnnotations, func(a, b KV) int {
			if c := strings.Compare(a.Key, b.Key); c != 0 {
				return c
			}
			return strings.Compare(a.Value, b.Value)
		})
		if node.Bottle.Transport.HTTPS != nil {
			slices.Sort(node.Bottle.Transport.HTTPS.AllowedRedirectHosts)
		}
		if derivation := node.Bottle.PrebuiltDerivation; derivation != nil {
			if derivation.Source.Transport.HTTPS != nil {
				slices.Sort(derivation.Source.Transport.HTTPS.AllowedRedirectHosts)
			}
			slices.Sort(derivation.ELF.NeededLibraries)
			slices.Sort(derivation.ELF.RPaths)
		}
		if len(node.Dependencies) == 0 {
			node.Dependencies = nil
		}
		if len(node.ExecutablePaths) == 0 {
			node.ExecutablePaths = nil
		}
		if len(node.Bottle.Tab.ChangedFiles) == 0 {
			node.Bottle.Tab.ChangedFiles = nil
		}
		if len(node.Bottle.Tab.Dependencies) == 0 {
			node.Bottle.Tab.Dependencies = nil
		}
		if len(node.Bottle.SelectedAnnotations) == 0 {
			node.Bottle.SelectedAnnotations = nil
		}
		if transport := node.Bottle.Transport.OCI; transport != nil {
			for _, descriptor := range []*Descriptor{&transport.Index, &transport.Manifest, &transport.Config, &transport.Layer} {
				if len(descriptor.Metadata) == 0 {
					descriptor.Metadata = nil
				}
			}
		}
	}
}

func ensureJSONEOF(dec *json.Decoder) error {
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateUniqueJSONV2(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if err := walkUniqueJSONV2(dec, token); err != nil {
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

func walkUniqueJSONV2(dec *json.Decoder, token json.Token) error {
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
			if err := walkUniqueJSONV2(dec, value); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for dec.More() {
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSONV2(dec, value); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}
