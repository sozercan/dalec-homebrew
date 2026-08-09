package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type options struct {
	output       string
	digestOutput string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
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
		inside, err := pathWithin(opts.output, opts.digestOutput)
		if err != nil {
			return err
		}
		if inside {
			return errors.New("--digest-output must be outside --output so the bundle contains exactly three files")
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
		inside, err := pathWithin(output, digestOutput)
		if err != nil {
			return err
		}
		if inside {
			return errors.New("digest output must be outside the metadata bundle directory")
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

func pathWithin(directory, path string) (bool, error) {
	directoryAbsolute, err := filepath.Abs(directory)
	if err != nil {
		return false, fmt.Errorf("resolve output directory path: %w", err)
	}
	pathAbsolute, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve digest output path: %w", err)
	}
	relative, err := filepath.Rel(directoryAbsolute, pathAbsolute)
	if err != nil {
		return false, fmt.Errorf("compare output paths: %w", err)
	}
	return relative == "." || (relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}
