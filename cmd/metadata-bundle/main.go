package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type options struct {
	output       string
	digestOutput string
}

type verifyOptions struct {
	manifest       string
	formula        string
	migrations     string
	expectedDigest string
}

type verificationRecord struct {
	BundleDigest string                `json:"bundle_digest"`
	Snapshot     metadata.SnapshotInfo `json:"snapshot"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args := os.Args[1:]
	var err error
	if len(args) > 0 && args[0] == "verify" {
		err = runVerify(args[1:], os.Stdout, os.Stderr)
	} else {
		err = run(ctx, args, os.Stderr)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "dalec-homebrew-metadata-bundle:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("dalec-homebrew-metadata-bundle", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.output, "output", "", "new output directory for the verified metadata bundle")
	flags.StringVar(&opts.digestOutput, "digest-output", "", "optional new file for the canonical manifest sha256 digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}
	if opts.output == "" {
		return errors.New("--output is required")
	}
	if opts.digestOutput != "" {
		if err := validateDigestOutputPath(opts.output, opts.digestOutput); err != nil {
			return err
		}
	}

	client, err := metadata.NewClient(metadata.Config{})
	if err != nil {
		return fmt.Errorf("configure metadata client: %w", err)
	}
	bundle, err := client.CaptureBundle(ctx, metadata.BundleCaptureOptions{})
	if err != nil {
		return fmt.Errorf("capture official Homebrew metadata: %w", err)
	}
	return writeBundleDirectory(opts.output, opts.digestOutput, bundle)
}

func runVerify(args []string, stdout, stderr io.Writer) error {
	var opts verifyOptions
	flags := flag.NewFlagSet("dalec-homebrew-metadata-bundle verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.manifest, "manifest", "", "metadata bundle manifest file")
	flags.StringVar(&opts.formula, "formula", "", "formula JWS file")
	flags.StringVar(&opts.migrations, "migrations", "", "Formula migration JWS file")
	flags.StringVar(&opts.expectedDigest, "expected-digest", "", "expected canonical manifest sha256 digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}
	for _, required := range []struct {
		name  string
		value string
	}{
		{name: "--manifest", value: opts.manifest},
		{name: "--formula", value: opts.formula},
		{name: "--migrations", value: opts.migrations},
		{name: "--expected-digest", value: opts.expectedDigest},
	} {
		if required.value == "" {
			return fmt.Errorf("%s is required", required.name)
		}
	}
	expected, err := digest.Parse(opts.expectedDigest)
	if err != nil || expected.Algorithm() != digest.SHA256 || expected.String() != opts.expectedDigest {
		return errors.New("--expected-digest must be one canonical sha256 digest")
	}

	manifestData, err := readBoundedRegularFile(opts.manifest, metadata.DefaultMaxBundleManifestBytes)
	if err != nil {
		return fmt.Errorf("read metadata bundle manifest: %w", err)
	}
	actual := digest.FromBytes(manifestData)
	if actual != expected {
		return fmt.Errorf("metadata bundle manifest digest %s does not match expected %s", actual, expected)
	}
	formulaData, err := readBoundedRegularFile(opts.formula, metadata.DefaultMaxFormulaBytes)
	if err != nil {
		return fmt.Errorf("read formula JWS: %w", err)
	}
	migrationsData, err := readBoundedRegularFile(opts.migrations, metadata.DefaultMaxMigrationsBytes)
	if err != nil {
		return fmt.Errorf("read Formula migration JWS: %w", err)
	}
	snapshot, _, err := metadata.LoadBundleBytes(manifestData, formulaData, migrationsData, metadata.BundleLoadOptions{
		Freshness: metadata.FreshnessPolicy{MaxAge: -1, MaxFutureSkew: metadata.DefaultMaxFutureSkew},
		Now:       time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("verify metadata bundle: %w", err)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(verificationRecord{BundleDigest: actual.String(), Snapshot: snapshot.Info()}); err != nil {
		return fmt.Errorf("write metadata bundle verification record: %w", err)
	}
	return nil
}

func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > limit {
		return nil, fmt.Errorf("file size %d is outside 1..%d", info.Size(), limit)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() || int64(len(data)) > limit {
		return nil, fmt.Errorf("file changed while reading or exceeded %d bytes", limit)
	}
	return data, nil
}

func writeBundleDirectory(output, digestOutput string, bundle *metadata.Bundle) (retErr error) {
	if output == "" {
		return errors.New("metadata bundle output directory is empty")
	}
	if bundle == nil {
		return errors.New("metadata bundle is nil")
	}
	manifest, err := bundle.CanonicalManifest()
	if err != nil {
		return fmt.Errorf("canonicalize metadata bundle manifest: %w", err)
	}
	digest, err := bundle.Digest()
	if err != nil {
		return fmt.Errorf("digest metadata bundle manifest: %w", err)
	}
	if digestOutput != "" {
		if err := validateDigestOutputPath(output, digestOutput); err != nil {
			return err
		}
	}

	if err := os.Mkdir(output, 0o700); err != nil {
		return fmt.Errorf("create new metadata bundle directory %q: %w", output, err)
	}
	created := []string{}
	complete := false
	defer func() {
		if complete {
			return
		}
		for i := len(created) - 1; i >= 0; i-- {
			if err := os.Remove(created[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("clean incomplete output %q: %w", created[i], err))
			}
		}
		if err := os.Remove(output); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("clean incomplete output directory %q: %w", output, err))
		}
	}()

	files := []struct {
		name string
		data []byte
	}{
		{name: metadata.BundleFormulaFilename, data: bundle.Formula},
		{name: metadata.BundleMigrationsFilename, data: bundle.Migrations},
		{name: metadata.BundleManifestFilename, data: manifest},
	}
	for _, file := range files {
		path := filepath.Join(output, file.name)
		if err := writeExclusiveFile(path, file.data, 0o644); err != nil {
			return fmt.Errorf("write metadata bundle file %q: %w", file.name, err)
		}
		created = append(created, path)
	}
	if err := os.Chmod(output, 0o755); err != nil {
		return fmt.Errorf("finalize metadata bundle directory permissions: %w", err)
	}
	if digestOutput != "" {
		if err := writeExclusiveFile(digestOutput, []byte(digest+"\n"), 0o644); err != nil {
			return fmt.Errorf("write metadata bundle digest output %q: %w", digestOutput, err)
		}
		created = append(created, digestOutput)
	}
	complete = true
	return nil
}

func writeExclusiveFile(path string, data []byte, mode os.FileMode) (retErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			retErr = errors.Join(retErr, file.Close())
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("clean incomplete file: %w", err))
			}
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func validateDigestOutputPath(output, digestOutput string) error {
	expected := filepath.Clean(output) + ".digest"
	if filepath.Clean(digestOutput) != expected {
		return fmt.Errorf("--digest-output must be the exact sibling %q so the bundle contains exactly three files", expected)
	}
	return nil
}
