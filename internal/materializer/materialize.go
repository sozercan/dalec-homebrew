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
	"path"
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
	epoch := time.Unix(cfg.Record.SourceDateEpoch, 0)
	for _, name := range []string{"materialization.json", "resolution.json", runtimefs.InventoryFileName, runtimefs.PruneFileName, runtimefs.ManifestFileName, runtimefs.SBOMFileName, "runtime-base-packages.tsv", "runtime-base-artifacts.tsv"} {
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
	sortSPDX(&doc)
	return doc, data, nil
}

func mergeRuntimeBaseArtifacts(doc runtimefs.SPDXDocument, filename string) (runtimefs.SPDXDocument, []byte, error) {
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil, nil
	}
	if err != nil {
		return doc, nil, err
	}
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), "\t")
		if len(parts) != 8 {
			return doc, nil, fmt.Errorf("invalid runtime base artifact row %q", scanner.Text())
		}
		kind, distribution, name, version, architecture, sourceRef, checksum, artifactPath := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5], parts[6], parts[7]
		if kind != "deb" || distribution == "" || name == "" || version == "" || architecture == "" {
			return doc, nil, fmt.Errorf("invalid runtime base artifact row %q", scanner.Text())
		}
		_, sourceDigest, pinned := strings.Cut(sourceRef, "@")
		parsedSourceDigest, sourceErr := digest.Parse(sourceDigest)
		if !pinned || sourceErr != nil || parsedSourceDigest.Algorithm() != digest.SHA256 {
			return doc, nil, fmt.Errorf("runtime base artifact %q source is not digest-pinned", name)
		}
		d, err := digest.Parse(checksum)
		if err != nil || d.Algorithm() != digest.SHA256 {
			return doc, nil, fmt.Errorf("runtime base artifact %q has invalid sha256 %q", name, checksum)
		}
		if !strings.HasPrefix(artifactPath, "/") || path.Clean(artifactPath) != artifactPath {
			return doc, nil, fmt.Errorf("runtime base artifact %q has invalid path %q", name, artifactPath)
		}
		identity := strings.Join(parts, "\x00")
		sum := sha256.Sum256([]byte(identity))
		id := "SPDXRef-RuntimeArtifact-" + hex.EncodeToString(sum[:8])
		if _, ok := seen[id]; ok {
			return doc, nil, fmt.Errorf("duplicate runtime base artifact %q", name)
		}
		seen[id] = struct{}{}
		doc.Packages = append(doc.Packages, runtimefs.SPDXPackage{
			Name:             name,
			SPDXID:           id,
			VersionInfo:      version,
			PackageFileName:  artifactPath,
			DownloadLocation: "NOASSERTION",
			FilesAnalyzed:    false,
			LicenseConcluded: "NOASSERTION",
			LicenseDeclared:  "NOASSERTION",
			CopyrightText:    "NOASSERTION",
			Checksums:        []runtimefs.SPDXChecksum{{Algorithm: "SHA256", ChecksumValue: d.Encoded()}},
			ExternalRefs: []runtimefs.SPDXExternalRef{
				{ReferenceCategory: "PACKAGE-MANAGER", ReferenceType: "purl", ReferenceLocator: "pkg:deb/" + url.QueryEscape(distribution) + "/" + url.QueryEscape(name) + "@" + url.QueryEscape(version) + "?arch=" + url.QueryEscape(architecture)},
				{ReferenceCategory: "OTHER", ReferenceType: "container-image", ReferenceLocator: sourceRef},
			},
		})
		doc.DocumentDescribes = append(doc.DocumentDescribes, id)
	}
	if err := scanner.Err(); err != nil {
		return doc, nil, err
	}
	sortSPDX(&doc)
	return doc, data, nil
}

func sortSPDX(doc *runtimefs.SPDXDocument) {
	slices.SortFunc(doc.Packages, func(a, b runtimefs.SPDXPackage) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.SPDXID, b.SPDXID)
	})
	slices.Sort(doc.DocumentDescribes)
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
