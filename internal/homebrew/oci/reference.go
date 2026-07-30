// Package oci resolves and verifies Homebrew bottles stored as OCI images.
package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// GHCRRegistry is the canonical registry used by homebrew/core bottles.
	GHCRRegistry = "ghcr.io"
	// HomebrewCoreRepository is the repository prefix used by homebrew/core.
	HomebrewCoreRepository = "homebrew/core"

	BottleTagX8664Linux = "x86_64_linux"
	BottleTagARM64Linux = "arm64_linux"
	BottleTagAll        = "all"
)

var (
	formulaNameRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9@+._-]*$`)
	repositorySegmentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)
	ociTagRE            = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Formula is the authenticated metadata needed to resolve one current stable
// homebrew/core Formula. It is intentionally local to this package so the OCI
// subsystem does not depend on a particular metadata decoder.
type Formula struct {
	Name          string
	FullName      string
	StableVersion string
	Revision      int
	VersionScheme int
	BottleRebuild int
	License       string
	KegOnly       bool
	BottleFiles   map[string]BottleFile
}

// BottleFile is the checksum and Cellar policy from authenticated Formula
// metadata for one Homebrew bottle tag.
type BottleFile struct {
	Cellar string
	SHA256 string
}

// FormulaReference is the canonical GHCR location and immutable-by-policy
// version/revision/rebuild tag for a Formula's OCI index.
type FormulaReference struct {
	Name                string
	FullName            string
	PkgVersion          string
	Registry            string
	Repository          string
	CanonicalRepository string
	IndexTag            string
}

// ResolveFormulaReference derives the canonical homebrew/core GHCR repository
// and index tag without trusting a registry-provided name or tag.
func ResolveFormulaReference(formula Formula) (FormulaReference, error) {
	if err := validateFormulaIdentity(formula); err != nil {
		return FormulaReference{}, err
	}

	repository, err := RepositoryPath(formula.Name)
	if err != nil {
		return FormulaReference{}, err
	}
	pkgVersion, err := PkgVersion(formula.StableVersion, formula.Revision)
	if err != nil {
		return FormulaReference{}, fmt.Errorf("formula %q package version: %w", formula.Name, err)
	}
	indexTag, err := IndexTag(pkgVersion, formula.BottleRebuild)
	if err != nil {
		return FormulaReference{}, fmt.Errorf("formula %q index tag: %w", formula.Name, err)
	}

	return FormulaReference{
		Name:                formula.Name,
		FullName:            canonicalFullName(formula.Name),
		PkgVersion:          pkgVersion,
		Registry:            GHCRRegistry,
		Repository:          repository,
		CanonicalRepository: GHCRRegistry + "/" + repository,
		IndexTag:            indexTag,
	}, nil
}

// EscapeFormulaName applies Homebrew's OCI repository escaping: versioned
// Formulae replace '@' with a path separator and '+' is represented as 'x'.
func EscapeFormulaName(name string) (string, error) {
	if err := validateFormulaName(name); err != nil {
		return "", err
	}
	escaped := strings.NewReplacer("@", "/", "+", "x").Replace(name)
	if err := validateRepository(escaped); err != nil {
		return "", fmt.Errorf("escaped Formula name: %w", err)
	}
	return escaped, nil
}

// RepositoryPath returns the registry-relative homebrew/core repository path.
func RepositoryPath(name string) (string, error) {
	escaped, err := EscapeFormulaName(name)
	if err != nil {
		return "", err
	}
	repository := HomebrewCoreRepository + "/" + escaped
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	return repository, nil
}

// CanonicalRepository returns the fully qualified canonical GHCR repository.
func CanonicalRepository(name string) (string, error) {
	repository, err := RepositoryPath(name)
	if err != nil {
		return "", err
	}
	return GHCRRegistry + "/" + repository, nil
}

// PkgVersion combines a Formula version and Formula revision using Homebrew's
// PkgVersion syntax.
func PkgVersion(version string, revision int) (string, error) {
	if strings.TrimSpace(version) != version || version == "" {
		return "", errors.New("version must be non-empty and have no surrounding whitespace")
	}
	if strings.ContainsAny(version, "/\\\x00\r\n\t ") {
		return "", fmt.Errorf("unsafe version %q", version)
	}
	if revision < 0 {
		return "", fmt.Errorf("negative Formula revision %d", revision)
	}
	if revision == 0 {
		return version, nil
	}
	return version + "_" + strconv.Itoa(revision), nil
}

// IndexTag appends a bottle rebuild to a PkgVersion using Homebrew's index-tag
// form: "-N". A zero rebuild has no suffix.
func IndexTag(pkgVersion string, rebuild int) (string, error) {
	if rebuild < 0 {
		return "", fmt.Errorf("negative bottle rebuild %d", rebuild)
	}
	tag := pkgVersion
	if rebuild > 0 {
		tag += "-" + strconv.Itoa(rebuild)
	}
	if !ociTagRE.MatchString(tag) {
		return "", fmt.Errorf("invalid OCI tag %q", tag)
	}
	return tag, nil
}

// ChildTag returns the ref-name annotation for a platform-specific or all
// child manifest. Bottle rebuilds use the child form ".N".
func ChildTag(pkgVersion, bottleTag string, rebuild int) (string, error) {
	if !validBottleTag(bottleTag) {
		return "", fmt.Errorf("unsupported bottle tag %q", bottleTag)
	}
	if rebuild < 0 {
		return "", fmt.Errorf("negative bottle rebuild %d", rebuild)
	}
	tag := pkgVersion + "." + bottleTag
	if rebuild > 0 {
		tag += "." + strconv.Itoa(rebuild)
	}
	if !ociTagRE.MatchString(tag) {
		return "", fmt.Errorf("invalid OCI child tag %q", tag)
	}
	return tag, nil
}

// BottleFilename returns Homebrew's canonical GHCR bottle layer filename.
func BottleFilename(name, pkgVersion, bottleTag string, rebuild int) (string, error) {
	if err := validateFormulaName(name); err != nil {
		return "", err
	}
	if _, err := IndexTag(pkgVersion, 0); err != nil {
		return "", fmt.Errorf("package version: %w", err)
	}
	if !validBottleTag(bottleTag) {
		return "", fmt.Errorf("unsupported bottle tag %q", bottleTag)
	}
	if rebuild < 0 {
		return "", fmt.Errorf("negative bottle rebuild %d", rebuild)
	}
	filename := name + "--" + pkgVersion + "." + bottleTag + ".bottle"
	if rebuild > 0 {
		filename += "." + strconv.Itoa(rebuild)
	}
	return filename + ".tar.gz", nil
}

func validateFormulaIdentity(formula Formula) error {
	if err := validateFormulaName(formula.Name); err != nil {
		return fmt.Errorf("formula name: %w", err)
	}
	if formula.FullName != "" && formula.FullName != formula.Name && formula.FullName != canonicalFullName(formula.Name) {
		return fmt.Errorf("formula %q has non-homebrew/core identity %q", formula.Name, formula.FullName)
	}
	if formula.VersionScheme < 0 {
		return fmt.Errorf("formula %q has negative version scheme %d", formula.Name, formula.VersionScheme)
	}
	if _, err := PkgVersion(formula.StableVersion, formula.Revision); err != nil {
		return err
	}
	if formula.BottleRebuild < 0 {
		return fmt.Errorf("formula %q has negative bottle rebuild %d", formula.Name, formula.BottleRebuild)
	}
	return nil
}

func validateFormulaName(name string) error {
	if !formulaNameRE.MatchString(name) {
		return fmt.Errorf("invalid canonical Formula name %q", name)
	}
	if strings.Count(name, "@") > 1 || strings.HasSuffix(name, "@") {
		return fmt.Errorf("invalid versioned Formula name %q", name)
	}
	return nil
}

func validateBottleFile(formulaName, tag string, file BottleFile) error {
	if !validBottleTag(tag) {
		return fmt.Errorf("formula %q has unsupported bottle tag %q", formulaName, tag)
	}
	if strings.TrimSpace(file.Cellar) == "" {
		return fmt.Errorf("formula %q bottle %q has empty Cellar", formulaName, tag)
	}
	if err := validateSHA256Hex(file.SHA256); err != nil {
		return fmt.Errorf("formula %q bottle %q checksum: %w", formulaName, tag, err)
	}
	return nil
}

func validateSHA256Hex(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("expected %d lowercase hex characters", sha256.Size*2)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return err
	}
	if hex.EncodeToString(decoded) != value {
		return errors.New("checksum must use lowercase hexadecimal")
	}
	return nil
}

func validBottleTag(tag string) bool {
	return tag == BottleTagX8664Linux || tag == BottleTagARM64Linux || tag == BottleTagAll
}

func canonicalFullName(name string) string {
	return HomebrewCoreRepository + "/" + name
}
