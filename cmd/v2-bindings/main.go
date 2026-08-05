package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	"github.com/sozercan/dalec-homebrew/internal/release"
)

type options struct {
	keyID                  string
	publicKeyPath          string
	catalogServiceDigest   string
	catalogExtractorDigest string
	catalogExtractorRef    string
	output                 string
	policyOutput           string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "dalec-homebrew-v2-bindings:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("dalec-homebrew-v2-bindings", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.keyID, "key-id", "", "catalog-set JWS signing key ID")
	flags.StringVar(&opts.publicKeyPath, "public-key", "", "path to the RSA public-key PEM")
	flags.StringVar(&opts.catalogServiceDigest, "catalog-service-digest", "", "authorized catalog-service sha256 digest")
	flags.StringVar(&opts.catalogExtractorDigest, "catalog-extractor-digest", "", "authorized catalog-extractor sha256 digest")
	flags.StringVar(&opts.catalogExtractorRef, "catalog-extractor-ref", "", "digest-pinned build-local catalog-extractor image")
	flags.StringVar(&opts.output, "output", "", "bindings output path, or - for stdout")
	flags.StringVar(&opts.policyOutput, "policy-output", "", "optional path for the canonical catalog key policy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}
	if opts.output == "" {
		return errors.New("--output is required")
	}
	localMode := opts.catalogExtractorRef != ""
	if localMode {
		if opts.keyID != "" || opts.publicKeyPath != "" || opts.catalogServiceDigest != "" || opts.catalogExtractorDigest != "" || opts.policyOutput != "" {
			return errors.New("--catalog-extractor-ref cannot be combined with ingestion JWS options")
		}
	} else {
		for _, field := range []struct{ name, value string }{
			{name: "--key-id", value: opts.keyID}, {name: "--public-key", value: opts.publicKeyPath},
			{name: "--catalog-service-digest", value: opts.catalogServiceDigest}, {name: "--catalog-extractor-digest", value: opts.catalogExtractorDigest},
		} {
			if field.value == "" {
				return fmt.Errorf("%s is required", field.name)
			}
		}
	}

	var (
		bindings  *release.V2Bindings
		keyPolicy []byte
		err       error
	)
	if localMode {
		bindings, err = release.GenerateBuildLocalV2Bindings(opts.catalogExtractorRef)
	} else {
		var publicKeyPEM []byte
		publicKeyPEM, err = readBoundedFile(opts.publicKeyPath, catalogkeys.MaxPolicyBytes)
		if err == nil {
			bindings, keyPolicy, err = release.GenerateV2Bindings(release.V2BindingsInput{KeyID: opts.keyID, PublicKeyPEM: publicKeyPEM, CatalogServiceDigest: opts.catalogServiceDigest, CatalogExtractorDigest: opts.catalogExtractorDigest})
		}
	}
	if err != nil {
		return err
	}
	data, err := release.CanonicalV2Bindings(bindings)
	if err != nil {
		return fmt.Errorf("canonicalize V2 bindings: %w", err)
	}
	if opts.output == "-" {
		if _, err := stdout.Write(data); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
	} else if err := os.WriteFile(opts.output, data, 0o644); err != nil {
		return fmt.Errorf("write V2 bindings %q: %w", opts.output, err)
	}
	if opts.policyOutput != "" {
		if err := os.WriteFile(opts.policyOutput, keyPolicy, 0o644); err != nil {
			return fmt.Errorf("write canonical key policy %q: %w", opts.policyOutput, err)
		}
	}
	return nil
}

func readBoundedFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > limit {
		return nil, fmt.Errorf("file size %d is outside 1..%d bytes", len(data), limit)
	}
	return data, nil
}
