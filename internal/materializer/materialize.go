package materializer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
)

type MaterializeConfig struct {
	Record     *resolution.Record
	BottlesDir string
	OutputRoot string
	Prefix     string
	User       string
	Timeout    time.Duration
	Runner     Runner
}

func Materialize(ctx context.Context, cfg MaterializeConfig) (*runtimefs.Result, error) {
	if cfg.Record == nil {
		return nil, fmt.Errorf("nil resolution record")
	}
	if cfg.OutputRoot == "" {
		return nil, fmt.Errorf("output root is required")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	allowlist, err := policy.BindRuntimePolicy(cfg.Record)
	if err != nil {
		return nil, err
	}
	installEvidence, err := Install(ctx, Config{Record: cfg.Record, BottlesDir: cfg.BottlesDir, Prefix: cfg.Prefix, User: cfg.User, Timeout: cfg.Timeout, Runner: cfg.Runner})
	if err != nil {
		return nil, err
	}
	for _, node := range cfg.Record.Nodes {
		stateDir := filepath.Join(cfg.Prefix, "var", node.Name)
		if err := os.MkdirAll(stateDir, 0o750); err != nil {
			return nil, err
		}
	}
	outputPrefix := filepath.Join(cfg.OutputRoot, filepath.FromSlash(cfg.Prefix))
	result, err := runtimefs.Assemble(cfg.Prefix, outputPrefix, cfg.Record, runtimefs.Options{InstallPrefix: cfg.Prefix, Allowlist: allowlist})
	if err != nil {
		return nil, err
	}
	evidenceDir := filepath.Join(cfg.OutputRoot, "usr/share/dalec-homebrew")
	var baseInventory []byte
	if merged, inventory, err := mergeRuntimeBaseSBOM(result.SBOM, "/usr/share/dalec-homebrew/runtime-base-packages.tsv"); err != nil {
		return nil, err
	} else if len(inventory) > 0 {
		baseInventory = inventory
		if err := replaceSBOM(result, merged); err != nil {
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
	if err := writeCanonical(filepath.Join(evidenceDir, "materialization.json"), installEvidence, 0o444); err != nil {
		return nil, err
	}
	resolutionJSON, err := resolution.Canonical(cfg.Record)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "resolution.json"), resolutionJSON, 0o444); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "manifest.json"), result.Evidence.RuntimeManifest, 0o444); err != nil {
		return nil, err
	}
	epoch := time.Unix(cfg.Record.SourceDateEpoch, 0)
	for _, name := range []string{"materialization.json", "resolution.json", "manifest.json", runtimefs.InventoryFileName, runtimefs.PruneFileName, runtimefs.ManifestFileName, runtimefs.SBOMFileName, "runtime-base-packages.tsv"} {
		_ = os.Chtimes(filepath.Join(evidenceDir, name), epoch, epoch)
	}
	return result, nil
}

func writeCanonical(filename string, value any, mode os.FileMode) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filename, data, mode)
}

func mergeRuntimeBaseSBOM(doc runtimefs.SPDXDocument, filename string) (runtimefs.SPDXDocument, []byte, error) {
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil, nil
	}
	if err != nil {
		return doc, nil, err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), "\t")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			return doc, nil, fmt.Errorf("invalid runtime base package row %q", scanner.Text())
		}
		sum := sha256.Sum256([]byte(parts[0] + "\x00" + parts[1] + "\x00" + parts[2]))
		id := "SPDXRef-Ubuntu-" + hex.EncodeToString(sum[:8])
		doc.Packages = append(doc.Packages, runtimefs.SPDXPackage{Name: parts[0], SPDXID: id, VersionInfo: parts[1], PackageFileName: "runtime-base", DownloadLocation: "NOASSERTION", FilesAnalyzed: false, LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", CopyrightText: "NOASSERTION", ExternalRefs: []runtimefs.SPDXExternalRef{{ReferenceCategory: "PACKAGE-MANAGER", ReferenceType: "purl", ReferenceLocator: "pkg:deb/ubuntu/" + url.QueryEscape(parts[0]) + "@" + url.QueryEscape(parts[1]) + "?arch=" + url.QueryEscape(parts[2])}}})
		doc.DocumentDescribes = append(doc.DocumentDescribes, id)
	}
	if err := scanner.Err(); err != nil {
		return doc, nil, err
	}
	slices.SortFunc(doc.Packages, func(a, b runtimefs.SPDXPackage) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.SPDXID, b.SPDXID)
	})
	slices.Sort(doc.DocumentDescribes)
	return doc, data, nil
}

func replaceSBOM(result *runtimefs.Result, sbom runtimefs.SPDXDocument) error {
	if result == nil {
		return fmt.Errorf("nil runtime filesystem result")
	}
	result.SBOM = sbom
	sbomJSON, err := json.Marshal(sbom)
	if err != nil {
		return err
	}
	result.Evidence.SBOM = sbomJSON
	result.Evidence.SBOMDigest = digest.FromBytes(sbomJSON).String()
	result.RuntimeManifest.SBOMDigest = result.Evidence.SBOMDigest
	manifestJSON, err := json.Marshal(result.RuntimeManifest)
	if err != nil {
		return err
	}
	result.Evidence.RuntimeManifest = manifestJSON
	result.Evidence.ManifestDigest = digest.FromBytes(manifestJSON).String()
	return nil
}
