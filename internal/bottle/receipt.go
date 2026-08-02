package bottle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	hbversion "github.com/sozercan/dalec-homebrew/internal/homebrew/version"
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

func (dependency *ReceiptDependency) UnmarshalJSON(data []byte) error {
	// Homebrew may add non-identity dependency metadata such as
	// compatibility_version. Preserve encoding/json's existing unknown-field
	// tolerance while tracking whether the exact pkg_version field was absent.
	var encoded struct {
		FullName         string          `json:"full_name"`
		Version          string          `json:"version"`
		Revision         int             `json:"revision"`
		BottleRebuild    int             `json:"bottle_rebuild"`
		PkgVersion       json.RawMessage `json:"pkg_version"`
		DeclaredDirectly bool            `json:"declared_directly,omitempty"`
	}
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	*dependency = ReceiptDependency{
		FullName:         encoded.FullName,
		Version:          encoded.Version,
		Revision:         encoded.Revision,
		BottleRebuild:    encoded.BottleRebuild,
		DeclaredDirectly: encoded.DeclaredDirectly,
	}
	if encoded.PkgVersion == nil {
		dependency.pkgVersionOmitted = true
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(encoded.PkgVersion), []byte("null")) {
		return fmt.Errorf("pkg_version cannot be null")
	}
	if err := json.Unmarshal(encoded.PkgVersion, &dependency.PkgVersion); err != nil {
		return fmt.Errorf("decode pkg_version: %w", err)
	}
	return nil
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
	receipt.RuntimeDeps = normalizeOmittedReceiptDependencyPkgVersions(receipt.RuntimeDeps)
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
	if expected.Compiler != "" && receipt.Compiler != expected.Compiler && (!requirePoured || !installedCompilerMatches(receipt.Compiler, expected.Compiler, expected.BottleTag)) {
		return ReceiptEvidence{}, fmt.Errorf("compiler %q does not match %q", receipt.Compiler, expected.Compiler)
	}
	var dependencyErr error
	if requirePoured {
		dependencyErr = compareInstalledReceiptDependencies(receipt.RuntimeDeps, expected.Dependencies)
	} else {
		dependencyErr = compareReceiptDependencies(receipt.RuntimeDeps, expected.Dependencies)
	}
	if dependencyErr != nil {
		return ReceiptEvidence{}, dependencyErr
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

func installedCompilerMatches(actual, expected, bottleTag string) bool {
	if compilerBaseWithOptionalVersion(actual, expected) {
		return true
	}
	// Homebrew may replace the compiler recorded by an architecture-neutral
	// bottle with the host default while pouring it. Compiler identity is not
	// meaningful for an "all" bottle, but keep the accepted values bounded to
	// Homebrew's supported compiler families.
	return bottleTag == "all" && compilerFamily(actual) != "" && compilerFamily(expected) != ""
}

func compilerBaseWithOptionalVersion(actual, expected string) bool {
	if actual == "" || expected == "" || !strings.HasPrefix(expected, actual+"-") {
		return false
	}
	return numericCompilerVersion(strings.TrimPrefix(expected, actual+"-"))
}

func compilerFamily(value string) string {
	for _, family := range []string{"clang", "gcc"} {
		if value == family || strings.HasPrefix(value, family+"-") && numericCompilerVersion(strings.TrimPrefix(value, family+"-")) {
			return family
		}
	}
	return ""
}

func numericCompilerVersion(version string) bool {
	parts := strings.Split(version, ".")
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
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
		return fmt.Errorf("runtime_dependencies do not match authenticated bottle tab: installed=%#v authenticated=%#v", a, e)
	}
	return nil
}

func compareInstalledReceiptDependencies(actual, expected []ReceiptDependency) error {
	if err := validateReceiptDependencies(actual); err != nil {
		return err
	}
	if err := validateReceiptDependencies(expected); err != nil {
		return fmt.Errorf("invalid verified closure dependency: %w", err)
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("runtime_dependencies do not match verified dependency closure")
	}
	return compareInstalledReceiptDependencyMembers(actual, expected)
}

func compareInstalledReceiptDependencySubset(actual, expected []ReceiptDependency) error {
	if err := validateReceiptDependencies(actual); err != nil {
		return err
	}
	if err := validateReceiptDependencies(expected); err != nil {
		return fmt.Errorf("invalid verified closure dependency: %w", err)
	}
	if len(actual) >= len(expected) {
		return fmt.Errorf("runtime_dependencies are not an incomplete verified-closure subset")
	}
	return compareInstalledReceiptDependencyMembers(actual, expected)
}

func compareInstalledReceiptDependencyMembers(actual, expected []ReceiptDependency) error {
	expectedByName := make(map[string]ReceiptDependency, len(expected))
	for _, dep := range expected {
		expectedByName[dep.FullName] = dep
	}
	for _, dep := range actual {
		want, ok := expectedByName[dep.FullName]
		if !ok || dep.Version != want.Version || dep.Revision != want.Revision || dep.PkgVersion != want.PkgVersion {
			return fmt.Errorf("runtime dependency %q does not match verified dependency closure", dep.FullName)
		}
		// Homebrew may lose bottle-rebuild metadata when it regenerates a Tab
		// from the current Formula object, but it must not invent another value.
		if dep.BottleRebuild != 0 && dep.BottleRebuild != want.BottleRebuild {
			return fmt.Errorf("runtime dependency %q bottle_rebuild %d does not match verified value %d", dep.FullName, dep.BottleRebuild, want.BottleRebuild)
		}
		// declared_directly is regenerated from current Formula declarations and
		// is not package identity; membership remains fixed by expectedByName.
	}
	return nil
}

func normalizeOmittedReceiptDependencyPkgVersions(deps []ReceiptDependency) []ReceiptDependency {
	normalized := append([]ReceiptDependency(nil), deps...)
	for i := range normalized {
		if !normalized[i].pkgVersionOmitted {
			continue
		}
		normalized[i].PkgVersion = hbversion.PkgVersion(normalized[i].Version, normalized[i].Revision)
		normalized[i].pkgVersionOmitted = false
	}
	return normalized
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

// InstalledReceiptNormalization is the deterministic result of replacing an
// incomplete Homebrew-generated runtime_dependencies array with the exact set
// derived from the already verified resolution policy. Data is non-nil only
// when Changed is true.
type InstalledReceiptNormalization struct {
	Data                  []byte
	Changed               bool
	BeforeDependencyCount int
	AfterDependencyCount  int
}

// NormalizeInstalledReceiptDependencies repairs only an incomplete
// runtime_dependencies array in a post-pour receipt. Pre-install bottle receipt
// verification does not call this function and remains exact against the
// authenticated bottle tab.
//
// Normalization is fail-closed:
//   - an already valid receipt is returned unchanged;
//   - every dependency already emitted by Homebrew must be an exact member of
//     the verified allowed set (except for the existing bounded rebuild/direct
//     metadata policy);
//   - extra, duplicate, or identity-tampered dependencies are rejected;
//   - all non-dependency receipt identity is validated by the existing strict
//     post-pour verifier before normalized bytes are returned.
func NormalizeInstalledReceiptDependencies(data []byte, node resolution.Node, closure []resolution.Node) (InstalledReceiptNormalization, error) {
	if _, err := VerifyInstalledReceipt(data, node, closure); err == nil {
		return InstalledReceiptNormalization{}, nil
	}

	expected, err := resolvedInstalledDependencies(node, closure)
	if err != nil {
		return InstalledReceiptNormalization{}, err
	}
	if len(expected) == 0 {
		return InstalledReceiptNormalization{}, fmt.Errorf("installed receipt for %q is invalid and has no policy-derived dependencies to normalize", node.Name)
	}
	if err := validateUniqueJSON(data); err != nil {
		return InstalledReceiptNormalization{}, fmt.Errorf("invalid receipt JSON: %w", err)
	}

	var receipt installReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return InstalledReceiptNormalization{}, fmt.Errorf("decode receipt: %w", err)
	}
	receipt.RuntimeDeps = normalizeOmittedReceiptDependencyPkgVersions(receipt.RuntimeDeps)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return InstalledReceiptNormalization{}, fmt.Errorf("decode receipt fields: %w", err)
	}
	rawDependencies, ok := fields["runtime_dependencies"]
	if !ok || !rawJSONArray(rawDependencies) {
		return InstalledReceiptNormalization{}, fmt.Errorf("installed receipt for %q does not contain a generated runtime_dependencies array", node.Name)
	}
	if err := compareInstalledReceiptDependencySubset(receipt.RuntimeDeps, expected); err != nil {
		return InstalledReceiptNormalization{}, fmt.Errorf("refuse to normalize installed receipt for %q: %w", node.Name, err)
	}

	// Use the exact, sorted policy projection rather than preserving whichever
	// partial order or non-identity directness metadata Homebrew happened to
	// emit. This makes the transformed bytes reproducible.
	normalizedDependencies := append([]ReceiptDependency(nil), expected...)
	slices.SortFunc(normalizedDependencies, func(a, b ReceiptDependency) int {
		return strings.Compare(a.FullName, b.FullName)
	})
	encodedDependencies, err := json.Marshal(normalizedDependencies)
	if err != nil {
		return InstalledReceiptNormalization{}, fmt.Errorf("encode normalized runtime_dependencies: %w", err)
	}
	fields["runtime_dependencies"] = encodedDependencies
	normalized, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return InstalledReceiptNormalization{}, fmt.Errorf("encode normalized installed receipt: %w", err)
	}
	normalized = append(normalized, '\n')
	if _, err := VerifyInstalledReceipt(normalized, node, closure); err != nil {
		return InstalledReceiptNormalization{}, fmt.Errorf("verify normalized installed receipt for %q: %w", node.Name, err)
	}
	return InstalledReceiptNormalization{
		Data:                  normalized,
		Changed:               true,
		BeforeDependencyCount: len(receipt.RuntimeDeps),
		AfterDependencyCount:  len(normalizedDependencies),
	}, nil
}

func rawJSONArray(data []byte) bool {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b == '['
		}
	}
	return false
}

// VerifyInstalledReceipt validates Homebrew's generated receipt for an installed keg.
// The keg path itself binds PkgVersion/revision; this check requires a poured bottle,
// stable homebrew/core identity, version scheme, architecture/compiler, and the exact
// root-specific recursive dependency graph recorded by the current signed resolution.
// Homebrew legitimately refreshes dependency versions and non-identity metadata while
// pouring older bottles. For every visited Formula, selected authenticated historical
// declared_directly edges are combined with current signed edges before traversal, so
// nested stale direct dependencies and cycles across historical/current edges remain
// visible. Historical transitive entries and unrelated closure nodes remain excluded.
// The historical bottle tab remains the exact pre-install authority and constrains
// minimums for current graph edges whose names remain.
// The installer Homebrew version may also differ from the version that produced the bottle.
func VerifyInstalledReceipt(data []byte, node resolution.Node, closure []resolution.Node) (ReceiptEvidence, error) {
	expected := ExpectationFromNode(node)
	expected.HomebrewVersion = ""

	installedDependencies, err := resolvedInstalledDependencies(node, closure)
	if err != nil {
		return ReceiptEvidence{}, err
	}
	expected.Dependencies = installedDependencies
	return validateReceiptWithPolicy(data, expected, true)
}

func resolvedInstalledDependencies(root resolution.Node, closure []resolution.Node) ([]ReceiptDependency, error) {
	if len(root.Dependencies) == 0 && len(closure) == 0 {
		return nil, nil
	}
	nodes := make(map[string]resolution.Node, len(closure))
	for _, node := range closure {
		if node.Name == "" || node.FullName != "homebrew/core/"+node.Name || node.FormulaVersion == "" || node.PkgVersion == "" || node.FormulaRevision < 0 || node.BottleRebuild < 0 {
			return nil, fmt.Errorf("invalid verified closure node %q", node.Name)
		}
		if _, exists := nodes[node.Name]; exists {
			return nil, fmt.Errorf("duplicate verified closure node %q", node.Name)
		}
		nodes[node.Name] = node
	}
	rootNode, ok := nodes[root.Name]
	if !ok {
		return nil, fmt.Errorf("receipt root %q is absent from verified closure", root.Name)
	}
	if rootNode.FullName != root.FullName || rootNode.FormulaVersion != root.FormulaVersion || rootNode.FormulaRevision != root.FormulaRevision || rootNode.PkgVersion != root.PkgVersion || rootNode.VersionScheme != root.VersionScheme || rootNode.BottleRebuild != root.BottleRebuild || !slices.Equal(rootNode.Dependencies, root.Dependencies) || !slices.Equal(rootNode.Bottle.Tab.Dependencies, root.Bottle.Tab.Dependencies) {
		return nil, fmt.Errorf("receipt root %q is inconsistent with verified closure", root.Name)
	}
	if err := validateCurrentDependencyGraph(nodes); err != nil {
		return nil, err
	}
	rootDirect := make(map[string]struct{})
	for _, requirement := range root.Dependencies {
		if requirement.Direct {
			rootDirect[requirement.Name] = struct{}{}
		}
	}
	rootHistorical, err := historicalDependencyMinimums(root.Bottle.Tab.Dependencies)
	if err != nil {
		return nil, fmt.Errorf("node %q historical bottle tab: %w", root.Name, err)
	}
	for name, dependency := range rootHistorical {
		if dependency.DeclaredDirectly {
			if _, selected := nodes[name]; selected {
				rootDirect[name] = struct{}{}
			}
		}
	}

	state := make(map[string]uint8, len(nodes))
	active := make(map[string]int, len(nodes))
	stack := make([]string, 0, len(nodes))
	stackEdges := make([]bool, 0, len(nodes))
	resolved := make([]ReceiptDependency, 0, len(nodes)-1)
	var visit func(string) error
	visit = func(name string) error {
		if state[name] == 2 {
			return nil
		}
		node, ok := nodes[name]
		if !ok {
			return fmt.Errorf("dependency edge %q is missing or crosses the verified closure", name)
		}
		active[name] = len(stack)
		stack = append(stack, name)

		historical, err := historicalDependencyMinimums(node.Bottle.Tab.Dependencies)
		if err != nil {
			return fmt.Errorf("node %q historical bottle tab: %w", name, err)
		}
		// The bool records whether an edge exists only because the verified
		// historical Formula declared it directly. That provenance lets a
		// historical branch terminate safely when a selected dependency's current
		// graph points back to the receipt root (as WebP/libtiff does), while
		// cycles wholly within the current graph or among non-root nodes still fail.
		edges := make(map[string]bool, len(node.Dependencies)+len(historical))
		seenCurrent := make(map[string]struct{}, len(node.Dependencies))
		for _, requirement := range node.Dependencies {
			if requirement.Name == "" || requirement.Revision < 0 || requirement.BottleRebuild < 0 {
				return fmt.Errorf("node %q has invalid current dependency %#v", name, requirement)
			}
			if _, duplicate := seenCurrent[requirement.Name]; duplicate {
				return fmt.Errorf("node %q has duplicate current dependency %q", name, requirement.Name)
			}
			seenCurrent[requirement.Name] = struct{}{}
			child, ok := nodes[requirement.Name]
			if !ok {
				return fmt.Errorf("node %q current dependency %q is missing or cross-root", name, requirement.Name)
			}
			effective := requirement
			if old, remains := historical[requirement.Name]; remains {
				if effective.Minimum == "" {
					if effective.Revision != 0 && effective.Revision != old.Revision || effective.BottleRebuild != 0 && effective.BottleRebuild != old.BottleRebuild {
						return fmt.Errorf("node %q dependency %q has partial minimum inconsistent with authenticated bottle tab", name, requirement.Name)
					}
					effective.Minimum = old.PkgVersion
					effective.Revision = old.Revision
					effective.BottleRebuild = old.BottleRebuild
				} else if effective.Minimum != old.PkgVersion || effective.Revision != old.Revision || effective.BottleRebuild != old.BottleRebuild {
					return fmt.Errorf("node %q dependency %q has minimum inconsistent with authenticated bottle tab", name, requirement.Name)
				}
			}
			if err := validateSelectedMinimum(name, effective, child); err != nil {
				return err
			}
			edges[requirement.Name] = false
		}
		for historicalName, dependency := range historical {
			if !dependency.DeclaredDirectly {
				continue
			}
			child, selected := nodes[historicalName]
			if !selected {
				// Historical embedded Formulae can name support Formulae that the
				// current signed graph no longer selects. They remain authenticated
				// bottle evidence, but cannot broaden the installed closure.
				continue
			}
			requirement := resolution.Requirement{
				Name:          historicalName,
				Minimum:       dependency.PkgVersion,
				Revision:      dependency.Revision,
				BottleRebuild: dependency.BottleRebuild,
				Direct:        true,
			}
			if err := validateSelectedMinimum(name+" historical Formula", requirement, child); err != nil {
				return err
			}
			if _, current := edges[historicalName]; !current {
				edges[historicalName] = true
			}
		}

		targets := make([]string, 0, len(edges))
		for target := range edges {
			targets = append(targets, target)
		}
		slices.Sort(targets)
		for _, target := range targets {
			if index, cycle := active[target]; cycle {
				// Current signed edges are expected to be acyclic. A cycle is
				// tolerated only when the cycle segment itself contains a selected,
				// authenticated historical-only edge; an unrelated historical prefix
				// must not mask a malformed current-only cycle deeper in the graph.
				hasHistoricalEdge := edges[target]
				for _, historical := range stackEdges[index:] {
					hasHistoricalEdge = hasHistoricalEdge || historical
				}
				if !hasHistoricalEdge {
					return fmt.Errorf("current dependency cycle reaches %q", target)
				}
				continue
			}
			stackEdges = append(stackEdges, edges[target])
			err := visit(target)
			stackEdges = stackEdges[:len(stackEdges)-1]
			if err != nil {
				return err
			}
		}
		delete(active, name)
		stack = stack[:len(stack)-1]
		state[name] = 2
		if name != root.Name {
			_, direct := rootDirect[name]
			resolved = append(resolved, ReceiptDependency{
				FullName:         node.Name,
				Version:          node.FormulaVersion,
				Revision:         node.FormulaRevision,
				BottleRebuild:    node.BottleRebuild,
				PkgVersion:       node.PkgVersion,
				DeclaredDirectly: direct,
			})
		}
		return nil
	}
	if err := visit(root.Name); err != nil {
		return nil, err
	}
	slices.SortFunc(resolved, func(a, b ReceiptDependency) int { return strings.Compare(a.FullName, b.FullName) })
	return resolved, nil
}

func validateCurrentDependencyGraph(nodes map[string]resolution.Node) error {
	state := make(map[string]uint8, len(nodes))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("current dependency cycle reaches %q", name)
		case 2:
			return nil
		}
		node, ok := nodes[name]
		if !ok {
			return fmt.Errorf("current dependency %q is missing from verified closure", name)
		}
		state[name] = 1
		seen := make(map[string]struct{}, len(node.Dependencies))
		for _, requirement := range node.Dependencies {
			if requirement.Name == "" {
				return fmt.Errorf("node %q has an empty current dependency", name)
			}
			if _, duplicate := seen[requirement.Name]; duplicate {
				return fmt.Errorf("node %q has duplicate current dependency %q", name, requirement.Name)
			}
			seen[requirement.Name] = struct{}{}
			if _, ok := nodes[requirement.Name]; !ok {
				return fmt.Errorf("node %q current dependency %q is missing or cross-root", name, requirement.Name)
			}
			if err := visit(requirement.Name); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func historicalDependencyMinimums(dependencies []resolution.RuntimeDependency) (map[string]resolution.RuntimeDependency, error) {
	result := make(map[string]resolution.RuntimeDependency, len(dependencies))
	for _, dependency := range dependencies {
		name := dependency.FullName
		if strings.HasPrefix(name, "homebrew/core/") {
			name = strings.TrimPrefix(name, "homebrew/core/")
		} else if strings.Contains(name, "/") {
			return nil, fmt.Errorf("non-homebrew/core dependency %q", dependency.FullName)
		}
		if name == "" || dependency.PkgVersion == "" || dependency.Revision < 0 || dependency.BottleRebuild < 0 {
			return nil, fmt.Errorf("invalid dependency %#v", dependency)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("duplicate dependency %q", name)
		}
		result[name] = dependency
	}
	return result, nil
}

func validateSelectedMinimum(parent string, requirement resolution.Requirement, selected resolution.Node) error {
	if requirement.Minimum == "" {
		if requirement.Revision != 0 || requirement.BottleRebuild != 0 {
			return fmt.Errorf("node %q dependency %q has revision or rebuild without a minimum version", parent, requirement.Name)
		}
		return nil
	}
	minimumVersion, minimumRevision, err := hbversion.SplitPkgVersion(requirement.Minimum)
	if err != nil {
		return fmt.Errorf("node %q dependency %q has invalid minimum %q: %w", parent, requirement.Name, requirement.Minimum, err)
	}
	requiredRevision := max(requirement.Revision, minimumRevision)
	if !hbversion.AtLeast(selected.FormulaVersion, selected.FormulaRevision, minimumVersion, requiredRevision) {
		return fmt.Errorf("node %q dependency %q selected %s does not satisfy minimum %s", parent, requirement.Name, selected.PkgVersion, requirement.Minimum)
	}
	if hbversion.Compare(selected.FormulaVersion, minimumVersion) == 0 && selected.FormulaRevision == requiredRevision && selected.BottleRebuild < requirement.BottleRebuild {
		return fmt.Errorf("node %q dependency %q selected bottle rebuild %d is below minimum %d", parent, requirement.Name, selected.BottleRebuild, requirement.BottleRebuild)
	}
	return nil
}
