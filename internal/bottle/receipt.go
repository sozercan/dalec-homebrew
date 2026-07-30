package bottle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type installReceipt struct {
	Name             string              `json:"name"`
	FullName         string              `json:"full_name"`
	PkgVersion       string              `json:"pkg_version"`
	Revision         *int                `json:"revision"`
	BottleRebuild    *int                `json:"bottle_rebuild"`
	HomebrewVersion  string              `json:"homebrew_version"`
	BuiltAsBottle    *bool               `json:"built_as_bottle"`
	PouredFromBottle *bool               `json:"poured_from_bottle"`
	Arch             string              `json:"arch"`
	Compiler         string              `json:"compiler"`
	RuntimeDeps      []ReceiptDependency `json:"runtime_dependencies"`
	Source           receiptSource       `json:"source"`
}

type receiptSource struct {
	Spec     string          `json:"spec"`
	Tap      string          `json:"tap"`
	Versions receiptVersions `json:"versions"`
}

type receiptVersions struct {
	Stable        string `json:"stable"`
	VersionScheme *int   `json:"version_scheme"`
}

func validateReceipt(data []byte, expected Expectation) (ReceiptEvidence, error) {
	return validateReceiptWithPolicy(data, expected, false)
}

func validateReceiptWithPolicy(data []byte, expected Expectation, requirePoured bool) (ReceiptEvidence, error) {
	if err := validateUniqueJSON(data); err != nil {
		return ReceiptEvidence{}, fmt.Errorf("invalid receipt JSON: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var receipt installReceipt
	if err := dec.Decode(&receipt); err != nil {
		return ReceiptEvidence{}, fmt.Errorf("decode receipt: %w", err)
	}
	if receipt.BuiltAsBottle == nil || !*receipt.BuiltAsBottle {
		return ReceiptEvidence{}, fmt.Errorf("built_as_bottle must be true")
	}
	if requirePoured && (receipt.PouredFromBottle == nil || !*receipt.PouredFromBottle) {
		return ReceiptEvidence{}, fmt.Errorf("poured_from_bottle must be true")
	}
	if receipt.Source.Spec != "stable" {
		return ReceiptEvidence{}, fmt.Errorf("source.spec %q is not stable", receipt.Source.Spec)
	}
	if receipt.Source.Versions.Stable == "" || receipt.Source.Versions.Stable != expected.FormulaVersion {
		return ReceiptEvidence{}, fmt.Errorf("source stable version %q does not match %q", receipt.Source.Versions.Stable, expected.FormulaVersion)
	}
	if receipt.Source.Versions.VersionScheme == nil || *receipt.Source.Versions.VersionScheme != expected.VersionScheme {
		return ReceiptEvidence{}, fmt.Errorf("version_scheme does not match %d", expected.VersionScheme)
	}
	if expected.ExpectedTap != "" && receipt.Source.Tap != expected.ExpectedTap {
		return ReceiptEvidence{}, fmt.Errorf("source tap %q does not match %q", receipt.Source.Tap, expected.ExpectedTap)
	}
	if receipt.Name != "" && receipt.Name != expected.Name {
		return ReceiptEvidence{}, fmt.Errorf("name %q does not match %q", receipt.Name, expected.Name)
	}
	if receipt.FullName != "" && expected.FullName != "" && receipt.FullName != expected.FullName {
		return ReceiptEvidence{}, fmt.Errorf("full_name %q does not match %q", receipt.FullName, expected.FullName)
	}
	if receipt.PkgVersion != "" && receipt.PkgVersion != expected.PkgVersion {
		return ReceiptEvidence{}, fmt.Errorf("pkg_version %q does not match %q", receipt.PkgVersion, expected.PkgVersion)
	}
	if receipt.Revision != nil && *receipt.Revision != expected.FormulaRevision {
		return ReceiptEvidence{}, fmt.Errorf("revision %d does not match %d", *receipt.Revision, expected.FormulaRevision)
	}
	if receipt.BottleRebuild != nil && *receipt.BottleRebuild != expected.BottleRebuild {
		return ReceiptEvidence{}, fmt.Errorf("bottle_rebuild %d does not match %d", *receipt.BottleRebuild, expected.BottleRebuild)
	}
	if expected.HomebrewVersion != "" && receipt.HomebrewVersion != expected.HomebrewVersion {
		return ReceiptEvidence{}, fmt.Errorf("homebrew_version %q does not match %q", receipt.HomebrewVersion, expected.HomebrewVersion)
	}
	if expected.Arch != "" && receipt.Arch != expected.Arch {
		return ReceiptEvidence{}, fmt.Errorf("arch %q does not match %q", receipt.Arch, expected.Arch)
	}
	if expected.Compiler != "" && receipt.Compiler != expected.Compiler && (!requirePoured || !installedCompilerMatches(receipt.Compiler, expected.Compiler)) {
		return ReceiptEvidence{}, fmt.Errorf("compiler %q does not match %q", receipt.Compiler, expected.Compiler)
	}
	if err := compareReceiptDependencies(receipt.RuntimeDeps, expected.Dependencies); err != nil {
		return ReceiptEvidence{}, err
	}

	return ReceiptEvidence{
		FormulaVersion:   receipt.Source.Versions.Stable,
		VersionScheme:    *receipt.Source.Versions.VersionScheme,
		BuiltAsBottle:    true,
		PouredFromBottle: receipt.PouredFromBottle != nil && *receipt.PouredFromBottle,
		HomebrewVersion:  receipt.HomebrewVersion,
		Arch:             receipt.Arch,
		RuntimeDepCount:  len(receipt.RuntimeDeps),
	}, nil
}

func installedCompilerMatches(actual, expected string) bool {
	if actual == "" || expected == "" || !strings.HasPrefix(expected, actual+"-") {
		return false
	}
	version := strings.TrimPrefix(expected, actual+"-")
	if version == "" {
		return false
	}
	for _, r := range version {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

func compareReceiptDependencies(actual, expected []ReceiptDependency) error {
	if err := validateReceiptDependencies(actual); err != nil {
		return err
	}
	a := append([]ReceiptDependency(nil), actual...)
	e := append([]ReceiptDependency(nil), expected...)
	sortDeps := func(deps []ReceiptDependency) {
		slices.SortFunc(deps, func(a, b ReceiptDependency) int {
			if c := strings.Compare(a.FullName, b.FullName); c != 0 {
				return c
			}
			if c := strings.Compare(a.PkgVersion, b.PkgVersion); c != 0 {
				return c
			}
			if a.Revision < b.Revision {
				return -1
			}
			if a.Revision > b.Revision {
				return 1
			}
			if a.BottleRebuild < b.BottleRebuild {
				return -1
			}
			if a.BottleRebuild > b.BottleRebuild {
				return 1
			}
			return 0
		})
	}
	sortDeps(a)
	sortDeps(e)
	if !slices.Equal(a, e) {
		return fmt.Errorf("runtime_dependencies do not match authenticated bottle tab")
	}
	return nil
}

func validateReceiptDependencies(deps []ReceiptDependency) error {
	seen := make(map[string]struct{}, len(deps))
	for _, dep := range deps {
		if dep.FullName == "" || dep.Version == "" || dep.PkgVersion == "" ||
			dep.Revision < 0 || dep.BottleRebuild < 0 {
			return fmt.Errorf("invalid runtime dependency %#v", dep)
		}
		if _, ok := seen[dep.FullName]; ok {
			return fmt.Errorf("duplicate runtime dependency %q", dep.FullName)
		}
		seen[dep.FullName] = struct{}{}
	}
	return nil
}

func validateUniqueJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if err := walkJSONValue(dec, tok); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func walkJSONValue(dec *json.Decoder, tok json.Token) error {
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			valueTok, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkJSONValue(dec, valueTok); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("unterminated object")
		}
	case '[':
		for dec.More() {
			valueTok, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkJSONValue(dec, valueTok); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

// VerifyInstalledReceipt validates Homebrew's generated receipt for an installed keg.
// The keg path itself binds PkgVersion/revision; this check requires a poured bottle,
// stable homebrew/core identity, version scheme, architecture/compiler, and the exact
// bottle-tab dependency evidence. The installer Homebrew version may legitimately
// differ from the version that produced the bottle.
func VerifyInstalledReceipt(data []byte, node resolution.Node) (ReceiptEvidence, error) {
	expected := ExpectationFromNode(node)
	expected.HomebrewVersion = ""
	return validateReceiptWithPolicy(data, expected, true)
}
