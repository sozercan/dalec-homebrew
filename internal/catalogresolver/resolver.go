// Package catalogresolver independently recomputes cross-tap dependency
// normalization and closure from authenticated core metadata and signed tap
// catalog documents.
package catalogresolver

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

type CoreCatalog interface {
	Lookup(name string) (metadata.Match, error)
}

type Resolver struct {
	core     CoreCatalog
	catalogs map[catalog.TapID]*catalog.TapCatalog
	formulae map[catalog.FormulaID]catalog.Formula
	mappings map[catalog.FormulaID]catalog.FormulaID
	policy   *policyv2.TapPolicy
}

// MissingTapError reports the exact public tap that must be ingested before a
// closure can be recomputed. It is used only by the catalog generator's
// on-demand ingestion loop; the frontend still treats the same condition as a
// hard authorization failure because its signed catalog set is already fixed.
type MissingTapError struct {
	Tap catalog.TapID
}

func (e *MissingTapError) Error() string {
	if e == nil {
		return "dependency discovers a missing tap"
	}
	return fmt.Sprintf("dependency discovers unauthorized or missing tap %s", e.Tap)
}

func New(core CoreCatalog, catalogs map[catalog.TapID]*catalog.TapCatalog) (*Resolver, error) {
	if core == nil {
		return nil, errors.New("core catalog is required")
	}
	tapPolicy, err := policyv2.LoadTapPolicy()
	if err != nil {
		return nil, fmt.Errorf("load release-bound tap policy: %w", err)
	}
	r := &Resolver{core: core, catalogs: make(map[catalog.TapID]*catalog.TapCatalog, len(catalogs)), formulae: map[catalog.FormulaID]catalog.Formula{}, mappings: map[catalog.FormulaID]catalog.FormulaID{}, policy: tapPolicy}
	if len(catalogs) > catalog.MaxTaps {
		return nil, fmt.Errorf("catalog count %d exceeds %d", len(catalogs), catalog.MaxTaps)
	}
	for tap, document := range catalogs {
		if tap.IsCore() {
			return nil, errors.New("external catalog set must not replace homebrew/core")
		}
		if document == nil {
			return nil, fmt.Errorf("nil catalog for %s", tap)
		}
		if err := catalog.ValidateTapCatalog(document); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", tap, err)
		}
		if document.Tap.ID != tap {
			return nil, fmt.Errorf("catalog map key %s does not match document tap %s", tap, document.Tap.ID)
		}
		r.catalogs[tap] = document
		for _, formula := range document.Formulae {
			if _, duplicate := r.formulae[formula.ID]; duplicate {
				return nil, fmt.Errorf("duplicate Formula ID %s", formula.ID)
			}
			r.formulae[formula.ID] = formula
		}
		for _, mapping := range append(append(slices.Clone(document.Aliases), document.Renames...), migrationsAsMappings(document.Migrations)...) {
			if _, exists := r.mappings[mapping.From]; exists {
				return nil, fmt.Errorf("duplicate alias/rename/migration source %s", mapping.From)
			}
			r.mappings[mapping.From] = mapping.To
		}
	}
	return r, nil
}

// Resolve computes a deterministic closure for one normalized Linux platform.
// Roots may include core and non-core Formula IDs, though frontend catalog
// requests normally pass only external roots.
func (r *Resolver) Resolve(roots []catalog.FormulaID, platform catalog.Platform) (catalog.ClosureResult, error) {
	if err := platform.Validate(); err != nil {
		return catalog.ClosureResult{}, err
	}
	if len(roots) == 0 {
		return catalog.ClosureResult{}, errors.New("no roots")
	}
	requested := make([]catalog.FormulaID, 0, len(roots))
	requestedMappings := make([]catalog.RequestedMapping, 0, len(roots))
	usedTaps := make(map[catalog.TapID]struct{})
	queue := make([]catalog.FormulaID, 0, len(roots))
	seenRoots := map[catalog.FormulaID]struct{}{}
	for _, raw := range roots {
		if err := raw.Validate(); err != nil {
			return catalog.ClosureResult{}, fmt.Errorf("root %q: %w", raw, err)
		}
		id, _, err := r.lookupExact(raw, usedTaps)
		if err != nil {
			return catalog.ClosureResult{}, fmt.Errorf("root %s: %w", raw, err)
		}
		if _, duplicate := seenRoots[id]; duplicate {
			return catalog.ClosureResult{}, fmt.Errorf("duplicate canonical root %s", id)
		}
		seenRoots[id] = struct{}{}
		requested = append(requested, id)
		requestedMappings = append(requestedMappings, catalog.RequestedMapping{Requested: raw, Resolved: id})
		queue = append(queue, id)
	}

	tag := bottleTag(platform)
	nodes := map[catalog.FormulaID]catalog.Node{}
	graph := map[catalog.FormulaID][]catalog.Requirement{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if _, done := nodes[id]; done {
			continue
		}
		if len(nodes) >= catalog.MaxClosureNodes {
			return catalog.ClosureResult{}, fmt.Errorf("closure exceeds %d nodes", catalog.MaxClosureNodes)
		}
		canonical, source, err := r.lookupExact(id, usedTaps)
		if err != nil {
			return catalog.ClosureResult{}, err
		}
		if canonical != id {
			return catalog.ClosureResult{}, fmt.Errorf("canonical queue identity %s maps to %s", id, canonical)
		}
		_, requestedRoot := seenRoots[id]
		node, dependencies, err := r.nodeAndDependencies(source, platform, tag, requestedRoot)
		if err != nil {
			return catalog.ClosureResult{}, fmt.Errorf("Formula %s: %w", id, err)
		}
		for _, dependency := range dependencies {
			signedID := dependency.ID
			depID, _, err := r.normalizeDependency(id.Tap(), dependency.Raw, usedTaps)
			if err != nil {
				return catalog.ClosureResult{}, fmt.Errorf("Formula %s dependency %q: %w", id, dependency.Raw, err)
			}
			if signedID != "" && signedID != depID {
				return catalog.ClosureResult{}, fmt.Errorf("Formula %s dependency %q normalizes to %s, signed catalog claimed %s", id, dependency.Raw, depID, signedID)
			}
			dependency.ID = depID
			graph[id] = append(graph[id], dependency)
			if _, done := nodes[depID]; !done {
				queue = append(queue, depID)
			}
		}
		slices.SortFunc(graph[id], func(a, b catalog.Requirement) int { return strings.Compare(string(a.ID), string(b.ID)) })
		node.Dependencies = slices.Clone(graph[id])
		nodes[id] = node
	}
	if err := validateRackCollisions(nodes); err != nil {
		return catalog.ClosureResult{}, err
	}
	order, err := topo(graph, requested)
	if err != nil {
		return catalog.ClosureResult{}, err
	}
	list := make([]catalog.Node, 0, len(nodes))
	for _, node := range nodes {
		list = append(list, node)
	}
	slices.SortFunc(list, func(a, b catalog.Node) int { return strings.Compare(string(a.ID), string(b.ID)) })
	normalizationTaps := make([]catalog.TapID, 0, len(usedTaps))
	for tap := range usedTaps {
		normalizationTaps = append(normalizationTaps, tap)
	}
	slices.Sort(normalizationTaps)
	closure := catalog.ClosureResult{Requested: requested, RequestedMappings: requestedMappings, NormalizationTaps: normalizationTaps, Nodes: list, InstallOrder: order}
	if err := catalog.ValidateClosureResult(closure); err != nil {
		return catalog.ClosureResult{}, err
	}
	return closure, nil
}

type formulaSource struct {
	core     *metadata.Formula
	external *catalog.Formula
	id       catalog.FormulaID
}

func (r *Resolver) lookupExact(id catalog.FormulaID, used map[catalog.TapID]struct{}) (catalog.FormulaID, formulaSource, error) {
	if id.IsCore() {
		match, err := r.core.Lookup(id.Name())
		if err != nil {
			return "", formulaSource{}, err
		}
		canonical, err := catalog.ParseFormulaID(match.Canonical)
		if err != nil {
			return "", formulaSource{}, fmt.Errorf("core lookup returned invalid identity: %w", err)
		}
		formula := match.Formula
		return canonical, formulaSource{core: &formula, id: canonical}, nil
	}
	if !id.IsCore() {
		used[id.Tap()] = struct{}{}
	}
	resolved, err := r.resolveMapping(id, used)
	if err != nil {
		return "", formulaSource{}, err
	}
	if resolved.IsCore() {
		match, err := r.core.Lookup(resolved.Name())
		if err != nil {
			return "", formulaSource{}, err
		}
		canonical, err := catalog.ParseFormulaID(match.Canonical)
		if err != nil || canonical != resolved {
			return "", formulaSource{}, fmt.Errorf("core migration target %s is not canonical", resolved)
		}
		formula := match.Formula
		return canonical, formulaSource{core: &formula, id: canonical}, nil
	}
	formula, ok := r.formulae[resolved]
	if !ok {
		if _, knownTap := r.catalogs[resolved.Tap()]; !knownTap {
			return "", formulaSource{}, &MissingTapError{Tap: resolved.Tap()}
		}
		return "", formulaSource{}, fmt.Errorf("Formula %s is absent from signed tap catalog", resolved)
	}
	return resolved, formulaSource{external: &formula, id: resolved}, nil
}

func (r *Resolver) normalizeDependency(owner catalog.TapID, raw string, used map[catalog.TapID]struct{}) (catalog.FormulaID, formulaSource, error) {
	if strings.Contains(raw, "/") {
		id, err := catalog.ParseFormulaID(raw)
		if err != nil {
			return "", formulaSource{}, err
		}
		return r.lookupExact(id, used)
	}
	core, err := catalog.ParseFormulaID(raw)
	if err != nil {
		return "", formulaSource{}, err
	}
	if canonical, source, coreErr := r.lookupExact(core, used); coreErr == nil {
		return canonical, source, nil
	} else if !errors.Is(coreErr, metadata.ErrFormulaNotFound) {
		return "", formulaSource{}, coreErr
	}
	local, err := catalog.ParseFormulaID(string(owner) + "/" + raw)
	if err != nil {
		return "", formulaSource{}, err
	}
	return r.lookupExact(local, used)
}

func (r *Resolver) resolveMapping(id catalog.FormulaID, used map[catalog.TapID]struct{}) (catalog.FormulaID, error) {
	active := map[catalog.FormulaID]int{}
	var chain []catalog.FormulaID
	for {
		if !id.IsCore() {
			used[id.Tap()] = struct{}{}
		}
		if index, cycle := active[id]; cycle {
			cycleIDs := append(slices.Clone(chain[index:]), id)
			parts := make([]string, len(cycleIDs))
			for i := range cycleIDs {
				parts[i] = string(cycleIDs[i])
			}
			return "", fmt.Errorf("alias/rename/migration cycle: %s", strings.Join(parts, " -> "))
		}
		active[id] = len(chain)
		chain = append(chain, id)
		next, ok := r.mappings[id]
		if !ok {
			return id, nil
		}
		id = next
	}
}

func (r *Resolver) nodeAndDependencies(source formulaSource, platform catalog.Platform, tag string, requestedRoot bool) (catalog.Node, []catalog.Requirement, error) {
	if source.core != nil {
		formula := *source.core
		if formula.Disabled {
			return catalog.Node{}, nil, metadata.ErrFormulaDisabled
		}
		if _, err := formula.BottleFor(tag); err != nil {
			return catalog.Node{}, nil, err
		}
		deps := formula.DependenciesFor(tag)
		requirements := make([]catalog.Requirement, len(deps))
		for i, dep := range deps {
			requirements[i] = catalog.Requirement{Raw: dep, DeclaredDirectly: true}
		}
		return catalog.Node{ID: source.id, Tap: source.id.Tap(), Name: formula.Name, HomebrewFullName: formula.FullName, FormulaVersion: formula.StableVersion, FormulaRevision: formula.Revision, PkgVersion: pkgVersion(formula.StableVersion, formula.Revision), VersionScheme: formula.VersionScheme, BottleRebuild: formula.Bottle.Rebuild, License: formula.License, KegOnly: formula.KegOnlyFor(tag)}, requirements, nil
	}
	formula := *source.external
	if formula.Disabled {
		return catalog.Node{}, nil, errors.New("Formula is disabled")
	}
	bottleRebuild := 0
	if hasNativeBottleForTag(formula.Bottle, tag) {
		bottleRebuild = formula.Bottle.Rebuild
	} else if err := r.authorizePrebuiltArchive(formula, platform, tag, requestedRoot); err != nil {
		unavailable := externalBottleUnavailableError(formula.Bottle, tag)
		if formula.PrebuiltArchive == nil {
			return catalog.Node{}, nil, unavailable
		}
		return catalog.Node{}, nil, fmt.Errorf("%w: prebuilt archive is not eligible: %v", unavailable, err)
	}
	deps := formula.Dependencies
	kegOnly := formula.KegOnly
	for _, variation := range formula.Variations {
		if variation.Tag != tag {
			continue
		}
		if variation.Unavailable {
			return catalog.Node{}, nil, fmt.Errorf("stable Formula is unavailable for %s", tag)
		}
		if variation.OverridesDependencies {
			deps = variation.Dependencies
		}
		if variation.OverridesKegOnly {
			kegOnly = variation.KegOnly
		}
	}
	requirements := make([]catalog.Requirement, len(deps))
	for i, dep := range deps {
		requirements[i] = catalog.Requirement{Raw: dep.Raw, ID: dep.ID, DeclaredDirectly: true}
	}
	return catalog.Node{ID: source.id, Tap: source.id.Tap(), Name: formula.Name, HomebrewFullName: formula.HomebrewFullName, FormulaVersion: formula.StableVersion, FormulaRevision: formula.Revision, PkgVersion: pkgVersion(formula.StableVersion, formula.Revision), VersionScheme: formula.VersionScheme, License: formula.License, KegOnly: kegOnly, BottleRebuild: bottleRebuild}, requirements, nil
}

func (r *Resolver) authorizePrebuiltArchive(formula catalog.Formula, platform catalog.Platform, tag string, requestedRoot bool) error {
	declaration := formula.PrebuiltArchive
	if declaration == nil {
		return errors.New("stable prebuilt archive metadata is unavailable")
	}
	policy, ok := r.policy.PrebuiltArchiveForFormula(string(formula.ID))
	if !ok {
		return fmt.Errorf("Formula ID %s has no exact release-policy authorization", formula.ID)
	}
	if policy.FormulaID != string(formula.ID) {
		return fmt.Errorf("release policy Formula ID %q does not match %s", policy.FormulaID, formula.ID)
	}
	if !policy.RootOnly {
		return errors.New("release policy does not require prebuilt archive use to be root-only")
	}
	if !requestedRoot {
		return errors.New("release policy permits the prebuilt archive only as an explicitly requested resolved root")
	}
	if formula.StableVersion != policy.Version {
		return fmt.Errorf("stable version %q does not match release policy %q", formula.StableVersion, policy.Version)
	}
	if formula.SourceDigest != policy.FormulaSourceDigest {
		return fmt.Errorf("Formula source digest %s does not match release policy %s", formula.SourceDigest, policy.FormulaSourceDigest)
	}
	if formula.License != policy.License {
		return fmt.Errorf("license %q does not match release policy %q", formula.License, policy.License)
	}
	if !policy.RequireNoBottle {
		return errors.New("release policy does not require native bottles to be absent")
	}
	if formula.Bottle != nil {
		return errors.New("Formula declares native bottle metadata but release policy requires no bottle")
	}
	if len(policy.Dependencies) != 0 {
		return errors.New("release policy does not require an empty dependency set")
	}
	if formulaHasDependencies(formula) {
		return errors.New("Formula declares dependencies but release policy requires none")
	}

	policyPlatform, ok := prebuiltPolicyPlatform(policy.Platforms, platform)
	if !ok {
		return fmt.Errorf("release policy has no entry for %s/%s", platform.OS, platform.Architecture)
	}
	file, ok := prebuiltArchiveFile(declaration.Files, tag)
	if !ok {
		return fmt.Errorf("prebuilt archive does not declare target tag %s", tag)
	}
	if file.URL != policyPlatform.URL {
		return fmt.Errorf("prebuilt archive URL %q does not match release policy %q", file.URL, policyPlatform.URL)
	}
	if file.SHA256 != policyPlatform.SHA256 {
		return fmt.Errorf("prebuilt archive digest %s does not match release policy %s", file.SHA256, policyPlatform.SHA256)
	}
	if file.Format != policy.Archive.Format {
		return fmt.Errorf("prebuilt archive format %q does not match release policy %q", file.Format, policy.Archive.Format)
	}
	return nil
}

func prebuiltPolicyPlatform(platforms []policyv2.PrebuiltArchivePlatformPolicy, target catalog.Platform) (policyv2.PrebuiltArchivePlatformPolicy, bool) {
	want := target.OS + "/" + target.Architecture
	for _, platform := range platforms {
		if platform.Platform == want {
			return platform, true
		}
	}
	return policyv2.PrebuiltArchivePlatformPolicy{}, false
}

func prebuiltArchiveFile(files []catalog.PrebuiltArchiveFile, tag string) (catalog.PrebuiltArchiveFile, bool) {
	for _, file := range files {
		if file.Tag == tag {
			return file, true
		}
	}
	return catalog.PrebuiltArchiveFile{}, false
}

func formulaHasDependencies(formula catalog.Formula) bool {
	if len(formula.Dependencies) != 0 {
		return true
	}
	for _, variation := range formula.Variations {
		if len(variation.Dependencies) != 0 {
			return true
		}
	}
	return false
}

func hasNativeBottleForTag(bottle *catalog.BottleDeclaration, tag string) bool {
	return bottle != nil && (hasBottleTag(bottle.Files, tag) || hasBottleTag(bottle.Files, "all"))
}

func externalBottleUnavailableError(bottle *catalog.BottleDeclaration, tag string) error {
	if bottle == nil {
		return errors.New("stable bottle metadata is unavailable")
	}
	return fmt.Errorf("stable bottle tag %s is unavailable", tag)
}

func validateRackCollisions(nodes map[catalog.FormulaID]catalog.Node) error {
	byRack := map[string]catalog.FormulaID{}
	for id, node := range nodes {
		if previous, collision := byRack[node.Name]; collision && previous != id {
			return fmt.Errorf("Formula IDs %s and %s share Cellar rack %q", previous, id, node.Name)
		}
		byRack[node.Name] = id
	}
	return nil
}

func topo(graph map[catalog.FormulaID][]catalog.Requirement, roots []catalog.FormulaID) ([]catalog.FormulaID, error) {
	state := map[catalog.FormulaID]uint8{}
	var order, stack []catalog.FormulaID
	var visit func(catalog.FormulaID) error
	visit = func(id catalog.FormulaID) error {
		switch state[id] {
		case 1:
			index := slices.Index(stack, id)
			if index < 0 {
				index = 0
			}
			cycle := append(slices.Clone(stack[index:]), id)
			parts := make([]string, len(cycle))
			for i := range cycle {
				parts[i] = string(cycle[i])
			}
			return fmt.Errorf("dependency cycle: %s", strings.Join(parts, " -> "))
		case 2:
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		for _, dependency := range graph[id] {
			if err := visit(dependency.ID); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		order = append(order, id)
		return nil
	}
	for _, root := range roots {
		if err := visit(root); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func migrationsAsMappings(values []catalog.Migration) []catalog.ScopedMapping {
	result := make([]catalog.ScopedMapping, len(values))
	for i, migration := range values {
		result[i] = catalog.ScopedMapping{From: migration.From, To: migration.To}
	}
	return result
}

func bottleTag(platform catalog.Platform) string {
	if platform.Architecture == "arm64" {
		return "arm64_linux"
	}
	return "x86_64_linux"
}

func pkgVersion(version string, revision int) string {
	if revision > 0 {
		return fmt.Sprintf("%s_%d", version, revision)
	}
	return version
}

func hasBottleTag(files []catalog.BottleFile, tag string) bool {
	for _, file := range files {
		if file.Tag == tag {
			return true
		}
	}
	return false
}
