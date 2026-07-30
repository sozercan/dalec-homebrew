package metadata

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var (
	formulaNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.@-]*$`)
	bottleTagPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)
)

// Catalog is immutable after construction. Every method that returns slices,
// maps, or Formula values returns caller-owned copies.
type Catalog struct {
	canonical          map[string]Formula
	aliases            map[string]string
	oldNames           map[string]string
	migrations         map[string]Migration
	resolvedMigrations map[string]migrationResolution
	canonicalNames     []string
}

type migrationResolution struct {
	canonical      string
	externalTarget string
}

// ParseCatalog parses the current signed Formula payload and signed tap
// migration payload after JWS verification. It also accepts the optional
// {"generated_date":...,"formulae":...} and
// {"generated_date":...,"migrations":...} wrappers used by fixtures and
// future API revisions.
func ParseCatalog(formulaPayload, migrationPayload []byte) (*Catalog, error) {
	formulaBody, _, err := unwrapFormulaPayload(formulaPayload)
	if err != nil {
		return nil, fmt.Errorf("%w: formula payload: %v", ErrInvalidCatalog, err)
	}
	migrationBody, _, err := unwrapMigrationPayload(migrationPayload)
	if err != nil {
		return nil, fmt.Errorf("%w: migration payload: %v", ErrInvalidCatalog, err)
	}
	formulae, err := parseFormulae(formulaBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCatalog, err)
	}
	migrations, err := parseMigrations(migrationBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCatalog, err)
	}
	return buildCatalog(formulae, migrations)
}

func parseFormulae(data []byte) ([]Formula, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("invalid formula JSON: %w", err)
	}
	var rawFormulae []rawFormula
	if err := json.Unmarshal(data, &rawFormulae); err != nil {
		return nil, fmt.Errorf("Formula payload must be an array: %w", err)
	}
	if rawFormulae == nil {
		return nil, fmt.Errorf("Formula payload is null")
	}
	formulae := make([]Formula, 0, len(rawFormulae))
	for i, raw := range rawFormulae {
		formula, err := normalizeFormula(raw)
		if err != nil {
			name := raw.Name
			if name == "" {
				name = fmt.Sprintf("index %d", i)
			}
			return nil, fmt.Errorf("Formula %q: %w", name, err)
		}
		formulae = append(formulae, formula)
	}
	return formulae, nil
}

type rawFormula struct {
	Name                    string                  `json:"name"`
	FullName                string                  `json:"full_name"`
	Tap                     string                  `json:"tap"`
	OldNames                []string                `json:"oldnames"`
	OldName                 string                  `json:"oldname"`
	Aliases                 []string                `json:"aliases"`
	VersionedFormulae       []string                `json:"versioned_formulae"`
	Description             string                  `json:"desc"`
	License                 string                  `json:"license"`
	Homepage                string                  `json:"homepage"`
	Versions                rawVersions             `json:"versions"`
	Revision                int                     `json:"revision"`
	VersionScheme           int                     `json:"version_scheme"`
	Bottle                  rawBottleRoot           `json:"bottle"`
	KegOnly                 bool                    `json:"keg_only"`
	Disabled                bool                    `json:"disabled"`
	Dependencies            []string                `json:"dependencies"`
	RecommendedDependencies []string                `json:"recommended_dependencies"`
	UsesFromMacOS           []json.RawMessage       `json:"uses_from_macos"`
	Variations              map[string]rawVariation `json:"variations"`
}

func runtimeUsesFromMacOS(values []json.RawMessage) ([]string, error) {
	var out []string
	for _, raw := range values {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			out = append(out, name)
			continue
		}
		var typed map[string]json.RawMessage
		if err := json.Unmarshal(raw, &typed); err != nil || len(typed) != 1 {
			return nil, fmt.Errorf("invalid uses_from_macos entry %s", raw)
		}
		for dependency, kindRaw := range typed {
			var kind string
			if json.Unmarshal(kindRaw, &kind) == nil {
				switch kind {
				case "build", "test":
					continue
				case "no_linkage":
					out = append(out, dependency)
				default:
					return nil, fmt.Errorf("unknown uses_from_macos kind %q", kind)
				}
				continue
			}
			var kinds []string
			if err := json.Unmarshal(kindRaw, &kinds); err != nil {
				return nil, fmt.Errorf("invalid uses_from_macos kind for %q", dependency)
			}
			runtime := false
			for _, item := range kinds {
				if item == "no_linkage" {
					runtime = true
				} else if item != "build" && item != "test" {
					return nil, fmt.Errorf("unknown uses_from_macos kind %q", item)
				}
			}
			if runtime {
				out = append(out, dependency)
			}
		}
	}
	return uniqueSorted(out), nil
}

func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return out
}

type rawVersions struct {
	Stable string `json:"stable"`
	Bottle bool   `json:"bottle"`
}

type rawBottleRoot struct {
	Stable *rawBottle `json:"stable"`
}

type rawBottle struct {
	Rebuild int                      `json:"rebuild"`
	RootURL string                   `json:"root_url"`
	Files   map[string]rawBottleFile `json:"files"`
}

type rawBottleFile struct {
	Cellar string `json:"cellar"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type rawVariation struct {
	Dependencies *[]string `json:"dependencies"`
	KegOnly      *bool     `json:"keg_only"`
}

func normalizeFormula(raw rawFormula) (Formula, error) {
	if err := validateSimpleName(raw.Name); err != nil {
		return Formula{}, err
	}
	if raw.Tap != "homebrew/core" {
		return Formula{}, fmt.Errorf("%w: tap is %q", ErrOutOfCore, raw.Tap)
	}
	if raw.FullName != raw.Name && raw.FullName != "homebrew/core/"+raw.Name {
		return Formula{}, fmt.Errorf("%w: full_name %q does not identify homebrew/core/%s", ErrOutOfCore, raw.FullName, raw.Name)
	}
	if strings.TrimSpace(raw.Versions.Stable) == "" {
		return Formula{}, fmt.Errorf("current stable version is empty")
	}
	if raw.Revision < 0 {
		return Formula{}, fmt.Errorf("negative revision %d", raw.Revision)
	}
	if raw.VersionScheme < 0 {
		return Formula{}, fmt.Errorf("negative version_scheme %d", raw.VersionScheme)
	}

	oldNames := slices.Clone(raw.OldNames)
	if raw.OldName != "" {
		oldNames = append(oldNames, raw.OldName)
	}
	if err := validateUniqueNames("oldname", oldNames); err != nil {
		return Formula{}, err
	}
	if err := validateUniqueNames("alias", raw.Aliases); err != nil {
		return Formula{}, err
	}
	if err := validateUniqueNames("versioned Formula", raw.VersionedFormulae); err != nil {
		return Formula{}, err
	}
	if err := validateUniqueNames("dependency", raw.Dependencies); err != nil {
		return Formula{}, err
	}
	if err := validateUniqueNames("recommended dependency", raw.RecommendedDependencies); err != nil {
		return Formula{}, err
	}
	uses, err := runtimeUsesFromMacOS(raw.UsesFromMacOS)
	if err != nil {
		return Formula{}, err
	}
	dependencies := append([]string(nil), raw.Dependencies...)
	dependencies = append(dependencies, raw.RecommendedDependencies...)
	dependencies = append(dependencies, uses...)
	dependencies = uniqueSorted(dependencies)
	if err := validateUniqueNames("dependency", dependencies); err != nil {
		return Formula{}, err
	}

	formula := Formula{
		Name:              raw.Name,
		FullName:          "homebrew/core/" + raw.Name,
		Tap:               raw.Tap,
		OldNames:          sortedClone(oldNames),
		Aliases:           sortedClone(raw.Aliases),
		VersionedFormulae: sortedClone(raw.VersionedFormulae),
		Description:       raw.Description,
		License:           raw.License,
		Homepage:          raw.Homepage,
		StableVersion:     raw.Versions.Stable,
		Revision:          raw.Revision,
		VersionScheme:     raw.VersionScheme,
		KegOnly:           raw.KegOnly,
		Disabled:          raw.Disabled,
		Dependencies:      dependencies,
	}

	variationTags := make([]string, 0, len(raw.Variations))
	for tag := range raw.Variations {
		variationTags = append(variationTags, tag)
	}
	slices.Sort(variationTags)
	for _, tag := range variationTags {
		if !bottleTagPattern.MatchString(tag) {
			return Formula{}, fmt.Errorf("invalid variation tag %q", tag)
		}
		rawVariation := raw.Variations[tag]
		variation := Variation{Tag: tag}
		if rawVariation.Dependencies != nil {
			if err := validateUniqueNames("variation dependency", *rawVariation.Dependencies); err != nil {
				return Formula{}, fmt.Errorf("variation %q: %w", tag, err)
			}
			combined := append([]string(nil), (*rawVariation.Dependencies)...)
			combined = append(combined, raw.RecommendedDependencies...)
			combined = append(combined, uses...)
			variation.Dependencies = uniqueSorted(combined)
			variation.OverridesDependencies = true
		}
		if rawVariation.KegOnly != nil {
			variation.KegOnly = *rawVariation.KegOnly
			variation.OverridesKegOnly = true
		}
		formula.Variations = append(formula.Variations, variation)
	}

	if raw.Versions.Bottle {
		if raw.Bottle.Stable == nil {
			return Formula{}, fmt.Errorf("versions.bottle is true but bottle.stable is missing")
		}
		bottle, err := normalizeBottle(*raw.Bottle.Stable)
		if err != nil {
			return Formula{}, err
		}
		formula.Bottle = &bottle
	} else if raw.Bottle.Stable != nil {
		return Formula{}, fmt.Errorf("versions.bottle is false but bottle.stable is present")
	}
	return formula, nil
}

func normalizeBottle(raw rawBottle) (Bottle, error) {
	if raw.Rebuild < 0 {
		return Bottle{}, fmt.Errorf("negative bottle rebuild %d", raw.Rebuild)
	}
	if err := validateAbsoluteHTTPSURL(raw.RootURL); err != nil {
		return Bottle{}, fmt.Errorf("invalid bottle root_url: %w", err)
	}
	if len(raw.Files) == 0 {
		return Bottle{}, fmt.Errorf("stable bottle files are empty")
	}
	tags := make([]string, 0, len(raw.Files))
	for tag := range raw.Files {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	bottle := Bottle{Rebuild: raw.Rebuild, RootURL: raw.RootURL, Files: make([]BottleFile, 0, len(tags))}
	for _, tag := range tags {
		if !bottleTagPattern.MatchString(tag) {
			return Bottle{}, fmt.Errorf("invalid bottle tag %q", tag)
		}
		file := raw.Files[tag]
		if strings.TrimSpace(file.Cellar) == "" {
			return Bottle{}, fmt.Errorf("bottle tag %q has an empty cellar", tag)
		}
		if err := validateAbsoluteHTTPSURL(file.URL); err != nil {
			return Bottle{}, fmt.Errorf("bottle tag %q has invalid URL: %w", tag, err)
		}
		if err := validateSHA256(file.SHA256); err != nil {
			return Bottle{}, fmt.Errorf("bottle tag %q has invalid sha256: %w", tag, err)
		}
		bottle.Files = append(bottle.Files, BottleFile{
			Tag: tag, Cellar: file.Cellar, URL: file.URL, SHA256: file.SHA256,
		})
	}
	return bottle, nil
}

func validateAbsoluteHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("expected an absolute https URL")
	}
	return nil
}

func validateSHA256(value string) error {
	if len(value) != 64 || value != strings.ToLower(value) {
		return fmt.Errorf("expected 64 lowercase hex characters")
	}
	_, err := hex.DecodeString(value)
	return err
}

func validateUniqueNames(kind string, names []string) error {
	seen := map[string]struct{}{}
	for _, name := range names {
		if err := validateSimpleName(name); err != nil {
			return fmt.Errorf("invalid %s: %w", kind, err)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate %s %q", kind, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func sortedClone(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func parseMigrations(data []byte) ([]Migration, error) {
	object, err := decodeJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("invalid migration JSON: %w", err)
	}
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	slices.Sort(names)
	migrations := make([]Migration, 0, len(names))
	for _, name := range names {
		if err := validateSimpleName(name); err != nil {
			return nil, fmt.Errorf("migration source: %w", err)
		}
		target, err := decodeString(object[name], "migration target")
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", name, err)
		}
		migration, err := normalizeMigration(name, target)
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, migration)
	}
	return migrations, nil
}

func normalizeMigration(name, target string) (Migration, error) {
	if strings.TrimSpace(target) == "" {
		return Migration{}, fmt.Errorf("migration %q has an empty target", name)
	}
	parts := strings.Split(target, "/")
	migration := Migration{Name: name, Target: target}
	switch len(parts) {
	case 1:
		if err := validateSimpleName(parts[0]); err != nil {
			return Migration{}, fmt.Errorf("migration %q target: %w", name, err)
		}
		migration.InCore = true
		migration.TargetName = parts[0]
	case 2:
		if parts[0] == "homebrew" && parts[1] == "core" {
			migration.InCore = true
			migration.TargetName = name
		} else if parts[0] == "" || parts[1] == "" {
			return Migration{}, fmt.Errorf("migration %q has malformed tap target %q", name, target)
		}
	case 3:
		if err := validateSimpleName(parts[2]); err != nil {
			return Migration{}, fmt.Errorf("migration %q target name: %w", name, err)
		}
		migration.TargetName = parts[2]
		migration.InCore = parts[0] == "homebrew" && parts[1] == "core"
		if parts[0] == "" || parts[1] == "" {
			return Migration{}, fmt.Errorf("migration %q has malformed tap target %q", name, target)
		}
	default:
		return Migration{}, fmt.Errorf("migration %q has malformed target %q", name, target)
	}
	return migration, nil
}

func buildCatalog(formulae []Formula, migrations []Migration) (*Catalog, error) {
	catalog := &Catalog{
		canonical:          make(map[string]Formula, len(formulae)),
		aliases:            map[string]string{},
		oldNames:           map[string]string{},
		migrations:         make(map[string]Migration, len(migrations)),
		resolvedMigrations: map[string]migrationResolution{},
	}
	claimed := map[string]string{}
	for _, formula := range formulae {
		if previous, duplicate := claimed[formula.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate canonical name %q (already %s)", ErrInvalidCatalog, formula.Name, previous)
		}
		claimed[formula.Name] = "canonical name"
		catalog.canonical[formula.Name] = cloneFormula(formula)
		catalog.canonicalNames = append(catalog.canonicalNames, formula.Name)
	}
	slices.Sort(catalog.canonicalNames)

	for _, name := range catalog.canonicalNames {
		formula := catalog.canonical[name]
		for _, versioned := range formula.VersionedFormulae {
			if versioned == name {
				return nil, fmt.Errorf("%w: Formula %q references itself in versioned_formulae", ErrInvalidCatalog, name)
			}
			if !strings.Contains(versioned, "@") {
				return nil, fmt.Errorf("%w: Formula %q has non-versioned versioned_formulae entry %q", ErrInvalidCatalog, name, versioned)
			}
			if _, ok := catalog.canonical[versioned]; !ok {
				return nil, fmt.Errorf("%w: Formula %q references missing versioned Formula %q", ErrInvalidCatalog, name, versioned)
			}
		}
		for _, alias := range formula.Aliases {
			if err := claimIndex(claimed, alias, "alias for "+name); err != nil {
				return nil, err
			}
			catalog.aliases[alias] = name
		}
		for _, oldName := range formula.OldNames {
			if err := claimIndex(claimed, oldName, "oldname for "+name); err != nil {
				return nil, err
			}
			catalog.oldNames[oldName] = name
		}
	}

	if err := canonicalizeCatalogDependencies(catalog); err != nil {
		return nil, err
	}
	if err := validateCatalogDependencies(catalog); err != nil {
		return nil, err
	}

	acceptedMigrations := make([]Migration, 0, len(migrations))
	for _, migration := range migrations {
		if migration.InCore && migration.TargetName == migration.Name {
			if _, ok := catalog.canonical[migration.Name]; ok {
				continue
			}
			if _, ok := catalog.aliases[migration.Name]; ok {
				continue
			}
			if _, ok := catalog.oldNames[migration.Name]; ok {
				continue
			}
			return nil, fmt.Errorf("%w: same-name migration %q has no canonical homebrew/core destination", ErrInvalidCatalog, migration.Name)
		}
		if err := claimIndex(claimed, migration.Name, "migration"); err != nil {
			return nil, err
		}
		catalog.migrations[migration.Name] = migration
		acceptedMigrations = append(acceptedMigrations, migration)
	}
	for _, migration := range acceptedMigrations {
		if _, err := catalog.resolveMigration(migration.Name, nil, map[string]int{}); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCatalog, err)
		}
	}
	return catalog, nil
}

func canonicalizeCatalogDependencies(catalog *Catalog) error {
	resolve := func(name string) (string, bool) {
		if _, ok := catalog.canonical[name]; ok {
			return name, true
		}
		if canonical, ok := catalog.aliases[name]; ok {
			return canonical, true
		}
		if canonical, ok := catalog.oldNames[name]; ok {
			return canonical, true
		}
		return "", false
	}
	for _, name := range catalog.canonicalNames {
		formula := catalog.canonical[name]
		for i, dependency := range formula.Dependencies {
			canonical, ok := resolve(dependency)
			if !ok {
				return fmt.Errorf("%w: Formula %q references missing dependency %q", ErrInvalidCatalog, name, dependency)
			}
			formula.Dependencies[i] = canonical
		}
		formula.Dependencies = uniqueSorted(formula.Dependencies)
		for i := range formula.Variations {
			for j, dependency := range formula.Variations[i].Dependencies {
				canonical, ok := resolve(dependency)
				if !ok {
					return fmt.Errorf("%w: Formula %q variation %q references missing dependency %q", ErrInvalidCatalog, name, formula.Variations[i].Tag, dependency)
				}
				formula.Variations[i].Dependencies[j] = canonical
			}
			formula.Variations[i].Dependencies = uniqueSorted(formula.Variations[i].Dependencies)
		}
		catalog.canonical[name] = formula
	}
	return nil
}

func validateCatalogDependencies(catalog *Catalog) error {
	tags := map[string]struct{}{"": {}}
	for _, name := range catalog.canonicalNames {
		formula := catalog.canonical[name]
		for _, dependency := range formula.Dependencies {
			if _, ok := catalog.canonical[dependency]; !ok {
				return fmt.Errorf("%w: Formula %q references missing dependency %q", ErrInvalidCatalog, name, dependency)
			}
		}
		for _, variation := range formula.Variations {
			if variation.Tag != "x86_64_linux" && variation.Tag != "arm64_linux" {
				continue
			}
			tags[variation.Tag] = struct{}{}
			if !variation.OverridesDependencies {
				continue
			}
			for _, dependency := range variation.Dependencies {
				if _, ok := catalog.canonical[dependency]; !ok {
					return fmt.Errorf("%w: Formula %q variation %q references missing dependency %q", ErrInvalidCatalog, name, variation.Tag, dependency)
				}
			}
		}
	}
	tagNames := make([]string, 0, len(tags))
	for tag := range tags {
		tagNames = append(tagNames, tag)
	}
	slices.Sort(tagNames)
	for _, tag := range tagNames {
		if err := validateDependencyCycles(catalog, tag); err != nil {
			return err
		}
	}
	return nil
}

func validateDependencyCycles(catalog *Catalog, tag string) error {
	state := make(map[string]uint8, len(catalog.canonical))
	positions := make(map[string]int, len(catalog.canonical))
	stack := make([]string, 0, len(catalog.canonical))
	var visit func(string) error
	visit = func(name string) error {
		state[name] = 1
		positions[name] = len(stack)
		stack = append(stack, name)
		formula := catalog.canonical[name]
		dependencies := formula.Dependencies
		if tag != "" {
			dependencies = formula.DependenciesFor(tag)
		}
		for _, dependency := range dependencies {
			switch state[dependency] {
			case 0:
				if err := visit(dependency); err != nil {
					return err
				}
			case 1:
				cycle := append(slices.Clone(stack[positions[dependency]:]), dependency)
				label := "base metadata"
				if tag != "" {
					label = "variation " + tag
				}
				return fmt.Errorf("%w: dependency cycle in %s: %s", ErrInvalidCatalog, label, strings.Join(cycle, " -> "))
			}
		}
		stack = stack[:len(stack)-1]
		delete(positions, name)
		state[name] = 2
		return nil
	}
	for _, name := range catalog.canonicalNames {
		if state[name] == 0 {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func claimIndex(claimed map[string]string, name, kind string) error {
	if previous, duplicate := claimed[name]; duplicate {
		return fmt.Errorf("%w: duplicate identity %q claimed by %s and %s", ErrInvalidCatalog, name, previous, kind)
	}
	claimed[name] = kind
	return nil
}

func (c *Catalog) resolveMigration(name string, stack []string, active map[string]int) (migrationResolution, error) {
	if resolved, ok := c.resolvedMigrations[name]; ok {
		return resolved, nil
	}
	if start, cycle := active[name]; cycle {
		path := append(slices.Clone(stack[start:]), name)
		return migrationResolution{}, fmt.Errorf("migration cycle: %s", strings.Join(path, " -> "))
	}
	migration, ok := c.migrations[name]
	if !ok {
		return migrationResolution{}, fmt.Errorf("missing migration %q", name)
	}
	active[name] = len(stack)
	stack = append(stack, name)
	defer delete(active, name)

	if !migration.InCore {
		resolved := migrationResolution{externalTarget: migration.Target}
		c.resolvedMigrations[name] = resolved
		return resolved, nil
	}
	target := migration.TargetName
	if formula, ok := c.canonical[target]; ok {
		resolved := migrationResolution{canonical: formula.Name}
		c.resolvedMigrations[name] = resolved
		return resolved, nil
	}
	if strings.Contains(target, "@") {
		return migrationResolution{}, fmt.Errorf("migration %q targets non-canonical versioned Formula %q", name, target)
	}
	if canonical, ok := c.aliases[target]; ok {
		resolved := migrationResolution{canonical: canonical}
		c.resolvedMigrations[name] = resolved
		return resolved, nil
	}
	if canonical, ok := c.oldNames[target]; ok {
		resolved := migrationResolution{canonical: canonical}
		c.resolvedMigrations[name] = resolved
		return resolved, nil
	}
	if _, ok := c.migrations[target]; ok {
		resolved, err := c.resolveMigration(target, stack, active)
		if err != nil {
			return migrationResolution{}, err
		}
		c.resolvedMigrations[name] = resolved
		return resolved, nil
	}
	return migrationResolution{}, fmt.Errorf("migration %q targets unknown homebrew/core Formula %q", name, target)
}

// Len returns the number of canonical Formulae.
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.canonical)
}

// CanonicalNames returns sorted canonical names.
func (c *Catalog) CanonicalNames() []string {
	if c == nil {
		return nil
	}
	return slices.Clone(c.canonicalNames)
}

// Aliases returns a caller-owned alias-to-canonical index.
func (c *Catalog) Aliases() map[string]string {
	return cloneStringMap(c.aliases)
}

// OldNames returns a caller-owned oldname-to-canonical index.
func (c *Catalog) OldNames() map[string]string {
	return cloneStringMap(c.oldNames)
}

// Migrations returns sorted, caller-owned migration values.
func (c *Catalog) Migrations() []Migration {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.migrations))
	for name := range c.migrations {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]Migration, 0, len(names))
	for _, name := range names {
		out = append(out, c.migrations[name])
	}
	return out
}

// Formula returns an exact canonical Formula without applying alias or bottle
// eligibility policy.
func (c *Catalog) Formula(name string) (Formula, bool) {
	if c == nil {
		return Formula{}, false
	}
	formula, ok := c.canonical[name]
	if !ok {
		return Formula{}, false
	}
	return cloneFormula(formula), true
}

// Lookup applies V1 name semantics. A request containing @ must exactly match
// a canonical Formula; aliases, old names, migrations, and versioned_formulae
// never act as implicit selectors for such a request.
func (c *Catalog) Lookup(name string) (Match, error) {
	if c == nil {
		return Match{}, &LookupError{Name: name, Err: ErrFormulaNotFound}
	}
	if err := validateSimpleName(name); err != nil {
		return Match{}, &LookupError{Name: name, Err: err}
	}
	if formula, ok := c.canonical[name]; ok {
		return c.match(name, formula.Name, MatchCanonical)
	}
	if strings.Contains(name, "@") {
		return Match{}, &LookupError{Name: name, Err: ErrVersionedFormulaNotExplicit}
	}
	if canonical, ok := c.aliases[name]; ok {
		return c.match(name, canonical, MatchAlias)
	}
	if canonical, ok := c.oldNames[name]; ok {
		return c.match(name, canonical, MatchOldName)
	}
	if _, ok := c.migrations[name]; ok {
		resolved := c.resolvedMigrations[name]
		if resolved.externalTarget != "" {
			return Match{}, &LookupError{Name: name, Target: resolved.externalTarget, Err: ErrOutOfCore}
		}
		return c.match(name, resolved.canonical, MatchMigration)
	}
	return Match{}, &LookupError{Name: name, Err: ErrFormulaNotFound}
}

func (c *Catalog) match(requested, canonical string, kind MatchKind) (Match, error) {
	formula, ok := c.canonical[canonical]
	if !ok {
		return Match{}, &LookupError{Name: requested, Target: canonical, Err: ErrFormulaNotFound}
	}
	if formula.Disabled {
		return Match{}, &LookupError{Name: requested, Target: canonical, Err: ErrFormulaDisabled}
	}
	if formula.Bottle == nil {
		return Match{}, &LookupError{Name: requested, Target: canonical, Err: ErrBottleUnavailable}
	}
	return Match{Requested: requested, Canonical: canonical, Kind: kind, Formula: cloneFormula(formula)}, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func unwrapFormulaPayload(payload []byte) ([]byte, string, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, "", fmt.Errorf("payload is empty")
	}
	if trimmed[0] == '[' {
		return slices.Clone(trimmed), "", nil
	}
	if trimmed[0] != '{' {
		return nil, "", fmt.Errorf("expected Formula array or wrapper object")
	}
	object, err := decodeJSONObject(trimmed)
	if err != nil {
		return nil, "", err
	}
	formulae, ok := object["formulae"]
	if !ok {
		return nil, "", fmt.Errorf("Formula wrapper is missing formulae")
	}
	generatedDate := ""
	if raw := object["generated_date"]; len(raw) != 0 {
		generatedDate, err = decodeString(raw, "generated_date")
		if err != nil {
			return nil, "", err
		}
	}
	return slices.Clone(formulae), generatedDate, nil
}

func unwrapMigrationPayload(payload []byte) ([]byte, string, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, "", fmt.Errorf("migration payload must be an object")
	}
	object, err := decodeJSONObject(trimmed)
	if err != nil {
		return nil, "", err
	}
	migrations, hasMigrations := object["migrations"]
	generated, hasGenerated := object["generated_date"]
	if hasMigrations && hasGenerated && len(object) <= 3 && len(bytes.TrimSpace(migrations)) > 0 && bytes.TrimSpace(migrations)[0] == '{' {
		generatedDate, err := decodeString(generated, "generated_date")
		if err != nil {
			return nil, "", err
		}
		return slices.Clone(migrations), generatedDate, nil
	}
	return slices.Clone(trimmed), "", nil
}
