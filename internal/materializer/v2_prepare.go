package materializer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/prebuilt"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
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
	tapPolicy, err := policyv2.LoadTapPolicy()
	if err != nil {
		return nil, fmt.Errorf("load V2 tap policy before preparation: %w", err)
	}
	for _, node := range cfg.Record.Nodes {
		if err := verifyPrebuiltDerivationPolicyV2(cfg.Record, node, tapPolicy); err != nil {
			return nil, fmt.Errorf("verify prebuilt policy for %q: %w", node.ID, err)
		}
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
	installerUID, installerGID, err := v2InstallerIdentity()
	if err != nil {
		return nil, fmt.Errorf("load V2 installer identity: %w", err)
	}
	if err := ensureWritablePrefixDirectoriesV2(cfg.Prefix, installerUID, installerGID); err != nil {
		return nil, fmt.Errorf("prepare writable Homebrew prefix structure: %w", err)
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
		preparedPath := filepath.Join(preparedBottles, node.Bottle.Filename)
		if err := copyPreparedBottleV2(sourcePath, preparedPath, node.Bottle.Size); err != nil {
			return nil, fmt.Errorf("prepare bottle %q: %w", node.Bottle.Filename, err)
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
		if err := verifyPrebuiltDerivedBottleV2(node, result); err != nil {
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

var writablePrefixDirectoriesV2 = []string{
	"Caskroom",
	"Cellar",
	"Frameworks",
	"bin",
	"etc",
	"include",
	"lib",
	"opt",
	"sbin",
	"share",
	"var",
}

func v2InstallerIdentity() (int, int, error) {
	runtimePolicy, err := policy.V2RuntimePolicy()
	if err != nil {
		return 0, 0, err
	}
	if runtimePolicy.Linuxbrew.UID <= 0 || runtimePolicy.Linuxbrew.GID <= 0 {
		return 0, 0, errors.New("V2 policy has an invalid installer identity")
	}
	return runtimePolicy.Linuxbrew.UID, runtimePolicy.Linuxbrew.GID, nil
}

func ensureWritablePrefixDirectoriesV2(prefix string, uid, gid int) error {
	if uid <= 0 || gid <= 0 {
		return errors.New("runtime UID and GID must be positive")
	}
	for _, directory := range writablePrefixDirectoriesV2 {
		target := filepath.Join(prefix, directory)
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(target, 0o755); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a real directory", target)
		}
		if err := os.Chown(target, uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(target, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func copyPreparedBottleV2(sourcePath, destinationPath string, expectedSize int64) error {
	if expectedSize <= 0 {
		return errors.New("prepared bottle size must be positive")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	info, statErr := source.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		closeErr := source.Close()
		return errors.Join(statErr, closeErr, fmt.Errorf("source is not a regular exact-size file"))
	}
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return errors.Join(err, source.Close())
	}
	copied, copyErr := io.Copy(destination, io.LimitReader(source, expectedSize+1))
	modeErr := destination.Chmod(0o444)
	sourceErr := source.Close()
	destinationErr := destination.Close()
	if copyErr != nil || modeErr != nil || sourceErr != nil || destinationErr != nil || copied != expectedSize {
		_ = os.Remove(destinationPath)
		return errors.Join(copyErr, modeErr, sourceErr, destinationErr, fmt.Errorf("copied %d bytes, expected %d", copied, expectedSize))
	}
	return nil
}

func verifyReceiptlessMarkerV2(node resolution.NodeV2, result *bottle.Result) error {
	if result == nil {
		return errors.New("nil verified bottle result")
	}
	if (node.Bottle.Transport.HTTPS != nil || node.Bottle.Transport.Local != nil) && node.Bottle.Tab.Receiptless != (result.Receipt == nil) {
		return fmt.Errorf("receiptless bottle %q marker does not match verified archive", node.ID)
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

func verifyPrebuiltDerivationPolicyV2(record *resolution.RecordV2, node resolution.NodeV2, tapPolicy *policyv2.TapPolicy) error {
	derivation := node.Bottle.PrebuiltDerivation
	if derivation == nil {
		return nil
	}
	if record == nil || tapPolicy == nil {
		return errors.New("V2 record and tap policy are required")
	}
	authorization, ok := tapPolicy.PrebuiltArchiveForFormula(node.ID.String())
	if !ok {
		return errors.New("Formula has no release-bound prebuilt authorization")
	}
	var errs []error
	if derivation.PolicyVersion != authorization.PolicyVersion {
		errs = append(errs, fmt.Errorf("derivation policy %q does not match authorization %q", derivation.PolicyVersion, authorization.PolicyVersion))
	}
	if derivation.PolicyDigest != record.Components.TapPolicyDigest {
		errs = append(errs, errors.New("derivation policy digest does not match the release-bound tap policy"))
	}
	if node.FormulaVersion != authorization.Version || node.FormulaRevision != 0 || node.PkgVersion != authorization.Version || node.VersionScheme != 0 || node.BottleRebuild != 0 {
		errs = append(errs, fmt.Errorf("Formula version identity %s/%d/%s does not match the exact authorized version %s", node.FormulaVersion, node.FormulaRevision, node.PkgVersion, authorization.Version))
	}
	if node.License != authorization.License {
		errs = append(errs, fmt.Errorf("Formula license %q does not match authorization %q", node.License, authorization.License))
	}
	if derivation.FormulaSource.SHA256 != authorization.FormulaSourceDigest || node.Bottle.CurrentFormulaSourceDigest != authorization.FormulaSourceDigest || node.Bottle.BottleFormulaSourceDigest != authorization.FormulaSourceDigest {
		errs = append(errs, errors.New("Formula source digest does not match the exact prebuilt authorization"))
	}
	if authorization.RootOnly {
		requested := false
		for _, root := range record.Requested {
			if root.ID == node.ID {
				requested = true
				break
			}
		}
		if !requested {
			errs = append(errs, errors.New("prebuilt authorization permits direct requested roots only"))
		}
	}
	currentDependencies := make([]string, len(node.Dependencies))
	for i, dependency := range node.Dependencies {
		currentDependencies[i] = dependency.ID.String()
	}
	slices.Sort(currentDependencies)
	authorizedDependencies := slices.Clone(authorization.Dependencies)
	slices.Sort(authorizedDependencies)
	if !slices.Equal(currentDependencies, authorizedDependencies) {
		errs = append(errs, errors.New("Formula dependencies do not match the exact prebuilt authorization"))
	}
	tabDependencies := make([]string, len(node.Bottle.Tab.Dependencies))
	for i, dependency := range node.Bottle.Tab.Dependencies {
		tabDependencies[i] = dependency.ID.String()
	}
	slices.Sort(tabDependencies)
	if !slices.Equal(tabDependencies, authorizedDependencies) {
		errs = append(errs, errors.New("derived bottle dependency evidence does not match the exact prebuilt authorization"))
	}

	platformKey := record.Input.Platform.OS + "/" + record.Input.Platform.Architecture
	var platformAuthorization *policyv2.PrebuiltArchivePlatformPolicy
	for i := range authorization.Platforms {
		if authorization.Platforms[i].Platform == platformKey {
			platformAuthorization = &authorization.Platforms[i]
			break
		}
	}
	if platformAuthorization == nil {
		errs = append(errs, fmt.Errorf("prebuilt authorization has no platform %q", platformKey))
	} else if transport := derivation.Source.Transport.HTTPS; transport == nil {
		errs = append(errs, errors.New("prebuilt source does not use HTTPS transport"))
	} else {
		if transport.URL != platformAuthorization.URL {
			errs = append(errs, fmt.Errorf("prebuilt source URL %q does not match authorization %q", transport.URL, platformAuthorization.URL))
		}
		if derivation.Source.SHA256 != platformAuthorization.SHA256 || transport.SHA256 != platformAuthorization.SHA256 {
			errs = append(errs, errors.New("prebuilt source digest does not match the platform authorization"))
		}
	}
	if derivation.Source.Format != authorization.Archive.Format {
		errs = append(errs, fmt.Errorf("prebuilt source format %q does not match authorization %q", derivation.Source.Format, authorization.Archive.Format))
	}
	if derivation.Source.Size <= 0 || derivation.Source.Size > authorization.Archive.MaxCompressedBytes {
		errs = append(errs, fmt.Errorf("prebuilt source size %d exceeds the authorized limit", derivation.Source.Size))
	}
	if derivation.SourceInventory.EntryCount != len(authorization.Archive.Members) || derivation.SourceInventory.EntryCount > authorization.Archive.MaxEntries {
		errs = append(errs, errors.New("prebuilt source inventory count does not match the authorized complete inventory"))
	}
	if derivation.SourceInventory.ExpandedSize <= 0 || derivation.SourceInventory.ExpandedSize > authorization.Archive.MaxExpandedBytes {
		errs = append(errs, errors.New("prebuilt source expanded size exceeds the authorized limit"))
	} else if derivation.Source.Size > 0 && derivation.SourceInventory.ExpandedSize > derivation.Source.Size*int64(authorization.Archive.MaxExpansionRatio) {
		errs = append(errs, errors.New("prebuilt source expansion ratio exceeds the authorized limit"))
	}
	if derivation.Payload.Size <= 0 || derivation.Payload.Size > authorization.Archive.MaxFileBytes {
		errs = append(errs, errors.New("prebuilt payload size exceeds the authorized file limit"))
	}

	archiveMode, archiveModeErr := authorizedArchiveModeV2(authorization, authorization.Install.Source)
	installMode, installModeErr := parsePrebuiltPolicyModeV2(authorization.Install.Mode)
	if archiveModeErr != nil || installModeErr != nil {
		errs = append(errs, archiveModeErr, installModeErr)
	} else if derivation.Payload.SourcePath != authorization.Install.Source || derivation.Payload.DestinationPath != authorization.Install.Destination || derivation.Payload.ArchiveMode != archiveMode || derivation.Payload.DerivedMode != installMode {
		errs = append(errs, errors.New("prebuilt payload transformation does not match the authorized install policy"))
	}

	var machine string
	for _, candidate := range authorization.Binary.Machines {
		if candidate.Platform == platformKey {
			machine = candidate.Machine
			break
		}
	}
	if machine == "" || derivation.ELF.Format != authorization.Binary.Format || derivation.ELF.Machine != machine || !derivation.ELF.StaticallyLinked || derivation.ELF.Interpreter != "" || !slices.Equal(derivation.ELF.NeededLibraries, authorization.Binary.NeededLibraries) || len(derivation.ELF.RPaths) != 0 || derivation.ELF.WritableExecutableSegments {
		errs = append(errs, errors.New("prebuilt ELF evidence does not match the authorized binary policy"))
	}
	expectedRecipeDigest, recipeErr := expectedPrebuiltRecipeDigestV2(record, node, authorization)
	if recipeErr != nil {
		errs = append(errs, recipeErr)
	} else if derivation.RecipeDigest != expectedRecipeDigest {
		errs = append(errs, fmt.Errorf("prebuilt recipe digest %s does not match authorized recipe %s", derivation.RecipeDigest, expectedRecipeDigest))
	}
	return errors.Join(errs...)
}

func expectedPrebuiltRecipeDigestV2(record *resolution.RecordV2, node resolution.NodeV2, authorization policyv2.PrebuiltArchivePolicy) (string, error) {
	limits := prebuilt.DefaultLimits()
	limits.MaxCompressedBytes = authorization.Archive.MaxCompressedBytes
	limits.MaxExpandedBytes = authorization.Archive.MaxExpandedBytes
	limits.MaxExpansionRatio = int64(authorization.Archive.MaxExpansionRatio)
	limits.MaxEntries = authorization.Archive.MaxEntries
	limits.MaxFileBytes = authorization.Archive.MaxFileBytes
	limits.MaxPathBytes = authorization.Archive.MaxPathBytes
	limits.MaxDepth = authorization.Archive.MaxDepth
	entries := make([]prebuilt.EntryProfile, len(authorization.Archive.Members))
	for i, member := range authorization.Archive.Members {
		mode, err := parsePrebuiltPolicyModeV2(member.Mode)
		if err != nil {
			return "", fmt.Errorf("prebuilt archive member %q mode: %w", member.Path, err)
		}
		entries[i] = prebuilt.EntryProfile{Path: member.Path, Mode: mode}
	}
	if authorization.Binary.CGOEnabled == nil {
		return "", errors.New("prebuilt authorization omits the CGO policy")
	}
	generatedAt := int64(0)
	for _, source := range record.MetadataSources {
		if source.Tap == node.Tap {
			generatedAt = source.GeneratedAt.Unix()
			break
		}
	}
	if generatedAt <= 0 {
		return "", errors.New("prebuilt Formula metadata source is missing")
	}
	profile := prebuilt.Profile{
		PolicyVersion:   authorization.PolicyVersion,
		Name:            node.Name,
		PkgVersion:      node.PkgVersion,
		Target:          prebuilt.Target{OS: record.Input.Platform.OS, Arch: record.Input.Platform.Architecture},
		Source:          prebuilt.SourceExpectation{Size: node.Bottle.PrebuiltDerivation.Source.Size, SHA256: node.Bottle.PrebuiltDerivation.Source.SHA256},
		FormulaSHA256:   authorization.FormulaSourceDigest,
		Entries:         entries,
		PayloadPath:     authorization.Install.Source,
		GoBuild:         prebuilt.GoBuildProfile{ModulePath: authorization.Binary.GoModule, CGOEnabled: *authorization.Binary.CGOEnabled},
		SourceDateEpoch: generatedAt,
		Limits:          limits,
	}
	canonical, err := prebuilt.CanonicalProfile(profile)
	if err != nil {
		return "", fmt.Errorf("canonicalize authorized prebuilt recipe: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func authorizedArchiveModeV2(authorization policyv2.PrebuiltArchivePolicy, sourcePath string) (uint32, error) {
	for _, member := range authorization.Archive.Members {
		if member.Path == sourcePath {
			return parsePrebuiltPolicyModeV2(member.Mode)
		}
	}
	return 0, fmt.Errorf("authorized archive omits payload %q", sourcePath)
}

func parsePrebuiltPolicyModeV2(value string) (uint32, error) {
	mode, err := strconv.ParseUint(value, 8, 32)
	if err != nil || mode == 0 || mode > 0o777 {
		return 0, fmt.Errorf("invalid prebuilt policy mode %q", value)
	}
	return uint32(mode), nil
}

func verifyPrebuiltDerivedBottleV2(node resolution.NodeV2, result *bottle.Result) error {
	derivation := node.Bottle.PrebuiltDerivation
	if derivation == nil {
		return nil
	}
	if result == nil {
		return errors.New("prebuilt derived bottle verification result is missing")
	}
	if result.Receipt != nil {
		return fmt.Errorf("prebuilt derived bottle %s unexpectedly contains a receipt", node.ID)
	}
	payload := derivation.Payload
	matches := 0
	for _, entry := range result.Inventory {
		if entry.KegPath != payload.DestinationPath {
			continue
		}
		matches++
		if entry.Type != bottle.EntryRegular || entry.SHA256 != payload.SHA256 || entry.Size != payload.Size || entry.Mode&0o777 != payload.DerivedMode {
			return fmt.Errorf("prebuilt derived payload %s does not match signed source relation", payload.DestinationPath)
		}
	}
	if matches != 1 {
		return fmt.Errorf("prebuilt derived payload %s occurs %d times in verified bottle inventory", payload.DestinationPath, matches)
	}
	return nil
}
