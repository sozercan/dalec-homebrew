package catalog

import (
	"encoding/json"
	"time"
)

// Platform is a normalized supported OCI target platform.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// PlatformRequest binds one normalized target platform to the exact external
// root set that applies after Dalec architecture filtering.
type PlatformRequest struct {
	Platform      Platform    `json:"platform"`
	ExternalRoots []FormulaID `json:"external_roots"`
	CoreRoots     []FormulaID `json:"core_roots,omitempty"`
}

// Request is the canonical body of POST /v1/catalog-sets. Canonical protocol
// JSON uses Targets so roots cannot be substituted across platforms.
type Request struct {
	SchemaVersion      string            `json:"schema_version"`
	Targets            []PlatformRequest `json:"targets"`
	HomebrewCommit     string            `json:"homebrew_commit"`
	CoreSnapshotDigest string            `json:"core_snapshot_digest"`

	// ExternalRoots and Platforms are an in-memory compatibility bridge for
	// callers being migrated to Targets. They are never serialized. A request
	// cannot mix these fields with Targets, and canonical bytes always contain
	// per-platform targets.
	ExternalRoots []FormulaID `json:"-"`
	Platforms     []Platform  `json:"-"`
}

// TapSource is the immutable source identity shared by a catalog and the
// signed reference to it.
type TapSource struct {
	ID            TapID  `json:"id"`
	Repository    string `json:"repository"`
	Commit        string `json:"commit"`
	TreeDigest    string `json:"tree_digest"`
	ArchiveDigest string `json:"archive_digest"`
}

// TapCatalog is one complete canonical catalog for an exact non-core tap
// commit. Its bytes are content addressed by CatalogReference.
type TapCatalog struct {
	SchemaVersion string          `json:"schema_version"`
	Tap           TapSource       `json:"tap"`
	PublishedAt   time.Time       `json:"published_at"`
	Sequence      uint64          `json:"sequence"`
	Formulae      []Formula       `json:"formulae"`
	Aliases       []ScopedMapping `json:"aliases,omitempty"`
	Renames       []ScopedMapping `json:"renames,omitempty"`
	Migrations    []Migration     `json:"migrations,omitempty"`
}

// Formula contains current stable metadata needed to resolve bottles without
// evaluating Formula code in the frontend.
type Formula struct {
	ID                FormulaID                   `json:"id"`
	Name              string                      `json:"name"`
	HomebrewFullName  string                      `json:"homebrew_full_name"`
	SourcePath        string                      `json:"source_path"`
	SourceDigest      string                      `json:"source_digest"`
	StableVersion     string                      `json:"stable_version"`
	Revision          int                         `json:"revision"`
	VersionScheme     int                         `json:"version_scheme"`
	Disabled          bool                        `json:"disabled,omitempty"`
	KegOnly           bool                        `json:"keg_only,omitempty"`
	License           string                      `json:"license,omitempty"`
	Dependencies      []Dependency                `json:"dependencies,omitempty"`
	Variations        []FormulaVariation          `json:"variations,omitempty"`
	VersionedFormulae []FormulaID                 `json:"versioned_formulae,omitempty"`
	Bottle            *BottleDeclaration          `json:"bottle,omitempty"`
	PrebuiltArchive   *PrebuiltArchiveDeclaration `json:"prebuilt_archive,omitempty"`
}

// Dependency preserves the Formula spelling that was evaluated and the
// independently normalized canonical dependency identity.
type Dependency struct {
	Raw string    `json:"raw"`
	ID  FormulaID `json:"id"`
}

// FormulaVariation captures the target-specific fields relevant to Linux
// closure and bottle selection.
type FormulaVariation struct {
	Tag                   string       `json:"tag"`
	Unavailable           bool         `json:"unavailable,omitempty"`
	Dependencies          []Dependency `json:"dependencies,omitempty"`
	OverridesDependencies bool         `json:"overrides_dependencies,omitempty"`
	KegOnly               bool         `json:"keg_only,omitempty"`
	OverridesKegOnly      bool         `json:"overrides_keg_only,omitempty"`
}

// BottleDeclaration records the current stable bottle root and per-tag
// authenticated declaration. Exact downloaded size and transport evidence live
// in BottleArtifact.
type BottleDeclaration struct {
	RootURL string       `json:"root_url"`
	Rebuild int          `json:"rebuild"`
	Files   []BottleFile `json:"files"`
}

// BottleFile is one supported Linux bottle declaration.
type BottleFile struct {
	Tag    string `json:"tag"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Cellar string `json:"cellar"`
}

// PrebuiltArchiveDeclaration records candidate stable source archives for
// target tags without a native bottle. Presence does not authorize selection;
// the release-bound tap policy remains authoritative.
type PrebuiltArchiveDeclaration struct {
	Files []PrebuiltArchiveFile `json:"files"`
}

// PrebuiltArchiveFile is one checksummed platform-specific stable archive.
type PrebuiltArchiveFile struct {
	Tag    string `json:"tag"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Format string `json:"format"`
}

// ScopedMapping is a signed same-tap alias or rename.
type ScopedMapping struct {
	From FormulaID `json:"from"`
	To   FormulaID `json:"to"`
}

// Migration is a signed migration. RawTarget is retained so validation can
// require a fully qualified target even when To is represented by FormulaID.
type Migration struct {
	From      FormulaID `json:"from"`
	RawTarget string    `json:"raw_target"`
	To        FormulaID `json:"to"`
}

// ComponentIdentity binds the catalog service and isolated extractor build
// identities into the signed catalog-set payload.
type ComponentIdentity struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// CatalogReference is the signed immutable locator for one reached tap
// catalog.
type CatalogReference struct {
	Tap         TapSource `json:"tap"`
	PublishedAt time.Time `json:"published_at"`
	Sequence    uint64    `json:"sequence"`
	URL         string    `json:"url"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
}

// CatalogSetPayload is the PS512 JWS payload returned for a completed catalog
// operation. JWS serialization and verification intentionally live outside
// this package.
type CatalogSetPayload struct {
	SchemaVersion      string             `json:"schema_version"`
	RequestDigest      string             `json:"request_digest"`
	CoreSnapshotDigest string             `json:"core_snapshot_digest"`
	GeneratedAt        time.Time          `json:"generated_at"`
	ExpiresAt          time.Time          `json:"expires_at"`
	CatalogService     ComponentIdentity  `json:"catalog_service"`
	Extractor          ComponentIdentity  `json:"extractor"`
	Catalogs           []CatalogReference `json:"catalogs"`
	Results            []PlatformResult   `json:"results"`
}

// PlatformResult is the service-computed closure and selected bottle artifact
// set for one requested platform.
type PlatformResult struct {
	Platform  Platform         `json:"platform"`
	Closure   ClosureResult    `json:"closure"`
	Artifacts []BottleArtifact `json:"artifacts"`
}

// ClosureResult separates canonical graph identity from Cellar rack identity.
type ClosureResult struct {
	Requested         []FormulaID        `json:"requested"`
	RequestedMappings []RequestedMapping `json:"requested_mappings"`
	NormalizationTaps []TapID            `json:"normalization_taps,omitempty"`
	Nodes             []Node             `json:"nodes"`
	InstallOrder      []FormulaID        `json:"install_order"`
}

// RequestedMapping preserves the pre-catalog root identity while binding its
// canonical resolved graph node after aliases, renames, and migrations.
type RequestedMapping struct {
	Requested FormulaID `json:"requested"`
	Resolved  FormulaID `json:"resolved"`
}

// Node is one resolved closure node.
type Node struct {
	ID               FormulaID     `json:"id"`
	Tap              TapID         `json:"tap"`
	Name             string        `json:"name"`
	HomebrewFullName string        `json:"homebrew_full_name"`
	FormulaVersion   string        `json:"formula_version"`
	FormulaRevision  int           `json:"formula_revision"`
	PkgVersion       string        `json:"pkg_version"`
	VersionScheme    int           `json:"version_scheme"`
	BottleRebuild    int           `json:"bottle_rebuild"`
	License          string        `json:"license,omitempty"`
	KegOnly          bool          `json:"keg_only,omitempty"`
	Dependencies     []Requirement `json:"dependencies,omitempty"`
}

// Requirement is the normalized closure edge. Minimum fields are populated
// from verified bottle receipt metadata when available.
type Requirement struct {
	Raw                  string    `json:"raw"`
	ID                   FormulaID `json:"id"`
	MinimumPkgVersion    string    `json:"minimum_pkg_version,omitempty"`
	MinimumRevision      int       `json:"minimum_revision"`
	MinimumBottleRebuild int       `json:"minimum_bottle_rebuild"`
	DeclaredDirectly     bool      `json:"declared_directly,omitempty"`
}

// BottleArtifact binds exact selected bytes, transport, static verification,
// Formula-source evidence, and provenance for one platform node.
type BottleArtifact struct {
	ID                         FormulaID           `json:"id"`
	Platform                   Platform            `json:"platform"`
	Tag                        string              `json:"tag"`
	Filename                   string              `json:"filename"`
	SHA256                     string              `json:"sha256"`
	Size                       int64               `json:"size"`
	Cellar                     string              `json:"cellar"`
	Tab                        BottleTab           `json:"tab"`
	CurrentFormulaSourceDigest string              `json:"current_formula_source_digest"`
	BottleFormulaSourceDigest  string              `json:"bottle_formula_source_digest"`
	BottleSourceRepository     string              `json:"bottle_source_repository"`
	BottleSourceCommit         string              `json:"bottle_source_commit"`
	BottleFormulaPath          string              `json:"bottle_formula_path"`
	BottleSourceWaiver         string              `json:"bottle_source_waiver,omitempty"`
	ExecutablePaths            []string            `json:"executable_paths,omitempty"`
	Transport                  Transport           `json:"transport"`
	Verification               BottleVerification  `json:"verification"`
	Provenance                 Provenance          `json:"provenance"`
	PrebuiltDerivation         *PrebuiltDerivation `json:"prebuilt_derivation,omitempty"`
}

// PrebuiltDerivation binds the policy-authorized upstream archive and exact
// deterministic transformation that produced a selected derived bottle. The
// outer BottleArtifact continues to describe the derived bottle bytes.
type PrebuiltDerivation struct {
	PolicyVersion   string                        `json:"policy_version"`
	PolicyDigest    string                        `json:"policy_digest"`
	Source          PrebuiltSourceArtifact        `json:"source"`
	SourceInventory PrebuiltSourceInventory       `json:"source_inventory"`
	Payload         PrebuiltPayloadEvidence       `json:"payload"`
	ELF             PrebuiltELFEvidence           `json:"elf"`
	FormulaSource   PrebuiltFormulaSourceEvidence `json:"formula_source"`
	RecipeDigest    string                        `json:"recipe_digest"`
	DerivedBottle   PrebuiltDerivedBottleRelation `json:"derived_bottle"`
}

// PrebuiltSourceArtifact is the exact upstream stable archive and its bounded
// public transport. Transport must contain HTTPS and never OCI.
type PrebuiltSourceArtifact struct {
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Format    string    `json:"format"`
	Transport Transport `json:"transport"`
}

// PrebuiltSourceInventory summarizes complete hostile-archive verification.
type PrebuiltSourceInventory struct {
	InventoryDigest string `json:"inventory_digest"`
	EntryCount      int    `json:"entry_count"`
	ExpandedSize    int64  `json:"expanded_size"`
}

// PrebuiltPayloadEvidence identifies the one executable archive member and the
// policy-selected path and mode used in the derived bottle.
type PrebuiltPayloadEvidence struct {
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size"`
	ArchiveMode     uint32 `json:"archive_mode"`
	DerivedMode     uint32 `json:"derived_mode"`
}

// PrebuiltELFEvidence records the static executable checks performed before a
// payload can be included in a derived bottle. Empty dynamic-linking arrays are
// serialized explicitly so absence is authenticated.
type PrebuiltELFEvidence struct {
	Format                     string   `json:"format"`
	Machine                    string   `json:"machine"`
	StaticallyLinked           bool     `json:"statically_linked"`
	Interpreter                string   `json:"interpreter"`
	NeededLibraries            []string `json:"needed_libraries"`
	RPaths                     []string `json:"rpaths"`
	WritableExecutableSegments bool     `json:"writable_executable_segments"`
}

// PrebuiltFormulaSourceEvidence binds the exact authenticated tap Formula used
// as the embedded Formula in the derived bottle.
type PrebuiltFormulaSourceEvidence struct {
	Transport TapFormulaSourceTransport `json:"transport"`
	SHA256    string                    `json:"sha256"`
	Size      int64                     `json:"size"`
}

// TapFormulaSourceTransport identifies Formula bytes within one exact
// authenticated default-GitHub tap snapshot.
type TapFormulaSourceTransport struct {
	Tap  TapSource `json:"tap"`
	Path string    `json:"path"`
}

// PrebuiltDerivedBottleRelation repeats the selected derived-bottle identity so
// validators can reject mix-and-match source, recipe, and bottle records.
type PrebuiltDerivedBottleRelation struct {
	Tag                 string             `json:"tag"`
	Filename            string             `json:"filename"`
	SHA256              string             `json:"sha256"`
	Size                int64              `json:"size"`
	Verification        BottleVerification `json:"verification"`
	FormulaSourceDigest string             `json:"formula_source_digest"`
}

// BottleTab records exact bottle-build receipt metadata independently of the
// current catalog graph.
type BottleTab struct {
	Receiptless     bool                      `json:"receiptless,omitempty"`
	HomebrewVersion string                    `json:"homebrew_version,omitempty"`
	Arch            string                    `json:"arch,omitempty"`
	Compiler        string                    `json:"compiler,omitempty"`
	ChangedFiles    []string                  `json:"changed_files,omitempty"`
	BuiltOn         BottleBuiltOn             `json:"built_on,omitempty"`
	Dependencies    []BottleRuntimeDependency `json:"runtime_dependencies,omitempty"`
}

type BottleBuiltOn struct {
	OS              string `json:"os,omitempty"`
	OSVersion       string `json:"os_version,omitempty"`
	CPUFamily       string `json:"cpu_family,omitempty"`
	OldestCPUFamily string `json:"oldest_cpu_family,omitempty"`
	GlibcVersion    string `json:"glibc_version,omitempty"`
}

type BottleRuntimeDependency struct {
	ID               FormulaID `json:"id"`
	HomebrewFullName FormulaID `json:"homebrew_full_name"`
	Version          string    `json:"version"`
	Revision         int       `json:"revision"`
	BottleRebuild    int       `json:"bottle_rebuild"`
	PkgVersion       string    `json:"pkg_version"`
	DeclaredDirectly bool      `json:"declared_directly,omitempty"`
}

// Transport is a strict union. Exactly one member must be set.
type Transport struct {
	OCI   *OCITransport   `json:"oci,omitempty"`
	HTTPS *HTTPSTransport `json:"https,omitempty"`
}

// OCITransport binds the exact OCI descriptor chain used to replay a bottle.
type OCITransport struct {
	Registry   string     `json:"registry"`
	Repository string     `json:"repository"`
	Index      Descriptor `json:"index"`
	Manifest   Descriptor `json:"manifest"`
	Config     Descriptor `json:"config"`
	Layer      Descriptor `json:"layer"`
}

// HTTPSTransport is the signed bounded-fetch request for a public bottle.
type HTTPSTransport struct {
	URL                  string   `json:"url"`
	ExpectedSize         int64    `json:"expected_size"`
	SHA256               string   `json:"sha256"`
	Filename             string   `json:"filename"`
	AllowedRedirectHosts []string `json:"allowed_redirect_hosts"`
	FetchPolicyVersion   string   `json:"fetch_policy_version"`
}

// Descriptor is the exact OCI identity of one index, manifest, config, or
// layer object.
type Descriptor struct {
	Digest      string       `json:"digest"`
	Size        int64        `json:"size"`
	MediaType   string       `json:"media_type"`
	Platform    *Platform    `json:"platform,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

// Annotation avoids map-order and duplicate-key ambiguity in signed data.
type Annotation struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// BottleVerification is the bounded static archive-verification summary.
type BottleVerification struct {
	PolicyVersion   string `json:"policy_version"`
	InventoryDigest string `json:"inventory_digest"`
	EntryCount      int    `json:"entry_count"`
	ExpandedSize    int64  `json:"expanded_size"`
}

// Provenance is a strict union. Exactly one member must be set.
type Provenance struct {
	Verified *VerifiedProvenance `json:"verified,omitempty"`
	Waiver   *ProvenanceWaiver   `json:"waiver,omitempty"`
}

// VerifiedProvenance records verified Sigstore/in-toto evidence whose subject
// is the exact selected bottle digest.
type VerifiedProvenance struct {
	PolicyVersion   string `json:"policy_version"`
	SubjectDigest   string `json:"subject_digest"`
	StatementDigest string `json:"statement_digest"`
	BundleDigest    string `json:"bundle_digest"`
	SignerIdentity  string `json:"signer_identity"`
	Issuer          string `json:"issuer"`
}

// ProvenanceWaiver is the only accepted fallback when supported provenance is
// unavailable.
type ProvenanceWaiver struct {
	Policy string `json:"policy"`
}

// OperationStatus is the stable state returned by GET /v1/operations/{id}.
type OperationStatus string

const (
	OperationPending   OperationStatus = "pending"
	OperationCompleted OperationStatus = "completed"
	OperationFailed    OperationStatus = "failed"
)

// FailureCode is a stable machine-readable catalog operation failure.
type FailureCode string

const (
	FailureTimeout       FailureCode = "timeout"
	FailureUnavailable   FailureCode = "unavailable"
	FailureInvalidTap    FailureCode = "invalid-tap"
	FailureMissingBottle FailureCode = "missing-bottle"
	FailurePolicy        FailureCode = "policy"
	FailureSignature     FailureCode = "signature"
)

// Failure is safe to persist and return to a client. Message is bounded and
// must not contain control characters.
type Failure struct {
	Code    FailureCode `json:"code"`
	Message string      `json:"message,omitempty"`
}

// CatalogSetResult carries an authenticated catalog-set JWS. PayloadDigest is
// the expected digest after JWS verification.
type CatalogSetResult struct {
	SchemaVersion string          `json:"schema_version"`
	RequestDigest string          `json:"request_digest"`
	PayloadDigest string          `json:"payload_digest"`
	JWS           json.RawMessage `json:"jws"`
}

// Operation is a strict status union. Pending operations carry RetryAfter;
// completed operations carry Result; failed operations carry Failure.
type Operation struct {
	SchemaVersion     string            `json:"schema_version"`
	ID                string            `json:"id"`
	Status            OperationStatus   `json:"status"`
	RetryAfterSeconds int               `json:"retry_after_seconds,omitempty"`
	Result            *CatalogSetResult `json:"result,omitempty"`
	Failure           *Failure          `json:"failure,omitempty"`
}
