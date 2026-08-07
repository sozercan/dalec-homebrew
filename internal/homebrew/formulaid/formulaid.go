// Package formulaid defines canonical, immutable Homebrew Formula and tap
// identities.
package formulaid

import (
	"errors"
	"fmt"
	"strings"
)

const (
	coreOwner   = "homebrew"
	coreTapName = "core"

	// GitHub account names are limited to 39 characters. A default tap
	// repository is named "homebrew-<tap>" and GitHub repository names are
	// limited to 100 characters, leaving 91 for the tap component.
	maxOwnerBytes = 39
	maxTapBytes   = 91

	// Formula names become filesystem path components with an appended .rb
	// suffix in synthetic tap trees; reserve those three bytes under NAME_MAX.
	maxFormulaBytes = 252

	maxTapIDBytes     = maxOwnerBytes + 1 + maxTapBytes
	maxFormulaIDBytes = maxTapIDBytes + 1 + maxFormulaBytes
)

var (
	// ErrInvalidFormulaID identifies malformed Formula input.
	ErrInvalidFormulaID = errors.New("invalid Homebrew Formula ID")
	// ErrInvalidTap identifies a malformed owner/tap identity.
	ErrInvalidTap = errors.New("invalid Homebrew tap")
	// ErrDuplicateRoot identifies roots that canonicalize to the same FormulaID.
	ErrDuplicateRoot = errors.New("duplicate canonical Formula root")
)

// Tap is a canonical owner/tap identity. Its fields are intentionally
// unexported so a non-zero value can only be constructed through this package.
// Tap values are immutable and comparable.
type Tap struct {
	owner string
	name  string
}

// FormulaID is a canonical owner/tap/formula identity. Bare Formula names are
// represented using the homebrew/core tap. Its fields are intentionally
// unexported so a non-zero value can only be constructed through this package.
// FormulaID values are immutable and comparable.
type FormulaID struct {
	tap  Tap
	name string
}

// CoreTap returns the canonical homebrew/core tap identity.
func CoreTap() Tap {
	return Tap{owner: coreOwner, name: coreTapName}
}

// NewTap validates and constructs an owner/tap identity.
func NewTap(owner, name string) (Tap, error) {
	tap, err := newTap(owner, name)
	if err != nil {
		return Tap{}, fmt.Errorf("%w: %v", ErrInvalidTap, err)
	}
	return tap, nil
}

// ParseTap parses an exact owner/tap identity.
func ParseTap(input string) (Tap, error) {
	if len(input) > maxTapIDBytes {
		return Tap{}, fmt.Errorf("%w: identity is %d bytes; maximum is %d", ErrInvalidTap, len(input), maxTapIDBytes)
	}
	if strings.Count(input, "/") != 1 {
		return Tap{}, fmt.Errorf("%w %q: expected owner/tap", ErrInvalidTap, input)
	}
	owner, name, _ := strings.Cut(input, "/")
	tap, err := newTap(owner, name)
	if err != nil {
		return Tap{}, fmt.Errorf("%w %q: %v", ErrInvalidTap, input, err)
	}
	return tap, nil
}

// Owner returns the tap's GitHub owner component.
func (t Tap) Owner() string {
	return t.owner
}

// Name returns the tap component without the owner or homebrew- repository
// prefix.
func (t Tap) Name() string {
	return t.name
}

// String returns the canonical owner/tap identity. The zero value renders as
// an empty string.
func (t Tap) String() string {
	if t == (Tap{}) {
		return ""
	}
	return t.owner + "/" + t.name
}

// New validates and constructs a FormulaID from an existing tap identity and a
// short Formula name.
func New(tap Tap, name string) (FormulaID, error) {
	id, err := newFormulaID(tap, name)
	if err != nil {
		return FormulaID{}, fmt.Errorf("%w: %v", ErrInvalidFormulaID, err)
	}
	return id, nil
}

// Parse parses a bare Formula name or an exact owner/tap/formula identity. Bare
// names and explicit homebrew/core names produce identical FormulaID values.
func Parse(input string) (FormulaID, error) {
	if len(input) > maxFormulaIDBytes {
		return FormulaID{}, fmt.Errorf("%w: identity is %d bytes; maximum is %d", ErrInvalidFormulaID, len(input), maxFormulaIDBytes)
	}
	var (
		id  FormulaID
		err error
	)
	switch strings.Count(input, "/") {
	case 0:
		id, err = newFormulaID(CoreTap(), input)
	case 2:
		parts := strings.Split(input, "/")
		var tap Tap
		tap, err = newTap(parts[0], parts[1])
		if err == nil {
			id, err = newFormulaID(tap, parts[2])
		}
	default:
		return FormulaID{}, fmt.Errorf("%w %q: expected formula or owner/tap/formula", ErrInvalidFormulaID, input)
	}
	if err != nil {
		return FormulaID{}, fmt.Errorf("%w %q: %v", ErrInvalidFormulaID, input, err)
	}
	return id, nil
}

// ParseRoots parses roots in input order and rejects duplicate canonical
// identities, including a bare core name paired with its explicit
// homebrew/core form.
func ParseRoots(roots []string) ([]FormulaID, error) {
	if roots == nil {
		return nil, nil
	}
	ids := make([]FormulaID, len(roots))
	seen := make(map[FormulaID]int, len(roots))
	for i, root := range roots {
		id, err := Parse(root)
		if err != nil {
			return nil, fmt.Errorf("parse Formula root %d: %w", i, err)
		}
		if first, ok := seen[id]; ok {
			return nil, fmt.Errorf("%w: roots[%d]=%q and roots[%d]=%q both identify %q", ErrDuplicateRoot, first, roots[first], i, root, id.String())
		}
		seen[id] = i
		ids[i] = id
	}
	return ids, nil
}

// Tap returns the Formula's canonical tap identity.
func (id FormulaID) Tap() Tap {
	return id.tap
}

// Name returns the short Formula name used for its Cellar rack.
func (id FormulaID) Name() string {
	return id.name
}

// String returns the canonical owner/tap/formula identity. The zero value
// renders as an empty string.
func (id FormulaID) String() string {
	if id == (FormulaID{}) {
		return ""
	}
	return id.tap.String() + "/" + id.name
}

func newTap(owner, name string) (Tap, error) {
	if err := validateOwner(owner); err != nil {
		return Tap{}, err
	}
	if err := validateTapName(name); err != nil {
		return Tap{}, err
	}
	return Tap{owner: owner, name: name}, nil
}

func newFormulaID(tap Tap, name string) (FormulaID, error) {
	if err := validateTap(tap); err != nil {
		return FormulaID{}, fmt.Errorf("tap: %v", err)
	}
	if err := validateFormulaName(name); err != nil {
		return FormulaID{}, err
	}
	return FormulaID{tap: tap, name: name}, nil
}

func validateTap(tap Tap) error {
	if err := validateOwner(tap.owner); err != nil {
		return err
	}
	return validateTapName(tap.name)
}

func validateOwner(owner string) error {
	if err := validateComponent("owner", owner, maxOwnerBytes); err != nil {
		return err
	}
	if !isLowerAlphaNumeric(owner[0]) || !isLowerAlphaNumeric(owner[len(owner)-1]) {
		return errors.New("owner must begin and end with a lowercase ASCII letter or digit")
	}
	previousHyphen := false
	for _, c := range []byte(owner) {
		switch {
		case isLowerAlphaNumeric(c):
			previousHyphen = false
		case c == '-' && !previousHyphen:
			previousHyphen = true
		default:
			return errors.New("owner must contain only lowercase ASCII letters, digits, and single hyphens")
		}
	}
	return nil
}

func validateTapName(name string) error {
	if err := validateComponent("tap", name, maxTapBytes); err != nil {
		return err
	}
	if !isLowerAlphaNumeric(name[0]) {
		return errors.New("tap must begin with a lowercase ASCII letter or digit")
	}
	for _, c := range []byte(name) {
		if !isLowerAlphaNumeric(c) && !strings.ContainsRune("._-", rune(c)) {
			return errors.New("tap must contain only lowercase ASCII letters, digits, dots, underscores, and hyphens")
		}
	}
	return nil
}

func validateFormulaName(name string) error {
	if err := validateComponent("Formula", name, maxFormulaBytes); err != nil {
		return err
	}
	if !isLowerAlphaNumeric(name[0]) {
		return errors.New("Formula must begin with a lowercase ASCII letter or digit")
	}
	if strings.Count(name, "@") > 1 || strings.HasSuffix(name, "@") {
		return errors.New("Formula has invalid versioned-Formula syntax")
	}
	for _, c := range []byte(name) {
		if !isLowerAlphaNumeric(c) && !strings.ContainsRune("+_.@-", rune(c)) {
			return errors.New("Formula must contain only lowercase ASCII letters, digits, plus, dot, underscore, at, and hyphen characters")
		}
	}
	return nil
}

func validateComponent(kind, value string, maxBytes int) error {
	if value == "" {
		return fmt.Errorf("%s component is empty", kind)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("%s component %q is not allowed", kind, value)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s component is %d bytes; maximum is %d", kind, len(value), maxBytes)
	}
	return nil
}

func isLowerAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}
