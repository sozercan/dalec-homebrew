package frontend

import (
	"context"
	"fmt"
	"os"
	"slices"

	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

const metadataBundleInputName = "dalec-homebrew-metadata"

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
	inputs, err := client.Inputs(ctx)
	if err != nil {
		return metadataBundleData{}, fmt.Errorf("list frontend inputs: %w", err)
	}
	state, ok := inputs[metadataBundleInputName]
	if !ok {
		return metadataBundleData{}, fmt.Errorf("required named input %q is missing", metadataBundleInputName)
	}
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

func readMetadataBundleReference(ctx context.Context, ref gwclient.Reference) (metadataBundleData, error) {
	files := []metadataBundleFile{
		{name: metadata.BundleManifestFilename, limit: metadata.DefaultMaxBundleManifestBytes},
		{name: metadata.BundleFormulaFilename, limit: metadata.DefaultMaxFormulaBytes},
		{name: metadata.BundleMigrationsFilename, limit: metadata.DefaultMaxMigrationsBytes},
	}
	expected := make([]string, len(files))
	for i, file := range files {
		expected[i] = file.name
	}
	slices.Sort(expected)

	entries, err := ref.ReadDir(ctx, gwclient.ReadDirRequest{Path: "/"})
	if err != nil {
		return metadataBundleData{}, fmt.Errorf("enumerate metadata bundle root: %w", err)
	}
	actual := make([]string, len(entries))
	for i, entry := range entries {
		if entry == nil {
			return metadataBundleData{}, fmt.Errorf("metadata bundle root contains a nil entry")
		}
		actual[i] = entry.Path
	}
	slices.Sort(actual)
	if !slices.Equal(actual, expected) {
		return metadataBundleData{}, fmt.Errorf("metadata bundle root must contain exactly %v, got %v", expected, actual)
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
