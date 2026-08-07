package catalogextractor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

const ExtractedTapSchemaVersion = "dalec-homebrew-extracted-tap/v1"

var catalogValidationTime = time.Unix(1, 0).UTC()

type CoreCatalog interface {
	Lookup(string) (metadata.Match, error)
}

// ExtractedTap is the bounded, unsigned result emitted by the network-disabled
// Ruby extractor. Dependency spellings are intentionally still raw here; the
// generator normalizes them against the exact authenticated core snapshot
// before a TapCatalog can be signed.
type ExtractedTap struct {
	SchemaVersion string             `json:"schema_version"`
	Tap           catalog.TapSource  `json:"tap"`
	Formulae      []ExtractedFormula `json:"formulae"`
	Aliases       map[string]string  `json:"aliases,omitempty"`
	Renames       map[string]string  `json:"renames,omitempty"`
	Migrations    map[string]string  `json:"migrations,omitempty"`
}

type ExtractedFormula struct {
	SourcePath   string                     `json:"source_path"`
	SourceDigest string                     `json:"source_digest"`
	Platforms    []ExtractedPlatformFormula `json:"platforms"`
}

type ExtractedPlatformFormula struct {
	Tag               string                 `json:"tag"`
	Name              string                 `json:"name"`
	HomebrewFullName  string                 `json:"homebrew_full_name"`
	StableVersion     string                 `json:"stable_version"`
	Revision          int                    `json:"revision"`
	VersionScheme     int                    `json:"version_scheme"`
	Disabled          bool                   `json:"disabled,omitempty"`
	KegOnly           bool                   `json:"keg_only,omitempty"`
	License           string                 `json:"license,omitempty"`
	Dependencies      []string               `json:"dependencies,omitempty"`
	VersionedFormulae []string               `json:"versioned_formulae,omitempty"`
	Bottle            *ExtractedBottle       `json:"bottle,omitempty"`
	StableSource      *ExtractedStableSource `json:"stable_source,omitempty"`
}

// ExtractedStableSource is the platform-selected, checksummed stable archive
// reported by Formula#to_hash. Policy authorization happens after extraction.
type ExtractedStableSource struct {
	URL           string `json:"url"`
	SHA256        string `json:"sha256"`
	ArchiveFormat string `json:"archive_format"`
}

type ExtractedBottle struct {
	RootURL string               `json:"root_url"`
	Rebuild int                  `json:"rebuild"`
	Files   []catalog.BottleFile `json:"files"`
}

func DecodeExtractedTap(data []byte) (*ExtractedTap, error) {
	if len(data) == 0 || int64(len(data)) > catalog.MaxCatalogDocumentBytes {
		return nil, fmt.Errorf("extracted tap size %d is outside 1..%d", len(data), catalog.MaxCatalogDocumentBytes)
	}
	if err := validateUniqueJSON(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var extracted ExtractedTap
	if err := dec.Decode(&extracted); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple extracted tap JSON values")
		}
		return nil, err
	}
	if extracted.SchemaVersion != ExtractedTapSchemaVersion {
		return nil, fmt.Errorf("unsupported extracted tap schema %q", extracted.SchemaVersion)
	}
	if len(extracted.Formulae) == 0 {
		return nil, errors.New("extracted tap has no Formulae")
	}
	if len(extracted.Formulae) > catalog.MaxClosureNodes*16 {
		return nil, errors.New("extracted tap Formula count exceeds bound")
	}
	return &extracted, nil
}

func ToCatalog(extracted *ExtractedTap, core CoreCatalog) (*catalog.TapCatalog, error) {
	if extracted == nil {
		return nil, errors.New("nil extracted tap")
	}
	if core == nil {
		return nil, errors.New("authenticated core catalog is required")
	}
	owner := extracted.Tap.ID
	if err := owner.Validate(); err != nil || owner.IsCore() {
		return nil, fmt.Errorf("invalid extracted tap identity %q", owner)
	}
	if extracted.Tap.Repository != owner.DefaultGitHubRepository() {
		return nil, fmt.Errorf("tap repository %q does not match %q", extracted.Tap.Repository, owner.DefaultGitHubRepository())
	}

	formulaNames := make(map[string]struct{}, len(extracted.Formulae))
	for _, raw := range extracted.Formulae {
		if len(raw.Platforms) == 0 {
			return nil, fmt.Errorf("Formula %q has no platform evaluations", raw.SourcePath)
		}
		name := raw.Platforms[0].Name
		if name == "" {
			return nil, fmt.Errorf("Formula %q has an empty name", raw.SourcePath)
		}
		if _, duplicate := formulaNames[name]; duplicate {
			return nil, fmt.Errorf("duplicate extracted Formula %q", name)
		}
		formulaNames[name] = struct{}{}
	}

	formulae := make([]catalog.Formula, 0, len(extracted.Formulae))
	for _, raw := range extracted.Formulae {
		formula, err := convertFormula(raw, owner, formulaNames, core)
		if err != nil {
			return nil, err
		}
		formulae = append(formulae, formula)
	}

	aliases, err := convertScopedMappings(extracted.Aliases, owner)
	if err != nil {
		return nil, fmt.Errorf("aliases: %w", err)
	}
	renames, err := convertScopedMappings(extracted.Renames, owner)
	if err != nil {
		return nil, fmt.Errorf("renames: %w", err)
	}
	migrations := make([]catalog.Migration, 0, len(extracted.Migrations))
	for from, rawTarget := range extracted.Migrations {
		fromID, err := catalog.ParseFormulaID(string(owner) + "/" + from)
		if err != nil {
			return nil, fmt.Errorf("migration source %q: %w", from, err)
		}
		if strings.Count(rawTarget, "/") != 2 {
			return nil, fmt.Errorf("migration %q target %q is not fully qualified", from, rawTarget)
		}
		to, err := catalog.ParseFormulaID(rawTarget)
		if err != nil {
			return nil, fmt.Errorf("migration %q target: %w", from, err)
		}
		migrations = append(migrations, catalog.Migration{From: fromID, RawTarget: rawTarget, To: to})
	}
	slices.SortFunc(migrations, func(a, b catalog.Migration) int { return strings.Compare(string(a.From), string(b.From)) })

	document := &catalog.TapCatalog{
		SchemaVersion: catalog.TapCatalogSchemaVersion,
		Tap:           extracted.Tap,
		PublishedAt:   catalogValidationTime,
		Sequence:      1,
		Formulae:      formulae,
		Aliases:       aliases,
		Renames:       renames,
		Migrations:    migrations,
	}
	if err := catalog.ValidateTapCatalog(document); err != nil {
		return nil, err
	}
	// These deterministic placeholders make the unsigned catalog usable by the
	// generator's independent resolver. Hosted publication may replace both;
	// build-local generation binds the core snapshot time and explicit no-shared-
	// rollback sequence semantics before producing a resolution.
	return document, nil
}

func convertFormula(raw ExtractedFormula, owner catalog.TapID, localNames map[string]struct{}, core CoreCatalog) (catalog.Formula, error) {
	platforms := make(map[string]ExtractedPlatformFormula, len(raw.Platforms))
	for _, value := range raw.Platforms {
		if value.Tag != "x86_64_linux" && value.Tag != "arm64_linux" {
			return catalog.Formula{}, fmt.Errorf("Formula %q has unsupported extracted platform %q", raw.SourcePath, value.Tag)
		}
		if _, duplicate := platforms[value.Tag]; duplicate {
			return catalog.Formula{}, fmt.Errorf("Formula %q repeats platform %q", raw.SourcePath, value.Tag)
		}
		platforms[value.Tag] = value
	}
	x86, okX86 := platforms["x86_64_linux"]
	arm, okARM := platforms["arm64_linux"]
	if !okX86 && !okARM {
		return catalog.Formula{}, fmt.Errorf("Formula %q is unavailable on Linux", raw.SourcePath)
	}
	base := x86
	baseTag := "x86_64_linux"
	if !okX86 {
		base = arm
		baseTag = "arm64_linux"
	}
	if okX86 && okARM && (x86.Name != arm.Name || x86.HomebrewFullName != arm.HomebrewFullName || x86.StableVersion != arm.StableVersion || x86.Revision != arm.Revision || x86.VersionScheme != arm.VersionScheme || x86.Disabled != arm.Disabled || x86.License != arm.License) {
		return catalog.Formula{}, fmt.Errorf("Formula %q changes unsupported identity/version fields across Linux architectures", base.Name)
	}
	id, err := catalog.ParseFormulaID(string(owner) + "/" + base.Name)
	if err != nil {
		return catalog.Formula{}, err
	}
	if base.HomebrewFullName != string(id) {
		return catalog.Formula{}, fmt.Errorf("Formula %q full name %q does not match %q", base.Name, base.HomebrewFullName, id)
	}
	baseDependencies, err := normalizeDependencies(base.Dependencies, owner, localNames, core)
	if err != nil {
		return catalog.Formula{}, fmt.Errorf("Formula %s %s dependencies: %w", id, baseTag, err)
	}

	var bottle *catalog.BottleDeclaration
	if okX86 && okARM {
		bottle, err = mergeBottleDeclarations(x86.Bottle, arm.Bottle)
	} else {
		bottle, err = mergeBottleDeclarations(base.Bottle, base.Bottle)
	}
	if err != nil {
		return catalog.Formula{}, fmt.Errorf("Formula %s bottle: %w", id, err)
	}
	prebuiltArchive, err := mergePrebuiltArchiveDeclarations(platforms, bottle)
	if err != nil {
		return catalog.Formula{}, fmt.Errorf("Formula %s prebuilt archive: %w", id, err)
	}
	slices.Sort(base.VersionedFormulae)
	if okX86 && okARM {
		slices.Sort(x86.VersionedFormulae)
		slices.Sort(arm.VersionedFormulae)
		if !slices.Equal(x86.VersionedFormulae, arm.VersionedFormulae) {
			return catalog.Formula{}, fmt.Errorf("Formula %s versioned_formulae vary by architecture", id)
		}
	}
	versioned := make([]catalog.FormulaID, 0, len(base.VersionedFormulae))
	for _, name := range base.VersionedFormulae {
		value, err := parseSameTapFormula(owner, name)
		if err != nil {
			return catalog.Formula{}, fmt.Errorf("Formula %s versioned target %q: %w", id, name, err)
		}
		versioned = append(versioned, value)
	}

	formula := catalog.Formula{ID: id, Name: base.Name, HomebrewFullName: base.HomebrewFullName, SourcePath: raw.SourcePath, SourceDigest: raw.SourceDigest, StableVersion: base.StableVersion, Revision: base.Revision, VersionScheme: base.VersionScheme, Disabled: base.Disabled, KegOnly: base.KegOnly, License: base.License, Dependencies: baseDependencies, VersionedFormulae: versioned, Bottle: bottle, PrebuiltArchive: prebuiltArchive}
	if !okX86 {
		formula.Variations = append(formula.Variations, catalog.FormulaVariation{Tag: "x86_64_linux", Unavailable: true})
	}
	if !okARM {
		formula.Variations = append(formula.Variations, catalog.FormulaVariation{Tag: "arm64_linux", Unavailable: true})
	}
	if okX86 && okARM {
		armDependencies, err := normalizeDependencies(arm.Dependencies, owner, localNames, core)
		if err != nil {
			return catalog.Formula{}, fmt.Errorf("Formula %s arm64_linux dependencies: %w", id, err)
		}
		dependenciesVary := !slices.Equal(baseDependencies, armDependencies)
		kegOnlyVaries := x86.KegOnly != arm.KegOnly
		if dependenciesVary || kegOnlyVaries {
			variation := catalog.FormulaVariation{Tag: "arm64_linux", KegOnly: arm.KegOnly, OverridesKegOnly: kegOnlyVaries}
			if dependenciesVary {
				variation.Dependencies = armDependencies
				variation.OverridesDependencies = true
			}
			formula.Variations = append(formula.Variations, variation)
		}
	}
	return formula, nil
}

func mergeBottleDeclarations(left, right *ExtractedBottle) (*catalog.BottleDeclaration, error) {
	if left == nil && right == nil {
		return nil, nil
	}
	if left == nil || right == nil {
		return nil, errors.New("stable bottle availability differs by Linux architecture")
	}
	if left.RootURL != right.RootURL || left.Rebuild != right.Rebuild {
		return nil, errors.New("bottle root or rebuild differs by Linux architecture")
	}
	files := make(map[string]catalog.BottleFile, len(left.Files)+len(right.Files))
	for _, file := range append(slices.Clone(left.Files), right.Files...) {
		if existing, ok := files[file.Tag]; ok && existing != file {
			return nil, fmt.Errorf("bottle tag %q has conflicting declarations", file.Tag)
		}
		files[file.Tag] = file
	}
	values := make([]catalog.BottleFile, 0, len(files))
	for _, file := range files {
		values = append(values, file)
	}
	slices.SortFunc(values, func(a, b catalog.BottleFile) int { return strings.Compare(a.Tag, b.Tag) })
	return &catalog.BottleDeclaration{RootURL: left.RootURL, Rebuild: left.Rebuild, Files: values}, nil
}

func mergePrebuiltArchiveDeclarations(platforms map[string]ExtractedPlatformFormula, bottle *catalog.BottleDeclaration) (*catalog.PrebuiltArchiveDeclaration, error) {
	bottleTags := map[string]struct{}{}
	if bottle != nil {
		for _, file := range bottle.Files {
			bottleTags[file.Tag] = struct{}{}
		}
	}
	files := make([]catalog.PrebuiltArchiveFile, 0, len(platforms))
	for _, tag := range []string{"x86_64_linux", "arm64_linux"} {
		platform, ok := platforms[tag]
		_, native := bottleTags[tag]
		_, universal := bottleTags["all"]
		if !ok || platform.StableSource == nil || native || universal {
			continue
		}
		files = append(files, catalog.PrebuiltArchiveFile{
			Tag:    tag,
			URL:    platform.StableSource.URL,
			SHA256: platform.StableSource.SHA256,
			Format: platform.StableSource.ArchiveFormat,
		})
	}
	if len(files) == 0 {
		return nil, nil
	}
	declaration := &catalog.PrebuiltArchiveDeclaration{Files: files}
	if err := catalog.ValidatePrebuiltArchiveDeclaration(*declaration); err != nil {
		return nil, err
	}
	return declaration, nil
}

func normalizeDependencies(raw []string, owner catalog.TapID, localNames map[string]struct{}, core CoreCatalog) ([]catalog.Dependency, error) {
	dependencies := make([]catalog.Dependency, 0, len(raw))
	seen := make(map[catalog.FormulaID]struct{}, len(raw))
	for _, spelling := range raw {
		var id catalog.FormulaID
		var err error
		if strings.Contains(spelling, "/") {
			id, err = catalog.ParseFormulaID(spelling)
		} else {
			match, coreErr := core.Lookup(spelling)
			switch {
			case coreErr == nil:
				id, err = catalog.ParseFormulaID(match.Canonical)
			case errors.Is(coreErr, metadata.ErrFormulaNotFound):
				if _, ok := localNames[spelling]; !ok {
					return nil, fmt.Errorf("bare dependency %q is absent from core and tap %s", spelling, owner)
				}
				id, err = catalog.ParseFormulaID(string(owner) + "/" + spelling)
			default:
				return nil, coreErr
			}
		}
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate normalized dependency %s", id)
		}
		seen[id] = struct{}{}
		dependencies = append(dependencies, catalog.Dependency{Raw: spelling, ID: id})
	}
	slices.SortFunc(dependencies, func(a, b catalog.Dependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return dependencies, nil
}

func convertScopedMappings(values map[string]string, owner catalog.TapID) ([]catalog.ScopedMapping, error) {
	result := make([]catalog.ScopedMapping, 0, len(values))
	for from, to := range values {
		fromID, err := parseSameTapFormula(owner, from)
		if err != nil {
			return nil, err
		}
		toID, err := parseSameTapFormula(owner, to)
		if err != nil {
			return nil, err
		}
		result = append(result, catalog.ScopedMapping{From: fromID, To: toID})
	}
	slices.SortFunc(result, func(a, b catalog.ScopedMapping) int { return strings.Compare(string(a.From), string(b.From)) })
	return result, nil
}

func parseSameTapFormula(owner catalog.TapID, value string) (catalog.FormulaID, error) {
	if strings.Count(value, "/") == 0 {
		return catalog.ParseFormulaID(string(owner) + "/" + value)
	}
	id, err := catalog.ParseFormulaID(value)
	if err != nil {
		return "", err
	}
	if id.Tap() != owner {
		return "", fmt.Errorf("Formula %s leaves tap %s", id, owner)
	}
	return id, nil
}

func validateUniqueJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil {
		return err
	}
	if err := walkUniqueJSON(dec, first); err != nil {
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

func walkUniqueJSON(dec *json.Decoder, token json.Token) error {
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
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, value); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	case '[':
		for dec.More() {
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, value); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}
