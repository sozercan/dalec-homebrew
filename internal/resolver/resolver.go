package resolver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/oci"
	hbversion "github.com/sozercan/dalec-homebrew/internal/homebrew/version"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type Catalog interface {
	Lookup(name string) (metadata.Match, error)
}

type BottleResolver interface {
	Resolve(ctx context.Context, formula oci.Formula, target ocispec.Platform) (*oci.Result, error)
}

type Options struct {
	SpecDigest    string
	TargetKey     string
	Now           time.Time
	Metadata      metadata.SnapshotInfo
	Components    resolution.Components
	Runtime       resolution.RuntimePolicy
	Attestation   resolution.AttestationPolicy
	PruningDigest string
}

func Resolve(ctx context.Context, catalog Catalog, bottles BottleResolver, roots []string, target ocispec.Platform, opts Options) (*resolution.Record, error) {
	if catalog == nil || bottles == nil {
		return nil, errors.New("resolver requires metadata catalog and OCI bottle resolver")
	}
	if target.OS != "linux" || (target.Architecture != "amd64" && target.Architecture != "arm64") || target.Variant != "" || len(target.OSFeatures) != 0 {
		return nil, fmt.Errorf("unsupported platform %s/%s", target.OS, target.Architecture)
	}
	if len(roots) == 0 {
		return nil, errors.New("no applicable runtime roots")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}

	requested := make([]resolution.RequestedRoot, 0, len(roots))
	queue := make([]string, 0, len(roots))
	matches := map[string]metadata.Formula{}
	for _, name := range roots {
		match, err := catalog.Lookup(name)
		if err != nil {
			return nil, fmt.Errorf("resolve root %q: %w", name, err)
		}
		if _, duplicate := matches[match.Canonical]; !duplicate {
			queue = append(queue, match.Canonical)
		}
		matches[match.Canonical] = match.Formula
		requested = append(requested, resolution.RequestedRoot{Requested: name, Canonical: match.Canonical, KegOnly: match.Formula.KegOnlyFor(targetTag(target))})
	}

	nodes := map[string]resolution.Node{}
	graph := map[string]map[string]resolution.Requirement{}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, done := nodes[name]; done {
			continue
		}
		formula, ok := matches[name]
		if !ok {
			match, err := catalog.Lookup(name)
			if err != nil {
				return nil, fmt.Errorf("resolve dependency %q: %w", name, err)
			}
			if match.Kind != metadata.MatchCanonical || match.Canonical != name {
				return nil, fmt.Errorf("dependency identity %q is not a canonical Formula", name)
			}
			formula = match.Formula
			matches[name] = formula
		}
		ociFormula, err := toOCIFormula(formula, target)
		if err != nil {
			return nil, err
		}
		selected, err := bottles.Resolve(ctx, ociFormula, target)
		if err != nil {
			return nil, fmt.Errorf("resolve bottle for %q: %w", name, err)
		}
		if err := validateRuntimeCompatibility(selected.Tab, target); err != nil {
			return nil, fmt.Errorf("formula %q runtime compatibility: %w", name, err)
		}
		node := selected.ResolutionNode()
		if node.Name != name || node.PkgVersion != formula.PkgVersion() {
			return nil, fmt.Errorf("OCI resolver returned mismatched identity for %q", name)
		}
		nodes[name] = node
		if graph[name] == nil {
			graph[name] = map[string]resolution.Requirement{}
		}

		// The signed Formula declaration is the package identity authority. The
		// selected bottle tab adds exact build-time minimums and is required to be
		// consistent, but cannot silently remove a signed direct dependency.
		signedDeps := formula.DependenciesFor(targetTag(target))
		tabDeps := make(map[string]resolution.Requirement, len(node.Dependencies))
		for _, req := range node.Dependencies {
			tabDeps[req.Name] = req
		}
		for _, dep := range signedDeps {
			match, err := catalog.Lookup(dep)
			if err != nil {
				return nil, fmt.Errorf("formula %q signed dependency %q: %w", name, dep, err)
			}
			if match.Kind != metadata.MatchCanonical || match.Canonical != dep {
				return nil, fmt.Errorf("formula %q dependency %q is not canonical (maps to %q)", name, dep, match.Canonical)
			}
			canonical := match.Canonical
			matches[canonical] = match.Formula
			req, exists := tabDeps[canonical]
			req.Direct = true
			if !exists {
				req = resolution.Requirement{Name: canonical, Direct: true}
			}
			graph[name][canonical] = mergeRequirement(graph[name][canonical], req)
			if _, done := nodes[canonical]; !done {
				queue = append(queue, canonical)
			}
		}
		// The complete tab remains in Bottle.Tab as evidence. Historical transitive
		// entries are not current graph edges and need not exist in the current catalog.
	}

	// Fill missing signed-only minimums with the selected current PkgVersion and
	// verify every tab minimum against the one selected current Formula.
	for parent, deps := range graph {
		for name, req := range deps {
			depNode, ok := nodes[name]
			if !ok {
				return nil, fmt.Errorf("dependency %q of %q was not resolved", name, parent)
			}
			if req.Minimum == "" {
				req.Minimum = depNode.PkgVersion
				req.Revision = depNode.FormulaRevision
				req.BottleRebuild = depNode.BottleRebuild
			}
			if !satisfies(depNode, req) {
				return nil, fmt.Errorf("%s requires %s >= %s (revision %d, rebuild %d), selected %s (revision %d, rebuild %d)", parent, name, req.Minimum, req.Revision, req.BottleRebuild, depNode.PkgVersion, depNode.FormulaRevision, depNode.BottleRebuild)
			}
			deps[name] = req
		}
		n := nodes[parent]
		n.Dependencies = mapRequirements(deps)
		nodes[parent] = n
	}

	order, err := topoOrder(graph, requested)
	if err != nil {
		return nil, err
	}
	nodeList := make([]resolution.Node, 0, len(nodes))
	for _, n := range nodes {
		nodeList = append(nodeList, n)
	}
	slices.SortFunc(nodeList, func(a, b resolution.Node) int { return strings.Compare(a.Name, b.Name) })

	meta := resolution.MetadataSnapshot{
		Digest: opts.Metadata.Digest, FormulaDigest: opts.Metadata.FormulaDigest, MigrationDigest: opts.Metadata.MigrationDigest,
		FormulaEnvelopeDigest: opts.Metadata.Formula.EnvelopeDigest, MigrationEnvelopeDigest: opts.Metadata.Migrations.EnvelopeDigest,
		FormulaFreshnessSource: string(opts.Metadata.Formula.GeneratedAtSource), MigrationFreshnessSource: string(opts.Metadata.Migrations.GeneratedAtSource),
		GeneratedAt: opts.Metadata.GeneratedAt, FetchedAt: opts.Metadata.FetchedAt,
		FormulaURL: opts.Metadata.Formula.URL, MigrationURL: opts.Metadata.Migrations.URL,
	}
	seenSigs := map[string]struct{}{}
	for docIndex, doc := range []metadata.DocumentInfo{opts.Metadata.Formula, opts.Metadata.Migrations} {
		for _, sig := range doc.Signatures {
			converted := resolution.Signature{KeyID: sig.KeyID, Algorithm: sig.Algorithm, Verified: sig.Verified}
			if docIndex == 0 {
				meta.FormulaSignatures = append(meta.FormulaSignatures, converted)
			} else {
				meta.MigrationSignatures = append(meta.MigrationSignatures, converted)
			}
			key := sig.KeyID + "\x00" + sig.Algorithm
			if _, ok := seenSigs[key]; ok {
				continue
			}
			seenSigs[key] = struct{}{}
			meta.Signatures = append(meta.Signatures, converted)
		}
	}
	epoch := opts.Metadata.GeneratedAt.Unix()
	if epoch <= 0 {
		return nil, errors.New("metadata snapshot has no usable source date epoch")
	}
	r := &resolution.Record{
		SchemaVersion: resolution.SchemaVersion, PolicyVersion: resolution.PolicyVersion,
		Input:    resolution.Input{DalecSpecDigest: opts.SpecDigest, TargetKey: opts.TargetKey, Platform: resolution.Platform{OS: target.OS, Architecture: target.Architecture, Variant: target.Variant}},
		Metadata: meta, ResolvedAt: opts.Now.UTC().Round(0), SourceDateEpoch: epoch,
		Requested: requested, Nodes: nodeList, InstallOrder: order,
		Components: opts.Components, Runtime: opts.Runtime, AttestationPolicy: opts.Attestation, PruningPolicyDigest: opts.PruningDigest,
	}
	if err := resolution.Validate(r); err != nil {
		return nil, fmt.Errorf("validate generated resolution: %w", err)
	}
	return r, nil
}

func toOCIFormula(f metadata.Formula, target ocispec.Platform) (oci.Formula, error) {
	if f.Bottle == nil {
		return oci.Formula{}, fmt.Errorf("formula %q has no current bottle", f.Name)
	}
	if f.Bottle.RootURL != "https://ghcr.io/v2/homebrew/core" {
		return oci.Formula{}, fmt.Errorf("formula %q uses unsupported bottle root %q", f.Name, f.Bottle.RootURL)
	}
	files := make(map[string]oci.BottleFile, len(f.Bottle.Files))
	for _, file := range f.Bottle.Files {
		files[file.Tag] = oci.BottleFile{Cellar: file.Cellar, SHA256: file.SHA256}
	}
	return oci.Formula{Name: f.Name, FullName: f.FullName, StableVersion: f.StableVersion, Revision: f.Revision, VersionScheme: f.VersionScheme, BottleRebuild: f.Bottle.Rebuild, License: f.License, KegOnly: f.KegOnlyFor(targetTag(target)), BottleFiles: files}, nil
}

func targetTag(p ocispec.Platform) string {
	if p.Architecture == "arm64" {
		return oci.BottleTagARM64Linux
	}
	return oci.BottleTagX8664Linux
}

func mergeRequirement(a, b resolution.Requirement) resolution.Requirement {
	if a.Name == "" {
		return b
	}
	if b.Name == "" {
		return a
	}
	out := a
	out.Direct = a.Direct || b.Direct
	as, ar, _ := hbversion.SplitPkgVersion(a.Minimum)
	bs, br, _ := hbversion.SplitPkgVersion(b.Minimum)
	if a.Minimum == "" || hbversion.Compare(bs, as) > 0 || (hbversion.Compare(bs, as) == 0 && br > ar) {
		out.Minimum = b.Minimum
		out.Revision = b.Revision
		out.BottleRebuild = b.BottleRebuild
	}
	return out
}

func satisfies(node resolution.Node, req resolution.Requirement) bool {
	minVersion, minRevision, err := hbversion.SplitPkgVersion(req.Minimum)
	if err != nil {
		return false
	}
	if !hbversion.AtLeast(node.FormulaVersion, node.FormulaRevision, minVersion, max(req.Revision, minRevision)) {
		return false
	}
	if hbversion.Compare(node.FormulaVersion, minVersion) == 0 && node.FormulaRevision == max(req.Revision, minRevision) {
		return node.BottleRebuild >= req.BottleRebuild
	}
	return true
}

func mapRequirements(in map[string]resolution.Requirement) []resolution.Requirement {
	out := make([]resolution.Requirement, 0, len(in))
	for _, r := range in {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b resolution.Requirement) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func topoOrder(graph map[string]map[string]resolution.Requirement, roots []resolution.RequestedRoot) ([]string, error) {
	state := map[string]uint8{}
	var order, stack []string
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			i := slices.Index(stack, name)
			if i < 0 {
				i = 0
			}
			return fmt.Errorf("dependency cycle: %s", strings.Join(append(append([]string(nil), stack[i:]...), name), " -> "))
		case 2:
			return nil
		}
		state[name] = 1
		stack = append(stack, name)
		var deps []string
		for dep := range graph[name] {
			deps = append(deps, dep)
		}
		slices.Sort(deps)
		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = 2
		order = append(order, name)
		return nil
	}
	for _, root := range roots {
		if err := visit(root.Canonical); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func validateRuntimeCompatibility(tab resolution.BottleTab, target ocispec.Platform) error {
	if glibc := tab.BuiltOn.GlibcVersion; glibc != "" && hbversion.Compare(glibc, "2.39") > 0 {
		return fmt.Errorf("requires glibc %s, runtime baseline is 2.39", glibc)
	}
	if tab.Arch == "" {
		return nil
	} // Homebrew all bottle
	expectedArch := "x86_64"
	if target.Architecture == "arm64" {
		expectedArch = "arm64"
	}
	if tab.Arch != expectedArch {
		return fmt.Errorf("bottle tab architecture %q does not match target %q", tab.Arch, expectedArch)
	}
	oldest := strings.ToLower(tab.BuiltOn.OldestCPUFamily)
	if target.Architecture == "arm64" {
		if oldest != "" && oldest != "armv8" && oldest != "arm" {
			return fmt.Errorf("requires CPU family %q, baseline is armv8", oldest)
		}
		return nil
	}
	if oldest == "" {
		return nil
	}
	rank := map[string]int{"x86_64": 0, "core2": 1, "penryn": 2, "nehalem": 3, "westmere": 4, "sandybridge": 5, "ivybridge": 6, "haswell": 7, "broadwell": 8, "skylake": 9, "zen": 10, "zen2": 11, "zen3": 12, "zen4": 13}
	r, ok := rank[oldest]
	if !ok {
		return fmt.Errorf("unknown CPU family %q", oldest)
	}
	if r > rank["core2"] {
		return fmt.Errorf("requires CPU family %q, baseline is core2", oldest)
	}
	return nil
}
