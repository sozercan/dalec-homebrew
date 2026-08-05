package frontend

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/dockerui"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogclient"
	"github.com/sozercan/dalec-homebrew/internal/catalogresolver"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	hboci "github.com/sozercan/dalec-homebrew/internal/homebrew/oci"
	"github.com/sozercan/dalec-homebrew/internal/llbutil"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/resolver"
	"github.com/sozercan/dalec-homebrew/internal/runtime"
	speccontract "github.com/sozercan/dalec-homebrew/internal/spec"
)

func resolveInvocationNonCore(ctx context.Context, gatewayClient gwclient.Client, cfg config.Config, snapshot *metadata.Snapshot, preflight []preflightPlatform) (*NonCoreResolution, error) {
	targets := make([]NonCoreTarget, len(preflight))
	hasExternal := false
	for i, input := range preflight {
		target := NonCoreTarget{Platform: catalog.Platform{OS: input.platform.OS, Architecture: input.platform.Architecture, Variant: input.platform.Variant}}
		for _, root := range input.selection.Roots {
			if root.ID.Tap() == formulaidCoreTap() {
				target.CoreRoots = append(target.CoreRoots, catalog.FormulaIDFromShared(root.ID))
				continue
			}
			hasExternal = true
			target.ExternalRoots = append(target.ExternalRoots, catalog.FormulaIDFromShared(root.ID))
		}
		targets[i] = target
	}
	if !hasExternal {
		return nil, nil
	}
	if !cfg.SupportsNonCoreTaps() {
		return nil, errors.New("qualified roots require a complete release-bound non-core capability")
	}
	if cfg.CatalogExtractorRef != "" {
		return ResolveNonCoreLocally(ctx, gatewayClient, snapshot, targets, cfg.HomebrewCommit, cfg.CatalogExtractorRef)
	}
	keyPolicy, err := config.CompiledCatalogKeyPolicy()
	if err != nil {
		return nil, err
	}
	client, err := catalogclient.New(catalogclient.Config{Origin: cfg.CatalogServiceOrigin, KeyPolicy: keyPolicy, KeyPolicyDigest: cfg.IngestionJWSKeyPolicyDigest})
	if err != nil {
		return nil, err
	}
	return ResolveNonCoreCatalogs(ctx, client, snapshot, targets, cfg.HomebrewCommit, snapshot.Info().Digest)
}

func formulaidCoreTap() formulaid.Tap { return formulaid.CoreTap() }

func buildV2(ctx context.Context, client gwclient.Client, dc *dockerui.Client, cfg config.Config, invokingFrontend, targetKey string, preflight []preflightPlatform, snapshot *metadata.Snapshot, registry *hboci.Client, nonCore *NonCoreResolution) (*gwclient.Result, error) {
	verifiedResults := make(map[string]catalog.PlatformResult)
	catalogSigner := resolution.Signature{}
	if nonCore != nil {
		for key, result := range nonCore.ByPlatform {
			verified, err := ResolveNonCoreOCIArtifacts(ctx, registry, snapshot, result, nonCore.Catalogs)
			if err != nil {
				return nil, err
			}
			verifiedResults[key] = verified
		}
		for _, signature := range nonCore.Signatures {
			if signature.Verified && signature.Algorithm == "PS512" {
				catalogSigner = resolution.Signature{KeyID: signature.KeyID, Algorithm: signature.Algorithm, Verified: true}
				break
			}
		}
		if !nonCore.Local && catalogSigner.KeyID == "" {
			return nil, errors.New("catalog-set verification produced no verified PS512 signer evidence")
		}
	}
	records := make([]*resolution.RecordV2, len(preflight))

	rb, err := dc.Build(ctx, func(ctx context.Context, platform *ocispec.Platform, idx int) (_ *dockerui.BuildResult, retErr error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				retErr = fmt.Errorf("platform V2 build panic: %v\n%s", recovered, debug.Stack())
			}
		}()
		if idx < 0 || idx >= len(preflight) {
			return nil, fmt.Errorf("unexpected platform callback index %d", idx)
		}
		input := preflight[idx]
		p := input.platform
		key := p.OS + "/" + p.Architecture
		frontendChildRef, _, err := resolveImageConfig(ctx, client, invokingFrontend, p, dc.ImageResolveMode.String(), "frontend child")
		if err != nil {
			return nil, err
		}
		baseRef, baseImage, err := resolveImageConfig(ctx, client, cfg.RuntimeBaseRef, p, dc.ImageResolveMode.String(), "runtime base")
		if err != nil {
			return nil, err
		}
		materializerRef, _, err := resolveImageConfig(ctx, client, cfg.MaterializerRef, p, dc.ImageResolveMode.String(), "materializer")
		if err != nil {
			return nil, err
		}
		fetcherRef, _, err := resolveImageConfig(ctx, client, cfg.BottleFetcherRef, p, dc.ImageResolveMode.String(), "bottle fetcher")
		if err != nil {
			return nil, err
		}
		identity, err := runtime.ParseIdentity(imageUser(input.selection.Image))
		if err != nil {
			return nil, err
		}
		cpuBaseline := "core2"
		if p.Architecture == "arm64" {
			cpuBaseline = "armv8"
		}
		components := resolution.ComponentsV2{
			FrontendIndexRef: invokingFrontend, FrontendRef: frontendChildRef, RuntimeBaseRef: baseRef, MaterializerRef: materializerRef, BottleFetcherRef: fetcherRef,
			CatalogExtractorRef: cfg.CatalogExtractorRef, CatalogServiceOrigin: cfg.CatalogServiceOrigin, IngestionJWSKeyPolicyDigest: cfg.IngestionJWSKeyPolicyDigest,
			TapPolicyDigest: cfg.TapPolicyDigest, ExecutableRuntimePolicyDigest: cfg.ExecutableRuntimePolicyDigest,
			HomebrewCommit: cfg.HomebrewCommit, RubyRuntime: cfg.PortableRubyVersion, VerificationKeys: cfg.VerificationKeysDigest,
			DalecModule: moduleVersion("github.com/project-dalec/dalec"), BuildKitModule: moduleVersion("github.com/moby/buildkit"),
			SupportedCatalogPolicyVersions:    append([]string(nil), cfg.SupportedCatalogPolicyVersions...),
			SupportedFetchPolicyVersions:      append([]string(nil), cfg.SupportedFetchPolicyVersions...),
			SupportedProvenancePolicyVersions: append([]string(nil), cfg.SupportedProvenancePolicyVersions...),
		}
		var record *resolution.RecordV2
		if nonCore != nil {
			platformResult, ok := verifiedResults[key]
			if !ok {
				return nil, fmt.Errorf("missing verified catalog result for %s", key)
			}
			requested, err := v2RequestedRoots(snapshot, nonCore.Catalogs, platformResult, input.selection.Roots)
			if err != nil {
				return nil, err
			}
			record, err = resolver.RecordV2FromCatalog(platformResult, requested, resolver.V2Options{
				SpecDigest: "sha256:" + sha256Hex(input.effectiveSpec), TargetKey: targetKey, ResolvedAt: time.Now().UTC().Round(0),
				CoreMetadata: snapshot.Info(), CatalogPayload: nonCore.Payload, Catalogs: nonCore.Catalogs, CatalogSigner: catalogSigner,
				Components: components, SequenceFloors: nonCore.MinimumSequences, CoreRollbackFloor: coreRollbackFloor(cfg.MetadataNotBefore), SetPayloadDigest: nonCore.SetPayloadDigest, SetEnvelopeDigest: nonCore.SetEnvelopeDigest, Runtime: resolution.RuntimePolicy{User: identity.User, UID: identity.UID, GID: identity.GID, CPUBaseline: cpuBaseline},
			})
		} else {
			rootNames := make([]string, len(input.selection.Roots))
			for i, root := range input.selection.Roots {
				rootNames[i] = root.Name
			}
			legacyComponents := resolution.Components{FrontendRef: frontendChildRef, RuntimeBaseRef: baseRef, MaterializerRef: materializerRef, HomebrewCommit: cfg.HomebrewCommit, RubyRuntime: cfg.PortableRubyVersion, VerificationKeys: cfg.VerificationKeysDigest, DalecModule: components.DalecModule, BuildKitModule: components.BuildKitModule}
			legacy, resolveErr := resolver.Resolve(ctx, snapshot, registry, rootNames, p, resolver.Options{SpecDigest: "sha256:" + sha256Hex(input.effectiveSpec), TargetKey: targetKey, Now: time.Now().UTC().Round(0), Metadata: snapshot.Info(), Components: legacyComponents, Runtime: resolution.RuntimePolicy{User: identity.User, UID: identity.UID, GID: identity.GID, CPUBaseline: cpuBaseline}, Attestation: resolution.AttestationPolicy{Waiver: cfg.AttestationWaiver}})
			if resolveErr != nil {
				return nil, resolveErr
			}
			record, err = resolver.RecordV2FromCore(legacy, components, snapshot.Info(), coreRollbackFloor(cfg.MetadataNotBefore))
		}
		if err != nil {
			return nil, err
		}
		finalImage, finalIdentity, _, err := runtime.BuildImageConfigV2(baseImage, input.selection.Image, record)
		if err != nil {
			return nil, err
		}
		record.Runtime.User = finalImage.Config.User
		record.Runtime.UID = finalIdentity.UID
		record.Runtime.GID = finalIdentity.GID
		finalPATH := runtime.EnvValue(finalImage.Config.Env, "PATH")
		record.Runtime.GeneratedPATH = strings.Split(finalPATH, ":")
		if _, err := policy.BindRuntimePolicyV2(record); err != nil {
			return nil, err
		}
		if err := resolution.ValidateV2(record); err != nil {
			return nil, err
		}
		var localBottleStates map[resolution.FormulaID]llb.State
		if nonCore != nil && nonCore.Local {
			localBottleStates, err = buildLocalBottleStates(nonCore.LocalBottles[key], record.SourceDateEpoch)
			if err != nil {
				return nil, err
			}
		}
		materialized, err := llbutil.MaterializeV2(materializerRef, fetcherRef, p, record, localBottleStates)
		if err != nil {
			return nil, err
		}
		finalState, err := llbutil.AssembleRuntimeV2(baseRef, p, materialized, record)
		if err != nil {
			return nil, err
		}
		finalState = llbutil.ApplyExecutionConfig(finalState, llbutil.ExecutionConfig(finalImage.Config.Env, finalImage.Config.WorkingDir, finalImage.Config.User))
		finalState, err = AddRuntimeVerificationV2(finalState, materializerRef, p, record)
		if err != nil {
			return nil, err
		}
		if !cfg.SkipTests && len(input.selection.Tests) > 0 {
			finalState, err = AddTests(finalState, materializerRef, p, input.selection.Tests, finalImage.Config.Env, finalImage.Config.WorkingDir, finalImage.Config.User, record.SourceDateEpoch)
			if err != nil {
				return nil, err
			}
		}
		def, err := finalState.Marshal(ctx, llb.Platform(p))
		if err != nil {
			return nil, err
		}
		solved, err := client.Solve(ctx, gwclient.SolveRequest{Definition: def.ToPB(), CacheImports: dc.CacheImports})
		if err != nil {
			return nil, err
		}
		ref, err := solved.SingleRef()
		if err != nil {
			return nil, err
		}
		epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
		finalImage.Created = &epoch
		records[idx] = record
		return &dockerui.BuildResult{Reference: ref, Image: finalImage, BaseImage: cloneImage(baseImage), Epoch: &epoch}, nil
	})
	if err != nil {
		return nil, err
	}
	if err := validateCrossPlatformRootsV2(records); err != nil {
		return nil, err
	}
	return rb.Finalize()
}

func v2RequestedRoots(core catalogresolver.CoreCatalog, catalogs map[catalog.TapID]*catalog.TapCatalog, result catalog.PlatformResult, roots []speccontract.Root) ([]resolver.V2RequestedRoot, error) {
	resolverV2, err := catalogresolver.New(core, catalogs)
	if err != nil {
		return nil, err
	}
	requested := make([]resolver.V2RequestedRoot, 0, len(roots))
	for _, root := range roots {
		id := catalog.FormulaIDFromShared(root.ID)
		closure, err := resolverV2.Resolve([]catalog.FormulaID{id}, result.Platform)
		if err != nil {
			return nil, err
		}
		id = closure.Requested[0]
		found := false
		for _, node := range result.Closure.Nodes {
			if node.ID == id {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("requested Formula %s is absent from the signed catalog closure", id)
		}
		requested = append(requested, resolver.V2RequestedRoot{Requested: root.Requested, ID: id})
	}
	return requested, nil
}

func buildLocalBottleStates(values map[catalog.FormulaID][]byte, sourceDateEpoch int64) (map[resolution.FormulaID]llb.State, error) {
	if len(values) == 0 {
		return nil, nil
	}
	epoch := time.Unix(sourceDateEpoch, 0).UTC()
	result := make(map[resolution.FormulaID]llb.State, len(values))
	for id, data := range values {
		if len(data) == 0 || int64(len(data)) > catalog.MaxBottleBytes {
			return nil, fmt.Errorf("build-local bottle %s size %d is invalid", id, len(data))
		}
		result[resolution.FormulaID(id)] = llb.Scratch().File(llb.Mkfile("/bottle", 0o444, data, llb.WithCreatedTime(epoch)))
	}
	return result, nil
}

func coreRollbackFloor(value time.Time) uint64 {
	if value.IsZero() || value.Unix() <= 0 {
		return 0
	}
	return uint64(value.Unix())
}

func validateCrossPlatformRootsV2(records []*resolution.RecordV2) error {
	type identity struct {
		ID         resolution.FormulaID
		PkgVersion string
		KegOnly    bool
	}
	byRequested := map[string]identity{}
	produced := false
	for _, record := range records {
		if record == nil {
			continue
		}
		produced = true
		for _, root := range record.Requested {
			var node *resolution.NodeV2
			for i := range record.Nodes {
				if record.Nodes[i].ID == root.ID {
					node = &record.Nodes[i]
					break
				}
			}
			if node == nil {
				return fmt.Errorf("root %q missing from %s/%s V2 record", root.ID, record.Input.Platform.OS, record.Input.Platform.Architecture)
			}
			current := identity{ID: root.ID, PkgVersion: node.PkgVersion, KegOnly: root.KegOnly}
			if previous, seen := byRequested[root.Requested]; seen && previous != current {
				return fmt.Errorf("requested root %q differs across platform manifests: %+v vs %+v", root.Requested, previous, current)
			}
			byRequested[root.Requested] = current
		}
	}
	if !produced {
		return errors.New("no V2 platform resolutions were produced")
	}
	return nil
}
