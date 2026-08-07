package prebuilt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// EvidenceSchemaVersion identifies the stable evidence shape emitted by
	// Derive.
	EvidenceSchemaVersion = "dalec-homebrew-prebuilt-evidence/v1"

	// DerivationPolicyVersion identifies the deterministic bottle construction
	// implemented by this package.
	DerivationPolicyVersion = "homebrew-receiptless-derived-bottle-v1"
)

var (
	safeFormulaName = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.@-]*$`)
	safePolicyToken = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)
)

// Limits bounds all attacker-controlled archive structures and derived output.
// Zero fields are replaced with DefaultLimits values.
type Limits struct {
	MaxCompressedBytes int64 `json:"max_compressed_bytes"`
	MaxExpandedBytes   int64 `json:"max_expanded_bytes"`
	MaxExpansionRatio  int64 `json:"max_expansion_ratio"`
	MaxEntries         int   `json:"max_entries"`
	MaxFileBytes       int64 `json:"max_file_bytes"`
	MaxPathBytes       int   `json:"max_path_bytes"`
	MaxDepth           int   `json:"max_depth"`
	MaxTarPaddingBytes int64 `json:"max_tar_padding_bytes"`
	MaxFormulaBytes    int64 `json:"max_formula_bytes"`
	MaxBottleBytes     int64 `json:"max_bottle_bytes"`
}

// DefaultLimits returns conservative limits for small, self-contained release
// archives. Release policy may tighten them per Formula.
func DefaultLimits() Limits {
	return Limits{
		MaxCompressedBytes: 64 << 20,
		MaxExpandedBytes:   128 << 20,
		MaxExpansionRatio:  16,
		MaxEntries:         32,
		MaxFileBytes:       64 << 20,
		MaxPathBytes:       255,
		MaxDepth:           8,
		MaxTarPaddingBytes: 1 << 20,
		MaxFormulaBytes:    1 << 20,
		MaxBottleBytes:     128 << 20,
	}
}

// Target is the authenticated platform for the selected prebuilt artifact.
// The initial implementation intentionally supports only Linux ELF targets.
type Target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// SourceExpectation binds the exact compressed release archive.
type SourceExpectation struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// EntryProfile declares one exact regular file accepted from the source
// archive. Mode contains only Unix permission bits and is compared exactly.
type EntryProfile struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

// GoBuildProfile binds the Go module identity and CGO setting. GOOS and GOARCH
// are required to equal Profile.Target.
type GoBuildProfile struct {
	ModulePath string `json:"module_path"`
	CGOEnabled bool   `json:"cgo_enabled"`
}

// Profile is the complete caller-provided policy for one archive derivation.
// It is normalized and canonically hashed before any archive parsing.
type Profile struct {
	PolicyVersion   string            `json:"policy_version"`
	Name            string            `json:"name"`
	PkgVersion      string            `json:"pkg_version"`
	Target          Target            `json:"target"`
	Source          SourceExpectation `json:"source"`
	FormulaSHA256   string            `json:"formula_sha256"`
	Entries         []EntryProfile    `json:"entries"`
	PayloadPath     string            `json:"payload_path"`
	GoBuild         GoBuildProfile    `json:"go_build"`
	SourceDateEpoch int64             `json:"source_date_epoch"`
	Limits          Limits            `json:"limits"`
}

// InventoryEntry is a canonical description of one verified regular file.
type InventoryEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SourceEvidence records verification of the original compressed archive and
// its exact regular-file inventory.
type SourceEvidence struct {
	SHA256          string           `json:"sha256"`
	Size            int64            `json:"size"`
	ExpandedSHA256  string           `json:"expanded_sha256"`
	ExpandedSize    int64            `json:"expanded_size"`
	InventorySHA256 string           `json:"inventory_sha256"`
	Inventory       []InventoryEntry `json:"inventory"`
	PayloadPath     string           `json:"payload_path"`
	PayloadSHA256   string           `json:"payload_sha256"`
	PayloadSize     int64            `json:"payload_size"`
}

// FormulaEvidence binds the exact Formula source embedded in the derived
// bottle. The Formula is never evaluated by this package.
type FormulaEvidence struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ELFEvidence summarizes static inspection of the selected payload.
type ELFEvidence struct {
	Class                      string   `json:"class"`
	Data                       string   `json:"data"`
	Type                       string   `json:"type"`
	Machine                    string   `json:"machine"`
	Entry                      uint64   `json:"entry"`
	ProgramHeaderCount         int      `json:"program_header_count"`
	Interpreter                string   `json:"interpreter"`
	ImportedLibraries          []string `json:"imported_libraries"`
	WritableExecutableSegments int      `json:"writable_executable_segments"`
}

// GoBuildEvidence records the authenticated build settings extracted from the
// payload's Go build information.
type GoBuildEvidence struct {
	GoVersion     string `json:"go_version"`
	MainPackage   string `json:"main_package"`
	ModulePath    string `json:"module_path"`
	ModuleVersion string `json:"module_version"`
	ModuleSum     string `json:"module_sum"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	CGOEnabled    bool   `json:"cgo_enabled"`
}

// DerivationEvidence binds the canonical receiptless bottle output.
type DerivationEvidence struct {
	PolicyVersion   string           `json:"policy_version"`
	Receiptless     bool             `json:"receiptless"`
	KegPrefix       string           `json:"keg_prefix"`
	FormulaPath     string           `json:"formula_path"`
	ExecutablePath  string           `json:"executable_path"`
	SourceDateEpoch int64            `json:"source_date_epoch"`
	Compression     string           `json:"compression"`
	SHA256          string           `json:"sha256"`
	Size            int64            `json:"size"`
	InventorySHA256 string           `json:"inventory_sha256"`
	Inventory       []InventoryEntry `json:"inventory"`
}

// Evidence is the complete stable result of source, ELF, Go-build, and bottle
// derivation verification.
type Evidence struct {
	SchemaVersion string             `json:"schema_version"`
	PolicyVersion string             `json:"policy_version"`
	ProfileSHA256 string             `json:"profile_sha256"`
	Source        SourceEvidence     `json:"source"`
	Formula       FormulaEvidence    `json:"formula"`
	ELF           ELFEvidence        `json:"elf"`
	GoBuild       GoBuildEvidence    `json:"go_build"`
	Derivation    DerivationEvidence `json:"derivation"`
}

// Result contains the deterministic derived bottle and its verification
// evidence. Bottle is a complete tar+gzip object suitable for read-only
// transport to the materializer.
type Result struct {
	Bottle   []byte
	Evidence Evidence
}

// ErrorCode provides stable failure categories to integration callers.
type ErrorCode string

const (
	CodeInvalidProfile  ErrorCode = "invalid_profile"
	CodeSourceLimit     ErrorCode = "source_limit"
	CodeSourceSize      ErrorCode = "source_size_mismatch"
	CodeSourceDigest    ErrorCode = "source_digest_mismatch"
	CodeInvalidGzip     ErrorCode = "invalid_gzip"
	CodeInvalidTar      ErrorCode = "invalid_tar"
	CodeArchiveLimit    ErrorCode = "archive_limit"
	CodeUnsafePath      ErrorCode = "unsafe_path"
	CodeUnsafeType      ErrorCode = "unsafe_type"
	CodeUnsafeMetadata  ErrorCode = "unsafe_metadata"
	CodeDuplicatePath   ErrorCode = "duplicate_path"
	CodeEntryMismatch   ErrorCode = "entry_mismatch"
	CodeFormulaMismatch ErrorCode = "formula_mismatch"
	CodeInvalidELF      ErrorCode = "invalid_elf"
	CodeGoBuildMismatch ErrorCode = "go_build_mismatch"
	CodeDerivation      ErrorCode = "derivation_failed"
)

// VerificationError is returned for policy, archive, executable, and
// derivation failures.
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
		return fmt.Sprintf("prebuilt verification %s at %q: %v", e.Code, e.Path, e.Err)
	}
	return fmt.Sprintf("prebuilt verification %s: %v", e.Code, e.Err)
}

// Unwrap exposes the underlying parser or validation error.
func (e *VerificationError) Unwrap() error { return e.Err }

func verificationError(code ErrorCode, pathValue, format string, args ...any) error {
	return &VerificationError{Code: code, Path: pathValue, Err: fmt.Errorf(format, args...)}
}

// CanonicalProfile validates, defaults, and deterministically serializes a
// profile. Entry order does not affect the returned bytes.
func CanonicalProfile(profile Profile) ([]byte, error) {
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// CanonicalInventory validates and deterministically serializes an inventory.
// Input order does not affect the returned bytes.
func CanonicalInventory(entries []InventoryEntry) ([]byte, error) {
	canonical := append([]InventoryEntry(nil), entries...)
	slices.SortFunc(canonical, func(a, b InventoryEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	for i := range canonical {
		entry := canonical[i]
		if err := validatePortablePath(entry.Path, math.MaxInt, math.MaxInt); err != nil {
			return nil, fmt.Errorf("inventory path %q: %w", entry.Path, err)
		}
		if entry.Mode == 0 || entry.Mode&^0o777 != 0 {
			return nil, fmt.Errorf("inventory path %q has invalid mode %#o", entry.Path, entry.Mode)
		}
		if entry.Size < 0 {
			return nil, fmt.Errorf("inventory path %q has negative size", entry.Path)
		}
		if _, err := normalizeSHA256(entry.SHA256); err != nil {
			return nil, fmt.Errorf("inventory path %q digest: %w", entry.Path, err)
		}
		if i > 0 && canonical[i-1].Path == entry.Path {
			return nil, fmt.Errorf("duplicate inventory path %q", entry.Path)
		}
	}
	return json.Marshal(canonical)
}

// CanonicalEvidence deterministically serializes evidence after canonicalizing
// both inventories. It rejects malformed or duplicate inventory entries.
func CanonicalEvidence(evidence Evidence) ([]byte, error) {
	if evidence.SchemaVersion != EvidenceSchemaVersion {
		return nil, fmt.Errorf("unsupported evidence schema %q", evidence.SchemaVersion)
	}
	sourceBytes, err := CanonicalInventory(evidence.Source.Inventory)
	if err != nil {
		return nil, fmt.Errorf("source inventory: %w", err)
	}
	if err := json.Unmarshal(sourceBytes, &evidence.Source.Inventory); err != nil {
		return nil, err
	}
	derivedBytes, err := CanonicalInventory(evidence.Derivation.Inventory)
	if err != nil {
		return nil, fmt.Errorf("derivation inventory: %w", err)
	}
	if err := json.Unmarshal(derivedBytes, &evidence.Derivation.Inventory); err != nil {
		return nil, err
	}
	return json.Marshal(evidence)
}

func normalizeProfile(profile Profile) (Profile, error) {
	profile.Limits = profile.Limits.withDefaults()
	if err := validateLimits(profile.Limits); err != nil {
		return Profile{}, verificationError(CodeInvalidProfile, "", "invalid limits: %v", err)
	}
	if len(profile.PolicyVersion) == 0 || len(profile.PolicyVersion) > 128 || !safePolicyToken.MatchString(profile.PolicyVersion) {
		return Profile{}, verificationError(CodeInvalidProfile, "", "invalid policy version %q", profile.PolicyVersion)
	}
	if len(profile.Name) > 128 || !safeFormulaName.MatchString(profile.Name) {
		return Profile{}, verificationError(CodeInvalidProfile, "", "invalid Formula name %q", profile.Name)
	}
	if err := validateComponent("package version", profile.PkgVersion, 128); err != nil {
		return Profile{}, verificationError(CodeInvalidProfile, "", "%v", err)
	}
	if profile.Target.OS != "linux" {
		return Profile{}, verificationError(CodeInvalidProfile, "", "unsupported target OS %q", profile.Target.OS)
	}
	if profile.Target.Arch != "amd64" && profile.Target.Arch != "arm64" {
		return Profile{}, verificationError(CodeInvalidProfile, "", "unsupported target architecture %q", profile.Target.Arch)
	}
	if profile.Source.Size <= 0 || profile.Source.Size > profile.Limits.MaxCompressedBytes {
		return Profile{}, verificationError(CodeInvalidProfile, "", "source size must be between 1 and %d bytes", profile.Limits.MaxCompressedBytes)
	}
	sourceDigest, err := normalizeSHA256(profile.Source.SHA256)
	if err != nil {
		return Profile{}, verificationError(CodeInvalidProfile, "", "source SHA-256: %v", err)
	}
	profile.Source.SHA256 = sourceDigest
	formulaDigest, err := normalizeSHA256(profile.FormulaSHA256)
	if err != nil {
		return Profile{}, verificationError(CodeInvalidProfile, "", "Formula SHA-256: %v", err)
	}
	profile.FormulaSHA256 = formulaDigest
	if profile.SourceDateEpoch < 0 || profile.SourceDateEpoch > math.MaxUint32 {
		return Profile{}, verificationError(CodeInvalidProfile, "", "source_date_epoch must fit an unsigned 32-bit Unix timestamp")
	}
	if len(profile.Entries) == 0 || len(profile.Entries) > profile.Limits.MaxEntries {
		return Profile{}, verificationError(CodeInvalidProfile, "", "entry profile count must be between 1 and %d", profile.Limits.MaxEntries)
	}
	profile.Entries = append([]EntryProfile(nil), profile.Entries...)
	slices.SortFunc(profile.Entries, func(a, b EntryProfile) int {
		return strings.Compare(a.Path, b.Path)
	})
	folded := make(map[string]string, len(profile.Entries))
	for i, entry := range profile.Entries {
		if err := validatePortablePath(entry.Path, profile.Limits.MaxPathBytes, profile.Limits.MaxDepth); err != nil {
			return Profile{}, verificationError(CodeInvalidProfile, entry.Path, "invalid entry path: %v", err)
		}
		if entry.Mode == 0 || entry.Mode&^0o777 != 0 {
			return Profile{}, verificationError(CodeInvalidProfile, entry.Path, "entry mode %#o must contain only non-zero permission bits", entry.Mode)
		}
		if i > 0 && profile.Entries[i-1].Path == entry.Path {
			return Profile{}, verificationError(CodeInvalidProfile, entry.Path, "duplicate entry profile")
		}
		fold := strings.ToLower(entry.Path)
		if previous, ok := folded[fold]; ok && previous != entry.Path {
			return Profile{}, verificationError(CodeInvalidProfile, entry.Path, "case-folds to entry %q", previous)
		}
		folded[fold] = entry.Path
	}
	if err := validatePortablePath(profile.PayloadPath, profile.Limits.MaxPathBytes, profile.Limits.MaxDepth); err != nil {
		return Profile{}, verificationError(CodeInvalidProfile, profile.PayloadPath, "invalid payload path: %v", err)
	}
	payloadFound := false
	for _, entry := range profile.Entries {
		if entry.Path == profile.PayloadPath {
			payloadFound = true
			if entry.Mode&0o111 == 0 {
				return Profile{}, verificationError(CodeInvalidProfile, entry.Path, "payload mode %#o is not executable", entry.Mode)
			}
			break
		}
	}
	if !payloadFound {
		return Profile{}, verificationError(CodeInvalidProfile, profile.PayloadPath, "payload path is not declared in entries")
	}
	if err := validateModulePath(profile.GoBuild.ModulePath); err != nil {
		return Profile{}, verificationError(CodeInvalidProfile, "", "Go module path: %v", err)
	}
	kegPrefix := profile.Name + "/" + profile.PkgVersion
	for _, derivedPath := range []string{
		kegPrefix + "/.brew/" + profile.Name + ".rb",
		kegPrefix + "/bin/" + profile.Name,
	} {
		if err := validatePortablePath(derivedPath, 255, 8); err != nil {
			return Profile{}, verificationError(CodeInvalidProfile, derivedPath, "derived bottle path is not USTAR-safe: %v", err)
		}
	}
	return profile, nil
}

func validateLimits(limits Limits) error {
	for label, value := range map[string]int64{
		"MaxCompressedBytes": limits.MaxCompressedBytes,
		"MaxExpandedBytes":   limits.MaxExpandedBytes,
		"MaxExpansionRatio":  limits.MaxExpansionRatio,
		"MaxFileBytes":       limits.MaxFileBytes,
		"MaxTarPaddingBytes": limits.MaxTarPaddingBytes,
		"MaxFormulaBytes":    limits.MaxFormulaBytes,
		"MaxBottleBytes":     limits.MaxBottleBytes,
	} {
		if value <= 0 || value == math.MaxInt64 {
			return fmt.Errorf("%s must be positive and less than MaxInt64", label)
		}
	}
	for label, value := range map[string]int{
		"MaxEntries":   limits.MaxEntries,
		"MaxPathBytes": limits.MaxPathBytes,
		"MaxDepth":     limits.MaxDepth,
	} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", label)
		}
	}
	if limits.MaxTarPaddingBytes < 1024 {
		return errors.New("MaxTarPaddingBytes must permit the two 512-byte tar terminator blocks")
	}
	if limits.MaxFileBytes > limits.MaxExpandedBytes {
		return errors.New("MaxFileBytes must not exceed MaxExpandedBytes")
	}
	return nil
}

func (limits Limits) withDefaults() Limits {
	defaults := DefaultLimits()
	if limits.MaxCompressedBytes == 0 {
		limits.MaxCompressedBytes = defaults.MaxCompressedBytes
	}
	if limits.MaxExpandedBytes == 0 {
		limits.MaxExpandedBytes = defaults.MaxExpandedBytes
	}
	if limits.MaxExpansionRatio == 0 {
		limits.MaxExpansionRatio = defaults.MaxExpansionRatio
	}
	if limits.MaxEntries == 0 {
		limits.MaxEntries = defaults.MaxEntries
	}
	if limits.MaxFileBytes == 0 {
		limits.MaxFileBytes = defaults.MaxFileBytes
	}
	if limits.MaxPathBytes == 0 {
		limits.MaxPathBytes = defaults.MaxPathBytes
	}
	if limits.MaxDepth == 0 {
		limits.MaxDepth = defaults.MaxDepth
	}
	if limits.MaxTarPaddingBytes == 0 {
		limits.MaxTarPaddingBytes = defaults.MaxTarPaddingBytes
	}
	if limits.MaxFormulaBytes == 0 {
		limits.MaxFormulaBytes = defaults.MaxFormulaBytes
	}
	if limits.MaxBottleBytes == 0 {
		limits.MaxBottleBytes = defaults.MaxBottleBytes
	}
	return limits
}

func validatePortablePath(value string, maxBytes, maxDepth int) error {
	if value == "" || len(value) > maxBytes {
		return fmt.Errorf("path must be between 1 and %d bytes", maxBytes)
	}
	if !utf8.ValidString(value) {
		return errors.New("path is not valid UTF-8")
	}
	if strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || path.IsAbs(value) {
		return errors.New("absolute paths and backslashes are forbidden")
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || unicode.IsControl(r) {
			return errors.New("path must contain only printable ASCII without spaces")
		}
	}
	parts := strings.Split(value, "/")
	if len(parts) > maxDepth {
		return fmt.Errorf("path depth %d exceeds %d", len(parts), maxDepth)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("empty, dot, and traversal components are forbidden")
		}
	}
	if clean := path.Clean(value); clean != value || clean == "." || strings.HasPrefix(clean, "../") {
		return errors.New("path is not canonical")
	}
	return nil
}

func validateComponent(label, value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || value == "." || value == ".." {
		return fmt.Errorf("invalid %s %q", label, value)
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || unicode.IsControl(r) || r == '/' || r == '\\' {
			return fmt.Errorf("invalid %s %q", label, value)
		}
	}
	return nil
}

func validateModulePath(value string) error {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "\\") {
		return fmt.Errorf("invalid module path %q", value)
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid module path %q", value)
		}
		for _, r := range part {
			if r > unicode.MaxASCII || unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune(`:@`, r) {
				return fmt.Errorf("invalid module path %q", value)
			}
		}
	}
	return nil
}

func normalizeSHA256(value string) (string, error) {
	if !strings.HasPrefix(value, "sha256:") {
		return "", errors.New("digest must use sha256:<hex> form")
	}
	hexValue := strings.TrimPrefix(value, "sha256:")
	if len(hexValue) != sha256.Size*2 || hexValue != strings.ToLower(hexValue) {
		return "", errors.New("digest must contain 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(hexValue)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.New("digest must contain 64 lowercase hexadecimal characters")
	}
	return "sha256:" + hexValue, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestInventory(entries []InventoryEntry) (string, error) {
	data, err := CanonicalInventory(entries)
	if err != nil {
		return "", err
	}
	return digestBytes(data), nil
}

func exceedsRatio(expanded, compressed, ratio int64) bool {
	if compressed <= 0 || ratio <= 0 {
		return true
	}
	if compressed > math.MaxInt64/ratio {
		return false
	}
	return expanded > compressed*ratio
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("nil reader")
	}
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, io.ErrShortBuffer
	}
	return data, nil
}
