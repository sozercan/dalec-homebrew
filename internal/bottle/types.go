// Package bottle verifies Homebrew bottle archives before they are exposed to
// the offline materializer. Verification is intentionally static: archive
// contents are parsed and hashed, but no file from the bottle is executed.
package bottle

import (
	"fmt"
	"io"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

// Limits bounds all attacker-controlled archive structures. Zero-valued
// fields are replaced with DefaultLimits values by Verify.
type Limits struct {
	MaxCompressedBytes int64
	MaxExpandedBytes   int64
	MaxFiles           int
	MaxDepth           int
	MaxFileBytes       int64
	MaxMetadataBytes   int64
	MaxTarPaddingBytes int64
	MaxPathBytes       int
	MaxLinkBytes       int
	MaxPAXRecords      int
	MaxXattrs          int
	MaxXattrBytes      int64
	MaxFormulaBytes    int64
	MaxReceiptBytes    int64
}

// DefaultLimits returns conservative limits that are large enough for normal
// Homebrew bottles while still bounding parser memory and expansion work.
func DefaultLimits() Limits {
	return Limits{
		MaxCompressedBytes: 1 << 30, // 1 GiB
		MaxExpandedBytes:   8 << 30, // 8 GiB
		MaxFiles:           250_000,
		MaxDepth:           64,
		MaxFileBytes:       4 << 30, // 4 GiB
		MaxMetadataBytes:   1 << 20, // 1 MiB per entry
		MaxTarPaddingBytes: 1 << 20, // accepts normal GNU tar record padding
		MaxPathBytes:       4 << 10,
		MaxLinkBytes:       4 << 10,
		MaxPAXRecords:      128,
		MaxXattrs:          64,
		MaxXattrBytes:      1 << 20,
		MaxFormulaBytes:    4 << 20,
		MaxReceiptBytes:    4 << 20,
	}
}

// Policy contains verification decisions that may vary by release policy.
type Policy struct {
	// RequirePreInstallReceipt rejects bottles that do not contain
	// INSTALL_RECEIPT.json. When a receipt is present it is always validated,
	// regardless of this flag.
	RequirePreInstallReceipt bool
}

// Options configures Verify.
type Options struct {
	Limits Limits
	Policy Policy
}

// Expectation contains authenticated facts established by resolution. The two
// SHA-256 values intentionally remain separate: CompressedSHA256 is the exact
// OCI layer digest, while HomebrewSHA256 comes from authenticated Homebrew
// metadata. Both are checked against the fetched bytes.
type Expectation struct {
	Name             string
	FullName         string
	FormulaVersion   string
	FormulaRevision  int
	PkgVersion       string
	VersionScheme    int
	BottleRebuild    int
	BottleTag        string
	CompressedSHA256 string
	CompressedSize   int64
	HomebrewSHA256   string
	HomebrewVersion  string
	Arch             string
	Compiler         string
	ExpectedTap      string
	Dependencies     []ReceiptDependency
	FormulaIdentity  string
}

// ReceiptDependency is the subset of bottle receipt dependency identity that
// is authenticated by the resolution record.
type ReceiptDependency struct {
	FullName         string `json:"full_name"`
	Version          string `json:"version"`
	Revision         int    `json:"revision"`
	BottleRebuild    int    `json:"bottle_rebuild"`
	PkgVersion       string `json:"pkg_version"`
	DeclaredDirectly bool   `json:"declared_directly,omitempty"`
}

// ExpectationFromNode projects the authenticated bottle facts from a
// resolution Node into the independent archive verifier's input.
func ExpectationFromNode(node resolution.Node) Expectation {
	deps := make([]ReceiptDependency, 0, len(node.Bottle.Tab.Dependencies))
	for _, dep := range node.Bottle.Tab.Dependencies {
		deps = append(deps, ReceiptDependency{
			FullName:         dep.FullName,
			Version:          dep.Version,
			Revision:         dep.Revision,
			BottleRebuild:    dep.BottleRebuild,
			PkgVersion:       dep.PkgVersion,
			DeclaredDirectly: dep.DeclaredDirectly,
		})
	}
	return Expectation{
		Name:             node.Name,
		FullName:         node.FullName,
		FormulaVersion:   node.FormulaVersion,
		FormulaRevision:  node.FormulaRevision,
		PkgVersion:       node.PkgVersion,
		VersionScheme:    node.VersionScheme,
		BottleRebuild:    node.BottleRebuild,
		BottleTag:        node.Bottle.Tag,
		CompressedSHA256: node.Bottle.Layer.Digest,
		CompressedSize:   node.Bottle.Layer.Size,
		HomebrewSHA256:   node.Bottle.HomebrewSHA256,
		HomebrewVersion:  node.Bottle.Tab.HomebrewVersion,
		Arch:             node.Bottle.Tab.Arch,
		Compiler:         node.Bottle.Tab.Compiler,
		ExpectedTap:      tapFromFullName(node.FullName),
		Dependencies:     deps,
		FormulaIdentity:  node.UpstreamFormulaID,
	}
}

// VerifyNode verifies a fetched bottle against a resolution Node.
func VerifyNode(r io.Reader, node resolution.Node, opts Options) (*Result, error) {
	return Verify(r, ExpectationFromNode(node), opts)
}

// EntryType is a materializer-safe archive entry classification.
type EntryType string

const (
	EntryRegular   EntryType = "regular"
	EntryDirectory EntryType = "directory"
	EntrySymlink   EntryType = "symlink"
	EntryHardlink  EntryType = "hardlink"
)

// Xattr is an allowed, non-security extended attribute. The slice on an
// InventoryEntry is sorted by Name.
type Xattr struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// InventoryEntry is a deterministic description of one safe archive member.
// Path and HardlinkTarget are archive-root-relative. SymlinkTarget preserves
// the link text that the materializer must create; ResolvedTarget records the
// containment-checked archive path.
type InventoryEntry struct {
	Path           string    `json:"path"`
	KegPath        string    `json:"keg_path"`
	Type           EntryType `json:"type"`
	Mode           uint32    `json:"mode"`
	Size           int64     `json:"size,omitempty"`
	SHA256         string    `json:"sha256,omitempty"`
	SymlinkTarget  string    `json:"symlink_target,omitempty"`
	HardlinkTarget string    `json:"hardlink_target,omitempty"`
	ResolvedTarget string    `json:"resolved_target,omitempty"`
	UID            int       `json:"uid"`
	GID            int       `json:"gid"`
	Xattrs         []Xattr   `json:"xattrs,omitempty"`
	Relocatable    bool      `json:"relocatable,omitempty"`
}

// FormulaEvidence identifies the embedded Formula source without evaluating
// untrusted Ruby.
type FormulaEvidence struct {
	Path      string `json:"path"`
	ClassName string `json:"class_name"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// ReceiptEvidence summarizes a validated pre-install receipt.
type ReceiptEvidence struct {
	Path             string `json:"path"`
	SHA256           string `json:"sha256"`
	Size             int64  `json:"size"`
	FormulaVersion   string `json:"formula_version"`
	VersionScheme    int    `json:"version_scheme"`
	BuiltAsBottle    bool   `json:"built_as_bottle"`
	PouredFromBottle bool   `json:"poured_from_bottle,omitempty"`
	HomebrewVersion  string `json:"homebrew_version,omitempty"`
	Arch             string `json:"arch,omitempty"`
	RuntimeDepCount  int    `json:"runtime_dependency_count"`
}

// Result is the verified, deterministic input to later materialization.
type Result struct {
	Name             string           `json:"name"`
	PkgVersion       string           `json:"pkg_version"`
	KegPrefix        string           `json:"keg_prefix"`
	CompressedSHA256 string           `json:"compressed_sha256"`
	CompressedSize   int64            `json:"compressed_size"`
	HomebrewSHA256   string           `json:"homebrew_sha256"`
	ExpandedSize     int64            `json:"expanded_size"`
	InventorySHA256  string           `json:"inventory_sha256"`
	Inventory        []InventoryEntry `json:"inventory"`
	Formula          FormulaEvidence  `json:"formula"`
	Receipt          *ReceiptEvidence `json:"receipt,omitempty"`
	// FormulaSource is the exact embedded Formula payload captured by the
	// verifier after the complete compressed object and archive have passed all
	// checks. It is transient materializer input and must never be serialized as
	// evidence.
	FormulaSource []byte `json:"-"`
}

// ErrorCode provides stable failure categories for callers and tests.
type ErrorCode string

const (
	CodeInvalidExpectation ErrorCode = "invalid_expectation"
	CodeCompressedLimit    ErrorCode = "compressed_limit"
	CodeSizeMismatch       ErrorCode = "size_mismatch"
	CodeDigestMismatch     ErrorCode = "digest_mismatch"
	CodeHomebrewMismatch   ErrorCode = "homebrew_checksum_mismatch"
	CodeInvalidGzip        ErrorCode = "invalid_gzip"
	CodeInvalidTar         ErrorCode = "invalid_tar"
	CodeArchiveLimit       ErrorCode = "archive_limit"
	CodeUnsafePath         ErrorCode = "unsafe_path"
	CodePathCollision      ErrorCode = "path_collision"
	CodeUnsafeType         ErrorCode = "unsafe_type"
	CodeUnsafeMode         ErrorCode = "unsafe_mode"
	CodeUnsafeLink         ErrorCode = "unsafe_link"
	CodeUnsafeMetadata     ErrorCode = "unsafe_metadata"
	CodeMissingFormula     ErrorCode = "missing_formula"
	CodeInvalidFormula     ErrorCode = "invalid_formula"
	CodeMissingReceipt     ErrorCode = "missing_receipt"
	CodeInvalidReceipt     ErrorCode = "invalid_receipt"
)

// VerificationError is returned for all policy and archive validation errors.
type VerificationError struct {
	Code ErrorCode
	Path string
	Err  error
}

func (e *VerificationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Path != "" {
		return fmt.Sprintf("bottle verification %s at %q: %v", e.Code, e.Path, e.Err)
	}
	return fmt.Sprintf("bottle verification %s: %v", e.Code, e.Err)
}

func (e *VerificationError) Unwrap() error { return e.Err }

func verificationError(code ErrorCode, path string, format string, args ...any) error {
	return &VerificationError{Code: code, Path: path, Err: fmt.Errorf(format, args...)}
}
