package cataloggenerator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogartifactstore"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
	hboci "github.com/sozercan/dalec-homebrew/internal/homebrew/oci"
	"github.com/sozercan/dalec-homebrew/internal/prebuilt"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

type ProductionArtifactBuilder struct {
	registry        *hboci.Client
	fetcher         artifactFetcher
	serviceOrigin   string
	artifactStore   GeneratedArtifactStore
	tapPolicy       *policyv2.TapPolicy
	tapPolicyDigest string
	derivePrebuilt  prebuiltDeriver
	inspectBottle   bottleInspector
}

func NewProductionArtifactBuilder(fetchConfig fetcher.Config, serviceOrigin string, artifactStore *catalogartifactstore.Store) (*ProductionArtifactBuilder, error) {
	var store GeneratedArtifactStore
	if artifactStore != nil {
		store = catalogArtifactStoreAdapter{store: artifactStore}
	}
	return newProductionArtifactBuilder(fetchConfig, serviceOrigin, store)
}

// NewBuildLocalArtifactBuilder creates a verifier that retains generated
// prebuilt-derived bottles in invocation-local memory rather than publishing
// them through an HTTP service.
func NewBuildLocalArtifactBuilder(fetchConfig fetcher.Config, artifactStore *MemoryArtifactStore) (*ProductionArtifactBuilder, error) {
	if artifactStore == nil {
		return nil, errors.New("build-local artifact store is required")
	}
	return newProductionArtifactBuilder(fetchConfig, "", artifactStore)
}

func newProductionArtifactBuilder(fetchConfig fetcher.Config, serviceOrigin string, artifactStore GeneratedArtifactStore) (*ProductionArtifactBuilder, error) {
	limits := hboci.DefaultLimits()
	limits.BlobBytes = catalog.MaxBottleBytes
	registry, err := hboci.NewClient("https://ghcr.io", hboci.WithLimits(limits))
	if err != nil {
		return nil, err
	}
	boundedFetcher, err := fetcher.New(fetchConfig)
	if err != nil {
		return nil, err
	}
	if serviceOrigin != "" && artifactStore == nil {
		return nil, errors.New("catalog service origin requires artifact storage")
	}
	if serviceOrigin != "" {
		if err := catalog.ValidateServiceOrigin(serviceOrigin); err != nil {
			return nil, fmt.Errorf("catalog service origin: %w", err)
		}
	}
	tapPolicy, err := policyv2.LoadTapPolicy()
	if err != nil {
		return nil, fmt.Errorf("load tap policy: %w", err)
	}
	tapPolicyDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		return nil, fmt.Errorf("digest tap policy: %w", err)
	}
	return &ProductionArtifactBuilder{
		registry:        registry,
		fetcher:         boundedFetcher,
		serviceOrigin:   serviceOrigin,
		artifactStore:   artifactStore,
		tapPolicy:       tapPolicy,
		tapPolicyDigest: tapPolicyDigest,
		derivePrebuilt:  prebuilt.Derive,
		inspectBottle:   bottle.InspectForCatalog,
	}, nil
}

func (b *ProductionArtifactBuilder) Build(ctx context.Context, request *catalog.Request, core CoreSnapshot, catalogs map[catalog.TapID]*catalog.TapCatalog, node catalog.Node, platform catalog.Platform) (catalog.BottleArtifact, error) {
	if b == nil || b.fetcher == nil {
		return catalog.BottleArtifact{}, errors.New("artifact builder is unavailable")
	}
	if request == nil || core == nil {
		return catalog.BottleArtifact{}, errors.New("request and core snapshot are required")
	}
	if node.ID.IsCore() {
		if b.registry == nil {
			return catalog.BottleArtifact{}, errors.New("OCI artifact resolver is unavailable")
		}
		return b.buildCore(ctx, request, core, node, platform)
	}
	document := catalogs[node.ID.Tap()]
	if document == nil {
		return catalog.BottleArtifact{}, fmt.Errorf("tap catalog %s is unavailable", node.ID.Tap())
	}
	var formula *catalog.Formula
	for i := range document.Formulae {
		if document.Formulae[i].ID == node.ID {
			formula = &document.Formulae[i]
			break
		}
	}
	if formula == nil {
		return catalog.BottleArtifact{}, fmt.Errorf("Formula %s is unavailable", node.ID)
	}
	tag := bottleTag(platform)
	if formula.Bottle == nil {
		return b.buildExternalPrebuilt(ctx, request, document, *formula, node, platform)
	}
	if _, ok := selectBottleFile(formula.Bottle.Files, tag); !ok {
		return b.buildExternalPrebuilt(ctx, request, document, *formula, node, platform)
	}
	sharedID, err := formulaid.Parse(string(node.ID))
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	ghcr, err := hboci.MatchHomebrewGHCRRoot(formula.Bottle.RootURL, sharedID.Tap())
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	if ghcr {
		if b.registry == nil {
			return catalog.BottleArtifact{}, errors.New("OCI artifact resolver is unavailable")
		}
		return b.buildExternalOCI(ctx, document, *formula, node, platform, sharedID)
	}
	return b.buildExternalHTTPS(ctx, document, *formula, node, platform)
}

func (b *ProductionArtifactBuilder) buildCore(ctx context.Context, request *catalog.Request, core CoreSnapshot, node catalog.Node, platform catalog.Platform) (catalog.BottleArtifact, error) {
	match, err := core.Lookup(node.ID.Name())
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	formula := match.Formula
	if formula.Bottle == nil {
		return catalog.BottleArtifact{}, fmt.Errorf("core Formula %s has no bottle", node.ID)
	}
	files := make(map[string]hboci.BottleFile, len(formula.Bottle.Files))
	for _, file := range formula.Bottle.Files {
		files[file.Tag] = hboci.BottleFile{Cellar: file.Cellar, SHA256: file.SHA256}
	}
	ociFormula := hboci.Formula{Name: formula.Name, FullName: formula.FullName, StableVersion: formula.StableVersion, Revision: formula.Revision, VersionScheme: formula.VersionScheme, BottleRebuild: formula.Bottle.Rebuild, License: formula.License, KegOnly: formula.KegOnlyFor(bottleTag(platform)), BottleFiles: files}
	sourceIdentity, err := b.registry.DiscoverCoreFormulaIdentity(ctx, ociFormula)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	resolved, err := b.registry.Resolve(ctx, ociFormula, ociPlatform(platform))
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	currentDigest := core.Info().FormulaDigest
	return b.finishOCI(ctx, node, platform, currentDigest, sourceIdentity.SourceRepository(), sourceIdentity.SourceCommit(), sourceIdentity.FormulaPath(), resolved.Filename, resolved.SelectedBottleTag, resolved.Cellar, resolved.HomebrewSHA256, resolved.Reference.Repository, resolved.Index, resolved.Manifest, resolved.Config, resolved.Layer, resolved.Tab, resolved.ExecutablePaths, resolved.ManifestAnnotations)
}

func (b *ProductionArtifactBuilder) buildExternalOCI(ctx context.Context, document *catalog.TapCatalog, formula catalog.Formula, node catalog.Node, platform catalog.Platform, sharedID formulaid.FormulaID) (catalog.BottleArtifact, error) {
	identity, err := b.registry.DiscoverAuthenticatedFormulaIdentity(ctx, sharedID, formula.HomebrewFullName, formula.StableVersion, formula.Revision, formula.Bottle.Rebuild)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	files := make(map[string]hboci.BottleFile, len(formula.Bottle.Files))
	for _, file := range formula.Bottle.Files {
		files[file.Tag] = hboci.BottleFile{Cellar: file.Cellar, SHA256: strings.TrimPrefix(file.SHA256, "sha256:")}
	}
	resolved, err := b.registry.ResolveAuthenticated(ctx, hboci.AuthenticatedFormula{Identity: identity, BottleRootURL: formula.Bottle.RootURL, StableVersion: formula.StableVersion, Revision: formula.Revision, VersionScheme: formula.VersionScheme, BottleRebuild: formula.Bottle.Rebuild, License: formula.License, KegOnly: node.KegOnly, BottleFiles: files}, ociPlatform(platform))
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	return b.finishOCI(ctx, node, platform, formula.SourceDigest, identity.SourceRepository(), identity.SourceCommit(), identity.FormulaPath(), resolved.Filename, resolved.SelectedBottleTag, resolved.Cellar, resolved.HomebrewSHA256, resolved.Reference.Repository, resolved.Index, resolved.Manifest, resolved.Config, resolved.Layer, resolved.Tab, resolved.ExecutablePaths, resolved.ManifestAnnotations)
}

func (b *ProductionArtifactBuilder) finishOCI(ctx context.Context, node catalog.Node, platform catalog.Platform, currentDigest, sourceRepository, sourceCommit, formulaPath, filename, tag, cellar, checksum, repository string, index, manifest, config, layer ocispec.Descriptor, tab resolution.BottleTab, executablePaths []string, manifestAnnotations map[string]string) (catalog.BottleArtifact, error) {
	if layer.Size <= 0 || layer.Size > catalog.MaxBottleBytes {
		return catalog.BottleArtifact{}, fmt.Errorf("OCI bottle layer size %d is outside 1..%d", layer.Size, catalog.MaxBottleBytes)
	}
	file, err := os.CreateTemp("", "dalec-homebrew-bottle-oci-")
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if err := b.registry.FetchBlobTo(ctx, repository, layer, file); err != nil {
		return catalog.BottleArtifact{}, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return catalog.BottleArtifact{}, err
	}
	inspection, err := bottle.InspectForCatalog(file, inspectionExpectation(node, tag, layer.Digest.String(), layer.Size, checksum, tab), bottle.Options{})
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	if err := b.verifyAnnotatedFormulaSource(ctx, sourceRepository, sourceCommit, formulaPath, inspection); err != nil {
		return catalog.BottleArtifact{}, err
	}
	provenance, err := b.provenanceForOCI(ctx, repository, sourceRepository, layer, index, manifest, manifestAnnotations)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	authenticated, err := catalogBottleTab(tab)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	if inspection.Receipt != nil {
		discovered, err := catalogDependencies(inspection.RuntimeDependencies)
		if err != nil {
			return catalog.BottleArtifact{}, err
		}
		if !reflect.DeepEqual(discovered, authenticated.Dependencies) {
			return catalog.BottleArtifact{}, errors.New("bottle receipt runtime dependencies differ from authenticated OCI tab")
		}
	}
	artifact := baseArtifact(node, platform, currentDigest, sourceRepository, sourceCommit, formulaPath, filename, tag, cellar, layer.Digest.String(), layer.Size, inspection, authenticated, executablePaths, provenance)
	artifact.Transport.OCI = &catalog.OCITransport{Registry: "ghcr.io", Repository: repository, Index: descriptor(index), Manifest: descriptor(manifest), Config: descriptor(config), Layer: descriptor(layer)}
	if err := catalog.ValidateBottleArtifact(artifact); err != nil {
		return catalog.BottleArtifact{}, err
	}
	return artifact, nil
}

func (b *ProductionArtifactBuilder) verifyAnnotatedFormulaSource(ctx context.Context, repository, commit, formulaPath string, inspection *bottle.CatalogInspection) error {
	if inspection == nil || inspection.Formula.SHA256 == "" || inspection.Formula.Size <= 0 {
		return errors.New("verified embedded Formula evidence is missing")
	}
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("annotated Formula repository is not canonical GitHub HTTPS")
	}
	rawURL := "https://raw.githubusercontent.com" + parsed.Path + "/" + commit + "/" + formulaPath
	probe, err := b.fetcher.Probe(ctx, rawURL)
	if err != nil {
		return fmt.Errorf("probe annotated Formula source: %w", err)
	}
	if probe.Size > bottle.DefaultLimits().MaxFormulaBytes {
		return fmt.Errorf("annotated Formula source size %d exceeds %d", probe.Size, bottle.DefaultLimits().MaxFormulaBytes)
	}
	var source bytes.Buffer
	_, err = b.fetcher.FetchObserved(ctx, rawURL, probe.Size, path.Base(formulaPath), uniqueSorted(probe.RedirectHostSequence), &source)
	if err != nil {
		return fmt.Errorf("fetch annotated Formula source: %w", err)
	}
	normalized, err := normalizedBottleFormula(source.Bytes())
	if err != nil {
		return err
	}
	digest := sha256.Sum256(normalized)
	actual := "sha256:" + hex.EncodeToString(digest[:])
	if int64(len(normalized)) != inspection.Formula.Size || actual != inspection.Formula.SHA256 {
		return fmt.Errorf("annotated Formula source normalized to %s/%d, embedded Formula is %s/%d", actual, len(normalized), inspection.Formula.SHA256, inspection.Formula.Size)
	}
	return nil
}

func (b *ProductionArtifactBuilder) buildExternalHTTPS(ctx context.Context, document *catalog.TapCatalog, formula catalog.Formula, node catalog.Node, platform catalog.Platform) (catalog.BottleArtifact, error) {
	declaration, ok := selectBottleFile(formula.Bottle.Files, bottleTag(platform))
	if !ok {
		return catalog.BottleArtifact{}, fmt.Errorf("Formula %s bottle is unavailable for %s", node.ID, platform.Architecture)
	}
	probe, err := b.fetcher.Probe(ctx, declaration.URL)
	if err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("probe bottle size: %w", err)
	}
	filename, err := bottleFilename(probe.FinalURL)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	allowlist := uniqueSorted(probe.RedirectHostSequence)
	request := fetcher.Request{SchemaVersion: fetcher.RequestSchemaVersion, FetchPolicyVersion: fetcher.FetchPolicyVersion, ArtifactID: string(node.ID), URL: declaration.URL, ExpectedSize: probe.Size, SHA256: strings.TrimPrefix(declaration.SHA256, "sha256:"), Filename: filename, AllowedRedirectHosts: allowlist}
	if err := fetcher.ValidateRequest(request); err != nil {
		return catalog.BottleArtifact{}, err
	}
	file, err := os.CreateTemp("", "dalec-homebrew-bottle-https-")
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	evidence, err := b.fetcher.Fetch(ctx, request, file)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	if err := fetcher.VerifyEvidence(evidence, request); err != nil {
		return catalog.BottleArtifact{}, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return catalog.BottleArtifact{}, err
	}
	inspection, err := bottle.InspectForCatalog(file, inspectionExpectation(node, declaration.Tag, declaration.SHA256, probe.Size, strings.TrimPrefix(declaration.SHA256, "sha256:"), resolution.BottleTab{}), bottle.Options{})
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	tab := catalog.BottleTab{}
	if inspection.Receipt != nil {
		dependencies, err := catalogDependencies(inspection.RuntimeDependencies)
		if err != nil {
			return catalog.BottleArtifact{}, err
		}
		tab.Dependencies = dependencies
		tab.HomebrewVersion = inspection.Receipt.HomebrewVersion
		tab.Arch = inspection.Receipt.Arch
	} else {
		tab.Receiptless = true
		tab.Arch = "x86_64"
		if platform.Architecture == "arm64" {
			tab.Arch = "arm64"
		}
	}
	if declaration.Tag == "all" {
		tab.Arch = ""
	}
	artifactDigest, err := digest.Parse(declaration.SHA256)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	provenance, err := b.provenanceForHTTPS(ctx, declaration.URL, document.Tap.Repository, artifactDigest)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	artifact := baseArtifact(node, platform, formula.SourceDigest, "", "", "", filename, declaration.Tag, declaration.Cellar, declaration.SHA256, probe.Size, inspection, tab, executablePathsFromInventory(inspection.Inventory), provenance)
	artifact.BottleSourceWaiver = catalog.HTTPSBottleSourceWaiver
	artifact.Transport.HTTPS = &catalog.HTTPSTransport{URL: declaration.URL, ExpectedSize: probe.Size, SHA256: declaration.SHA256, Filename: filename, AllowedRedirectHosts: allowlist, FetchPolicyVersion: catalog.HTTPSFetchPolicyVersion}
	if err := catalog.ValidateBottleArtifact(artifact); err != nil {
		return catalog.BottleArtifact{}, err
	}
	return artifact, nil
}

func baseArtifact(node catalog.Node, platform catalog.Platform, currentDigest, sourceRepository, sourceCommit, formulaPath, filename, tag, cellar, sha string, size int64, inspection *bottle.CatalogInspection, tab catalog.BottleTab, executablePaths []string, provenance catalog.Provenance) catalog.BottleArtifact {
	slices.Sort(executablePaths)
	return catalog.BottleArtifact{
		ID: node.ID, Platform: platform, Tag: tag, Filename: filename, SHA256: sha, Size: size, Cellar: cellar, Tab: tab,
		CurrentFormulaSourceDigest: currentDigest, BottleFormulaSourceDigest: inspection.Formula.SHA256,
		BottleSourceRepository: sourceRepository, BottleSourceCommit: sourceCommit, BottleFormulaPath: formulaPath,
		ExecutablePaths: executablePaths,
		Verification:    catalog.BottleVerification{PolicyVersion: catalog.BottleVerificationPolicy, InventoryDigest: inspection.InventorySHA256, EntryCount: len(inspection.Inventory), ExpandedSize: inspection.ExpandedSize},
		Provenance:      provenance,
	}
}

func inspectionExpectation(node catalog.Node, tag, compressedDigest string, size int64, homebrewChecksum string, tab resolution.BottleTab) bottle.Expectation {
	return bottle.Expectation{Name: node.Name, FullName: node.HomebrewFullName, FormulaVersion: node.FormulaVersion, FormulaRevision: node.FormulaRevision, PkgVersion: node.PkgVersion, VersionScheme: node.VersionScheme, BottleRebuild: node.BottleRebuild, BottleTag: tag, CompressedSHA256: compressedDigest, CompressedSize: size, HomebrewSHA256: homebrewChecksum, HomebrewVersion: tab.HomebrewVersion, Arch: tab.Arch, Compiler: tab.Compiler, ExpectedTap: string(node.Tap), FormulaIdentity: string(node.ID)}
}

func catalogDependencies(values []bottle.ReceiptDependency) ([]catalog.BottleRuntimeDependency, error) {
	result := make([]catalog.BottleRuntimeDependency, 0, len(values))
	for _, value := range values {
		id, err := formulaid.Parse(value.FullName)
		if err != nil {
			return nil, err
		}
		canonical := catalog.FormulaIDFromShared(id)
		result = append(result, catalog.BottleRuntimeDependency{ID: canonical, HomebrewFullName: canonical, Version: value.Version, Revision: value.Revision, BottleRebuild: value.BottleRebuild, PkgVersion: value.PkgVersion, DeclaredDirectly: value.DeclaredDirectly})
	}
	slices.SortFunc(result, func(a, b catalog.BottleRuntimeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return result, nil
}

func catalogBottleTab(tab resolution.BottleTab) (catalog.BottleTab, error) {
	dependencies := make([]bottle.ReceiptDependency, 0, len(tab.Dependencies))
	for _, value := range tab.Dependencies {
		dependencies = append(dependencies, bottle.ReceiptDependency{FullName: value.FullName, Version: value.Version, Revision: value.Revision, BottleRebuild: value.BottleRebuild, PkgVersion: value.PkgVersion, DeclaredDirectly: value.DeclaredDirectly})
	}
	converted, err := catalogDependencies(dependencies)
	if err != nil {
		return catalog.BottleTab{}, err
	}
	if len(converted) == 0 {
		converted = nil
	}
	changedFiles := slices.Clone(tab.ChangedFiles)
	if len(changedFiles) == 0 {
		changedFiles = nil
	}
	return catalog.BottleTab{HomebrewVersion: tab.HomebrewVersion, Arch: tab.Arch, Compiler: tab.Compiler, ChangedFiles: changedFiles, BuiltOn: catalog.BottleBuiltOn{OS: tab.BuiltOn.OS, OSVersion: tab.BuiltOn.OSVersion, CPUFamily: tab.BuiltOn.CPUFamily, OldestCPUFamily: tab.BuiltOn.OldestCPUFamily, GlibcVersion: tab.BuiltOn.GlibcVersion}, Dependencies: converted}, nil
}

func descriptor(value ocispec.Descriptor) catalog.Descriptor {
	annotations := make([]catalog.Annotation, 0, len(value.Annotations))
	for key, entry := range value.Annotations {
		annotations = append(annotations, catalog.Annotation{Key: key, Value: entry})
	}
	slices.SortFunc(annotations, func(a, b catalog.Annotation) int {
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
	var platform *catalog.Platform
	if value.Platform != nil {
		platform = &catalog.Platform{OS: value.Platform.OS, Architecture: value.Platform.Architecture, Variant: value.Platform.Variant}
	}
	return catalog.Descriptor{Digest: value.Digest.String(), Size: value.Size, MediaType: value.MediaType, Platform: platform, Annotations: annotations}
}

func ociPlatform(platform catalog.Platform) ocispec.Platform {
	return ocispec.Platform{OS: platform.OS, Architecture: platform.Architecture, Variant: platform.Variant}
}

func bottleTag(platform catalog.Platform) string {
	if platform.Architecture == "arm64" {
		return hboci.BottleTagARM64Linux
	}
	return hboci.BottleTagX8664Linux
}

func selectBottleFile(files []catalog.BottleFile, tag string) (catalog.BottleFile, bool) {
	for _, file := range files {
		if file.Tag == tag {
			return file, true
		}
	}
	for _, file := range files {
		if file.Tag == "all" {
			return file, true
		}
	}
	return catalog.BottleFile{}, false
}

func bottleFilename(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	filename, err := url.PathUnescape(path.Base(parsed.EscapedPath()))
	if err != nil || filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\\`) {
		return "", errors.New("bottle URL has no safe filename")
	}
	return filename, nil
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func executablePathsFromInventory(entries []bottle.InventoryEntry) []string {
	result := make([]string, 0)
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if entry.Type != bottle.EntryRegular || entry.Mode&0o111 == 0 || !(strings.HasPrefix(entry.KegPath, "bin/") || strings.HasPrefix(entry.KegPath, "sbin/")) {
			continue
		}
		if _, duplicate := seen[entry.KegPath]; duplicate {
			continue
		}
		seen[entry.KegPath] = struct{}{}
		result = append(result, entry.KegPath)
	}
	slices.Sort(result)
	return result
}
