package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/dockerui"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/project-dalec/dalec"
	dalecfrontend "github.com/project-dalec/dalec/frontend"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	hboci "github.com/sozercan/dalec-homebrew/internal/homebrew/oci"
	"github.com/sozercan/dalec-homebrew/internal/llbutil"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/resolver"
	"github.com/sozercan/dalec-homebrew/internal/runtime"
	speccontract "github.com/sozercan/dalec-homebrew/internal/spec"
)

func dalecLoadOptions() []dalecfrontend.LoadOpt {
	return []dalecfrontend.LoadOpt{dalecfrontend.WithAllowArgs(config.BuildArgNames()...)}
}

type preflightPlatform struct {
	platform      ocispec.Platform
	selection     *speccontract.Selection
	effectiveSpec []byte
}

func Handle(ctx context.Context, client gwclient.Client) (*gwclient.Result, error) {
	dc, err := dockerui.NewClient(client)
	if err != nil {
		return nil, err
	}
	opts := client.BuildOpts().Opts
	cfg, err := config.FromBuildOpts(opts)
	if err != nil {
		return nil, err
	}
	invokingFrontend := opts["source"]
	if invokingFrontend == "" {
		invokingFrontend = cfg.FrontendRef
	}
	if opts["build-arg:DALEC_HOMEBREW_FRONTEND_REF"] != "" && opts["source"] != "" && opts["build-arg:DALEC_HOMEBREW_FRONTEND_REF"] != opts["source"] {
		return nil, errors.New("claimed frontend reference does not match the invoking gateway source")
	}
	if err := resolution.ValidatePinnedReference(invokingFrontend); err != nil {
		return nil, fmt.Errorf("invoking gateway frontend: %w", err)
	}
	targetKey := dalecfrontend.GetTargetKey(dc)
	if targetKey != forwardedTargetKey {
		return nil, fmt.Errorf("forwarded Dalec target must be %q, got %q", forwardedTargetKey, targetKey)
	}

	// This raw pass is intentionally before any metadata or registry request.
	// Dalec remains the authoritative typed decoder inside each platform callback.
	source, err := dc.ReadEntrypoint(ctx, "dalec")
	if err != nil {
		return nil, fmt.Errorf("read Dalec spec for preflight: %w", err)
	}
	rawSpec := strings.TrimSpace(string(source.Data))
	nonCoreCapability := speccontract.Capabilities{NonCoreTaps: cfg.SupportsNonCoreTaps()}
	if err := speccontract.PreflightFormulaNames([]byte(rawSpec), targetKey, nonCoreCapability); err != nil {
		return nil, err
	}
	targets := append([]ocispec.Platform(nil), dc.TargetPlatforms...)
	if len(targets) == 0 {
		targets = []ocispec.Platform{platforms.DefaultSpec()}
	}
	preflight := make([]preflightPlatform, len(targets))
	for i, target := range targets {
		p := platforms.Normalize(target)
		if p.Architecture == "arm64" && p.Variant == "v8" {
			p.Variant = ""
		}
		if p.OS != "linux" || (p.Architecture != "amd64" && p.Architecture != "arm64") {
			return nil, fmt.Errorf("unsupported target platform %s", platforms.Format(p))
		}
		dalecSpec, err := dalecfrontend.LoadSpec(ctx, dc, &p, dalecLoadOptions()...)
		if err != nil {
			return nil, err
		}
		selection, err := speccontract.ValidateForwarded(dalecSpec, targetKey, p.Architecture, speccontract.Forwarding{
			Source:  opts["source"],
			CmdLine: opts["cmdline"],
		}, nonCoreCapability)
		if err != nil {
			return nil, err
		}
		effective, err := marshalEffectiveSpec(dalecSpec, selection.RuntimeDependencyOrder)
		if err != nil {
			return nil, err
		}
		preflight[i] = preflightPlatform{platform: p, selection: selection, effectiveSpec: effective}
	}

	baseURL, err := metadataBaseURL(cfg.FormulaURL, cfg.MigrationsURL)
	if err != nil {
		return nil, err
	}
	snapshot, err := metadata.Fetch(ctx, metadata.Config{BaseURL: baseURL, Freshness: metadata.FreshnessPolicy{MaxAge: cfg.MetadataMaxAge, RollbackFloor: cfg.MetadataNotBefore}})
	if err != nil {
		return nil, fmt.Errorf("authenticate Homebrew metadata: %w", err)
	}
	registry, err := hboci.NewClient("https://ghcr.io")
	if err != nil {
		return nil, err
	}

	nonCore, err := resolveInvocationNonCore(ctx, client, cfg, snapshot, preflight)
	if err != nil {
		return nil, err
	}
	if cfg.SupportsNonCoreTaps() {
		return buildV2(ctx, client, dc, cfg, invokingFrontend, targetKey, preflight, snapshot, registry, nonCore)
	}

	records := make([]*resolution.Record, len(preflight))
	rb, err := dc.Build(ctx, func(ctx context.Context, platform *ocispec.Platform, idx int) (_ *dockerui.BuildResult, retErr error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				retErr = fmt.Errorf("platform build panic: %v\n%s", recovered, debug.Stack())
			}
		}()
		if idx < 0 || idx >= len(preflight) {
			return nil, fmt.Errorf("unexpected platform callback index %d", idx)
		}
		input := preflight[idx]
		p := input.platform
		selection := input.selection
		effectiveSpec := input.effectiveSpec

		frontendRef := invokingFrontend
		baseRef, baseImage, err := resolveImageConfig(ctx, client, cfg.RuntimeBaseRef, p, dc.ImageResolveMode.String(), "runtime base")
		if err != nil {
			return nil, err
		}
		materializerRef, _, err := resolveImageConfig(ctx, client, cfg.MaterializerRef, p, dc.ImageResolveMode.String(), "materializer")
		if err != nil {
			return nil, err
		}
		identity, err := runtime.ParseIdentity(imageUser(selection.Image))
		if err != nil {
			return nil, err
		}
		cpuBaseline := "core2"
		if p.Architecture == "arm64" {
			cpuBaseline = "armv8"
		}
		rootNames := make([]string, len(selection.Roots))
		for i, root := range selection.Roots {
			rootNames[i] = root.Name
		}
		components := resolution.Components{FrontendRef: frontendRef, RuntimeBaseRef: baseRef, MaterializerRef: materializerRef, HomebrewCommit: cfg.HomebrewCommit, RubyRuntime: cfg.PortableRubyVersion, VerificationKeys: cfg.VerificationKeysDigest, DalecModule: moduleVersion("github.com/project-dalec/dalec"), BuildKitModule: moduleVersion("github.com/moby/buildkit")}
		record, err := resolver.Resolve(ctx, snapshot, registry, rootNames, p, resolver.Options{
			SpecDigest: "sha256:" + sha256Hex(effectiveSpec), TargetKey: targetKey, Now: time.Now().UTC().Round(0), Metadata: snapshot.Info(), Components: components,
			Runtime:     resolution.RuntimePolicy{User: identity.User, UID: identity.UID, GID: identity.GID, CPUBaseline: cpuBaseline},
			Attestation: resolution.AttestationPolicy{Waiver: cfg.AttestationWaiver},
		})
		if err != nil {
			return nil, err
		}
		if _, err := policy.BindRuntimePolicy(record); err != nil {
			return nil, fmt.Errorf("bind runtime filesystem policy: %w", err)
		}
		finalImage, finalIdentity, _, err := runtime.BuildImageConfig(baseImage, selection.Image, record.Requested, record.Nodes)
		if err != nil {
			return nil, err
		}
		record.Runtime.User = finalImage.Config.User
		record.Runtime.UID = finalIdentity.UID
		record.Runtime.GID = finalIdentity.GID
		finalPATH := runtime.EnvValue(finalImage.Config.Env, "PATH")
		record.Runtime.GeneratedPATH = strings.Split(finalPATH, ":")
		if err := resolution.Validate(record); err != nil {
			return nil, err
		}

		materialized, err := llbutil.Materialize(materializerRef, p, record)
		if err != nil {
			return nil, err
		}
		finalState, err := llbutil.AssembleRuntime(baseRef, p, materialized, record)
		if err != nil {
			return nil, err
		}
		finalState = llbutil.ApplyExecutionConfig(finalState, llbutil.ExecutionConfig(finalImage.Config.Env, finalImage.Config.WorkingDir, finalImage.Config.User))
		finalState, err = AddRuntimeVerification(finalState, materializerRef, p, record)
		if err != nil {
			return nil, err
		}
		if !cfg.SkipTests && len(selection.Tests) > 0 {
			finalState, err = AddTests(finalState, materializerRef, p, selection.Tests, finalImage.Config.Env, finalImage.Config.WorkingDir, finalImage.Config.User, record.SourceDateEpoch)
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
	if err := validateCrossPlatformRoots(records); err != nil {
		return nil, err
	}
	return rb.Finalize()
}

func marshalEffectiveSpec(spec *dalec.Spec, runtimeDependencyOrder []string) ([]byte, error) {
	effective, err := json.Marshal(struct {
		SchemaVersion          string      `json:"schema_version"`
		DalecSpec              *dalec.Spec `json:"dalec_spec"`
		RuntimeDependencyOrder []string    `json:"runtime_dependency_order"`
	}{
		SchemaVersion:          "dalec-homebrew-effective-input/v1",
		DalecSpec:              spec,
		RuntimeDependencyOrder: runtimeDependencyOrder,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal effective Dalec spec: %w", err)
	}
	return effective, nil
}

func resolveImageConfig(ctx context.Context, client gwclient.Client, ref string, p ocispec.Platform, mode, name string) (string, *dalec.DockerImageSpec, error) {
	resolved, manifest, data, err := client.ResolveImageConfig(ctx, ref, sourceresolver.Opt{LogName: "load " + name + " metadata", ImageOpt: &sourceresolver.ResolveImageOpt{Platform: &p, ResolveMode: mode}})
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s: %w", name, err)
	}
	if resolved == "" || manifest == "" {
		return "", nil, fmt.Errorf("resolve %s returned no immutable identity", name)
	}
	named, err := reference.ParseNormalizedNamed(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("parse resolved %s reference: %w", name, err)
	}
	resolved = reference.TrimNamed(named).String() + "@" + manifest.String()
	var image dalec.DockerImageSpec
	if err := json.Unmarshal(data, &image); err != nil {
		return "", nil, fmt.Errorf("decode %s image config: %w", name, err)
	}
	return resolved, &image, nil
}

func validateCrossPlatformRoots(records []*resolution.Record) error {
	versions := map[string]string{}
	produced := false
	for _, record := range records {
		if record == nil {
			continue
		}
		produced = true
		for _, root := range record.Requested {
			node, ok := recordNode(record, root.Canonical)
			if !ok {
				return fmt.Errorf("root %q missing from %s/%s record", root.Canonical, record.Input.Platform.OS, record.Input.Platform.Architecture)
			}
			if previous, seen := versions[root.Canonical]; seen && previous != node.PkgVersion {
				return fmt.Errorf("root %q differs across platform manifests: %s vs %s", root.Canonical, previous, node.PkgVersion)
			}
			versions[root.Canonical] = node.PkgVersion
		}
	}
	if !produced {
		return errors.New("no platform resolutions were produced")
	}
	return nil
}

func recordNode(record *resolution.Record, name string) (resolution.Node, bool) {
	for _, node := range record.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return resolution.Node{}, false
}
func imageUser(img *dalec.ImageConfig) string {
	if img == nil {
		return ""
	}
	return img.User
}
func cloneImage(in *dalec.DockerImageSpec) *dalec.DockerImageSpec {
	if in == nil {
		return nil
	}
	data, _ := json.Marshal(in)
	var out dalec.DockerImageSpec
	_ = json.Unmarshal(data, &out)
	return &out
}

func metadataBaseURL(formulaURL, migrationURL string) (string, error) {
	f, err := url.Parse(formulaURL)
	if err != nil {
		return "", err
	}
	m, err := url.Parse(migrationURL)
	if err != nil {
		return "", err
	}
	if f.Scheme != m.Scheme || f.Host != m.Host || path.Dir(f.Path) != path.Dir(m.Path) || path.Base(f.Path) != metadata.FormulaEndpoint || path.Base(m.Path) != metadata.MigrationsEndpoint {
		return "", errors.New("metadata and migration URLs must be the standard signed endpoint names under one origin and directory")
	}
	f.Path = strings.TrimSuffix(path.Dir(f.Path), "/") + "/"
	f.RawQuery = ""
	f.Fragment = ""
	return f.String(), nil
}

func moduleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Path == path {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path == path {
			if dep.Replace != nil {
				return dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "unknown"
}
