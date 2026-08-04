package frontend

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogresolver"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
	hboci "github.com/sozercan/dalec-homebrew/internal/homebrew/oci"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

// ResolveNonCoreOCIArtifacts independently repeats descriptor-chain and
// annotation validation for every non-core GHCR artifact and requires exact
// agreement with the signed catalog-service result.
func ResolveNonCoreOCIArtifacts(ctx context.Context, client *hboci.Client, core catalogresolver.CoreCatalog, result catalog.PlatformResult, catalogs map[catalog.TapID]*catalog.TapCatalog) (catalog.PlatformResult, error) {
	if client == nil {
		return catalog.PlatformResult{}, fmt.Errorf("nil OCI client")
	}
	formulae := map[catalog.FormulaID]struct {
		formula catalog.Formula
		source  catalog.TapSource
	}{}
	for _, document := range catalogs {
		for _, formula := range document.Formulae {
			formulae[formula.ID] = struct {
				formula catalog.Formula
				source  catalog.TapSource
			}{formula: formula, source: document.Tap}
		}
	}
	for i, artifact := range result.Artifacts {
		if artifact.ID.IsCore() {
			if artifact.Transport.OCI == nil {
				return catalog.PlatformResult{}, fmt.Errorf("core Formula %s did not use authenticated OCI transport", artifact.ID)
			}
			if core == nil {
				return catalog.PlatformResult{}, fmt.Errorf("core metadata is required for %s", artifact.ID)
			}
			match, err := core.Lookup(artifact.ID.Name())
			if err != nil {
				return catalog.PlatformResult{}, err
			}
			if match.Formula.Bottle == nil {
				return catalog.PlatformResult{}, fmt.Errorf("core Formula %s has no authenticated bottle declaration", artifact.ID)
			}
			files := make(map[string]hboci.BottleFile, len(match.Formula.Bottle.Files))
			for _, file := range match.Formula.Bottle.Files {
				files[file.Tag] = hboci.BottleFile{Cellar: file.Cellar, SHA256: file.SHA256}
			}
			resolved, err := client.Resolve(ctx, hboci.Formula{Name: match.Formula.Name, FullName: match.Formula.FullName, StableVersion: match.Formula.StableVersion, Revision: match.Formula.Revision, VersionScheme: match.Formula.VersionScheme, BottleRebuild: match.Formula.Bottle.Rebuild, License: match.Formula.License, KegOnly: match.Formula.KegOnlyFor(bottleTagForCatalogPlatform(result.Platform)), BottleFiles: files}, ocispec.Platform{OS: result.Platform.OS, Architecture: result.Platform.Architecture, Variant: result.Platform.Variant})
			if err != nil {
				return catalog.PlatformResult{}, fmt.Errorf("independently resolve core OCI bottle %s: %w", artifact.ID, err)
			}
			recomputed := catalog.OCITransport{Registry: resolved.Reference.Registry, Repository: resolved.Reference.Repository, Index: catalogDescriptor(resolved.Index), Manifest: catalogDescriptor(resolved.Manifest), Config: catalogDescriptor(resolved.Config), Layer: catalogDescriptor(resolved.Layer)}
			resolvedTab, err := catalogBottleTab(resolved.Tab)
			if err != nil {
				return catalog.PlatformResult{}, err
			}
			if artifact.Filename != resolved.Filename || artifact.Tag != resolved.SelectedBottleTag || artifact.Cellar != resolved.Cellar || artifact.SHA256 != "sha256:"+resolved.HomebrewSHA256 || artifact.Size != resolved.Layer.Size || !reflect.DeepEqual(canonicalOCITransport(*artifact.Transport.OCI), canonicalOCITransport(recomputed)) || !reflect.DeepEqual(artifact.Tab, resolvedTab) || !slices.Equal(artifact.ExecutablePaths, resolved.ExecutablePaths) {
				return catalog.PlatformResult{}, fmt.Errorf("signed core OCI artifact for %s differs from official metadata resolution", artifact.ID)
			}
			result.Artifacts[i].Transport.OCI = &recomputed
			continue
		}
		entry, ok := formulae[artifact.ID]
		if !ok {
			return catalog.PlatformResult{}, fmt.Errorf("Formula %s is absent from fetched catalogs", artifact.ID)
		}
		if entry.formula.Bottle == nil {
			return catalog.PlatformResult{}, fmt.Errorf("Formula %s has no bottle declaration", artifact.ID)
		}
		sharedID, err := formulaid.Parse(string(artifact.ID))
		if err != nil {
			return catalog.PlatformResult{}, err
		}
		ghcr, err := hboci.MatchHomebrewGHCRRoot(entry.formula.Bottle.RootURL, sharedID.Tap())
		if err != nil {
			return catalog.PlatformResult{}, fmt.Errorf("classify bottle root for %s: %w", artifact.ID, err)
		}
		if ghcr && artifact.Transport.OCI == nil {
			return catalog.PlatformResult{}, fmt.Errorf("GHCR-backed Formula %s must use OCI transport", artifact.ID)
		}
		if !ghcr && artifact.Transport.OCI != nil {
			return catalog.PlatformResult{}, fmt.Errorf("non-GHCR Formula %s must use HTTPS transport", artifact.ID)
		}
		if artifact.Transport.OCI == nil {
			if artifact.Transport.HTTPS == nil {
				return catalog.PlatformResult{}, fmt.Errorf("HTTPS Formula %s has no authenticated bottle declaration", artifact.ID)
			}
			declaration, ok := selectCatalogBottleFile(entry.formula.Bottle.Files, artifact.Tag)
			if !ok || declaration.URL != artifact.Transport.HTTPS.URL || declaration.SHA256 != artifact.SHA256 || declaration.Cellar != artifact.Cellar || artifact.Transport.HTTPS.SHA256 != artifact.SHA256 || artifact.Transport.HTTPS.Filename != artifact.Filename || artifact.Transport.HTTPS.ExpectedSize != artifact.Size {
				return catalog.PlatformResult{}, fmt.Errorf("signed HTTPS artifact for %s differs from its authenticated Formula declaration", artifact.ID)
			}
			continue
		}
		identity, err := hboci.NewAuthenticatedFormulaIdentity(sharedID, entry.formula.HomebrewFullName, artifact.BottleSourceRepository, artifact.BottleSourceCommit, artifact.BottleFormulaPath)
		if err != nil {
			return catalog.PlatformResult{}, err
		}
		files := make(map[string]hboci.BottleFile, len(entry.formula.Bottle.Files))
		for _, file := range entry.formula.Bottle.Files {
			sha := strings.TrimPrefix(file.SHA256, "sha256:")
			files[file.Tag] = hboci.BottleFile{Cellar: file.Cellar, SHA256: sha}
		}
		resolved, err := client.ResolveAuthenticated(ctx, hboci.AuthenticatedFormula{Identity: identity, BottleRootURL: entry.formula.Bottle.RootURL, StableVersion: entry.formula.StableVersion, Revision: entry.formula.Revision, VersionScheme: entry.formula.VersionScheme, BottleRebuild: entry.formula.Bottle.Rebuild, License: entry.formula.License, KegOnly: entry.formula.KegOnly, BottleFiles: files}, ocispec.Platform{OS: result.Platform.OS, Architecture: result.Platform.Architecture, Variant: result.Platform.Variant})
		if err != nil {
			return catalog.PlatformResult{}, fmt.Errorf("independently resolve OCI bottle %s: %w", artifact.ID, err)
		}
		recomputed := catalog.OCITransport{Registry: resolved.Reference.Registry, Repository: resolved.Reference.Repository, Index: catalogDescriptor(resolved.Index), Manifest: catalogDescriptor(resolved.Manifest), Config: catalogDescriptor(resolved.Config), Layer: catalogDescriptor(resolved.Layer)}
		resolvedTab, err := catalogBottleTab(resolved.Tab)
		if err != nil {
			return catalog.PlatformResult{}, err
		}
		if artifact.Filename != resolved.Filename || artifact.Tag != resolved.SelectedBottleTag || artifact.Cellar != resolved.Cellar || artifact.SHA256 != "sha256:"+resolved.HomebrewSHA256 || artifact.Size != resolved.Layer.Size || !reflect.DeepEqual(artifact.Tab, resolvedTab) || !slices.Equal(artifact.ExecutablePaths, resolved.ExecutablePaths) {
			return catalog.PlatformResult{}, fmt.Errorf("signed OCI artifact identity for %s differs from independent resolution", artifact.ID)
		}
		if !reflect.DeepEqual(canonicalOCITransport(*artifact.Transport.OCI), canonicalOCITransport(recomputed)) {
			return catalog.PlatformResult{}, fmt.Errorf("signed OCI descriptor chain for %s differs from independent resolution", artifact.ID)
		}
		result.Artifacts[i].Transport.OCI = &recomputed
	}
	return result, nil
}

func catalogDescriptor(value ocispec.Descriptor) catalog.Descriptor {
	annotations := make([]catalog.Annotation, 0, len(value.Annotations))
	for key, entry := range value.Annotations {
		annotations = append(annotations, catalog.Annotation{Key: key, Value: entry})
	}
	var platform *catalog.Platform
	if value.Platform != nil {
		platform = &catalog.Platform{OS: value.Platform.OS, Architecture: value.Platform.Architecture, Variant: value.Platform.Variant}
	}
	if len(annotations) == 0 {
		annotations = nil
	}
	return catalog.Descriptor{Digest: value.Digest.String(), Size: value.Size, MediaType: value.MediaType, Platform: platform, Annotations: annotations}
}

func canonicalOCITransport(value catalog.OCITransport) catalog.OCITransport {
	for _, descriptor := range []*catalog.Descriptor{&value.Index, &value.Manifest, &value.Config, &value.Layer} {
		slicesSortAnnotations(descriptor.Annotations)
	}
	return value
}

func slicesSortAnnotations(values []catalog.Annotation) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && (values[j].Key < values[j-1].Key || values[j].Key == values[j-1].Key && values[j].Value < values[j-1].Value); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func selectCatalogBottleFile(files []catalog.BottleFile, tag string) (catalog.BottleFile, bool) {
	for _, file := range files {
		if file.Tag == tag {
			return file, true
		}
	}
	if tag != "all" {
		for _, file := range files {
			if file.Tag == "all" {
				return file, true
			}
		}
	}
	return catalog.BottleFile{}, false
}

func catalogBottleTab(tab resolution.BottleTab) (catalog.BottleTab, error) {
	dependencies := make([]catalog.BottleRuntimeDependency, len(tab.Dependencies))
	for i, dependency := range tab.Dependencies {
		id, err := formulaid.Parse(dependency.FullName)
		if err != nil {
			return catalog.BottleTab{}, err
		}
		canonical := catalog.FormulaIDFromShared(id)
		dependencies[i] = catalog.BottleRuntimeDependency{ID: canonical, HomebrewFullName: canonical, Version: dependency.Version, Revision: dependency.Revision, BottleRebuild: dependency.BottleRebuild, PkgVersion: dependency.PkgVersion, DeclaredDirectly: dependency.DeclaredDirectly}
	}
	changedFiles := append([]string(nil), tab.ChangedFiles...)
	if len(changedFiles) == 0 {
		changedFiles = nil
	}
	if len(dependencies) == 0 {
		dependencies = nil
	}
	return catalog.BottleTab{HomebrewVersion: tab.HomebrewVersion, Arch: tab.Arch, Compiler: tab.Compiler, ChangedFiles: changedFiles, BuiltOn: catalog.BottleBuiltOn{OS: tab.BuiltOn.OS, OSVersion: tab.BuiltOn.OSVersion, CPUFamily: tab.BuiltOn.CPUFamily, OldestCPUFamily: tab.BuiltOn.OldestCPUFamily, GlibcVersion: tab.BuiltOn.GlibcVersion}, Dependencies: dependencies}, nil
}

func bottleTagForCatalogPlatform(platform catalog.Platform) string {
	if platform.Architecture == "arm64" {
		return hboci.BottleTagARM64Linux
	}
	return hboci.BottleTagX8664Linux
}
