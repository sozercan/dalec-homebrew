package resolution

import (
	"errors"
	"fmt"
	"strings"
)

// ProjectV2ForRuntime creates a rack-keyed compatibility view for mature
// filesystem/install helpers. It is not a serialized resolution record and
// must never be accepted as V1 input. Callers must validate and digest the
// original RecordV2 independently.
func ProjectV2ForRuntime(record *RecordV2) (*Record, map[FormulaID]string, error) {
	if record == nil {
		return nil, nil, errors.New("nil V2 resolution")
	}
	if err := ValidateV2(record); err != nil {
		return nil, nil, err
	}
	byID := make(map[FormulaID]NodeV2, len(record.Nodes))
	rackByID := make(map[FormulaID]string, len(record.Nodes))
	seenRacks := make(map[string]FormulaID, len(record.Nodes))
	for _, node := range record.Nodes {
		if previous, collision := seenRacks[node.Name]; collision && previous != node.ID {
			return nil, nil, fmt.Errorf("V2 rack %q is shared by %q and %q", node.Name, previous, node.ID)
		}
		seenRacks[node.Name] = node.ID
		byID[node.ID] = node
		rackByID[node.ID] = node.Name
	}
	nodes := make([]Node, 0, len(record.Nodes))
	for _, node := range record.Nodes {
		dependencies := make([]Requirement, len(node.Dependencies))
		for i, requirement := range node.Dependencies {
			dependency, ok := byID[requirement.ID]
			if !ok {
				return nil, nil, fmt.Errorf("missing projected dependency %q", requirement.ID)
			}
			dependencies[i] = Requirement{Name: dependency.Name, Minimum: requirement.Minimum, Revision: requirement.Revision, BottleRebuild: requirement.BottleRebuild, Direct: requirement.Direct}
		}
		tabDependencies := make([]RuntimeDependency, len(node.Bottle.Tab.Dependencies))
		for i, dependency := range node.Bottle.Tab.Dependencies {
			// Bottle tabs describe the dependency identities used when the bottle
			// was built. Those historical dependencies can legitimately be absent
			// from the current closure, so preserve the authenticated full identity
			// instead of projecting it through the current rack-name index.
			tabDependencies[i] = RuntimeDependency{FullName: dependency.HomebrewFullName.String(), Version: dependency.Version, Revision: dependency.Revision, BottleRebuild: dependency.BottleRebuild, PkgVersion: dependency.PkgVersion, DeclaredDirectly: dependency.DeclaredDirectly}
		}
		layer := Descriptor{Digest: node.Bottle.SHA256, Size: node.Bottle.Size, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"}
		repository := "https-bottle.invalid/" + strings.ReplaceAll(node.ID.String(), "/", "-")
		var index, manifest, config Descriptor
		if node.Bottle.Transport.OCI != nil {
			repository = node.Bottle.Transport.OCI.Repository
			index, manifest, config, layer = node.Bottle.Transport.OCI.Index, node.Bottle.Transport.OCI.Manifest, node.Bottle.Transport.OCI.Config, node.Bottle.Transport.OCI.Layer
		}
		nodes = append(nodes, Node{Name: node.Name, FullName: node.HomebrewFullName.String(), FormulaVersion: node.FormulaVersion, FormulaRevision: node.FormulaRevision, PkgVersion: node.PkgVersion, VersionScheme: node.VersionScheme, BottleRebuild: node.BottleRebuild, License: node.License, KegOnly: node.KegOnly, Dependencies: dependencies, Bottle: Bottle{Tag: node.Bottle.Tag, Filename: node.Bottle.Filename, Repository: repository, Index: index, Manifest: manifest, Config: config, Layer: layer, HomebrewSHA256: node.Bottle.SHA256, Cellar: node.Bottle.Cellar, Tab: BottleTab{HomebrewVersion: node.Bottle.Tab.HomebrewVersion, Arch: node.Bottle.Tab.Arch, Compiler: node.Bottle.Tab.Compiler, ChangedFiles: append([]string(nil), node.Bottle.Tab.ChangedFiles...), BuiltOn: node.Bottle.Tab.BuiltOn, Dependencies: tabDependencies}}, ExecutablePaths: append([]string(nil), node.ExecutablePaths...), UpstreamFormulaID: node.UpstreamFormulaID.String(), PolicyFormulaID: node.ID.String()})
	}
	requested := make([]RequestedRoot, len(record.Requested))
	for i, root := range record.Requested {
		requested[i] = RequestedRoot{Requested: root.Requested, Canonical: rackByID[root.ID], KegOnly: root.KegOnly}
	}
	order := make([]string, len(record.InstallOrder))
	for i, id := range record.InstallOrder {
		order[i] = rackByID[id]
	}
	projected := &Record{SchemaVersion: SchemaVersionV1, PolicyVersion: PolicyVersionV2, Input: record.Input, ResolvedAt: record.ResolvedAt, SourceDateEpoch: record.SourceDateEpoch, Requested: requested, Nodes: nodes, InstallOrder: order, Runtime: record.Runtime, PruningPolicyDigest: record.PruningPolicyDigest}
	return projected, rackByID, nil
}
