package runtimefs

import (
	"errors"
	"fmt"
	"os"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

const (
	DefaultInstallPrefix = "/home/linuxbrew/.linuxbrew"

	InventorySchemaVersion                = "dalec-homebrew-runtime-inventory/v1"
	InventorySchemaVersionV2              = "dalec-homebrew-runtime-inventory/v2"
	PruneSchemaVersion                    = "dalec-homebrew-prune-manifest/v2"
	PruneSchemaVersionV2                  = "dalec-homebrew-prune-manifest/v4"
	PruneSubtreeCommitmentSchemaVersion   = "dalec-homebrew-prune-subtree-commitment/v1"
	PruneSubtreeCommitmentSchemaVersionV2 = "dalec-homebrew-prune-subtree-commitment/v2"
	ManifestSchemaVersion                 = "dalec-homebrew-runtime-manifest/v1"
	ManifestSchemaVersionV2               = "dalec-homebrew-runtime-manifest/v2"

	InventoryFileName = "runtime-inventory.json"
	PruneFileName     = "prune-manifest.json"
	ManifestFileName  = "manifest.json"
	SBOMFileName      = "sbom.spdx.json"
)

// PathRule approves one package-owned subtree. Path may be relative to the
// Homebrew prefix, relative to the containing Etc/Var/Owners group, or an
// absolute path below Options.InstallPrefix.
//
// Writable is accepted only for Var rules. A writable rule must also appear in
// resolution.Record.Runtime.WritablePaths. Required makes absence an error.
type PathRule struct {
	Path     string `json:"path"`
	Package  string `json:"package"`
	Writable bool   `json:"writable,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// Allowlist is intentionally explicit. Cellar and Opt must be enabled. The
// global booleans approve those complete global link/data roots, subject to the
// package-manager denylist. Etc and Var approve only their listed subtrees.
// Owners supplies explicit attribution for otherwise ambiguous regular files
// below approved global roots.
type Allowlist struct {
	Cellar bool `json:"cellar"`
	Opt    bool `json:"opt"`
	Bin    bool `json:"bin,omitempty"`
	Sbin   bool `json:"sbin,omitempty"`
	Lib    bool `json:"lib,omitempty"`
	Share  bool `json:"share,omitempty"`

	// PruningProfile and PruningRules select one release-bound, executable
	// runtime-minimization policy. Empty values retain the complete allowlisted
	// package payload and preserve the legacy V1 behavior.
	PruningProfile string   `json:"pruning_profile,omitempty"`
	PruningRules   []string `json:"pruning_rules,omitempty"`

	Etc    []PathRule `json:"etc,omitempty"`
	Var    []PathRule `json:"var,omitempty"`
	Owners []PathRule `json:"owners,omitempty"`
}

// ChownFunc applies intended ownership. symlink is true when lchown semantics
// are required. A nil function uses os.Chown/os.Lchown and therefore normally
// requires root. Tests can inject a recorder or no-op implementation.
type ChownFunc func(path string, uid, gid int, symlink bool) error

// Options controls assembly and verification.
type Options struct {
	// InstallPrefix is the logical in-image prefix used to validate and rewrite
	// absolute symlink targets. It defaults to DefaultInstallPrefix.
	InstallPrefix string
	Allowlist     Allowlist

	// Chown overrides ownership application. When injected, Verify trusts the
	// callback result and skips host uid/gid inspection, allowing unprivileged
	// tests while still checking every requested ownership operation.
	Chown ChownFunc
}

// EntryType is the normalized type recorded in inventory and prune evidence.
type EntryType string

const (
	TypeDirectory EntryType = "directory"
	TypeRegular   EntryType = "file"
	TypeSymlink   EntryType = "symlink"
	TypeHardlink  EntryType = "hardlink"
)

// InventoryEntry describes one retained path relative to the Homebrew prefix.
type InventoryEntry struct {
	Path       string    `json:"path"`
	Type       EntryType `json:"type"`
	Mode       string    `json:"mode"`
	UID        int       `json:"uid"`
	GID        int       `json:"gid"`
	MTime      int64     `json:"mtime"`
	Size       int64     `json:"size,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	LinkTarget string    `json:"link_target,omitempty"`
	HardlinkTo string    `json:"hardlink_to,omitempty"`
	Package    string    `json:"package,omitempty"`
	FormulaID  string    `json:"formula_id,omitempty"`
	Writable   bool      `json:"writable,omitempty"`
}

// Inventory is the deterministic allowlist used to construct and re-verify the
// clean output prefix.
type Inventory struct {
	SchemaVersion       string           `json:"schema_version"`
	PolicyVersion       string           `json:"policy_version"`
	ResolutionDigest    string           `json:"resolution_digest"`
	PruningPolicyDigest string           `json:"pruning_policy_digest"`
	SourceDateEpoch     int64            `json:"source_date_epoch"`
	Prefix              string           `json:"prefix"`
	Entries             []InventoryEntry `json:"entries"`
}

// PruneReason is a stable omission category.
type PruneReason string

const (
	PruneNotAllowlisted  PruneReason = "not_allowlisted"
	PruneRepository      PruneReason = "homebrew_repository"
	PruneBrewExecutable  PruneReason = "brew_executable"
	PruneCache           PruneReason = "package_manager_cache"
	PruneLog             PruneReason = "installer_log"
	PruneManagerState    PruneReason = "package_manager_state"
	PruneFormulaMetadata PruneReason = "formula_metadata_exported"
	PruneReceipt         PruneReason = "receipt_metadata_exported"
	PrunePackageSBOM     PruneReason = "package_manager_sbom_exported"
	PruneTooling         PruneReason = "materializer_tooling"
	PruneOptionalTooling PruneReason = "optional_dependency_tooling"
	PruneRuntimeBase     PruneReason = "runtime_base_provided"
	PruneRuntimeHeaders  PruneReason = "transitive_runtime_headers"
	PruneRuntimeDocs     PruneReason = "transitive_runtime_man_info"
	PruneRuntimeBuild    PruneReason = "transitive_runtime_build_metadata"
	PruneRuntimeTests    PruneReason = "transitive_runtime_python_tests"
	PruneRuntimeStatic   PruneReason = "transitive_runtime_static_archives"
	PruneRuntimeShell    PruneReason = "transitive_runtime_shell_completions"
	PruneRuntimeShareDoc PruneReason = "transitive_runtime_share_doc"
)

// PruneEntry records every omitted source path. Regular files are hashed before
// omission. MetadataExport and ExportedTo prove that removed package-manager
// records were represented in durable runtime evidence.
type PruneEntry struct {
	Path           string      `json:"path"`
	Type           EntryType   `json:"type"`
	Mode           string      `json:"mode"`
	Size           int64       `json:"size,omitempty"`
	SHA256         string      `json:"sha256,omitempty"`
	LinkTarget     string      `json:"link_target,omitempty"`
	Reason         PruneReason `json:"reason"`
	Package        string      `json:"package,omitempty"`
	FormulaID      string      `json:"formula_id,omitempty"`
	MetadataExport string      `json:"metadata_export,omitempty"`
	ExportedTo     []string    `json:"exported_to,omitempty"`
}

// PruneSubtree commits a completely excluded source subtree without embedding
// one manifest row per descendant. CommitmentDigest is the digest of the
// versioned, path-sorted commitment tuples identified by CommitmentSchema.
// Each tuple includes path, type, mode, normalized size, and either a content
// digest or link target. V2 attributed commitments also bind Package and
// FormulaID in their header. EntryCount includes the subtree root;
// RegularBytes is the sum of regular-file entry sizes represented by the
// commitment.
type PruneSubtree struct {
	Path             string      `json:"path"`
	Reason           PruneReason `json:"reason"`
	Package          string      `json:"package,omitempty"`
	FormulaID        string      `json:"formula_id,omitempty"`
	EntryCount       int         `json:"entry_count"`
	RegularBytes     int64       `json:"regular_bytes"`
	CommitmentSchema string      `json:"commitment_schema"`
	CommitmentDigest string      `json:"commitment_digest"`
}

// PruneManifest is an exact deterministic record of source paths not copied to
// the runtime prefix. Entries remain explicit where package attribution or
// exported metadata matters. Fully excluded infrastructure subtrees may be
// represented by deterministic commitments in Subtrees.
type PruneManifest struct {
	SchemaVersion       string         `json:"schema_version"`
	PolicyVersion       string         `json:"policy_version"`
	ResolutionDigest    string         `json:"resolution_digest"`
	PruningPolicyDigest string         `json:"pruning_policy_digest"`
	SourceDateEpoch     int64          `json:"source_date_epoch"`
	Prefix              string         `json:"prefix"`
	Subtrees            []PruneSubtree `json:"subtrees,omitempty"`
	Entries             []PruneEntry   `json:"entries"`
}

// MetadataExport identifies package-manager metadata removed after its package
// identity and source digest were exported.
type MetadataExport struct {
	Package    string `json:"package"`
	FormulaID  string `json:"formula_id,omitempty"`
	Kind       string `json:"kind"`
	SourcePath string `json:"source_path"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
}

// RuntimePackage is the compact non-secret package identity embedded in the
// final image.
type RuntimePackage struct {
	FormulaID         string           `json:"formula_id,omitempty"`
	UpstreamFormulaID string           `json:"upstream_formula_id,omitempty"`
	Name              string           `json:"name"`
	FullName          string           `json:"full_name"`
	FormulaVersion    string           `json:"formula_version"`
	FormulaRevision   int              `json:"formula_revision"`
	PkgVersion        string           `json:"pkg_version"`
	VersionScheme     int              `json:"version_scheme"`
	BottleRebuild     int              `json:"bottle_rebuild"`
	BottleTag         string           `json:"bottle_tag"`
	BottleLayer       string           `json:"bottle_layer_digest"`
	BottleLayerSize   int64            `json:"bottle_layer_size"`
	License           string           `json:"license,omitempty"`
	KegPath           string           `json:"keg_path"`
	ExportedMetadata  []MetadataExport `json:"exported_metadata,omitempty"`
}

// RuntimeManifest is suitable for embedding at
// /usr/share/dalec-homebrew/manifest.json by the caller assembling the final
// rootfs. It intentionally contains no self-referential digest.
type RuntimeManifest struct {
	SchemaVersion       string                   `json:"schema_version"`
	PolicyVersion       string                   `json:"policy_version"`
	ResolutionDigest    string                   `json:"resolution_digest"`
	PruningPolicyDigest string                   `json:"pruning_policy_digest"`
	InventoryDigest     string                   `json:"inventory_digest"`
	PruneManifestDigest string                   `json:"prune_manifest_digest"`
	SBOMDigest          string                   `json:"sbom_digest"`
	GeneratedAt         string                   `json:"generated_at"`
	SourceDateEpoch     int64                    `json:"source_date_epoch"`
	Platform            resolution.Platform      `json:"platform"`
	Prefix              string                   `json:"prefix"`
	Runtime             resolution.RuntimePolicy `json:"runtime"`
	Packages            []RuntimePackage         `json:"packages"`
}

// SPDXDocument is the deterministic SPDX 2.3 JSON emitted for the Homebrew
// runtime closure.
type SPDXDocument struct {
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	SPDXID            string             `json:"SPDXID"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	CreationInfo      SPDXCreationInfo   `json:"creationInfo"`
	DocumentDescribes []string           `json:"documentDescribes"`
	Packages          []SPDXPackage      `json:"packages"`
	Files             []SPDXFile         `json:"files"`
	Relationships     []SPDXRelationship `json:"relationships"`
}

type SPDXCreationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type SPDXChecksum struct {
	Algorithm     string `json:"algorithm"`
	ChecksumValue string `json:"checksumValue"`
}

type SPDXExternalRef struct {
	ReferenceCategory string `json:"referenceCategory"`
	ReferenceType     string `json:"referenceType"`
	ReferenceLocator  string `json:"referenceLocator"`
}

type SPDXPackage struct {
	Name             string            `json:"name"`
	SPDXID           string            `json:"SPDXID"`
	VersionInfo      string            `json:"versionInfo"`
	PackageFileName  string            `json:"packageFileName"`
	DownloadLocation string            `json:"downloadLocation"`
	FilesAnalyzed    bool              `json:"filesAnalyzed"`
	LicenseConcluded string            `json:"licenseConcluded"`
	LicenseDeclared  string            `json:"licenseDeclared"`
	CopyrightText    string            `json:"copyrightText"`
	Checksums        []SPDXChecksum    `json:"checksums"`
	ExternalRefs     []SPDXExternalRef `json:"externalRefs,omitempty"`
	HasFiles         []string          `json:"hasFiles,omitempty"`
}

type SPDXFile struct {
	FileName           string         `json:"fileName"`
	SPDXID             string         `json:"SPDXID"`
	FileTypes          []string       `json:"fileTypes"`
	Checksums          []SPDXChecksum `json:"checksums"`
	LicenseConcluded   string         `json:"licenseConcluded"`
	LicenseInfoInFiles []string       `json:"licenseInfoInFiles"`
	CopyrightText      string         `json:"copyrightText"`
	FileComment        string         `json:"comment,omitempty"`
}

type SPDXRelationship struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
}

// EvidenceJSON contains canonical, newline-free JSON bytes and their digests.
type EvidenceJSON struct {
	Inventory       []byte
	InventoryDigest string
	Prune           []byte
	PruneDigest     string
	RuntimeManifest []byte
	ManifestDigest  string
	SBOM            []byte
	SBOMDigest      string
}

// Result is returned only after the copied prefix passes Verify.
type Result struct {
	OutputPrefix    string
	Inventory       Inventory
	PruneManifest   PruneManifest
	RuntimeManifest RuntimeManifest
	SBOM            SPDXDocument
	Evidence        EvidenceJSON
}

// ErrorCode is a stable failure category for callers and tests.
type ErrorCode string

const (
	CodeInvalidOptions     ErrorCode = "invalid_options"
	CodeInvalidRecord      ErrorCode = "invalid_record"
	CodeUnsafeSource       ErrorCode = "unsafe_source"
	CodeUnsafeType         ErrorCode = "unsafe_type"
	CodeUnsafeMode         ErrorCode = "unsafe_mode"
	CodeUnsafeXAttr        ErrorCode = "unsafe_xattr"
	CodeUnexpectedKeg      ErrorCode = "unexpected_keg"
	CodeMissingKeg         ErrorCode = "missing_keg"
	CodeInvalidOptLink     ErrorCode = "invalid_opt_link"
	CodeUnsafeLink         ErrorCode = "unsafe_link"
	CodeDanglingLink       ErrorCode = "dangling_link"
	CodeUnattributed       ErrorCode = "unattributed_path"
	CodeSourceChanged      ErrorCode = "source_changed"
	CodeCopy               ErrorCode = "copy_failed"
	CodeOwnership          ErrorCode = "ownership_failed"
	CodeVerification       ErrorCode = "verification_failed"
	CodeUnexpectedWritable ErrorCode = "unexpected_writable_path"
	CodeEvidence           ErrorCode = "evidence_failed"
)

// Error is returned for all policy, copy, and verification failures.
type Error struct {
	Code ErrorCode
	Path string
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Path != "" {
		return fmt.Sprintf("runtime filesystem %s at %q: %v", e.Code, e.Path, e.Err)
	}
	return fmt.Sprintf("runtime filesystem %s: %v", e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func runtimeError(code ErrorCode, path, format string, args ...any) error {
	return &Error{Code: code, Path: path, Err: fmt.Errorf(format, args...)}
}

func defaultChown(path string, uid, gid int, symlink bool) error {
	if symlink {
		return os.Lchown(path, uid, gid)
	}
	return os.Chown(path, uid, gid)
}

func errorCode(err error) ErrorCode {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}
