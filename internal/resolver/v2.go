package resolver

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
)

type V2RequestedRoot struct {
	Requested string
	ID        catalog.FormulaID
}

type V2Options struct {
	SpecDigest        string
	TargetKey         string
	ResolvedAt        time.Time
	CoreMetadata      metadata.SnapshotInfo
	CatalogPayload    *catalog.CatalogSetPayload
	Catalogs          map[catalog.TapID]*catalog.TapCatalog
	CatalogSigner     resolution.Signature
	Components        resolution.ComponentsV2
	Runtime           resolution.RuntimePolicy
	SequenceFloors    map[catalog.TapID]uint64
	CoreRollbackFloor uint64
	SetPayloadDigest  string
	SetEnvelopeDigest string
}

// RecordV2FromCatalog converts one independently verified catalog platform
// result into the replay record consumed by V2 transport and materialization.
func RecordV2FromCatalog(result catalog.PlatformResult, requested []V2RequestedRoot, opts V2Options) (*resolution.RecordV2, error) {
	if err := catalog.ValidatePlatformResult(result); err != nil {
		return nil, err
	}
	if opts.CatalogPayload == nil {
		return nil, errors.New("catalog-set payload is required")
	}
	if opts.ResolvedAt.IsZero() {
		opts.ResolvedAt = time.Now().UTC().Round(0)
	}
	if opts.CatalogSigner.KeyID == "" || opts.CatalogSigner.Algorithm != "PS512" || !opts.CatalogSigner.Verified {
		return nil, errors.New("verified catalog signer is required")
	}
	if result.Platform.OS != "linux" || (result.Platform.Architecture != "amd64" && result.Platform.Architecture != "arm64") {
		return nil, errors.New("unsupported V2 platform result")
	}

	nodesByID := make(map[catalog.FormulaID]catalog.Node, len(result.Closure.Nodes))
	for _, node := range result.Closure.Nodes {
		nodesByID[node.ID] = node
	}
	artifacts := make(map[catalog.FormulaID]catalog.BottleArtifact, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		artifacts[artifact.ID] = artifact
	}

	formulaSources := make(map[catalog.FormulaID]string)
	for _, document := range opts.Catalogs {
		for _, formula := range document.Formulae {
			formulaSources[formula.ID] = formula.SourceDigest
		}
	}
	nodes := make([]resolution.NodeV2, 0, len(result.Closure.Nodes))
	for _, node := range result.Closure.Nodes {
		artifact, ok := artifacts[node.ID]
		if !ok {
			return nil, fmt.Errorf("Formula %s has no selected bottle artifact", node.ID)
		}
		if !node.ID.IsCore() {
			sourceDigest, present := formulaSources[node.ID]
			if !present || artifact.CurrentFormulaSourceDigest != sourceDigest {
				return nil, fmt.Errorf("Formula %s current source digest does not match its signed catalog", node.ID)
			}
		}
		converted, err := convertCatalogNodeV2(node, artifact, nodesByID)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, converted)
	}

	requestedV2 := make([]resolution.RequestedRootV2, 0, len(requested))
	seenRequested := map[catalog.FormulaID]struct{}{}
	for _, root := range requested {
		if _, duplicate := seenRequested[root.ID]; duplicate {
			return nil, fmt.Errorf("duplicate requested Formula ID %s", root.ID)
		}
		seenRequested[root.ID] = struct{}{}
		node, ok := nodesByID[root.ID]
		if !ok {
			return nil, fmt.Errorf("requested Formula ID %s is absent from closure", root.ID)
		}
		requestedV2 = append(requestedV2, resolution.RequestedRootV2{Requested: root.Requested, ID: resolution.FormulaID(root.ID), KegOnly: node.KegOnly})
	}
	if len(requestedV2) == 0 {
		return nil, errors.New("V2 requested roots are empty")
	}

	metadataSources, earliest, err := metadataSourcesV2(opts)
	if err != nil {
		return nil, err
	}
	installOrder := make([]resolution.FormulaID, len(result.Closure.InstallOrder))
	for i, id := range result.Closure.InstallOrder {
		installOrder[i] = resolution.FormulaID(id)
	}
	writable := make([]string, 0, len(nodes))
	for _, node := range nodes {
		writable = append(writable, runtimefs.DefaultInstallPrefix+"/var/"+node.Name)
	}
	slices.Sort(writable)
	opts.Runtime.WritablePaths = writable

	record := &resolution.RecordV2{
		SchemaVersion: resolution.SchemaVersionV2, PolicyVersion: resolution.PolicyVersionV2,
		Input:           resolution.Input{DalecSpecDigest: opts.SpecDigest, TargetKey: opts.TargetKey, Platform: resolution.Platform{OS: result.Platform.OS, Architecture: result.Platform.Architecture, Variant: result.Platform.Variant}},
		MetadataSources: metadataSources, ResolvedAt: opts.ResolvedAt.UTC().Round(0), SourceDateEpoch: earliest,
		Requested: requestedV2, Nodes: nodes, InstallOrder: installOrder, Components: opts.Components, Runtime: opts.Runtime,
	}
	if err := resolution.ValidateV2(record); err != nil {
		return nil, err
	}
	return record, nil
}

func convertCatalogNodeV2(node catalog.Node, artifact catalog.BottleArtifact, nodes map[catalog.FormulaID]catalog.Node) (resolution.NodeV2, error) {
	deps := make([]resolution.RequirementV2, len(node.Dependencies))
	for i, requirement := range node.Dependencies {
		dependency, ok := nodes[requirement.ID]
		if !ok {
			return resolution.NodeV2{}, fmt.Errorf("Formula %s dependency %s is missing", node.ID, requirement.ID)
		}
		minimum := requirement.MinimumPkgVersion
		if minimum == "" {
			minimum = dependency.PkgVersion
		}
		deps[i] = resolution.RequirementV2{ID: resolution.FormulaID(requirement.ID), Minimum: minimum, Revision: requirement.MinimumRevision, BottleRebuild: requirement.MinimumBottleRebuild, Direct: requirement.DeclaredDirectly}
	}
	slices.SortFunc(deps, func(a, b resolution.RequirementV2) int { return strings.Compare(a.ID.String(), b.ID.String()) })

	tabDependencies := make([]resolution.RuntimeDependencyV2, len(artifact.Tab.Dependencies))
	for i, dependency := range artifact.Tab.Dependencies {
		tabDependencies[i] = resolution.RuntimeDependencyV2{ID: resolution.FormulaID(dependency.ID), HomebrewFullName: resolution.FormulaID(dependency.HomebrewFullName), Version: dependency.Version, Revision: dependency.Revision, BottleRebuild: dependency.BottleRebuild, PkgVersion: dependency.PkgVersion, DeclaredDirectly: dependency.DeclaredDirectly}
	}
	bottle := resolution.BottleV2{
		Tag: artifact.Tag, Filename: artifact.Filename, Size: artifact.Size, SHA256: artifact.SHA256, Cellar: artifact.Cellar,
		CurrentFormulaSourceDigest: artifact.CurrentFormulaSourceDigest, BottleFormulaSourceDigest: artifact.BottleFormulaSourceDigest,
		BottleSourceRepository: artifact.BottleSourceRepository, BottleSourceCommit: artifact.BottleSourceCommit, BottleFormulaPath: artifact.BottleFormulaPath, BottleSourceWaiver: artifact.BottleSourceWaiver,
		Verification: resolution.BottleVerificationV2{PolicyVersion: artifact.Verification.PolicyVersion, InventoryDigest: artifact.Verification.InventoryDigest, EntryCount: artifact.Verification.EntryCount, ExpandedSize: artifact.Verification.ExpandedSize},
		Tab:          resolution.BottleTabV2{Receiptless: artifact.Tab.Receiptless, HomebrewVersion: artifact.Tab.HomebrewVersion, Arch: artifact.Tab.Arch, Compiler: artifact.Tab.Compiler, ChangedFiles: slices.Clone(artifact.Tab.ChangedFiles), BuiltOn: resolution.BuiltOn{OS: artifact.Tab.BuiltOn.OS, OSVersion: artifact.Tab.BuiltOn.OSVersion, CPUFamily: artifact.Tab.BuiltOn.CPUFamily, OldestCPUFamily: artifact.Tab.BuiltOn.OldestCPUFamily, GlibcVersion: artifact.Tab.BuiltOn.GlibcVersion}, Dependencies: tabDependencies},
	}
	if artifact.Transport.OCI != nil {
		transport := artifact.Transport.OCI
		bottle.Transport.OCI = &resolution.OCITransport{Registry: transport.Registry, Repository: transport.Repository, Index: convertCatalogDescriptor(transport.Index), Manifest: convertCatalogDescriptor(transport.Manifest), Config: convertCatalogDescriptor(transport.Config), Layer: convertCatalogDescriptor(transport.Layer)}
		for _, annotation := range transport.Manifest.Annotations {
			bottle.SelectedAnnotations = append(bottle.SelectedAnnotations, resolution.KV{Key: annotation.Key, Value: annotation.Value})
		}
	}
	if artifact.Transport.HTTPS != nil {
		bottle.Transport.HTTPS = convertCatalogHTTPSTransportV2(*artifact.Transport.HTTPS)
	}
	if artifact.PrebuiltDerivation != nil {
		bottle.PrebuiltDerivation = convertCatalogPrebuiltDerivationV2(*artifact.PrebuiltDerivation)
	}
	provenance := resolution.Provenance{}
	if artifact.Provenance.Verified != nil {
		value := artifact.Provenance.Verified
		provenance.Verified = &resolution.VerifiedProvenance{PolicyVersion: value.PolicyVersion, SubjectDigest: value.SubjectDigest, StatementDigest: value.StatementDigest, BundleDigest: value.BundleDigest, SignerIdentity: value.SignerIdentity, Issuer: value.Issuer}
	}
	if artifact.Provenance.Waiver != nil {
		provenance.Waiver = &resolution.ProvenanceWaiver{Policy: artifact.Provenance.Waiver.Policy}
	}
	return resolution.NodeV2{ID: resolution.FormulaID(node.ID), Tap: resolution.TapID(node.Tap), Name: node.Name, HomebrewFullName: resolution.FormulaID(node.HomebrewFullName), FormulaVersion: node.FormulaVersion, FormulaRevision: node.FormulaRevision, PkgVersion: node.PkgVersion, VersionScheme: node.VersionScheme, BottleRebuild: node.BottleRebuild, License: node.License, KegOnly: node.KegOnly, Dependencies: deps, Bottle: bottle, Provenance: provenance, ExecutablePaths: slices.Clone(artifact.ExecutablePaths)}, nil
}

func convertCatalogPrebuiltDerivationV2(value catalog.PrebuiltDerivation) *resolution.PrebuiltDerivationV2 {
	return &resolution.PrebuiltDerivationV2{
		PolicyVersion: value.PolicyVersion,
		PolicyDigest:  value.PolicyDigest,
		Source: resolution.PrebuiltSourceArtifactV2{
			Filename:  value.Source.Filename,
			Size:      value.Source.Size,
			SHA256:    value.Source.SHA256,
			Format:    value.Source.Format,
			Transport: convertCatalogTransportV2(value.Source.Transport),
		},
		SourceInventory: resolution.PrebuiltSourceInventoryV2{
			InventoryDigest: value.SourceInventory.InventoryDigest,
			EntryCount:      value.SourceInventory.EntryCount,
			ExpandedSize:    value.SourceInventory.ExpandedSize,
		},
		Payload: resolution.PrebuiltPayloadEvidenceV2{
			SourcePath:      value.Payload.SourcePath,
			DestinationPath: value.Payload.DestinationPath,
			SHA256:          value.Payload.SHA256,
			Size:            value.Payload.Size,
			ArchiveMode:     value.Payload.ArchiveMode,
			DerivedMode:     value.Payload.DerivedMode,
		},
		ELF: resolution.PrebuiltELFEvidenceV2{
			Format:                     value.ELF.Format,
			Machine:                    value.ELF.Machine,
			StaticallyLinked:           value.ELF.StaticallyLinked,
			Interpreter:                value.ELF.Interpreter,
			NeededLibraries:            cloneStringsV2(value.ELF.NeededLibraries),
			RPaths:                     cloneStringsV2(value.ELF.RPaths),
			WritableExecutableSegments: value.ELF.WritableExecutableSegments,
		},
		FormulaSource: resolution.PrebuiltFormulaSourceEvidenceV2{
			Transport: resolution.TapFormulaSourceTransportV2{
				Tap: resolution.TapSourceV2{
					ID:            resolution.TapID(value.FormulaSource.Transport.Tap.ID),
					Repository:    value.FormulaSource.Transport.Tap.Repository,
					Commit:        value.FormulaSource.Transport.Tap.Commit,
					TreeDigest:    value.FormulaSource.Transport.Tap.TreeDigest,
					ArchiveDigest: value.FormulaSource.Transport.Tap.ArchiveDigest,
				},
				Path: value.FormulaSource.Transport.Path,
			},
			SHA256: value.FormulaSource.SHA256,
			Size:   value.FormulaSource.Size,
		},
		RecipeDigest: value.RecipeDigest,
		DerivedBottle: resolution.PrebuiltDerivedBottleRelationV2{
			Tag:                 value.DerivedBottle.Tag,
			Filename:            value.DerivedBottle.Filename,
			SHA256:              value.DerivedBottle.SHA256,
			Size:                value.DerivedBottle.Size,
			Verification:        convertCatalogBottleVerificationV2(value.DerivedBottle.Verification),
			FormulaSourceDigest: value.DerivedBottle.FormulaSourceDigest,
		},
	}
}

func convertCatalogTransportV2(value catalog.Transport) resolution.BottleTransport {
	var result resolution.BottleTransport
	if value.OCI != nil {
		result.OCI = &resolution.OCITransport{Registry: value.OCI.Registry, Repository: value.OCI.Repository, Index: convertCatalogDescriptor(value.OCI.Index), Manifest: convertCatalogDescriptor(value.OCI.Manifest), Config: convertCatalogDescriptor(value.OCI.Config), Layer: convertCatalogDescriptor(value.OCI.Layer)}
	}
	if value.HTTPS != nil {
		result.HTTPS = convertCatalogHTTPSTransportV2(*value.HTTPS)
	}
	return result
}

func convertCatalogHTTPSTransportV2(value catalog.HTTPSTransport) *resolution.HTTPSTransport {
	return &resolution.HTTPSTransport{URL: value.URL, ExpectedSize: value.ExpectedSize, SHA256: value.SHA256, Filename: value.Filename, AllowedRedirectHosts: slices.Clone(value.AllowedRedirectHosts), FetchPolicyVersion: value.FetchPolicyVersion}
}

func convertCatalogBottleVerificationV2(value catalog.BottleVerification) resolution.BottleVerificationV2 {
	return resolution.BottleVerificationV2{PolicyVersion: value.PolicyVersion, InventoryDigest: value.InventoryDigest, EntryCount: value.EntryCount, ExpandedSize: value.ExpandedSize}
}

func cloneStringsV2(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func convertCatalogDescriptor(value catalog.Descriptor) resolution.Descriptor {
	metadataValues := make(map[string]string, len(value.Annotations))
	for _, annotation := range value.Annotations {
		metadataValues[annotation.Key] = annotation.Value
	}
	var platform *resolution.Platform
	if value.Platform != nil {
		platform = &resolution.Platform{OS: value.Platform.OS, Architecture: value.Platform.Architecture, Variant: value.Platform.Variant}
	}
	return resolution.Descriptor{Digest: value.Digest, Size: value.Size, MediaType: value.MediaType, Platform: platform, Metadata: metadataValues}
}

func metadataSourcesV2(opts V2Options) ([]resolution.MetadataSource, int64, error) {
	info := opts.CoreMetadata
	if info.GeneratedAt.IsZero() || info.FetchedAt.IsZero() {
		return nil, 0, errors.New("authenticated core metadata timestamps are required")
	}
	coreSignature := resolution.Signature{}
	for _, signature := range info.Formula.Signatures {
		if signature.Verified {
			coreSignature = resolution.Signature{KeyID: signature.KeyID, Algorithm: signature.Algorithm, Verified: true}
			break
		}
	}
	if coreSignature.KeyID == "" {
		return nil, 0, errors.New("authenticated core metadata signer is missing")
	}
	coreSequence := uint64(info.GeneratedAt.Unix())
	core := resolution.MetadataSource{Tap: "homebrew/core", Commit: opts.Components.HomebrewCommit, Signer: coreSignature, GeneratedAt: info.GeneratedAt, FetchedAt: info.FetchedAt, Sequence: coreSequence, Rollback: resolution.RollbackEvidence{Policy: resolution.CoreMetadataRollbackPolicyV1, SequenceFloor: opts.CoreRollbackFloor, StateDigest: info.Digest}, Documents: []resolution.MetadataDocument{{Name: "formula", Digest: info.FormulaDigest, EnvelopeDigest: info.Formula.EnvelopeDigest}, {Name: "migrations", Digest: info.MigrationDigest, EnvelopeDigest: info.Migrations.EnvelopeDigest}}}
	sources := []resolution.MetadataSource{core}
	earliest := info.GeneratedAt.Unix()
	for name, value := range map[string]string{"catalog-set payload": opts.SetPayloadDigest, "catalog-set envelope": opts.SetEnvelopeDigest} {
		if d, err := digest.Parse(value); err != nil || d.Algorithm() != digest.SHA256 || d.Validate() != nil {
			return nil, 0, fmt.Errorf("%s digest is invalid", name)
		}
	}
	policyVersion := catalog.TapCatalogPolicyVersion
	if !slices.Contains(opts.Components.SupportedCatalogPolicyVersions, policyVersion) {
		return nil, 0, fmt.Errorf("catalog policy %q is absent from the component binding", policyVersion)
	}
	for _, reference := range opts.CatalogPayload.Catalogs {
		document := opts.Catalogs[reference.Tap.ID]
		if document == nil {
			return nil, 0, fmt.Errorf("catalog document %s is missing", reference.Tap.ID)
		}
		generated := reference.PublishedAt
		if generated.Unix() < earliest {
			earliest = generated.Unix()
		}
		sources = append(sources, resolution.MetadataSource{Tap: resolution.TapID(reference.Tap.ID), Commit: reference.Tap.Commit, CatalogPolicyVersion: policyVersion, Signer: opts.CatalogSigner, Documents: []resolution.MetadataDocument{{Name: "catalog", Digest: reference.SHA256}, {Name: "set", Digest: opts.SetPayloadDigest, EnvelopeDigest: opts.SetEnvelopeDigest}}, GeneratedAt: generated, FetchedAt: opts.ResolvedAt, Sequence: reference.Sequence, Rollback: resolution.RollbackEvidence{Policy: resolution.MetadataRollbackPolicyV1, SequenceFloor: opts.SequenceFloors[reference.Tap.ID], StateDigest: reference.SHA256}})
	}
	return sources, earliest, nil
}

// RecordV2FromCore upgrades an independently verified V1 core resolution into
// the V2 identity/component schema without contacting the catalog service.
// Bottle Formula and static-inventory evidence remain explicitly deferred to
// the mandatory prepare phase, matching the existing core trust path.
func RecordV2FromCore(core *resolution.Record, components resolution.ComponentsV2, coreMetadata metadata.SnapshotInfo, coreRollbackFloor uint64) (*resolution.RecordV2, error) {
	if core == nil {
		return nil, errors.New("nil core resolution")
	}
	if err := resolution.Validate(core); err != nil {
		return nil, err
	}
	nodes := make([]resolution.NodeV2, 0, len(core.Nodes))
	for _, node := range core.Nodes {
		id, err := catalog.ParseFormulaID(node.Name)
		if err != nil {
			return nil, err
		}
		dependencies := make([]resolution.RequirementV2, len(node.Dependencies))
		for i, requirement := range node.Dependencies {
			depID, err := catalog.ParseFormulaID(requirement.Name)
			if err != nil {
				return nil, err
			}
			dependencies[i] = resolution.RequirementV2{ID: resolution.FormulaID(depID), Minimum: requirement.Minimum, Revision: requirement.Revision, BottleRebuild: requirement.BottleRebuild, Direct: requirement.Direct}
		}
		tabDependencies := make([]resolution.RuntimeDependencyV2, len(node.Bottle.Tab.Dependencies))
		for i, dependency := range node.Bottle.Tab.Dependencies {
			depID, err := catalog.ParseFormulaID(dependency.FullName)
			if err != nil {
				return nil, err
			}
			tabDependencies[i] = resolution.RuntimeDependencyV2{ID: resolution.FormulaID(depID), HomebrewFullName: resolution.FormulaID(depID), Version: dependency.Version, Revision: dependency.Revision, BottleRebuild: dependency.BottleRebuild, PkgVersion: dependency.PkgVersion, DeclaredDirectly: dependency.DeclaredDirectly}
		}
		var upstream resolution.FormulaID
		if node.UpstreamFormulaID != "" {
			upstreamID, err := catalog.ParseFormulaID(node.UpstreamFormulaID)
			if err != nil {
				return nil, err
			}
			upstream = resolution.FormulaID(upstreamID)
		}
		nodes = append(nodes, resolution.NodeV2{ID: resolution.FormulaID(id), Tap: "homebrew/core", Name: node.Name, HomebrewFullName: resolution.FormulaID(id), FormulaVersion: node.FormulaVersion, FormulaRevision: node.FormulaRevision, PkgVersion: node.PkgVersion, VersionScheme: node.VersionScheme, BottleRebuild: node.BottleRebuild, License: node.License, KegOnly: node.KegOnly, Dependencies: dependencies, Bottle: resolution.BottleV2{Tag: node.Bottle.Tag, Filename: node.Bottle.Filename, Size: node.Bottle.Layer.Size, SHA256: node.Bottle.Layer.Digest, Cellar: node.Bottle.Cellar, CurrentFormulaSourceDigest: coreMetadata.FormulaDigest, Verification: resolution.BottleVerificationV2{PolicyVersion: resolution.CoreBottleVerificationDeferredV1}, Tab: resolution.BottleTabV2{HomebrewVersion: node.Bottle.Tab.HomebrewVersion, Arch: node.Bottle.Tab.Arch, Compiler: node.Bottle.Tab.Compiler, ChangedFiles: slices.Clone(node.Bottle.Tab.ChangedFiles), BuiltOn: node.Bottle.Tab.BuiltOn, Dependencies: tabDependencies}, SelectedAnnotations: slices.Clone(node.Bottle.SelectedAnnotations), Transport: resolution.BottleTransport{OCI: &resolution.OCITransport{Registry: "ghcr.io", Repository: strings.TrimPrefix(node.Bottle.Repository, "ghcr.io/"), Index: node.Bottle.Index, Manifest: node.Bottle.Manifest, Config: node.Bottle.Config, Layer: node.Bottle.Layer}}}, Provenance: resolution.Provenance{Waiver: &resolution.ProvenanceWaiver{Policy: resolution.CoreProvenanceWaiverPolicyV1}}, ExecutablePaths: slices.Clone(node.ExecutablePaths), UpstreamFormulaID: upstream})
	}
	requested := make([]resolution.RequestedRootV2, len(core.Requested))
	for i, root := range core.Requested {
		id, err := catalog.ParseFormulaID(root.Canonical)
		if err != nil {
			return nil, err
		}
		requested[i] = resolution.RequestedRootV2{Requested: root.Requested, ID: resolution.FormulaID(id), KegOnly: root.KegOnly}
	}
	order := make([]resolution.FormulaID, len(core.InstallOrder))
	for i, name := range core.InstallOrder {
		id, err := catalog.ParseFormulaID(name)
		if err != nil {
			return nil, err
		}
		order[i] = resolution.FormulaID(id)
	}
	coreSignature := resolution.Signature{}
	for _, signature := range coreMetadata.Formula.Signatures {
		if signature.Verified {
			coreSignature = resolution.Signature{KeyID: signature.KeyID, Algorithm: signature.Algorithm, Verified: true}
			break
		}
	}
	if coreSignature.KeyID == "" {
		return nil, errors.New("core metadata signer is missing")
	}
	sequence := uint64(coreMetadata.GeneratedAt.Unix())
	source := resolution.MetadataSource{Tap: "homebrew/core", Commit: components.HomebrewCommit, Signer: coreSignature, Documents: []resolution.MetadataDocument{{Name: "formula", Digest: coreMetadata.FormulaDigest, EnvelopeDigest: coreMetadata.Formula.EnvelopeDigest}, {Name: "migrations", Digest: coreMetadata.MigrationDigest, EnvelopeDigest: coreMetadata.Migrations.EnvelopeDigest}}, GeneratedAt: coreMetadata.GeneratedAt, FetchedAt: coreMetadata.FetchedAt, Sequence: sequence, Rollback: resolution.RollbackEvidence{Policy: resolution.CoreMetadataRollbackPolicyV1, SequenceFloor: coreRollbackFloor, StateDigest: coreMetadata.Digest}}
	record := &resolution.RecordV2{SchemaVersion: resolution.SchemaVersionV2, PolicyVersion: resolution.PolicyVersionV2, Input: core.Input, MetadataSources: []resolution.MetadataSource{source}, ResolvedAt: core.ResolvedAt, SourceDateEpoch: core.SourceDateEpoch, Requested: requested, Nodes: nodes, InstallOrder: order, Components: components, Runtime: core.Runtime}
	if err := resolution.ValidateV2(record); err != nil {
		return nil, err
	}
	return record, nil
}
