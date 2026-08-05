package materializer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
)

type FinalizeV2Config struct {
	Record              *resolution.RecordV2
	Prefix              string
	OutputRoot          string
	PreparationEvidence string
	InstallEvidenceDir  string
}

type MaterializationEvidenceV2 struct {
	SchemaVersion string            `json:"schema_version"`
	Preparation   json.RawMessage   `json:"preparation"`
	InstallDeltas []json.RawMessage `json:"install_deltas"`
}

const (
	MaterializationEvidenceSchemaV2 = "dalec-homebrew-materialization/v2"
	MaxMaterializationEvidenceBytes = 64 << 20
)

func FinalizeV2(ctx context.Context, cfg FinalizeV2Config) (*runtimefs.Result, error) {
	if cfg.Record == nil {
		return nil, errors.New("nil V2 resolution")
	}
	if err := resolution.ValidateV2(cfg.Record); err != nil {
		return nil, err
	}
	if cfg.Prefix == "" || cfg.OutputRoot == "" || cfg.PreparationEvidence == "" || cfg.InstallEvidenceDir == "" {
		return nil, errors.New("prefix, output, preparation evidence, and install evidence are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allowlist, err := policy.VerifyMaterializerRuntimePolicyV2(cfg.Record)
	if err != nil {
		return nil, fmt.Errorf("verify V2 policy before finalization: %w", err)
	}
	projected, _, err := resolution.ProjectV2ForRuntime(cfg.Record)
	if err != nil {
		return nil, err
	}
	if err := verifyClosure(cfg.Prefix, projected); err != nil {
		return nil, err
	}
	for _, node := range cfg.Record.Nodes {
		stateDir := filepath.Join(cfg.Prefix, "var", node.Name)
		if err := os.MkdirAll(stateDir, 0o750); err != nil {
			return nil, err
		}
	}
	outputPrefix := filepath.Join(cfg.OutputRoot, filepath.FromSlash(cfg.Prefix))
	result, err := runtimefs.AssembleV2(cfg.Prefix, outputPrefix, cfg.Record, runtimefs.Options{InstallPrefix: cfg.Prefix, Allowlist: allowlist})
	if err != nil {
		return nil, err
	}
	evidenceDir := filepath.Join(cfg.OutputRoot, "usr/share/dalec-homebrew")
	mergedSBOM := result.SBOM
	var baseInventory, baseArtifacts []byte
	if merged, inventory, err := mergeRuntimeBaseSBOM(mergedSBOM, "/usr/share/dalec-homebrew/runtime-base-packages.tsv"); err != nil {
		return nil, err
	} else if len(inventory) > 0 {
		baseInventory = inventory
		mergedSBOM = merged
	}
	if merged, inventory, err := mergeRuntimeBaseArtifacts(mergedSBOM, "/usr/share/dalec-homebrew/runtime-base-artifacts.tsv"); err != nil {
		return nil, err
	} else if len(inventory) > 0 {
		baseArtifacts = inventory
		mergedSBOM = merged
	}
	if len(baseInventory) > 0 || len(baseArtifacts) > 0 {
		if err := replaceSBOM(result, mergedSBOM); err != nil {
			return nil, err
		}
	}
	if err := result.WriteEvidence(evidenceDir); err != nil {
		return nil, err
	}
	if len(baseInventory) > 0 {
		if err := os.WriteFile(filepath.Join(evidenceDir, "runtime-base-packages.tsv"), baseInventory, 0o444); err != nil {
			return nil, err
		}
	}
	if len(baseArtifacts) > 0 {
		if err := os.WriteFile(filepath.Join(evidenceDir, "runtime-base-artifacts.tsv"), baseArtifacts, 0o444); err != nil {
			return nil, err
		}
	}
	materialization, err := collectMaterializationEvidenceV2(cfg.PreparationEvidence, cfg.InstallEvidenceDir, cfg.Record)
	if err != nil {
		return nil, err
	}
	materializationBytes, err := json.Marshal(materialization)
	if err != nil {
		return nil, err
	}
	if len(materializationBytes) > MaxMaterializationEvidenceBytes {
		return nil, fmt.Errorf("materialization evidence exceeds %d bytes", MaxMaterializationEvidenceBytes)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "materialization-v2.json"), materializationBytes, 0o444); err != nil {
		return nil, err
	}
	resolutionJSON, err := resolution.CanonicalV2(cfg.Record)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "resolution.json"), resolutionJSON, 0o444); err != nil {
		return nil, err
	}
	epoch := time.Unix(cfg.Record.SourceDateEpoch, 0)
	for _, name := range []string{"materialization-v2.json", "resolution.json", runtimefs.InventoryFileName, runtimefs.PruneFileName, runtimefs.ManifestFileName, runtimefs.SBOMFileName, "runtime-base-packages.tsv", "runtime-base-artifacts.tsv"} {
		_ = os.Chtimes(filepath.Join(evidenceDir, name), epoch, epoch)
	}
	return result, nil
}

func validatePreparationEvidenceV2(evidence PreparationEvidenceV2, record *resolution.RecordV2) error {
	if evidence.SchemaVersion != PreparationEvidenceSchemaV2 || evidence.TrustFile != filepath.ToSlash(filepath.Join("homebrew-config", V2TapTrustFileName)) {
		return errors.New("preparation evidence identity is invalid")
	}
	nodes := make(map[resolution.FormulaID]resolution.NodeV2, len(record.Nodes))
	for _, node := range record.Nodes {
		nodes[node.ID] = node
	}
	seenBottles := map[resolution.FormulaID]struct{}{}
	for _, verified := range evidence.VerifiedBottles {
		node, ok := nodes[verified.ID]
		if !ok {
			return fmt.Errorf("preparation evidence contains unknown bottle %s", verified.ID)
		}
		if _, duplicate := seenBottles[verified.ID]; duplicate {
			return fmt.Errorf("duplicate prepared bottle %s", verified.ID)
		}
		seenBottles[verified.ID] = struct{}{}
		deferredCore := node.Bottle.Verification.PolicyVersion == resolution.CoreBottleVerificationDeferredV1
		if verified.CompressedSHA256 != node.Bottle.SHA256 || verified.CompressedSize != node.Bottle.Size || verified.Formula.Size <= 0 {
			return fmt.Errorf("prepared bottle summary for %s does not match resolution", verified.ID)
		}
		if !deferredCore && (verified.ExpandedSize != node.Bottle.Verification.ExpandedSize || verified.InventorySHA256 != node.Bottle.Verification.InventoryDigest || verified.InventoryEntries != node.Bottle.Verification.EntryCount || verified.Formula.SHA256 != node.Bottle.BottleFormulaSourceDigest) {
			return fmt.Errorf("prepared bottle verification for %s does not match signed catalog", verified.ID)
		}
	}
	if len(seenBottles) != len(nodes) {
		return errors.New("preparation evidence omits verified bottles")
	}
	seenFormulae := map[resolution.FormulaID]struct{}{}
	for _, staged := range evidence.StagedFormulae {
		node, ok := nodes[staged.ID]
		if !ok || staged.Tap != node.Tap || staged.Name != node.Name || staged.Size <= 0 {
			return fmt.Errorf("staged Formula evidence for %s is invalid", staged.ID)
		}
		if node.Bottle.Verification.PolicyVersion != resolution.CoreBottleVerificationDeferredV1 && staged.SHA256 != node.Bottle.BottleFormulaSourceDigest {
			return fmt.Errorf("staged Formula digest for %s does not match resolution", staged.ID)
		}
		expectedPath, err := FormulaTapPathV2(node)
		if err != nil || staged.Path != expectedPath {
			return fmt.Errorf("staged Formula path for %s is invalid", staged.ID)
		}
		if _, duplicate := seenFormulae[staged.ID]; duplicate {
			return fmt.Errorf("duplicate staged Formula %s", staged.ID)
		}
		seenFormulae[staged.ID] = struct{}{}
	}
	if len(seenFormulae) != len(nodes) {
		return errors.New("preparation evidence omits staged Formulae")
	}
	fetchByID := map[string]fetcher.Evidence{}
	for _, item := range evidence.FetchEvidence {
		if _, duplicate := fetchByID[item.ArtifactID]; duplicate {
			return fmt.Errorf("duplicate fetch evidence for %s", item.ArtifactID)
		}
		fetchByID[item.ArtifactID] = item
	}
	for _, node := range record.Nodes {
		item, present := fetchByID[node.ID.String()]
		if node.Bottle.Transport.HTTPS == nil {
			if present {
				return fmt.Errorf("non-HTTPS node %s has HTTPS fetch evidence", node.ID)
			}
			continue
		}
		if !present {
			return fmt.Errorf("HTTPS node %s has no fetch evidence", node.ID)
		}
		transport := node.Bottle.Transport.HTTPS
		request := fetcher.Request{SchemaVersion: fetcher.RequestSchemaVersion, FetchPolicyVersion: transport.FetchPolicyVersion, ArtifactID: node.ID.String(), URL: transport.URL, ExpectedSize: transport.ExpectedSize, SHA256: strings.TrimPrefix(transport.SHA256, "sha256:"), Filename: transport.Filename, AllowedRedirectHosts: slices.Clone(transport.AllowedRedirectHosts)}
		if err := fetcher.VerifyEvidence(item, request); err != nil {
			return fmt.Errorf("fetch evidence for %s: %w", node.ID, err)
		}
	}
	if len(fetchByID) != countHTTPSNodes(record.Nodes) {
		return errors.New("preparation fetch evidence set is not exact")
	}
	return nil
}

func countHTTPSNodes(nodes []resolution.NodeV2) int {
	count := 0
	for _, node := range nodes {
		if node.Bottle.Transport.HTTPS != nil {
			count++
		}
	}
	return count
}

func readBoundedEvidenceFile(filename string, limit int) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("evidence file %s exceeds %d bytes", filepath.Base(filename), limit)
	}
	return data, nil
}

func collectMaterializationEvidenceV2(preparationPath, deltasDir string, record *resolution.RecordV2) (MaterializationEvidenceV2, error) {
	preparation, err := readBoundedEvidenceFile(preparationPath, MaxPreparationEvidenceBytes)
	if err != nil {
		return MaterializationEvidenceV2{}, err
	}
	var preparationValue PreparationEvidenceV2
	if err := json.Unmarshal(preparation, &preparationValue); err != nil {
		return MaterializationEvidenceV2{}, err
	}
	if err := validatePreparationEvidenceV2(preparationValue, record); err != nil {
		return MaterializationEvidenceV2{}, err
	}
	entries, err := os.ReadDir(deltasDir)
	if err != nil {
		return MaterializationEvidenceV2{}, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	deltas := make([]json.RawMessage, 0, len(names))
	totalBytes := len(preparation)
	seenIDs := make(map[resolution.FormulaID]struct{}, len(names))
	for _, name := range names {
		data, err := readBoundedEvidenceFile(filepath.Join(deltasDir, name), MaxInstallDeltaEvidenceBytes)
		if err != nil {
			return MaterializationEvidenceV2{}, err
		}
		totalBytes += len(data)
		if totalBytes > MaxMaterializationEvidenceBytes {
			return MaterializationEvidenceV2{}, fmt.Errorf("aggregate materialization evidence exceeds %d bytes", MaxMaterializationEvidenceBytes)
		}
		var value InstallDeltaV2
		if err := json.Unmarshal(data, &value); err != nil || value.SchemaVersion != InstallDeltaSchemaV2 {
			return MaterializationEvidenceV2{}, fmt.Errorf("invalid install delta %s", name)
		}
		if _, duplicate := seenIDs[value.ID]; duplicate {
			return MaterializationEvidenceV2{}, fmt.Errorf("duplicate install delta for %s", value.ID)
		}
		seenIDs[value.ID] = struct{}{}
		deltas = append(deltas, json.RawMessage(data))
	}
	if len(seenIDs) != len(record.InstallOrder) {
		return MaterializationEvidenceV2{}, fmt.Errorf("install delta count %d does not match install order %d", len(seenIDs), len(record.InstallOrder))
	}
	for _, id := range record.InstallOrder {
		if _, ok := seenIDs[id]; !ok {
			return MaterializationEvidenceV2{}, fmt.Errorf("install delta for %s is missing", id)
		}
	}
	return MaterializationEvidenceV2{SchemaVersion: MaterializationEvidenceSchemaV2, Preparation: json.RawMessage(preparation), InstallDeltas: deltas}, nil
}
