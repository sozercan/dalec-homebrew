package materializer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type InstallOneV2Config struct {
	Record              *resolution.RecordV2
	ID                  resolution.FormulaID
	BottlePath          string
	Prefix              string
	HomebrewConfig      string
	PreparationEvidence string
	EvidencePath        string
	User                string
	Timeout             time.Duration
	Runner              Runner
}

type InstallDeltaV2 struct {
	SchemaVersion        string                        `json:"schema_version"`
	ID                   resolution.FormulaID          `json:"id"`
	ReceiptNormalization *ReceiptNormalizationEvidence `json:"receipt_normalization,omitempty"`
	Changes              []Change                      `json:"changes"`
}

const (
	InstallDeltaSchemaV2         = "dalec-homebrew-install-delta/v2"
	MaxInstallDeltaEvidenceBytes = 16 << 20
)

func InstallOneV2(ctx context.Context, cfg InstallOneV2Config) (*InstallDeltaV2, error) {
	if cfg.Record == nil {
		return nil, errors.New("nil V2 resolution")
	}
	if err := resolution.ValidateV2(cfg.Record); err != nil {
		return nil, err
	}
	if _, err := policy.VerifyMaterializerRuntimePolicyV2(cfg.Record); err != nil {
		return nil, fmt.Errorf("verify V2 policy before install: %w", err)
	}
	node, ok := nodeV2ByID(cfg.Record.Nodes, cfg.ID)
	if !ok {
		return nil, fmt.Errorf("Formula ID %q is absent from V2 resolution", cfg.ID)
	}
	prefix, err := normalizeMaterializerPrefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	cfg.Prefix = prefix
	if cfg.User == "" {
		cfg.User = "linuxbrew"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if cfg.Runner == nil {
		cfg.Runner = OSRunner{}
	}
	if cfg.BottlePath == "" || cfg.HomebrewConfig == "" || cfg.PreparationEvidence == "" || cfg.EvidencePath == "" {
		return nil, errors.New("bottle, Homebrew config, and evidence paths are required")
	}
	if err := requireRealDirectory(cfg.HomebrewConfig); err != nil {
		return nil, fmt.Errorf("Homebrew config: %w", err)
	}
	trustPath := filepath.Join(cfg.HomebrewConfig, V2TapTrustFileName)
	trustInfo, err := os.Lstat(trustPath)
	if err != nil || !trustInfo.Mode().IsRegular() || trustInfo.Mode().Perm()&0o222 != 0 {
		return nil, errors.New("V2 Homebrew trust file is missing or writable")
	}
	actualTrust, err := os.ReadFile(trustPath)
	if err != nil {
		return nil, err
	}
	expectedTrust, err := V2TapTrustFile(cfg.Record)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(actualTrust, expectedTrust) {
		return nil, errors.New("V2 Homebrew trust file does not exactly match selected Formula IDs")
	}

	legacy, rackByID, err := legacyProjectionV2(cfg.Record)
	if err != nil {
		return nil, err
	}
	legacyNode, ok := nodeByName(legacy, node.Name)
	if !ok {
		return nil, fmt.Errorf("projected node %q is absent", node.Name)
	}
	installIndex := slices.Index(legacy.InstallOrder, node.Name)
	if installIndex < 0 {
		return nil, fmt.Errorf("projected install-order entry %q is absent", node.Name)
	}
	if err := verifyInstalledPrefixCount(cfg.Prefix, legacy, installIndex); err != nil {
		return nil, fmt.Errorf("verify cumulative prefix before %q: %w", cfg.ID, err)
	}
	bottleFile, err := os.Open(cfg.BottlePath)
	if err != nil {
		return nil, err
	}
	verified, verifyErr := bottle.VerifyNodeV2(bottleFile, node, cfg.Record.Nodes, bottle.Options{Policy: bottle.Policy{RequirePreInstallReceipt: false}})
	closeErr := bottleFile.Close()
	if verifyErr != nil || closeErr != nil {
		return nil, errors.Join(verifyErr, closeErr)
	}
	stagedRelative, err := FormulaTapPathV2(node)
	if err != nil {
		return nil, err
	}
	stagedPath := filepath.Join(cfg.Prefix, filepath.FromSlash(stagedRelative))
	preparationData, err := readBoundedEvidenceFile(cfg.PreparationEvidence, MaxPreparationEvidenceBytes)
	if err != nil {
		return nil, err
	}
	var preparation PreparationEvidenceV2
	if err := json.Unmarshal(preparationData, &preparation); err != nil {
		return nil, err
	}
	if err := validatePreparationEvidenceV2(preparation, cfg.Record); err != nil {
		return nil, err
	}
	stagedDigest := node.Bottle.BottleFormulaSourceDigest
	if node.Bottle.Verification.PolicyVersion == resolution.CoreBottleVerificationDeferredV1 {
		stagedDigest = ""
		for _, staged := range preparation.StagedFormulae {
			if staged.ID == node.ID {
				stagedDigest = staged.SHA256
				break
			}
		}
		if stagedDigest == "" {
			return nil, fmt.Errorf("prepared Formula digest for %s is missing", node.ID)
		}
	}
	if err := verifyStagedFormulaV2(stagedPath, stagedDigest); err != nil {
		return nil, err
	}
	tapsRoot := filepath.Join(cfg.Prefix, "Homebrew", "Library", "Taps")
	tapsBefore, err := snapshotContext(ctx, tapsRoot)
	if err != nil {
		return nil, fmt.Errorf("snapshot protected tap trees before %q: %w", cfg.ID, err)
	}
	if err := validateNoPrefixBrewEnv(cfg.Prefix); err != nil {
		return nil, err
	}
	stepCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	before, err := snapshotContext(stepCtx, cfg.Prefix)
	if err != nil {
		return nil, err
	}
	if err := validatePreinstallSymlinks(cfg.Prefix, before, legacy); err != nil {
		return nil, err
	}
	if err := validateExternalBottleSymlinkTargets(cfg.Prefix, before, legacyNode, *verified, legacy.Nodes); err != nil {
		return nil, err
	}
	var priorGdkPixbufCache []byte
	if state, ok := before[gdkPixbufLoadersCachePath]; ok {
		if state.Type != "regular" {
			return nil, errors.New("gdk-pixbuf loader cache is not regular")
		}
		priorGdkPixbufCache, err = readStableSnapshotFile(cfg.Prefix, gdkPixbufLoadersCachePath, state)
		if err != nil {
			return nil, err
		}
	}
	command := Command{Path: filepath.Join(cfg.Prefix, filepath.FromSlash(protectedHomebrewBrew)), Args: []string{"ruby", pourScriptPath, node.ID.String(), stagedPath, cfg.BottlePath}, Env: installEnvV2(cfg.Prefix, cfg.HomebrewConfig), Dir: "/home/linuxbrew", User: cfg.User}
	if err := cfg.Runner.Run(stepCtx, command); err != nil {
		return nil, fmt.Errorf("offline install %q: %w", cfg.ID, err)
	}
	if err := validateNoPrefixBrewEnv(cfg.Prefix); err != nil {
		return nil, err
	}
	tapsAfter, err := snapshotContext(stepCtx, tapsRoot)
	if err != nil {
		return nil, err
	}
	if !maps.Equal(tapsBefore, tapsAfter) {
		return nil, fmt.Errorf("Homebrew modified protected tap trees while installing %q", cfg.ID)
	}
	normalization, err := normalizeInstalledReceipt(cfg.Prefix, legacyNode, legacy.Nodes, cfg.Record.SourceDateEpoch)
	if err != nil {
		return nil, err
	}
	after, err := snapshotContext(stepCtx, cfg.Prefix)
	if err != nil {
		return nil, err
	}
	// OSRunner returns only after the Linux subreaper has killed and reaped
	// the complete installer process tree.
	changes := diff(before, after)
	if err := classify(cfg.Prefix, legacyNode, before, after, changes, classifyOptions{optNames: optNamesForNode(legacy, legacyNode.Name), closureKegs: resolvedClosureKegs(legacy), verified: *verified, runtimeUID: uint32(cfg.Record.Runtime.UID), runtimeGID: uint32(cfg.Record.Runtime.GID), priorGdkPixbufCache: priorGdkPixbufCache}); err != nil {
		return nil, fmt.Errorf("contain install %q: %w", cfg.ID, err)
	}
	if err := reconcileInstalledKeg(cfg.Prefix, legacyNode, *verified, after, reconcileKegOptions{closure: legacy.Nodes}); err != nil {
		return nil, err
	}
	if err := verifyInstalledSubset(cfg.Prefix, legacy, rackByID[cfg.ID]); err != nil {
		return nil, err
	}
	delta := &InstallDeltaV2{SchemaVersion: InstallDeltaSchemaV2, ID: cfg.ID, ReceiptNormalization: normalization, Changes: changes}
	data, err := json.Marshal(delta)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxInstallDeltaEvidenceBytes {
		return nil, fmt.Errorf("install delta evidence exceeds %d bytes", MaxInstallDeltaEvidenceBytes)
	}
	if err := writeExclusiveSealed(cfg.EvidencePath, data); err != nil {
		return nil, err
	}
	return delta, nil
}

func installEnvV2(prefix, configRoot string) []string {
	environment := installEnv(prefix)
	environment = append(environment, "HOMEBREW_USER_CONFIG_HOME="+configRoot, "HOMEBREW_REQUIRE_TAP_TRUST=1")
	return environment
}

func verifyStagedFormulaV2(filename, expectedDigest string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 || info.Size() < 0 || info.Size() > bottle.DefaultLimits().MaxFormulaBytes {
		return errors.New("staged Formula is not a sealed bounded regular file")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != expectedDigest {
		return fmt.Errorf("staged Formula digest %s does not match %s", actual, expectedDigest)
	}
	return nil
}

func legacyProjectionV2(record *resolution.RecordV2) (*resolution.Record, map[resolution.FormulaID]string, error) {
	return resolution.ProjectV2ForRuntime(record)
}
