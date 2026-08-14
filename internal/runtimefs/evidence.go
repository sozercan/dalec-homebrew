package runtimefs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func buildInventory(scan *sourceScan, record *resolution.Record, policy *normalizedPolicy, resolutionDigest string) Inventory {
	entries := make([]InventoryEntry, 0, len(scan.retained))
	for _, entry := range scan.retained {
		inventoryType := entry.typeName
		item := InventoryEntry{
			Path:       entry.rel,
			Type:       inventoryType,
			Mode:       modeString(entry.desiredMode),
			UID:        entry.uid,
			GID:        entry.gid,
			MTime:      record.SourceDateEpoch,
			Size:       entry.size,
			SHA256:     entry.sha256,
			LinkTarget: entry.linkOutput,
			HardlinkTo: entry.hardlinkTo,
			Package:    entry.packageName,
			FormulaID:  formulaIDForRack(record, entry.packageName),
			Writable:   entry.writable,
		}
		if item.Type == TypeDirectory {
			item.Size = 0
			item.SHA256 = ""
		} else if item.Type == TypeSymlink {
			item.Size = int64(len(item.LinkTarget))
			item.SHA256 = sha256String(item.LinkTarget)
		}
		entries = append(entries, item)
	}
	slices.SortFunc(entries, func(a, b InventoryEntry) int { return strings.Compare(a.Path, b.Path) })
	return Inventory{
		SchemaVersion:       inventorySchemaVersion(record),
		PolicyVersion:       record.PolicyVersion,
		ResolutionDigest:    resolutionDigest,
		PruningPolicyDigest: policy.digest,
		SourceDateEpoch:     record.SourceDateEpoch,
		Prefix:              policy.installPrefix,
		Entries:             entries,
	}
}

func isV2RuntimeRecord(record *resolution.Record) bool {
	return record != nil && record.PolicyVersion == resolution.PolicyVersionV2
}

func inventorySchemaVersion(record *resolution.Record) string {
	if isV2RuntimeRecord(record) {
		return InventorySchemaVersionV2
	}
	return InventorySchemaVersion
}

func pruneSchemaVersion(record *resolution.Record) string {
	if isV2RuntimeRecord(record) {
		return PruneSchemaVersionV2
	}
	return PruneSchemaVersion
}

func manifestSchemaVersion(record *resolution.Record) string {
	if isV2RuntimeRecord(record) {
		return ManifestSchemaVersionV2
	}
	return ManifestSchemaVersion
}

func formulaIDForNode(record *resolution.Record, node resolution.Node) string {
	if !isV2RuntimeRecord(record) {
		return ""
	}
	return node.FullName
}

func formulaIDForRack(record *resolution.Record, rack string) string {
	if !isV2RuntimeRecord(record) || rack == "" {
		return ""
	}
	for _, node := range record.Nodes {
		if node.Name == rack {
			return formulaIDForNode(record, node)
		}
	}
	return ""
}

const homebrewRepositoryRoot = "Homebrew"

type pruneSubtreeCommitmentV1 struct {
	SchemaVersion string                        `json:"schema_version"`
	Root          string                        `json:"root"`
	Reason        PruneReason                   `json:"reason"`
	EntryCount    int                           `json:"entry_count"`
	RegularBytes  int64                         `json:"regular_bytes"`
	Entries       []pruneSubtreeCommitmentEntry `json:"entries"`
}

type pruneSubtreeCommitmentV2 struct {
	SchemaVersion string                        `json:"schema_version"`
	Root          string                        `json:"root"`
	Reason        PruneReason                   `json:"reason"`
	Package       string                        `json:"package"`
	FormulaID     string                        `json:"formula_id"`
	EntryCount    int                           `json:"entry_count"`
	RegularBytes  int64                         `json:"regular_bytes"`
	Entries       []pruneSubtreeCommitmentEntry `json:"entries"`
}

type pruneSubtreeCommitmentEntry struct {
	Path           string    `json:"path"`
	Type           EntryType `json:"type"`
	Mode           string    `json:"mode"`
	Size           int64     `json:"size"`
	ContentSHA256  string    `json:"content_sha256,omitempty"`
	LinkTarget     string    `json:"link_target,omitempty"`
	HardlinkTarget string    `json:"hardlink_target,omitempty"`
}

func buildPruneManifest(scan *sourceScan, record *resolution.Record, policy *normalizedPolicy, resolutionDigest string) (PruneManifest, error) {
	subtree, compacted, err := compactHomebrewRepository(scan, record)
	if err != nil {
		return PruneManifest{}, runtimeError(CodeEvidence, homebrewRepositoryRoot, "commit pruned repository subtree: %v", err)
	}
	var subtrees []PruneSubtree
	if subtree != nil {
		subtrees = append(subtrees, *subtree)
	}
	if isV2RuntimeRecord(record) {
		runtimeSubtrees, runtimeCompacted, err := compactV2RuntimeSubtrees(scan, record, compacted)
		if err != nil {
			return PruneManifest{}, runtimeError(CodeEvidence, "", "commit pruned runtime subtree: %v", err)
		}
		subtrees = append(subtrees, runtimeSubtrees...)
		if compacted == nil {
			compacted = make(map[string]struct{}, len(runtimeCompacted))
		}
		for rel := range runtimeCompacted {
			compacted[rel] = struct{}{}
		}
	}

	entries := make([]PruneEntry, 0, len(scan.pruned))
	for _, entry := range scan.pruned {
		if _, ok := compacted[entry.rel]; ok {
			continue
		}
		entries = append(entries, buildPruneEntry(entry, record))
	}
	slices.SortFunc(entries, func(a, b PruneEntry) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(subtrees, func(a, b PruneSubtree) int { return strings.Compare(a.Path, b.Path) })
	return PruneManifest{
		SchemaVersion:       pruneSchemaVersion(record),
		PolicyVersion:       record.PolicyVersion,
		ResolutionDigest:    resolutionDigest,
		PruningPolicyDigest: policy.digest,
		SourceDateEpoch:     record.SourceDateEpoch,
		Prefix:              policy.installPrefix,
		Subtrees:            subtrees,
		Entries:             entries,
	}, nil
}

func buildPruneEntry(entry *sourceEntry, record *resolution.Record) PruneEntry {
	item := PruneEntry{
		Path:       entry.rel,
		Type:       entry.typeName,
		Mode:       modeString(entry.mode),
		Size:       entry.size,
		SHA256:     entry.sha256,
		LinkTarget: entry.linkSource,
		Reason:     entry.pruneReason,
		Package:    entry.packageName,
		FormulaID:  formulaIDForRack(record, entry.packageName),
	}
	if entry.typeName == TypeRegular {
		item.MetadataExport = entry.metadataExport
	}
	if item.Reason == "" {
		item.Reason = PruneNotAllowlisted
	}
	if item.Type == TypeDirectory {
		item.Size = 0
		item.SHA256 = ""
	}
	if item.MetadataExport != "" && item.SHA256 != "" {
		item.ExportedTo = []string{ManifestFileName}
	}
	return item
}

func compactHomebrewRepository(scan *sourceScan, record *resolution.Record) (*PruneSubtree, map[string]struct{}, error) {
	root := scan.byPath[homebrewRepositoryRoot]
	if root == nil || root.typeName != TypeDirectory || root.retain || root.pruneReason != PruneRepository {
		return nil, nil, nil
	}

	var entries []*sourceEntry
	for _, entry := range scan.entries {
		if !isWithin(entry.rel, homebrewRepositoryRoot) {
			continue
		}
		if entry.retain || entry.pruneReason != PruneRepository || entry.packageName != "" || entry.metadataExport != "" {
			// This is not a uniform, attribution-free repository subtree. Preserve
			// the existing per-file evidence instead of partially compacting it.
			return nil, nil, nil
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, nil, nil
	}

	var subtree PruneSubtree
	var err error
	if isV2RuntimeRecord(record) {
		subtree, err = commitPrunedSubtreeV2(homebrewRepositoryRoot, PruneRepository, "", "", entries)
	} else {
		subtree, err = commitPrunedSubtree(homebrewRepositoryRoot, PruneRepository, entries)
	}
	if err != nil {
		return nil, nil, err
	}
	compacted := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		compacted[entry.rel] = struct{}{}
	}
	return &subtree, compacted, nil
}

func compactV2RuntimeSubtrees(scan *sourceScan, record *resolution.Record, alreadyCompacted map[string]struct{}) ([]PruneSubtree, map[string]struct{}, error) {
	entries := slices.Clone(scan.entries)
	slices.SortFunc(entries, func(a, b *sourceEntry) int { return strings.Compare(a.rel, b.rel) })

	var subtrees []PruneSubtree
	compacted := map[string]struct{}{}
	for i := 0; i < len(entries); i++ {
		root := entries[i]
		if root == nil || root.typeName != TypeDirectory || root.retain || !compactableV2RuntimeReason(root.pruneReason) {
			continue
		}
		if _, exists := alreadyCompacted[root.rel]; exists {
			continue
		}
		if root.packageName == "" || root.metadataExport != "" {
			continue
		}
		formulaID := formulaIDForRack(record, root.packageName)
		if formulaID == "" {
			continue
		}

		end := i + 1
		for end < len(entries) && isWithin(entries[end].rel, root.rel) {
			end++
		}
		// A one-row commitment is larger than its explicit directory entry and
		// provides no compaction benefit.
		if end-i < 2 {
			continue
		}
		uniform := true
		for _, entry := range entries[i:end] {
			if entry == nil || entry.retain || entry.pruneReason != root.pruneReason || entry.packageName != root.packageName || entry.metadataExport != "" {
				uniform = false
				break
			}
			if _, exists := alreadyCompacted[entry.rel]; exists {
				uniform = false
				break
			}
		}
		if !uniform {
			continue
		}

		subtree, err := commitPrunedSubtreeV2(root.rel, root.pruneReason, root.packageName, formulaID, entries[i:end])
		if err != nil {
			return nil, nil, err
		}
		subtrees = append(subtrees, subtree)
		for _, entry := range entries[i:end] {
			compacted[entry.rel] = struct{}{}
		}
		i = end - 1
	}
	return subtrees, compacted, nil
}

func compactableV2RuntimeReason(reason PruneReason) bool {
	switch reason {
	case PruneRuntimeHeaders,
		PruneRuntimeDocs,
		PruneRuntimeBuild,
		PruneRuntimeTests,
		PruneRuntimeShell,
		PruneRuntimeShareDoc:
		return true
	default:
		return false
	}
}

func commitPrunedSubtree(root string, reason PruneReason, entries []*sourceEntry) (PruneSubtree, error) {
	return commitPrunedSubtreeVersion(PruneSubtreeCommitmentSchemaVersion, root, reason, "", "", entries)
}

func commitPrunedSubtreeV2(root string, reason PruneReason, packageName, formulaID string, entries []*sourceEntry) (PruneSubtree, error) {
	if (packageName == "") != (formulaID == "") {
		return PruneSubtree{}, fmt.Errorf("package and formula identity must both be empty or both be set")
	}
	return commitPrunedSubtreeVersion(PruneSubtreeCommitmentSchemaVersionV2, root, reason, packageName, formulaID, entries)
}

func commitPrunedSubtreeVersion(schemaVersion, root string, reason PruneReason, packageName, formulaID string, entries []*sourceEntry) (PruneSubtree, error) {
	if root == "" {
		return PruneSubtree{}, fmt.Errorf("empty subtree root")
	}
	if len(entries) == 0 {
		return PruneSubtree{}, fmt.Errorf("empty subtree")
	}

	commitmentEntries := make([]pruneSubtreeCommitmentEntry, 0, len(entries))
	var regularBytes int64
	for _, entry := range entries {
		if entry == nil || !isWithin(entry.rel, root) {
			return PruneSubtree{}, fmt.Errorf("entry is nil or outside root")
		}
		item := pruneSubtreeCommitmentEntry{
			Path: entry.rel,
			Type: entry.typeName,
			Mode: pruneCommitmentModeString(entry.mode),
			Size: entry.size,
		}
		switch entry.typeName {
		case TypeDirectory:
			item.Size = 0
		case TypeRegular:
			if entry.sha256 == "" {
				return PruneSubtree{}, fmt.Errorf("regular file %q has no content digest", entry.rel)
			}
			item.ContentSHA256 = entry.sha256
			regularBytes += item.Size
		case TypeSymlink:
			if entry.linkSource == "" {
				return PruneSubtree{}, fmt.Errorf("symlink %q has no target", entry.rel)
			}
			item.Size = int64(len(entry.linkSource))
			item.LinkTarget = entry.linkSource
		case TypeHardlink:
			if entry.hardlinkTo == "" {
				return PruneSubtree{}, fmt.Errorf("hardlink %q has no target", entry.rel)
			}
			item.HardlinkTarget = entry.hardlinkTo
			item.ContentSHA256 = entry.sha256
		default:
			return PruneSubtree{}, fmt.Errorf("entry %q has unsupported type %q", entry.rel, entry.typeName)
		}
		commitmentEntries = append(commitmentEntries, item)
	}
	slices.SortFunc(commitmentEntries, func(a, b pruneSubtreeCommitmentEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	for i := 1; i < len(commitmentEntries); i++ {
		if commitmentEntries[i-1].Path == commitmentEntries[i].Path {
			return PruneSubtree{}, fmt.Errorf("duplicate path %q", commitmentEntries[i].Path)
		}
	}
	if commitmentEntries[0].Path != root {
		return PruneSubtree{}, fmt.Errorf("subtree root %q is absent", root)
	}
	if commitmentEntries[0].Type != TypeDirectory {
		return PruneSubtree{}, fmt.Errorf("subtree root %q is not a directory", root)
	}

	var commitment any
	switch schemaVersion {
	case PruneSubtreeCommitmentSchemaVersion:
		if packageName != "" || formulaID != "" {
			return PruneSubtree{}, fmt.Errorf("v1 subtree commitment cannot carry package attribution")
		}
		commitment = pruneSubtreeCommitmentV1{
			SchemaVersion: schemaVersion,
			Root:          root,
			Reason:        reason,
			EntryCount:    len(commitmentEntries),
			RegularBytes:  regularBytes,
			Entries:       commitmentEntries,
		}
	case PruneSubtreeCommitmentSchemaVersionV2:
		commitment = pruneSubtreeCommitmentV2{
			SchemaVersion: schemaVersion,
			Root:          root,
			Reason:        reason,
			Package:       packageName,
			FormulaID:     formulaID,
			EntryCount:    len(commitmentEntries),
			RegularBytes:  regularBytes,
			Entries:       commitmentEntries,
		}
	default:
		return PruneSubtree{}, fmt.Errorf("unsupported subtree commitment schema %q", schemaVersion)
	}
	data, err := canonicalJSON(commitment)
	if err != nil {
		return PruneSubtree{}, err
	}
	return PruneSubtree{
		Path:             root,
		Reason:           reason,
		Package:          packageName,
		FormulaID:        formulaID,
		EntryCount:       len(commitmentEntries),
		RegularBytes:     regularBytes,
		CommitmentSchema: schemaVersion,
		CommitmentDigest: digest.FromBytes(data).String(),
	}, nil
}

func pruneCommitmentModeString(mode os.FileMode) string {
	value := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		value |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		value |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		value |= 0o1000
	}
	return fmt.Sprintf("%04o", value)
}

func buildResult(outputRoot string, record *resolution.Record, inventory Inventory, prune PruneManifest, metadata []MetadataExport, policy *normalizedPolicy) (*Result, error) {
	inventoryJSON, err := canonicalJSON(inventory)
	if err != nil {
		return nil, runtimeError(CodeEvidence, "", "encode inventory: %v", err)
	}
	pruneJSON, err := canonicalJSON(prune)
	if err != nil {
		return nil, runtimeError(CodeEvidence, "", "encode prune manifest: %v", err)
	}
	inventoryDigest := digest.FromBytes(inventoryJSON).String()
	pruneDigest := digest.FromBytes(pruneJSON).String()

	sbom, err := buildSPDX(record, inventory)
	if err != nil {
		return nil, runtimeError(CodeEvidence, "", "build SPDX SBOM: %v", err)
	}
	sbomJSON, err := canonicalJSON(sbom)
	if err != nil {
		return nil, runtimeError(CodeEvidence, "", "encode SPDX SBOM: %v", err)
	}
	sbomDigest := digest.FromBytes(sbomJSON).String()

	manifest := buildRuntimeManifest(record, policy, inventory.ResolutionDigest, inventoryDigest, pruneDigest, sbomDigest, metadata)
	manifestJSON, err := canonicalJSON(manifest)
	if err != nil {
		return nil, runtimeError(CodeEvidence, "", "encode runtime manifest: %v", err)
	}
	manifestDigest := digest.FromBytes(manifestJSON).String()

	return &Result{
		OutputPrefix:    outputRoot,
		Inventory:       inventory,
		PruneManifest:   prune,
		RuntimeManifest: manifest,
		SBOM:            sbom,
		Evidence: EvidenceJSON{
			Inventory:       inventoryJSON,
			InventoryDigest: inventoryDigest,
			Prune:           pruneJSON,
			PruneDigest:     pruneDigest,
			RuntimeManifest: manifestJSON,
			ManifestDigest:  manifestDigest,
			SBOM:            sbomJSON,
			SBOMDigest:      sbomDigest,
		},
	}, nil
}

func buildRuntimeManifest(record *resolution.Record, policy *normalizedPolicy, resolutionDigest, inventoryDigest, pruneDigest, sbomDigest string, metadata []MetadataExport) RuntimeManifest {
	byPackage := make(map[string][]MetadataExport)
	for _, item := range metadata {
		item.FormulaID = formulaIDForRack(record, item.Package)
		byPackage[item.Package] = append(byPackage[item.Package], item)
	}
	packages := make([]RuntimePackage, 0, len(record.Nodes))
	for _, node := range record.Nodes {
		exported := append([]MetadataExport(nil), byPackage[node.Name]...)
		slices.SortFunc(exported, func(a, b MetadataExport) int {
			if c := strings.Compare(a.Kind, b.Kind); c != 0 {
				return c
			}
			return strings.Compare(a.SourcePath, b.SourcePath)
		})
		packages = append(packages, RuntimePackage{
			FormulaID:         formulaIDForNode(record, node),
			UpstreamFormulaID: node.UpstreamFormulaID,
			Name:              node.Name,
			FullName:          node.FullName,
			FormulaVersion:    node.FormulaVersion,
			FormulaRevision:   node.FormulaRevision,
			PkgVersion:        node.PkgVersion,
			VersionScheme:     node.VersionScheme,
			BottleRebuild:     node.BottleRebuild,
			BottleTag:         node.Bottle.Tag,
			BottleLayer:       node.Bottle.Layer.Digest,
			BottleLayerSize:   node.Bottle.Layer.Size,
			License:           node.License,
			KegPath:           path.Join(policy.installPrefix, "Cellar", node.Name, node.PkgVersion),
			ExportedMetadata:  exported,
		})
	}
	slices.SortFunc(packages, func(a, b RuntimePackage) int { return strings.Compare(a.Name, b.Name) })
	return RuntimeManifest{
		SchemaVersion:       manifestSchemaVersion(record),
		PolicyVersion:       record.PolicyVersion,
		ResolutionDigest:    resolutionDigest,
		PruningPolicyDigest: policy.digest,
		InventoryDigest:     inventoryDigest,
		PruneManifestDigest: pruneDigest,
		SBOMDigest:          sbomDigest,
		GeneratedAt:         time.Unix(record.SourceDateEpoch, 0).UTC().Format(time.RFC3339),
		SourceDateEpoch:     record.SourceDateEpoch,
		Platform:            record.Input.Platform,
		Prefix:              policy.installPrefix,
		Runtime:             record.Runtime,
		Packages:            packages,
	}
}

func buildSPDX(record *resolution.Record, inventory Inventory) (SPDXDocument, error) {
	packageIDs := make(map[string]string, len(record.Nodes))
	packages := make([]SPDXPackage, 0, len(record.Nodes))
	for _, node := range record.Nodes {
		packageIdentity := node.Name
		if formulaID := formulaIDForNode(record, node); formulaID != "" {
			packageIdentity = formulaID
		}
		id := spdxPackageID(packageIdentity)
		packageIDs[node.Name] = id
		license := node.License
		if strings.TrimSpace(license) == "" {
			license = "NOASSERTION"
		}
		layerChecksum := strings.TrimPrefix(node.Bottle.Layer.Digest, "sha256:")
		packages = append(packages, SPDXPackage{
			Name:             packageIdentity,
			SPDXID:           id,
			VersionInfo:      node.PkgVersion,
			PackageFileName:  node.Bottle.Filename,
			DownloadLocation: "NOASSERTION",
			FilesAnalyzed:    false,
			LicenseConcluded: "NOASSERTION",
			LicenseDeclared:  license,
			CopyrightText:    "NOASSERTION",
			Checksums: []SPDXChecksum{{
				Algorithm:     "SHA256",
				ChecksumValue: layerChecksum,
			}},
			ExternalRefs: []SPDXExternalRef{{
				ReferenceCategory: "PACKAGE-MANAGER",
				ReferenceType:     "purl",
				ReferenceLocator:  "pkg:brew/" + purlEscape(packageIdentity) + "@" + purlEscape(node.PkgVersion),
			}},
		})
	}
	slices.SortFunc(packages, func(a, b SPDXPackage) int { return strings.Compare(a.Name, b.Name) })

	files := make([]SPDXFile, 0)
	for _, entry := range inventory.Entries {
		if entry.Type == TypeDirectory {
			continue
		}
		if _, ok := packageIDs[entry.Package]; !ok {
			return SPDXDocument{}, fmt.Errorf("inventory path %q has unknown package %q", entry.Path, entry.Package)
		}
		checksum := entry.SHA256
		if len(checksum) != sha256.Size*2 {
			return SPDXDocument{}, fmt.Errorf("inventory path %q has invalid sha256", entry.Path)
		}
		fileID := spdxFileID(entry.Path)
		fileTypes := []string{"OTHER"}
		mode, _ := strconv.ParseUint(entry.Mode, 8, 32)
		if entry.Type != TypeSymlink && os.FileMode(mode)&0o111 != 0 {
			fileTypes = []string{"BINARY"}
		} else if looksLikeLegalText(entry.Path) {
			fileTypes = []string{"TEXT"}
		}
		comment := ""
		if entry.Type == TypeSymlink {
			comment = "Symbolic link to " + entry.LinkTarget
		} else if entry.Type == TypeHardlink {
			comment = "Hard link to " + entry.HardlinkTo
		}
		files = append(files, SPDXFile{
			FileName:  "./" + entry.Path,
			SPDXID:    fileID,
			FileTypes: fileTypes,
			Checksums: []SPDXChecksum{{
				Algorithm:     "SHA256",
				ChecksumValue: checksum,
			}},
			LicenseConcluded:   "NOASSERTION",
			LicenseInfoInFiles: []string{"NOASSERTION"},
			CopyrightText:      "NOASSERTION",
			FileComment:        comment,
		})
	}
	slices.SortFunc(files, func(a, b SPDXFile) int { return strings.Compare(a.FileName, b.FileName) })

	describes := make([]string, 0, len(packages))
	var relationships []SPDXRelationship
	for i := range packages {
		describes = append(describes, packages[i].SPDXID)
	}
	slices.Sort(describes)

	namespaceSeed := inventory.ResolutionDigest + "\n" + inventoryDigestForNamespace(inventory)
	namespaceHash := sha256.Sum256([]byte(namespaceSeed))
	platformName := record.Input.Platform.OS + "-" + record.Input.Platform.Architecture
	return SPDXDocument{
		SPDXVersion:       "SPDX-2.3",
		DataLicense:       "CC0-1.0",
		SPDXID:            "SPDXRef-DOCUMENT",
		Name:              "dalec-homebrew-" + platformName,
		DocumentNamespace: "urn:dalec-homebrew:" + hex.EncodeToString(namespaceHash[:]),
		CreationInfo: SPDXCreationInfo{
			Created:  time.Unix(record.SourceDateEpoch, 0).UTC().Format(time.RFC3339),
			Creators: []string{"Tool: dalec-homebrew/runtimefs"},
		},
		DocumentDescribes: describes,
		Packages:          packages,
		Files:             files,
		Relationships:     relationships,
	}, nil
}

func inventoryDigestForNamespace(inventory Inventory) string {
	data, err := canonicalJSON(inventory)
	if err != nil {
		return ""
	}
	return digest.FromBytes(data).String()
}

func spdxPackageID(name string) string {
	return "SPDXRef-Package-" + sanitizeSPDXID(name) + "-" + shortHash(name)
}

func spdxFileID(name string) string {
	return "SPDXRef-File-" + shortHash(name)
}

func sanitizeSPDXID(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "package"
	}
	return out
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func looksLikeLegalText(filename string) bool {
	base := strings.ToLower(path.Base(filename))
	for _, name := range []string{
		"license", "licenses", "licence", "licences",
		"copying",
		"notice", "notices",
		"copyright", "copyrights",
		"patent", "patents",
		"unlicense", "unlicenses", "unlicence", "unlicences",
		"legal",
	} {
		if base == name {
			return true
		}
		if strings.HasPrefix(base, name+".") || strings.HasPrefix(base, name+"-") || strings.HasPrefix(base, name+"_") {
			return true
		}
	}
	return false
}

func canonicalJSON(value any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// WriteEvidence publishes the four canonical evidence files as one directory transaction.
// The destination must not exist or must be an empty real directory.
func (r *Result) WriteEvidence(directory string) (retErr error) {
	if r == nil {
		return runtimeError(CodeEvidence, "", "nil result")
	}
	parent := filepath.Dir(directory)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return runtimeError(CodeEvidence, parent, "create evidence parent: %v", err)
	}
	existed := false
	if info, err := os.Lstat(directory); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return runtimeError(CodeEvidence, directory, "destination is not a real directory")
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return runtimeError(CodeEvidence, directory, "destination directory is not empty")
		}
		existed = true
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(directory)+".evidence-")
	if err != nil {
		return runtimeError(CodeEvidence, directory, "create staging directory: %v", err)
	}
	defer func() {
		if stage != "" {
			_ = os.RemoveAll(stage)
		}
	}()
	files := []struct {
		name string
		data []byte
	}{{InventoryFileName, r.Evidence.Inventory}, {PruneFileName, r.Evidence.Prune}, {ManifestFileName, r.Evidence.RuntimeManifest}, {SBOMFileName, r.Evidence.SBOM}}
	epoch := time.Unix(r.RuntimeManifest.SourceDateEpoch, 0).UTC()
	for _, item := range files {
		filename := filepath.Join(stage, item.name)
		if err := os.WriteFile(filename, item.data, 0o444); err != nil {
			return runtimeError(CodeEvidence, filename, "write: %v", err)
		}
		if err := os.Chtimes(filename, epoch, epoch); err != nil {
			return runtimeError(CodeEvidence, filename, "normalize mtime: %v", err)
		}
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if err := os.Chtimes(stage, epoch, epoch); err != nil {
		return err
	}
	if existed {
		if err := os.Remove(directory); err != nil {
			return runtimeError(CodeEvidence, directory, "remove empty destination: %v", err)
		}
	}
	if err := os.Rename(stage, directory); err != nil {
		return runtimeError(CodeEvidence, directory, "publish evidence directory: %v", err)
	}
	stage = ""
	return nil
}

func purlEscape(value string) string { return url.QueryEscape(value) }
