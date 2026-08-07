package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
	"github.com/sozercan/dalec-homebrew/internal/resolution"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	annotationPackageType     = "com.github.package.type"
	annotationBottleDigest    = "sh.brew.bottle.digest"
	annotationBottleSize      = "sh.brew.bottle.size"
	annotationBottleTab       = "sh.brew.tab"
	annotationBottleLicense   = "sh.brew.license"
	annotationPathExecFiles   = "sh.brew.path_exec_files"
	homebrewBottlePackageType = "homebrew_bottle"
)

// Result is a fully selected and verified Formula bottle descriptor chain.
type Result struct {
	Formula             Formula
	Reference           FormulaReference
	Target              ocispec.Platform
	SelectedBottleTag   string
	SelectedChildTag    string
	Filename            string
	Cellar              string
	HomebrewSHA256      string
	Index               ocispec.Descriptor
	Manifest            ocispec.Descriptor
	Config              ocispec.Descriptor
	Layer               ocispec.Descriptor
	Tab                 resolution.BottleTab
	SelectedAnnotations map[string]string
	ManifestAnnotations map[string]string
	ExecutablePaths     []string
}

// AuthenticatedResult is the V2 counterpart of Result. It preserves the exact
// verified descriptor chain while retaining the catalog-authenticated tap,
// source, commit, Formula path, and full-name identity used to validate OCI
// annotations.
type AuthenticatedResult struct {
	Formula             AuthenticatedFormula
	Reference           FormulaReference
	Target              ocispec.Platform
	SelectedBottleTag   string
	SelectedChildTag    string
	Filename            string
	Cellar              string
	HomebrewSHA256      string
	Index               ocispec.Descriptor
	Manifest            ocispec.Descriptor
	Config              ocispec.Descriptor
	Layer               ocispec.Descriptor
	Tab                 resolution.BottleTab
	SelectedAnnotations map[string]string
	ManifestAnnotations map[string]string
	ExecutablePaths     []string
}

// ReplayReference returns the immutable repository@layer-digest reference used
// to materialize the already-verified bottle without replaying a mutable tag.
func (result *AuthenticatedResult) ReplayReference() string {
	if result == nil || result.Reference.CanonicalRepository == "" || result.Layer.Digest == "" {
		return ""
	}
	return result.Reference.CanonicalRepository + "@" + result.Layer.Digest.String()
}

type resolvedBottle struct {
	reference           FormulaReference
	target              ocispec.Platform
	selectedBottleTag   string
	selectedChildTag    string
	filename            string
	cellar              string
	homebrewSHA256      string
	index               ocispec.Descriptor
	manifest            ocispec.Descriptor
	config              ocispec.Descriptor
	layer               ocispec.Descriptor
	tab                 resolution.BottleTab
	selectedAnnotations map[string]string
	manifestAnnotations map[string]string
	executablePaths     []string
}

type commonAnnotationValidator func(map[string]string, string, string, string) error
type bottleTabParser func(string, ocispec.Platform, bool) (resolution.BottleTab, error)

// Resolve selects and verifies a current stable Homebrew bottle for target.
func Resolve(ctx context.Context, client *Client, formula Formula, target ocispec.Platform) (*Result, error) {
	if client == nil {
		return nil, errors.New("nil OCI client")
	}
	return client.Resolve(ctx, formula, target)
}

// Resolve selects and verifies a current stable Homebrew bottle for target.
func (client *Client) Resolve(ctx context.Context, formula Formula, target ocispec.Platform) (*Result, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	reference, err := ResolveFormulaReference(formula)
	if err != nil {
		return nil, err
	}
	commonAnnotations := func(annotations map[string]string, pkgVersion, refName, title string) error {
		return validateCommonAnnotations(annotations, formula.Name, pkgVersion, refName, title)
	}
	resolved, err := client.resolveDescriptorChain(
		ctx,
		formula.Name,
		formula.Name,
		formula.License,
		formula.BottleRebuild,
		formula.BottleFiles,
		reference,
		target,
		formula.Name,
		formula.Name,
		commonAnnotations,
		parseBottleTab,
	)
	if err != nil {
		return nil, err
	}
	return &Result{
		Formula:             cloneFormula(formula),
		Reference:           resolved.reference,
		Target:              resolved.target,
		SelectedBottleTag:   resolved.selectedBottleTag,
		SelectedChildTag:    resolved.selectedChildTag,
		Filename:            resolved.filename,
		Cellar:              resolved.cellar,
		HomebrewSHA256:      resolved.homebrewSHA256,
		Index:               resolved.index,
		Manifest:            resolved.manifest,
		Config:              resolved.config,
		Layer:               resolved.layer,
		Tab:                 resolved.tab,
		SelectedAnnotations: resolved.selectedAnnotations,
		ManifestAnnotations: resolved.manifestAnnotations,
		ExecutablePaths:     resolved.executablePaths,
	}, nil
}

// ResolveAuthenticated selects and verifies a catalog-authenticated V2
// Formula bottle for target.
func ResolveAuthenticated(ctx context.Context, client *Client, formula AuthenticatedFormula, target ocispec.Platform) (*AuthenticatedResult, error) {
	if client == nil {
		return nil, errors.New("nil OCI client")
	}
	return client.ResolveAuthenticated(ctx, formula, target)
}

// ResolveAuthenticated selects and verifies a catalog-authenticated V2
// Formula bottle for target while binding all OCI identity annotations to the
// authenticated tap source.
func (client *Client) ResolveAuthenticated(ctx context.Context, formula AuthenticatedFormula, target ocispec.Platform) (*AuthenticatedResult, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	reference, err := ResolveAuthenticatedFormulaReference(formula)
	if err != nil {
		return nil, err
	}
	commonAnnotations := func(annotations map[string]string, pkgVersion, refName, title string) error {
		return validateAuthenticatedCommonAnnotations(annotations, formula.Identity, pkgVersion, refName, title)
	}
	resolved, err := client.resolveDescriptorChain(
		ctx,
		formula.Identity.ID().String(),
		formula.Identity.Name(),
		formula.License,
		formula.BottleRebuild,
		formula.BottleFiles,
		reference,
		target,
		formula.Identity.HomebrewFullName(),
		formula.Identity.HomebrewFullName(),
		commonAnnotations,
		parseAuthenticatedBottleTab,
	)
	if err != nil {
		return nil, err
	}
	return &AuthenticatedResult{
		Formula:             cloneAuthenticatedFormula(formula),
		Reference:           resolved.reference,
		Target:              resolved.target,
		SelectedBottleTag:   resolved.selectedBottleTag,
		SelectedChildTag:    resolved.selectedChildTag,
		Filename:            resolved.filename,
		Cellar:              resolved.cellar,
		HomebrewSHA256:      resolved.homebrewSHA256,
		Index:               resolved.index,
		Manifest:            resolved.manifest,
		Config:              resolved.config,
		Layer:               resolved.layer,
		Tab:                 resolved.tab,
		SelectedAnnotations: resolved.selectedAnnotations,
		ManifestAnnotations: resolved.manifestAnnotations,
		ExecutablePaths:     resolved.executablePaths,
	}, nil
}

func (client *Client) resolveDescriptorChain(
	ctx context.Context,
	formulaLabel string,
	formulaName string,
	license string,
	bottleRebuild int,
	bottleFiles map[string]BottleFile,
	reference FormulaReference,
	target ocispec.Platform,
	indexTitle string,
	manifestTitlePrefix string,
	validateAnnotations commonAnnotationValidator,
	parseTab bottleTabParser,
) (*resolvedBottle, error) {
	selectedBottleTag, bottleFile, err := selectBottleFileFromMap(formulaLabel, bottleFiles, target)
	if err != nil {
		return nil, err
	}
	childTag, err := ChildTag(reference.PkgVersion, selectedBottleTag, bottleRebuild)
	if err != nil {
		return nil, err
	}
	filename, err := BottleFilename(formulaName, reference.PkgVersion, selectedBottleTag, bottleRebuild)
	if err != nil {
		return nil, err
	}

	indexContent, err := client.FetchIndex(ctx, reference.Repository, reference.IndexTag)
	if err != nil {
		return nil, fmt.Errorf("fetch Formula %q OCI index: %w", formulaLabel, err)
	}
	var index ocispec.Index
	if err := decodeJSON(indexContent.Bytes, &index); err != nil {
		return nil, fmt.Errorf("decode Formula %q OCI index: %w", formulaLabel, err)
	}
	selected, tab, executablePaths, err := validateAndSelectIndexWith(
		client.limits,
		index,
		formulaLabel,
		license,
		reference,
		target,
		selectedBottleTag,
		childTag,
		bottleFile,
		indexTitle,
		validateAnnotations,
		parseTab,
	)
	if err != nil {
		return nil, fmt.Errorf("validate Formula %q OCI index: %w", formulaLabel, err)
	}

	manifestContent, err := client.FetchManifest(ctx, reference.Repository, selected)
	if err != nil {
		return nil, fmt.Errorf("fetch Formula %q child manifest: %w", formulaLabel, err)
	}
	var manifest ocispec.Manifest
	if err := decodeJSON(manifestContent.Bytes, &manifest); err != nil {
		return nil, fmt.Errorf("decode Formula %q child manifest: %w", formulaLabel, err)
	}
	if err := validateManifestWith(
		client.limits,
		manifest,
		reference,
		selected,
		childTag,
		filename,
		bottleFile,
		manifestTitlePrefix+" "+childTag,
		validateAnnotations,
	); err != nil {
		return nil, fmt.Errorf("validate Formula %q child manifest: %w", formulaLabel, err)
	}

	configContent, err := client.FetchConfig(ctx, reference.Repository, manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("fetch Formula %q image config: %w", formulaLabel, err)
	}
	var config ocispec.Image
	if err := decodeJSON(configContent.Bytes, &config); err != nil {
		return nil, fmt.Errorf("decode Formula %q image config: %w", formulaLabel, err)
	}
	if err := validateConfig(config, target, selectedBottleTag == BottleTagAll); err != nil {
		return nil, fmt.Errorf("validate Formula %q image config: %w", formulaLabel, err)
	}

	indexDescriptor := cloneDescriptor(indexContent.Descriptor)
	indexDescriptor.Annotations = cloneAnnotations(index.Annotations)
	return &resolvedBottle{
		reference:           reference,
		target:              target,
		selectedBottleTag:   selectedBottleTag,
		selectedChildTag:    childTag,
		filename:            filename,
		cellar:              bottleFile.Cellar,
		homebrewSHA256:      bottleFile.SHA256,
		index:               indexDescriptor,
		manifest:            cloneDescriptor(selected),
		config:              cloneDescriptor(manifest.Config),
		layer:               cloneDescriptor(manifest.Layers[0]),
		tab:                 tab,
		selectedAnnotations: cloneAnnotations(selected.Annotations),
		manifestAnnotations: cloneAnnotations(manifest.Annotations),
		executablePaths:     append([]string(nil), executablePaths...),
	}, nil
}

// ResolutionBottle converts the verified result into the immutable resolution
// record's bottle representation.
func (result *Result) ResolutionBottle() resolution.Bottle {
	if result == nil {
		return resolution.Bottle{}
	}
	annotations := make([]resolution.KV, 0, len(result.SelectedAnnotations))
	for key, value := range result.SelectedAnnotations {
		annotations = append(annotations, resolution.KV{Key: key, Value: value})
	}
	slices.SortFunc(annotations, func(left, right resolution.KV) int {
		if compared := strings.Compare(left.Key, right.Key); compared != 0 {
			return compared
		}
		return strings.Compare(left.Value, right.Value)
	})
	return resolution.Bottle{
		Tag:                 result.SelectedBottleTag,
		Filename:            result.Filename,
		Repository:          result.Reference.CanonicalRepository,
		Index:               resolutionDescriptor(result.Index),
		Manifest:            resolutionDescriptor(result.Manifest),
		Config:              resolutionDescriptor(result.Config),
		Layer:               resolutionDescriptor(result.Layer),
		HomebrewSHA256:      result.HomebrewSHA256,
		Cellar:              result.Cellar,
		Tab:                 result.Tab,
		SelectedAnnotations: annotations,
	}
}

// ResolutionNode converts the verified result into a resolution closure node.
func (result *Result) ResolutionNode() resolution.Node {
	if result == nil {
		return resolution.Node{}
	}
	dependencies := make([]resolution.Requirement, 0, len(result.Tab.Dependencies))
	for _, dependency := range result.Tab.Dependencies {
		dependencies = append(dependencies, resolution.Requirement{
			Name:          dependency.FullName,
			Minimum:       dependency.PkgVersion,
			Revision:      dependency.Revision,
			BottleRebuild: dependency.BottleRebuild,
			Direct:        dependency.DeclaredDirectly,
		})
	}
	slices.SortFunc(dependencies, func(left, right resolution.Requirement) int {
		return strings.Compare(left.Name, right.Name)
	})
	return resolution.Node{
		Name:              result.Formula.Name,
		FullName:          canonicalFullName(result.Formula.Name),
		FormulaVersion:    result.Formula.StableVersion,
		FormulaRevision:   result.Formula.Revision,
		PkgVersion:        result.Reference.PkgVersion,
		VersionScheme:     result.Formula.VersionScheme,
		BottleRebuild:     result.Formula.BottleRebuild,
		License:           result.Formula.License,
		KegOnly:           result.Formula.KegOnly,
		Dependencies:      dependencies,
		Bottle:            result.ResolutionBottle(),
		ExecutablePaths:   append([]string(nil), result.ExecutablePaths...),
		UpstreamFormulaID: canonicalFullName(result.Formula.Name),
	}
}

func selectBottleFile(formula Formula, target ocispec.Platform) (string, BottleFile, error) {
	return selectBottleFileFromMap(formula.Name, formula.BottleFiles, target)
}

func selectBottleFileFromMap(formulaLabel string, bottleFiles map[string]BottleFile, target ocispec.Platform) (string, BottleFile, error) {
	targetTag, err := targetBottleTag(target)
	if err != nil {
		return "", BottleFile{}, err
	}
	if file, ok := bottleFiles[targetTag]; ok {
		if err := validateBottleFile(formulaLabel, targetTag, file); err != nil {
			return "", BottleFile{}, err
		}
		return targetTag, file, nil
	}
	if file, ok := bottleFiles[BottleTagAll]; ok {
		if err := validateBottleFile(formulaLabel, BottleTagAll, file); err != nil {
			return "", BottleFile{}, err
		}
		return BottleTagAll, file, nil
	}
	return "", BottleFile{}, fmt.Errorf("formula %q has no %s or all bottle in authenticated metadata", formulaLabel, targetTag)
}

func validateAndSelectIndex(limits Limits, index ocispec.Index, formula Formula, reference FormulaReference, target ocispec.Platform, selectedBottleTag, childTag string, bottleFile BottleFile) (ocispec.Descriptor, resolution.BottleTab, []string, error) {
	commonAnnotations := func(annotations map[string]string, pkgVersion, refName, title string) error {
		return validateCommonAnnotations(annotations, formula.Name, pkgVersion, refName, title)
	}
	return validateAndSelectIndexWith(
		limits,
		index,
		formula.Name,
		formula.License,
		reference,
		target,
		selectedBottleTag,
		childTag,
		bottleFile,
		formula.Name,
		commonAnnotations,
		parseBottleTab,
	)
}

func validateAndSelectIndexWith(
	limits Limits,
	index ocispec.Index,
	_ string,
	license string,
	reference FormulaReference,
	target ocispec.Platform,
	selectedBottleTag string,
	childTag string,
	bottleFile BottleFile,
	indexTitle string,
	validateAnnotations commonAnnotationValidator,
	parseTab bottleTabParser,
) (ocispec.Descriptor, resolution.BottleTab, []string, error) {
	if index.SchemaVersion != 2 {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, fmt.Errorf("unsupported schemaVersion %d", index.SchemaVersion)
	}
	if index.MediaType != "" && index.MediaType != ocispec.MediaTypeImageIndex {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, fmt.Errorf("unexpected index mediaType %q", index.MediaType)
	}
	if index.ArtifactType != "" || index.Subject != nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, errors.New("Homebrew bottle index cannot be an artifact or have a subject")
	}
	if len(index.Manifests) == 0 {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, errors.New("index has no child manifests")
	}
	if validateAnnotations == nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, errors.New("nil common-annotation validator")
	}
	if parseTab == nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, errors.New("nil bottle-tab parser")
	}
	if err := validateAnnotations(index.Annotations, reference.PkgVersion, reference.IndexTag, indexTitle); err != nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, fmt.Errorf("index annotations: %w", err)
	}

	seenReferences := make(map[string]struct{}, len(index.Manifests))
	var selected *ocispec.Descriptor
	for indexPosition := range index.Manifests {
		descriptor := &index.Manifests[indexPosition]
		if err := validateDescriptor(*descriptor, ocispec.MediaTypeImageManifest, limits.ManifestBytes); err != nil {
			return ocispec.Descriptor{}, resolution.BottleTab{}, nil, fmt.Errorf("child descriptor %d: %w", indexPosition, err)
		}
		refName := descriptor.Annotations[ocispec.AnnotationRefName]
		if refName == "" {
			return ocispec.Descriptor{}, resolution.BottleTab{}, nil, fmt.Errorf("child descriptor %d has no %s annotation", indexPosition, ocispec.AnnotationRefName)
		}
		if _, duplicate := seenReferences[refName]; duplicate {
			return ocispec.Descriptor{}, resolution.BottleTab{}, nil, fmt.Errorf("duplicate child ref-name %q", refName)
		}
		seenReferences[refName] = struct{}{}
		if refName == childTag {
			copy := cloneDescriptor(*descriptor)
			selected = &copy
		}
	}
	if selected == nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, fmt.Errorf("index has no expected child %q", childTag)
	}
	if err := validateSelectedPlatform(selected.Platform, target, selectedBottleTag == BottleTagAll); err != nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, err
	}
	if err := validateSelectedAnnotations(selected.Annotations, license, bottleFile, limits.BlobBytes, childTag); err != nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, err
	}
	tab, err := parseTab(selected.Annotations[annotationBottleTab], target, selectedBottleTag == BottleTagAll)
	if err != nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, err
	}
	executablePaths, err := parseExecutablePaths(selected.Annotations[annotationPathExecFiles])
	if err != nil {
		return ocispec.Descriptor{}, resolution.BottleTab{}, nil, err
	}
	return *selected, tab, executablePaths, nil
}

func validateManifest(limits Limits, manifest ocispec.Manifest, formula Formula, reference FormulaReference, selected ocispec.Descriptor, childTag, filename string, bottleFile BottleFile) error {
	commonAnnotations := func(annotations map[string]string, pkgVersion, refName, title string) error {
		return validateCommonAnnotations(annotations, formula.Name, pkgVersion, refName, title)
	}
	return validateManifestWith(
		limits,
		manifest,
		reference,
		selected,
		childTag,
		filename,
		bottleFile,
		formula.Name+" "+childTag,
		commonAnnotations,
	)
}

func validateManifestWith(
	limits Limits,
	manifest ocispec.Manifest,
	reference FormulaReference,
	selected ocispec.Descriptor,
	childTag string,
	filename string,
	bottleFile BottleFile,
	manifestTitle string,
	validateAnnotations commonAnnotationValidator,
) error {
	if manifest.SchemaVersion != 2 {
		return fmt.Errorf("unsupported schemaVersion %d", manifest.SchemaVersion)
	}
	if manifest.MediaType != "" && manifest.MediaType != ocispec.MediaTypeImageManifest {
		return fmt.Errorf("unexpected manifest mediaType %q", manifest.MediaType)
	}
	if manifest.ArtifactType != "" || manifest.Subject != nil {
		return errors.New("Homebrew bottle manifest cannot be an artifact or have a subject")
	}
	if err := validateDescriptor(manifest.Config, ocispec.MediaTypeImageConfig, limits.ConfigBytes); err != nil {
		return fmt.Errorf("config descriptor: %w", err)
	}
	if manifest.Config.Platform != nil {
		return errors.New("config descriptor unexpectedly has a platform")
	}
	if len(manifest.Layers) != 1 {
		return fmt.Errorf("manifest has %d layers, expected exactly one bottle layer", len(manifest.Layers))
	}
	layer := manifest.Layers[0]
	if err := validateDescriptor(layer, ocispec.MediaTypeImageLayerGzip, limits.BlobBytes); err != nil {
		return fmt.Errorf("bottle layer descriptor: %w", err)
	}
	if layer.Platform != nil {
		return errors.New("bottle layer descriptor unexpectedly has a platform")
	}
	if validateAnnotations == nil {
		return errors.New("nil common-annotation validator")
	}
	if err := validateAnnotations(manifest.Annotations, reference.PkgVersion, childTag, manifestTitle); err != nil {
		return fmt.Errorf("manifest annotations: %w", err)
	}
	for key, expected := range selected.Annotations {
		if actual, ok := manifest.Annotations[key]; !ok || actual != expected {
			return fmt.Errorf("manifest annotation %q does not match selected index descriptor", key)
		}
	}
	if manifest.Annotations[annotationBottleDigest] != bottleFile.SHA256 {
		return fmt.Errorf("manifest bottle checksum %q does not match authenticated checksum %q", manifest.Annotations[annotationBottleDigest], bottleFile.SHA256)
	}
	if layer.Digest.String() != "sha256:"+bottleFile.SHA256 {
		return fmt.Errorf("bottle layer digest %s does not match authenticated checksum %s", layer.Digest, bottleFile.SHA256)
	}
	if rawSize := manifest.Annotations[annotationBottleSize]; rawSize != "" {
		annotatedSize, err := parseCanonicalPositiveInt(rawSize, annotationBottleSize)
		if err != nil {
			return err
		}
		if layer.Size != annotatedSize {
			return fmt.Errorf("bottle layer size %d does not match annotated size %d", layer.Size, annotatedSize)
		}
	}
	if title := layer.Annotations[ocispec.AnnotationTitle]; title != filename {
		return fmt.Errorf("bottle layer filename %q, expected %q", title, filename)
	}
	return nil
}

func validateConfig(config ocispec.Image, target ocispec.Platform, allBottle bool) error {
	if config.Architecture == "" || config.OS == "" {
		return errors.New("config has an empty platform")
	}
	if allBottle {
		if config.Architecture != "amd64" && config.Architecture != "arm64" {
			return fmt.Errorf("all-bottle config has unsupported architecture %q", config.Architecture)
		}
		if config.OS != "linux" && config.OS != "darwin" {
			return fmt.Errorf("all-bottle config has unsupported OS %q", config.OS)
		}
	} else if config.Architecture != target.Architecture || config.OS != target.OS {
		return fmt.Errorf("config platform %s/%s does not match target %s/%s", config.OS, config.Architecture, target.OS, target.Architecture)
	}
	if config.RootFS.Type != "layers" {
		return fmt.Errorf("config rootfs type %q, expected %q", config.RootFS.Type, "layers")
	}
	if len(config.RootFS.DiffIDs) != 1 {
		return fmt.Errorf("config has %d diff IDs, expected one", len(config.RootFS.DiffIDs))
	}
	if _, err := parseSHA256Digest(config.RootFS.DiffIDs[0].String()); err != nil {
		return fmt.Errorf("config diff ID: %w", err)
	}
	return nil
}

func validateCommonAnnotations(annotations map[string]string, formulaName, pkgVersion, refName, title string) error {
	for key, expected := range map[string]string{
		annotationPackageType:     homebrewBottlePackageType,
		ocispec.AnnotationTitle:   title,
		ocispec.AnnotationVersion: pkgVersion,
		ocispec.AnnotationRefName: refName,
	} {
		if actual := annotations[key]; actual != expected {
			return fmt.Errorf("annotation %q is %q, expected %q", key, actual, expected)
		}
	}
	if !strings.EqualFold(annotations[ocispec.AnnotationVendor], "homebrew") {
		return fmt.Errorf("annotation %q is %q, expected Homebrew", ocispec.AnnotationVendor, annotations[ocispec.AnnotationVendor])
	}
	source := annotations[ocispec.AnnotationSource]
	if !strings.HasPrefix(strings.ToLower(source), "https://github.com/homebrew/homebrew-core/") {
		return fmt.Errorf("annotation %q is not a homebrew/homebrew-core source URL", ocispec.AnnotationSource)
	}
	if formulaName == "" {
		return errors.New("empty Formula name")
	}
	return nil
}

func validateAuthenticatedCommonAnnotations(annotations map[string]string, identity AuthenticatedFormulaIdentity, pkgVersion, refName, title string) error {
	if err := identity.validate(); err != nil {
		return fmt.Errorf("authenticated Formula identity: %w", err)
	}
	for _, expected := range []struct {
		key   string
		value string
	}{
		{annotationPackageType, homebrewBottlePackageType},
		{ocispec.AnnotationVendor, identity.Publisher()},
		{ocispec.AnnotationTitle, title},
		{ocispec.AnnotationVersion, pkgVersion},
		{ocispec.AnnotationRefName, refName},
		{ocispec.AnnotationRevision, identity.SourceCommit()},
		{ocispec.AnnotationSource, identity.SourceURL()},
	} {
		if actual := annotations[expected.key]; actual != expected.value {
			return fmt.Errorf("annotation %q is %q, expected %q", expected.key, actual, expected.value)
		}
	}
	return nil
}

func validateSelectedAnnotations(annotations map[string]string, _ string, bottleFile BottleFile, maxLayerBytes int64, childTag string) error {
	if annotations[ocispec.AnnotationRefName] != childTag {
		return fmt.Errorf("selected child ref-name %q, expected %q", annotations[ocispec.AnnotationRefName], childTag)
	}
	checksum := annotations[annotationBottleDigest]
	if err := validateSHA256Hex(checksum); err != nil {
		return fmt.Errorf("selected child bottle digest: %w", err)
	}
	if checksum != bottleFile.SHA256 {
		return fmt.Errorf("selected child checksum %q does not match authenticated checksum %q", checksum, bottleFile.SHA256)
	}
	if rawSize := annotations[annotationBottleSize]; rawSize != "" {
		size, err := parseCanonicalPositiveInt(rawSize, annotationBottleSize)
		if err != nil {
			return err
		}
		if size > maxLayerBytes {
			return fmt.Errorf("annotated bottle size %d exceeds %d-byte limit", size, maxLayerBytes)
		}
	}
	if strings.TrimSpace(annotations[annotationBottleTab]) == "" {
		return fmt.Errorf("selected child has no %s annotation", annotationBottleTab)
	}
	return nil
}

func validateSelectedPlatform(platform *ocispec.Platform, target ocispec.Platform, allBottle bool) error {
	if allBottle {
		if platform != nil {
			return fmt.Errorf("all bottle descriptor unexpectedly has platform %s/%s", platform.OS, platform.Architecture)
		}
		return nil
	}
	if platform == nil {
		return errors.New("target-specific bottle descriptor has no platform")
	}
	if platform.OS != target.OS || platform.Architecture != target.Architecture {
		return fmt.Errorf("selected descriptor platform %s/%s does not match target %s/%s", platform.OS, platform.Architecture, target.OS, target.Architecture)
	}
	if platform.Variant != "" || len(platform.OSFeatures) != 0 {
		return errors.New("selected descriptor has unsupported platform variant or OS features")
	}
	return nil
}

type bottleTabWire struct {
	HomebrewVersion string                  `json:"homebrew_version"`
	Arch            string                  `json:"arch"`
	Compiler        string                  `json:"compiler"`
	ChangedFiles    []string                `json:"changed_files"`
	BuiltOn         bottleBuiltOnWire       `json:"built_on"`
	Dependencies    []runtimeDependencyWire `json:"runtime_dependencies"`
}

type bottleBuiltOnWire struct {
	OS              string `json:"os"`
	OSVersion       string `json:"os_version"`
	CPUFamily       string `json:"cpu_family"`
	OldestCPUFamily string `json:"oldest_cpu_family"`
	GlibcVersion    string `json:"glibc_version"`
}

type runtimeDependencyWire struct {
	FullName         string          `json:"full_name"`
	Version          string          `json:"version"`
	Revision         int             `json:"revision"`
	BottleRebuild    int             `json:"bottle_rebuild"`
	PkgVersion       json.RawMessage `json:"pkg_version"`
	DeclaredDirectly bool            `json:"declared_directly"`
}

func parseBottleTab(value string, target ocispec.Platform, allBottle bool) (resolution.BottleTab, error) {
	return parseBottleTabWith(value, target, allBottle, canonicalDependencyName)
}

func parseAuthenticatedBottleTab(value string, target ocispec.Platform, allBottle bool) (resolution.BottleTab, error) {
	return parseBottleTabWith(value, target, allBottle, canonicalAuthenticatedDependencyName)
}

func parseBottleTabWith(value string, target ocispec.Platform, allBottle bool, canonicalizeDependency func(string) (string, error)) (resolution.BottleTab, error) {
	if canonicalizeDependency == nil {
		return resolution.BottleTab{}, errors.New("nil runtime-dependency canonicalizer")
	}
	data := []byte(value)
	var rawFields map[string]json.RawMessage
	if err := decodeJSON(data, &rawFields); err != nil {
		return resolution.BottleTab{}, fmt.Errorf("decode %s: %w", annotationBottleTab, err)
	}
	if _, ok := rawFields["homebrew_version"]; !ok {
		return resolution.BottleTab{}, fmt.Errorf("%s has no homebrew_version", annotationBottleTab)
	}
	rawDependencies, ok := rawFields["runtime_dependencies"]
	if !ok {
		return resolution.BottleTab{}, fmt.Errorf("%s has no runtime_dependencies", annotationBottleTab)
	}
	if strings.TrimSpace(string(rawDependencies)) == "null" {
		return resolution.BottleTab{}, fmt.Errorf("%s runtime_dependencies cannot be null", annotationBottleTab)
	}
	var wire bottleTabWire
	if err := decodeJSON(data, &wire); err != nil {
		return resolution.BottleTab{}, fmt.Errorf("decode %s: %w", annotationBottleTab, err)
	}
	if strings.TrimSpace(wire.HomebrewVersion) == "" {
		return resolution.BottleTab{}, fmt.Errorf("%s has an empty homebrew_version", annotationBottleTab)
	}
	if allBottle {
		if wire.Arch != "" || wire.BuiltOn != (bottleBuiltOnWire{}) {
			return resolution.BottleTab{}, fmt.Errorf("all bottle %s must omit arch and built_on", annotationBottleTab)
		}
	} else {
		expectedArch := "x86_64"
		if target.Architecture == "arm64" {
			expectedArch = "arm64"
		}
		if wire.Arch != expectedArch {
			return resolution.BottleTab{}, fmt.Errorf("tab architecture %q, expected %q", wire.Arch, expectedArch)
		}
		if wire.BuiltOn.OS != "Linux" {
			return resolution.BottleTab{}, fmt.Errorf("tab built_on OS %q, expected Linux", wire.BuiltOn.OS)
		}
	}

	dependencies := make([]resolution.RuntimeDependency, 0, len(wire.Dependencies))
	seen := make(map[string]struct{}, len(wire.Dependencies))
	for index, dependency := range wire.Dependencies {
		name, err := canonicalizeDependency(dependency.FullName)
		if err != nil {
			return resolution.BottleTab{}, fmt.Errorf("runtime dependency %d: %w", index, err)
		}
		if _, duplicate := seen[name]; duplicate {
			return resolution.BottleTab{}, fmt.Errorf("duplicate runtime dependency %q", name)
		}
		seen[name] = struct{}{}
		expectedPkgVersion, err := PkgVersion(dependency.Version, dependency.Revision)
		if err != nil {
			return resolution.BottleTab{}, fmt.Errorf("runtime dependency %q: %w", name, err)
		}
		// Homebrew bottle tabs created before pkg_version was added still carry
		// the version and revision needed to derive it. Preserve the exact raw tab
		// annotation separately, but use the canonical derived value in the record.
		pkgVersion := expectedPkgVersion
		if dependency.PkgVersion != nil {
			if strings.TrimSpace(string(dependency.PkgVersion)) == "null" {
				return resolution.BottleTab{}, fmt.Errorf("runtime dependency %q pkg_version cannot be null", name)
			}
			var annotatedPkgVersion string
			if err := decodeJSON(dependency.PkgVersion, &annotatedPkgVersion); err != nil {
				return resolution.BottleTab{}, fmt.Errorf("runtime dependency %q pkg_version: %w", name, err)
			}
			if annotatedPkgVersion != expectedPkgVersion {
				return resolution.BottleTab{}, fmt.Errorf("runtime dependency %q pkg_version %q, expected %q", name, annotatedPkgVersion, expectedPkgVersion)
			}
			pkgVersion = annotatedPkgVersion
		}
		if dependency.BottleRebuild < 0 {
			return resolution.BottleTab{}, fmt.Errorf("runtime dependency %q has negative bottle rebuild", name)
		}
		dependencies = append(dependencies, resolution.RuntimeDependency{
			FullName:         name,
			Version:          dependency.Version,
			Revision:         dependency.Revision,
			BottleRebuild:    dependency.BottleRebuild,
			PkgVersion:       pkgVersion,
			DeclaredDirectly: dependency.DeclaredDirectly,
		})
	}
	slices.SortFunc(dependencies, func(left, right resolution.RuntimeDependency) int {
		return strings.Compare(left.FullName, right.FullName)
	})
	changed, err := validateChangedFiles(wire.ChangedFiles)
	if err != nil {
		return resolution.BottleTab{}, err
	}
	return resolution.BottleTab{
		HomebrewVersion: wire.HomebrewVersion,
		Arch:            wire.Arch,
		Compiler:        wire.Compiler,
		ChangedFiles:    changed,
		BuiltOn: resolution.BuiltOn{
			OS:              wire.BuiltOn.OS,
			OSVersion:       wire.BuiltOn.OSVersion,
			CPUFamily:       wire.BuiltOn.CPUFamily,
			OldestCPUFamily: wire.BuiltOn.OldestCPUFamily,
			GlibcVersion:    wire.BuiltOn.GlibcVersion,
		},
		Dependencies: dependencies,
	}, nil
}

func validateChangedFiles(values []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || path.IsAbs(value) || strings.Contains(value, "\\") || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
			return nil, fmt.Errorf("invalid changed_files path %q", value)
		}
		if _, ok := seen[value]; ok {
			return nil, fmt.Errorf("duplicate changed_files path %q", value)
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	slices.Sort(out)
	return out, nil
}

func parseExecutablePaths(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item == "" || strings.TrimSpace(item) != item || strings.ContainsAny(item, "\\\x00\r\n") || path.IsAbs(item) || path.Clean(item) != item || item == "." || item == ".." || strings.HasPrefix(item, "../") {
			return nil, fmt.Errorf("invalid %s entry %q", annotationPathExecFiles, item)
		}
		if _, duplicate := seen[item]; duplicate {
			return nil, fmt.Errorf("duplicate %s entry %q", annotationPathExecFiles, item)
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	slices.Sort(result)
	return result, nil
}

func canonicalDependencyName(fullName string) (string, error) {
	name := fullName
	if strings.HasPrefix(fullName, HomebrewCoreRepository+"/") {
		name = strings.TrimPrefix(fullName, HomebrewCoreRepository+"/")
	} else if strings.Contains(fullName, "/") {
		return "", fmt.Errorf("dependency %q is outside homebrew/core", fullName)
	}
	if err := validateFormulaName(name); err != nil {
		return "", err
	}
	return name, nil
}

func canonicalAuthenticatedDependencyName(fullName string) (string, error) {
	id, err := formulaid.Parse(fullName)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func parseCanonicalPositiveInt(value, annotation string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("missing %s annotation", annotation)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
		return 0, fmt.Errorf("invalid %s annotation %q", annotation, value)
	}
	return parsed, nil
}

func validateTarget(target ocispec.Platform) error {
	if target.OS != "linux" || (target.Architecture != "amd64" && target.Architecture != "arm64") {
		return fmt.Errorf("unsupported target platform %s/%s", target.OS, target.Architecture)
	}
	if target.Variant != "" || len(target.OSFeatures) != 0 {
		return errors.New("target variants and OS features are unsupported")
	}
	return nil
}

func targetBottleTag(target ocispec.Platform) (string, error) {
	if err := validateTarget(target); err != nil {
		return "", err
	}
	if target.Architecture == "arm64" {
		return BottleTagARM64Linux, nil
	}
	return BottleTagX8664Linux, nil
}

func resolutionDescriptor(descriptor ocispec.Descriptor) resolution.Descriptor {
	converted := resolution.Descriptor{
		Digest:    descriptor.Digest.String(),
		Size:      descriptor.Size,
		MediaType: descriptor.MediaType,
		Metadata:  cloneAnnotations(descriptor.Annotations),
	}
	if descriptor.Platform != nil {
		converted.Platform = &resolution.Platform{
			OS: descriptor.Platform.OS, Architecture: descriptor.Platform.Architecture,
			Variant: descriptor.Platform.Variant,
		}
	}
	if len(converted.Metadata) == 0 {
		converted.Metadata = nil
	}
	return converted
}

func cloneAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	clone := make(map[string]string, len(annotations))
	for key, value := range annotations {
		clone[key] = value
	}
	return clone
}

func cloneFormula(formula Formula) Formula {
	clone := formula
	if formula.BottleFiles != nil {
		clone.BottleFiles = make(map[string]BottleFile, len(formula.BottleFiles))
		for tag, file := range formula.BottleFiles {
			clone.BottleFiles[tag] = file
		}
	}
	return clone
}

func cloneAuthenticatedFormula(formula AuthenticatedFormula) AuthenticatedFormula {
	clone := formula
	if formula.BottleFiles != nil {
		clone.BottleFiles = make(map[string]BottleFile, len(formula.BottleFiles))
		for tag, file := range formula.BottleFiles {
			clone.BottleFiles[tag] = file
		}
	}
	return clone
}
