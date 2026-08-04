package oci

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

const maxFormulaSourcePathBytes = 1024

// AuthenticatedFormulaIdentity binds a canonical Formula ID to the exact
// default GitHub tap repository, bottle-source commit, Formula path, and
// Homebrew full name authenticated for a V2 OCI artifact. Values are immutable
// and can only be constructed through NewAuthenticatedFormulaIdentity.
type AuthenticatedFormulaIdentity struct {
	id               formulaid.FormulaID
	homebrewFullName string
	sourceRepository string
	sourceCommit     string
	formulaPath      string
}

// NewAuthenticatedFormulaIdentity validates and constructs a tap-aware
// Formula identity. V2 public taps are restricted to default GitHub
// repositories named owner/homebrew-tap and exact 40-character source commits.
func NewAuthenticatedFormulaIdentity(id formulaid.FormulaID, homebrewFullName, sourceRepository, sourceCommit, formulaPath string) (AuthenticatedFormulaIdentity, error) {
	identity := AuthenticatedFormulaIdentity{
		id:               id,
		homebrewFullName: homebrewFullName,
		sourceRepository: sourceRepository,
		sourceCommit:     sourceCommit,
		formulaPath:      formulaPath,
	}
	if err := identity.validate(); err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	return identity, nil
}

// ID returns the canonical owner/tap/formula identity.
func (identity AuthenticatedFormulaIdentity) ID() formulaid.FormulaID {
	return identity.id
}

// Tap returns the canonical owner/tap identity.
func (identity AuthenticatedFormulaIdentity) Tap() formulaid.Tap {
	return identity.id.Tap()
}

// Name returns the short Formula/Cellar rack name.
func (identity AuthenticatedFormulaIdentity) Name() string {
	return identity.id.Name()
}

// HomebrewFullName returns the exact authenticated Homebrew receipt and OCI
// package identity.
func (identity AuthenticatedFormulaIdentity) HomebrewFullName() string {
	return identity.homebrewFullName
}

// Publisher returns the GitHub/GHCR publisher derived from the authenticated
// tap owner.
func (identity AuthenticatedFormulaIdentity) Publisher() string {
	return identity.id.Tap().Owner()
}

// SourceRepository returns the authenticated default GitHub tap repository.
func (identity AuthenticatedFormulaIdentity) SourceRepository() string {
	return identity.sourceRepository
}

// SourceCommit returns the exact authenticated bottle-source commit.
func (identity AuthenticatedFormulaIdentity) SourceCommit() string {
	return identity.sourceCommit
}

// FormulaPath returns the authenticated bottle-source Formula path.
func (identity AuthenticatedFormulaIdentity) FormulaPath() string {
	return identity.formulaPath
}

// SourceURL returns the exact OCI source annotation expected for the Formula.
func (identity AuthenticatedFormulaIdentity) SourceURL() string {
	if identity.sourceRepository == "" || identity.sourceCommit == "" || identity.formulaPath == "" {
		return ""
	}
	return identity.sourceRepository + "/blob/" + identity.sourceCommit + "/" + identity.formulaPath
}

func (identity AuthenticatedFormulaIdentity) validate() error {
	if err := validateFormulaID(identity.id); err != nil {
		return fmt.Errorf("Formula ID: %w", err)
	}
	if identity.homebrewFullName != identity.id.String() {
		return fmt.Errorf("Homebrew full name %q does not match Formula ID %q", identity.homebrewFullName, identity.id.String())
	}
	if _, err := RepositoryPathForFormulaID(identity.id); err != nil {
		return fmt.Errorf("GHCR repository: %w", err)
	}
	expectedRepository, err := DefaultGitHubRepository(identity.id.Tap())
	if err != nil {
		return err
	}
	if identity.sourceRepository != expectedRepository {
		return fmt.Errorf("source repository %q does not match default tap repository %q", identity.sourceRepository, expectedRepository)
	}
	if err := validateSourceCommit(identity.sourceCommit); err != nil {
		return fmt.Errorf("source commit: %w", err)
	}
	if err := validateFormulaSourcePath(identity.formulaPath, identity.id.Name()); err != nil {
		return fmt.Errorf("Formula path: %w", err)
	}
	return nil
}

// AuthenticatedFormula is the catalog-authenticated metadata needed to
// resolve one V2 Formula whose bottle root uses Homebrew's GHCR layout.
type AuthenticatedFormula struct {
	Identity      AuthenticatedFormulaIdentity
	BottleRootURL string
	StableVersion string
	Revision      int
	VersionScheme int
	BottleRebuild int
	License       string
	KegOnly       bool
	BottleFiles   map[string]BottleFile
}

// DefaultGitHubRepository returns the only GitHub source repository supported
// for a public V2 tap.
func DefaultGitHubRepository(tap formulaid.Tap) (string, error) {
	if err := validateTap(tap); err != nil {
		return "", err
	}
	return "https://github.com/" + tap.Owner() + "/homebrew-" + tap.Name(), nil
}

// HomebrewGHCRRootURL returns Homebrew's canonical GHCR bottle root for a tap.
// The homebrew- source repository prefix is deliberately omitted in GHCR.
func HomebrewGHCRRootURL(tap formulaid.Tap) (string, error) {
	if err := validateTap(tap); err != nil {
		return "", err
	}
	return "https://" + GHCRRegistry + "/v2/" + tap.Owner() + "/" + tap.Name(), nil
}

// MatchHomebrewGHCRRoot reports whether rootURL is the exact canonical GHCR
// root for tap. Non-GHCR URLs return false so callers can route them to the
// bounded HTTPS fetcher. A URL that targets GHCR but does not exactly match the
// authenticated tap fails closed instead of falling back to generic HTTPS.
func MatchHomebrewGHCRRoot(rootURL string, tap formulaid.Tap) (bool, error) {
	expected, err := HomebrewGHCRRootURL(tap)
	if err != nil {
		return false, err
	}
	parsed, err := url.Parse(rootURL)
	if err != nil {
		return false, fmt.Errorf("parse bottle root URL: %w", err)
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname != GHCRRegistry {
		return false, nil
	}
	if rootURL != expected {
		return false, fmt.Errorf("GHCR bottle root %q does not match authenticated tap root %q", rootURL, expected)
	}
	return true, nil
}

func validateFormulaID(id formulaid.FormulaID) error {
	if id.String() == "" {
		return errors.New("identity is empty")
	}
	parsed, err := formulaid.Parse(id.String())
	if err != nil {
		return err
	}
	if parsed != id {
		return errors.New("identity is not canonical")
	}
	return nil
}

func validateTap(tap formulaid.Tap) error {
	if tap.String() == "" {
		return errors.New("tap identity is empty")
	}
	parsed, err := formulaid.ParseTap(tap.String())
	if err != nil {
		return err
	}
	if parsed != tap {
		return errors.New("tap identity is not canonical")
	}
	return nil
}

func validateSourceCommit(commit string) error {
	if len(commit) != 40 {
		return errors.New("expected 40 lowercase hexadecimal characters")
	}
	for i := range len(commit) {
		value := commit[i]
		if !(value >= '0' && value <= '9' || value >= 'a' && value <= 'f') {
			return errors.New("expected 40 lowercase hexadecimal characters")
		}
	}
	return nil
}

func validateFormulaSourcePath(value, formulaName string) error {
	if value == "" || len(value) > maxFormulaSourcePathBytes || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return errors.New("must be a bounded relative slash-separated path")
	}
	if path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("path %q is not clean and contained", value)
	}
	if path.Base(value) != formulaName+".rb" {
		return fmt.Errorf("path basename %q does not match Formula %q", path.Base(value), formulaName)
	}
	for i := range len(value) {
		character := value[i]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("/@+._-", rune(character)) {
			continue
		}
		return fmt.Errorf("path %q contains unsupported character %q", value, character)
	}
	return nil
}
