package catalog

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

const (
	maxGitHubOwnerBytes = 39
	maxTapNameBytes     = 91
	maxFormulaNameBytes = 255
)

// TapID is the canonical wire representation of a shared formulaid.Tap. It is
// kept as a string in protocol documents while all parsing and grammar rules
// are delegated to the immutable shared identity package.
type TapID string

// FormulaID is the canonical wire representation of a shared
// formulaid.FormulaID. Bare names are accepted only by ParseFormulaID and are
// stored as fully qualified homebrew/core identities.
type FormulaID string

// ParseTapID validates and canonicalizes an owner/tap identity using the shared
// Homebrew identity package.
func ParseTapID(value string) (TapID, error) {
	tap, err := formulaid.ParseTap(value)
	if err != nil {
		return "", err
	}
	return TapID(tap.String()), nil
}

// ParseFormulaID validates a bare Formula name or a fully qualified
// owner/tap/formula identity using the shared Homebrew identity package.
func ParseFormulaID(value string) (FormulaID, error) {
	id, err := formulaid.Parse(value)
	if err != nil {
		return "", err
	}
	return FormulaID(id.String()), nil
}

// TapIDFromShared converts an already validated shared tap identity to its
// canonical protocol representation.
func TapIDFromShared(tap formulaid.Tap) TapID {
	return TapID(tap.String())
}

// FormulaIDFromShared converts an already validated shared Formula identity to
// its canonical protocol representation.
func FormulaIDFromShared(id formulaid.FormulaID) FormulaID {
	return FormulaID(id.String())
}

// Shared parses id into the immutable shared tap identity type.
func (id TapID) Shared() (formulaid.Tap, error) {
	return formulaid.ParseTap(string(id))
}

// Shared parses id into the immutable shared Formula identity type.
func (id FormulaID) Shared() (formulaid.FormulaID, error) {
	parsed, err := formulaid.Parse(string(id))
	if err != nil {
		return formulaid.FormulaID{}, err
	}
	if parsed.String() != string(id) {
		return formulaid.FormulaID{}, fmt.Errorf("Formula ID %q is not fully qualified", id)
	}
	return parsed, nil
}

// ParseFormulaIDs canonicalizes a root list and rejects duplicate canonical
// identities, including a bare core name paired with its explicit core form.
func ParseFormulaIDs(values []string) ([]FormulaID, error) {
	ids, err := formulaid.ParseRoots(values)
	if err != nil {
		return nil, err
	}
	result := make([]FormulaID, len(ids))
	for i, id := range ids {
		result[i] = FormulaID(id.String())
	}
	return result, nil
}

// Validate reports whether id is already in canonical owner/tap form.
func (id TapID) Validate() error {
	parsed, err := formulaid.ParseTap(string(id))
	if err != nil {
		return err
	}
	if parsed.String() != string(id) {
		return fmt.Errorf("tap ID %q is not canonical", id)
	}
	return nil
}

// Validate reports whether id is already fully qualified and canonical.
func (id FormulaID) Validate() error {
	parsed, err := formulaid.Parse(string(id))
	if err != nil {
		return err
	}
	if parsed.String() != string(id) {
		return fmt.Errorf("Formula ID %q is not fully qualified", id)
	}
	return nil
}

// Tap returns the owner/tap portion of a validated Formula ID.
func (id FormulaID) Tap() TapID {
	parsed, err := formulaid.Parse(string(id))
	if err != nil || parsed.String() != string(id) {
		return ""
	}
	return TapID(parsed.Tap().String())
}

// Name returns the short Formula/Cellar rack name of a validated Formula ID.
func (id FormulaID) Name() string {
	parsed, err := formulaid.Parse(string(id))
	if err != nil || parsed.String() != string(id) {
		return ""
	}
	return parsed.Name()
}

// Owner returns the GitHub owner component of a validated tap ID.
func (id TapID) Owner() string {
	parsed, err := formulaid.ParseTap(string(id))
	if err != nil || parsed.String() != string(id) {
		return ""
	}
	return parsed.Owner()
}

// Name returns the repository suffix component of a validated tap ID.
func (id TapID) Name() string {
	parsed, err := formulaid.ParseTap(string(id))
	if err != nil || parsed.String() != string(id) {
		return ""
	}
	return parsed.Name()
}

// IsCore reports whether id belongs to homebrew/core.
func (id FormulaID) IsCore() bool { return id.Tap() == TapID(formulaid.CoreTap().String()) }

// IsCore reports whether id is homebrew/core.
func (id TapID) IsCore() bool { return id == TapID(formulaid.CoreTap().String()) }

// DefaultGitHubRepository returns the only repository form supported by the V2
// public-tap contract.
func (id TapID) DefaultGitHubRepository() string {
	if id.Owner() == "" || id.Name() == "" {
		return ""
	}
	return "https://github.com/" + id.Owner() + "/homebrew-" + id.Name()
}

func (id *TapID) UnmarshalJSON(data []byte) error {
	if id == nil {
		return errors.New("nil TapID receiver")
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("tap ID must be a string: %w", err)
	}
	parsed, err := ParseTapID(value)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *FormulaID) UnmarshalJSON(data []byte) error {
	if id == nil {
		return errors.New("nil FormulaID receiver")
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("Formula ID must be a string: %w", err)
	}
	parsed, err := ParseFormulaID(value)
	if err != nil {
		return err
	}
	if string(parsed) != value {
		return fmt.Errorf("Formula ID %q is not fully qualified", value)
	}
	*id = parsed
	return nil
}

func validateFormulaName(value string) error {
	_, err := formulaid.New(formulaid.CoreTap(), value)
	return err
}

func isASCIILowerAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}
