package frontend

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

const metadataBundleInputName = "dalec-homebrew-metadata"

const (
	metadataBundleContextOption = "context:" + metadataBundleInputName
	metadataBundleLocalSource   = "local:" + metadataBundleInputName
	metadataBundleSessionOption = "local-sessionid:" + metadataBundleInputName
)

type metadataBundleData struct {
	manifest   []byte
	formula    []byte
	migrations []byte
}

type metadataBundleFile struct {
	name  string
	limit int64
}

func loadMetadataSnapshot(ctx context.Context, client gwclient.Client, cfg config.Config) (*metadata.Snapshot, error) {
	if cfg.MetadataBundleDigest == "" {
		if err := validateMetadataBundleContext(client.BuildOpts(), false); err != nil {
			return nil, fmt.Errorf("validate Homebrew metadata bundle input: %w", err)
		}
		baseURL, err := metadataBaseURL(cfg.FormulaURL, cfg.MigrationsURL)
		if err != nil {
			return nil, err
		}
		snapshot, err := metadata.Fetch(ctx, metadata.Config{
			BaseURL:   baseURL,
			Freshness: metadata.FreshnessPolicy{MaxAge: cfg.MetadataMaxAge, RollbackFloor: cfg.MetadataNotBefore},
		})
		if err != nil {
			return nil, fmt.Errorf("authenticate Homebrew metadata: %w", err)
		}
		return snapshot, nil
	}

	bundle, err := readMetadataBundleInput(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("load Homebrew metadata bundle input: %w", err)
	}
	actualDigest := digest.FromBytes(bundle.manifest).String()
	if actualDigest != cfg.MetadataBundleDigest {
		return nil, fmt.Errorf("metadata bundle manifest digest %s does not match release-bound digest %s", actualDigest, cfg.MetadataBundleDigest)
	}
	snapshot, _, err := metadata.LoadBundleBytes(bundle.manifest, bundle.formula, bundle.migrations, metadata.BundleLoadOptions{
		Freshness: metadata.FreshnessPolicy{MaxAge: cfg.MetadataMaxAge, RollbackFloor: cfg.MetadataNotBefore},
	})
	if err != nil {
		return nil, fmt.Errorf("authenticate Homebrew metadata bundle: %w", err)
	}
	return snapshot, nil
}

func readMetadataBundleInput(ctx context.Context, client gwclient.Client) (metadataBundleData, error) {
	buildOpts := client.BuildOpts()
	if err := validateMetadataBundleContext(buildOpts, true); err != nil {
		return metadataBundleData{}, err
	}
	files := []string{
		metadata.BundleManifestFilename,
		metadata.BundleFormulaFilename,
		metadata.BundleMigrationsFilename,
	}
	descendants := make([]string, len(files))
	for i, file := range files {
		descendants[i] = file + "/**"
	}
	// dockerui.NamedContext probes .dockerignore even when ignore processing is
	// disabled. Build the reserved local state directly so the caller can transfer
	// only the three authenticated bundle members and no directory descendants.
	state := llb.Local(metadataBundleInputName,
		llb.SessionID(buildOpts.SessionID),
		llb.SharedKeyHint("context:"+metadataBundleInputName+"-"+buildOpts.Opts["sharedkey:localdir:"+metadataBundleInputName]),
		llb.IncludePatterns(files),
		llb.ExcludePatterns(descendants),
		llb.WithCustomName("[context "+metadataBundleInputName+"] load authenticated metadata bundle"),
	)
	definition, err := state.Marshal(ctx)
	if err != nil {
		return metadataBundleData{}, fmt.Errorf("marshal named input %q: %w", metadataBundleInputName, err)
	}
	result, err := client.Solve(ctx, gwclient.SolveRequest{Definition: definition.ToPB()})
	if err != nil {
		return metadataBundleData{}, fmt.Errorf("solve named input %q: %w", metadataBundleInputName, err)
	}
	ref, err := result.SingleRef()
	if err != nil {
		return metadataBundleData{}, fmt.Errorf("resolve named input %q: %w", metadataBundleInputName, err)
	}
	if ref == nil {
		return metadataBundleData{}, fmt.Errorf("named input %q produced no reference", metadataBundleInputName)
	}
	return readMetadataBundleReference(ctx, ref)
}

func validateMetadataBundleContext(buildOpts gwclient.BuildOpts, required bool) error {
	contextSource, ok := buildOpts.Opts[metadataBundleContextOption]
	if !required {
		var contexts []string
		for key := range buildOpts.Opts {
			if strings.HasPrefix(key, "context:") {
				contexts = append(contexts, strings.TrimPrefix(key, "context:"))
			}
		}
		if len(contexts) != 0 {
			slices.Sort(contexts)
			return fmt.Errorf("named contexts require a release-bound metadata bundle: %s", strings.Join(contexts, ", "))
		}
		if _, ok := buildOpts.Opts[metadataBundleSessionOption]; ok {
			return fmt.Errorf("named context session override %q requires a release-bound metadata bundle", metadataBundleInputName)
		}
		return nil
	}
	if !ok {
		return fmt.Errorf("required named context %q is missing", metadataBundleInputName)
	}
	if contextSource != metadataBundleLocalSource {
		return fmt.Errorf("named context %q must use local source %q, got %q", metadataBundleInputName, metadataBundleLocalSource, contextSource)
	}
	var unsupportedContexts []string
	for key := range buildOpts.Opts {
		if strings.HasPrefix(key, "context:") && key != metadataBundleContextOption {
			unsupportedContexts = append(unsupportedContexts, strings.TrimPrefix(key, "context:"))
		}
	}
	if len(unsupportedContexts) != 0 {
		slices.Sort(unsupportedContexts)
		return fmt.Errorf("unsupported named contexts: %s", strings.Join(unsupportedContexts, ", "))
	}
	if buildOpts.SessionID == "" {
		return fmt.Errorf("named context %q has no BuildKit session", metadataBundleInputName)
	}
	if override, ok := buildOpts.Opts[metadataBundleSessionOption]; ok && override != buildOpts.SessionID {
		return fmt.Errorf("named context %q session override does not match the active BuildKit session", metadataBundleInputName)
	}
	return nil
}

func readMetadataBundleReference(ctx context.Context, ref gwclient.Reference) (metadataBundleData, error) {
	files := []metadataBundleFile{
		{name: metadata.BundleManifestFilename, limit: metadata.DefaultMaxBundleManifestBytes},
		{name: metadata.BundleFormulaFilename, limit: metadata.DefaultMaxFormulaBytes},
		{name: metadata.BundleMigrationsFilename, limit: metadata.DefaultMaxMigrationsBytes},
	}
	contents := make(map[string][]byte, len(files))
	for _, file := range files {
		filename := "/" + file.name
		stat, err := ref.StatFile(ctx, gwclient.StatRequest{Path: filename})
		if err != nil {
			return metadataBundleData{}, fmt.Errorf("stat metadata bundle file %q: %w", file.name, err)
		}
		if stat == nil {
			return metadataBundleData{}, fmt.Errorf("stat metadata bundle file %q returned no metadata", file.name)
		}
		if !os.FileMode(stat.Mode).IsRegular() || stat.Linkname != "" {
			return metadataBundleData{}, fmt.Errorf("metadata bundle file %q must be a regular file", file.name)
		}
		if stat.Size <= 0 || stat.Size > file.limit {
			return metadataBundleData{}, fmt.Errorf("metadata bundle file %q size %d is outside 1..%d", file.name, stat.Size, file.limit)
		}
		data, err := ref.ReadFile(ctx, gwclient.ReadRequest{
			Filename: filename,
			Range:    &gwclient.FileRange{Length: int(stat.Size)},
		})
		if err != nil {
			return metadataBundleData{}, fmt.Errorf("read metadata bundle file %q: %w", file.name, err)
		}
		if int64(len(data)) != stat.Size || int64(len(data)) > file.limit {
			return metadataBundleData{}, fmt.Errorf("metadata bundle file %q read %d bytes after stat reported %d", file.name, len(data), stat.Size)
		}
		contents[file.name] = data
	}
	return metadataBundleData{
		manifest:   contents[metadata.BundleManifestFilename],
		formula:    contents[metadata.BundleFormulaFilename],
		migrations: contents[metadata.BundleMigrationsFilename],
	}, nil
}
