package materializer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type PrepareV2Config struct {
	Record           *resolution.RecordV2
	BottlesDir       string
	FetchEvidenceDir string
	Prefix           string
	PreparedRoot     string
}

type PreparationEvidenceV2 struct {
	SchemaVersion   string                    `json:"schema_version"`
	VerifiedBottles []VerifiedBottleV2        `json:"verified_bottles"`
	FetchEvidence   []fetcher.Evidence        `json:"fetch_evidence,omitempty"`
	StagedFormulae  []StagedFormulaEvidenceV2 `json:"staged_formulae"`
	TrustFile       string                    `json:"trust_file"`
}

type VerifiedBottleV2 struct {
	ID               resolution.FormulaID    `json:"id"`
	CompressedSHA256 string                  `json:"compressed_sha256"`
	CompressedSize   int64                   `json:"compressed_size"`
	ExpandedSize     int64                   `json:"expanded_size"`
	InventorySHA256  string                  `json:"inventory_sha256"`
	InventoryEntries int                     `json:"inventory_entries"`
	Formula          bottle.FormulaEvidence  `json:"formula"`
	Receipt          *bottle.ReceiptEvidence `json:"receipt,omitempty"`
}

const (
	PreparationEvidenceSchemaV2         = "dalec-homebrew-preparation/v2"
	MaxPreparedFormulaSourceBytes int64 = 256 << 20
	MaxPreparationEvidenceBytes         = 64 << 20
)

// PrepareV2 verifies all fetched bytes and transport evidence before any
// Homebrew process executes, then stages bottle-embedded Formula sources into
// sealed synthetic tap trees and emits an invocation-local trust store.
func PrepareV2(ctx context.Context, cfg PrepareV2Config) (*PreparationEvidenceV2, error) {
	if cfg.Record == nil {
		return nil, errors.New("nil V2 resolution")
	}
	if err := resolution.ValidateV2(cfg.Record); err != nil {
		return nil, fmt.Errorf("verify V2 resolution before preparation: %w", err)
	}
	if _, err := policy.VerifyMaterializerRuntimePolicyV2(cfg.Record); err != nil {
		return nil, fmt.Errorf("verify V2 policy before preparation: %w", err)
	}
	if cfg.Prefix == "" || cfg.PreparedRoot == "" || cfg.BottlesDir == "" {
		return nil, errors.New("prefix, prepared root, and bottles directory are required")
	}
	for label, directory := range map[string]string{"prefix": cfg.Prefix, "bottles": cfg.BottlesDir, "prepared root": cfg.PreparedRoot} {
		if err := requireRealDirectory(directory); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
	}
	if cfg.FetchEvidenceDir != "" {
		if err := requireRealDirectory(cfg.FetchEvidenceDir); err != nil {
			return nil, fmt.Errorf("fetch evidence: %w", err)
		}
	}
	preparedBottles := filepath.Join(cfg.PreparedRoot, "bottles")
	if err := os.Mkdir(preparedBottles, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	verified := make(map[resolution.FormulaID]bottle.Result, len(cfg.Record.Nodes))
	var formulaSourceBytes int64
	evidence := &PreparationEvidenceV2{SchemaVersion: PreparationEvidenceSchemaV2}
	for installIndex, id := range cfg.Record.InstallOrder {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node, ok := nodeV2ByID(cfg.Record.Nodes, id)
		if !ok {
			return nil, fmt.Errorf("install node %q is absent", id)
		}
		if node.Bottle.Filename == "" || filepath.Base(node.Bottle.Filename) != node.Bottle.Filename || strings.ContainsAny(node.Bottle.Filename, `/\\`) {
			return nil, fmt.Errorf("invalid bottle filename %q", node.Bottle.Filename)
		}
		if node.Bottle.Transport.HTTPS != nil {
			if cfg.FetchEvidenceDir == "" {
				return nil, fmt.Errorf("HTTPS bottle %q has no fetch evidence directory", id)
			}
			fetchEvidence, err := readAndVerifyFetchEvidence(filepath.Join(cfg.FetchEvidenceDir, fmt.Sprintf("%03d.fetch.json", installIndex)), node)
			if err != nil {
				return nil, fmt.Errorf("verify fetch evidence for %q: %w", id, err)
			}
			evidence.FetchEvidence = append(evidence.FetchEvidence, fetchEvidence)
		}
		sourcePath := filepath.Join(cfg.BottlesDir, node.Bottle.Filename)
		source, err := os.Open(sourcePath)
		if err != nil {
			return nil, err
		}
		info, err := source.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != node.Bottle.Size {
			source.Close()
			return nil, fmt.Errorf("bottle %q is not a regular exact-size file", node.Bottle.Filename)
		}
		preparedPath := filepath.Join(preparedBottles, node.Bottle.Filename)
		destination, err := os.OpenFile(preparedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
		if err != nil {
			source.Close()
			return nil, err
		}
		copied, copyErr := io.Copy(destination, io.LimitReader(source, node.Bottle.Size+1))
		sourceErr := source.Close()
		destinationErr := destination.Close()
		if copyErr != nil || sourceErr != nil || destinationErr != nil || copied != node.Bottle.Size {
			_ = os.Remove(preparedPath)
			return nil, errors.Join(copyErr, sourceErr, destinationErr, fmt.Errorf("copied %d bytes, expected %d", copied, node.Bottle.Size))
		}
		file, err := os.Open(preparedPath)
		if err != nil {
			return nil, err
		}
		result, verifyErr := bottle.VerifyNodeV2(file, node, cfg.Record.Nodes, bottle.Options{Policy: bottle.Policy{RequirePreInstallReceipt: false}})
		closeErr := file.Close()
		if verifyErr != nil || closeErr != nil {
			return nil, errors.Join(verifyErr, closeErr)
		}
		if err := verifyReceiptlessMarkerV2(node, result); err != nil {
			return nil, err
		}
		if result.Formula.Size > MaxPreparedFormulaSourceBytes-formulaSourceBytes {
			return nil, fmt.Errorf("aggregate bottle Formula source exceeds %d bytes", MaxPreparedFormulaSourceBytes)
		}
		formulaSourceBytes += result.Formula.Size
		if err := validateStandaloneFormulaSource(result.FormulaSource); err != nil {
			return nil, fmt.Errorf("Formula %q is not self-contained: %w", id, err)
		}
		deferredCore := node.Bottle.Verification.PolicyVersion == resolution.CoreBottleVerificationDeferredV1
		if !deferredCore && result.Formula.SHA256 != node.Bottle.BottleFormulaSourceDigest {
			return nil, fmt.Errorf("bottle Formula source digest for %q is %s, expected %s", id, result.Formula.SHA256, node.Bottle.BottleFormulaSourceDigest)
		}
		if !deferredCore && (result.InventorySHA256 != node.Bottle.Verification.InventoryDigest || len(result.Inventory) != node.Bottle.Verification.EntryCount || result.ExpandedSize != node.Bottle.Verification.ExpandedSize) {
			return nil, fmt.Errorf("static bottle verification summary for %q differs from signed catalog result", id)
		}
		stagingResult := *result
		stagingResult.Inventory = nil
		verified[id] = stagingResult
		evidence.VerifiedBottles = append(evidence.VerifiedBottles, VerifiedBottleV2{ID: id, CompressedSHA256: result.CompressedSHA256, CompressedSize: result.CompressedSize, ExpandedSize: result.ExpandedSize, InventorySHA256: result.InventorySHA256, InventoryEntries: len(result.Inventory), Formula: result.Formula, Receipt: result.Receipt})
	}
	if err := os.Chmod(preparedBottles, 0o555); err != nil {
		return nil, err
	}
	staged, err := StageFormulaeV2(cfg.Prefix, cfg.Record, verified)
	if err != nil {
		return nil, err
	}
	evidence.StagedFormulae = staged
	trust, err := V2TapTrustFile(cfg.Record)
	if err != nil {
		return nil, err
	}
	configRoot := filepath.Join(cfg.PreparedRoot, "homebrew-config")
	if err := os.Mkdir(configRoot, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	trustPath := filepath.Join(configRoot, V2TapTrustFileName)
	if err := writeExclusiveSealed(trustPath, trust); err != nil {
		return nil, err
	}
	if err := os.Chmod(configRoot, 0o555); err != nil {
		return nil, err
	}
	evidence.TrustFile = filepath.ToSlash(filepath.Join("homebrew-config", V2TapTrustFileName))
	data, err := json.Marshal(evidence)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxPreparationEvidenceBytes {
		return nil, fmt.Errorf("preparation evidence exceeds %d bytes", MaxPreparationEvidenceBytes)
	}
	if err := writeExclusiveSealed(filepath.Join(cfg.PreparedRoot, "preparation.json"), data); err != nil {
		return nil, err
	}
	return evidence, nil
}

func verifyReceiptlessMarkerV2(node resolution.NodeV2, result *bottle.Result) error {
	if result == nil {
		return errors.New("nil verified bottle result")
	}
	if node.Bottle.Transport.HTTPS != nil && node.Bottle.Tab.Receiptless != (result.Receipt == nil) {
		return fmt.Errorf("HTTPS bottle %q receiptless marker does not match verified archive", node.ID)
	}
	return nil
}

var tapLocalFormulaLoad = regexp.MustCompile(`(?m)^\s*(require_relative\b|load\s*[ (]|require\s*[ (]["']\.{1,2}/)`)

func validateStandaloneFormulaSource(source []byte) error {
	if tapLocalFormulaLoad.Find(source) != nil {
		return errors.New("tap-local require_relative/load helpers are unsupported; ingestion must provide a self-contained bottle Formula")
	}
	return nil
}

func readAndVerifyFetchEvidence(filename string, node resolution.NodeV2) (fetcher.Evidence, error) {
	file, err := os.Open(filename)
	if err != nil {
		return fetcher.Evidence{}, err
	}
	defer file.Close()
	evidence, err := fetcher.DecodeEvidence(file)
	if err != nil {
		return fetcher.Evidence{}, err
	}
	transport := node.Bottle.Transport.HTTPS
	if transport == nil {
		return fetcher.Evidence{}, errors.New("node does not use HTTPS transport")
	}
	request := fetcher.Request{SchemaVersion: fetcher.RequestSchemaVersion, FetchPolicyVersion: transport.FetchPolicyVersion, ArtifactID: node.ID.String(), URL: transport.URL, ExpectedSize: transport.ExpectedSize, SHA256: strings.TrimPrefix(transport.SHA256, "sha256:"), Filename: transport.Filename, AllowedRedirectHosts: append([]string(nil), transport.AllowedRedirectHosts...)}
	if err := fetcher.VerifyEvidence(evidence, request); err != nil {
		return fetcher.Evidence{}, err
	}
	return evidence, nil
}

func nodeV2ByID(nodes []resolution.NodeV2, id resolution.FormulaID) (resolution.NodeV2, bool) {
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return resolution.NodeV2{}, false
}
