package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/sozercan/dalec-homebrew/internal/catalogextractor"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dalec-homebrew-catalog-extractor:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "extract" {
		return runExtract(args[1:])
	}
	if len(args) > 0 && args[0] == "source-metadata" {
		return runSourceMetadata(args[1:])
	}
	if len(args) > 0 && args[0] == "validate-extracted" {
		return runValidateExtracted(args[1:])
	}
	flags := flag.NewFlagSet("dalec-homebrew-catalog-extractor", flag.ContinueOnError)
	input := flags.String("input", "", "raw canonical tap catalog JSON")
	output := flags.String("output", "", "canonical tap catalog JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *input == "" || *output == "" {
		return fmt.Errorf("--input and --output are required; positional arguments are unsupported")
	}
	return catalogextractor.CanonicalizeFile(*input, *output)
}

func runValidateExtracted(args []string) error {
	flags := flag.NewFlagSet("dalec-homebrew-catalog-extractor validate-extracted", flag.ContinueOnError)
	input := flags.String("input", "", "untrusted evaluator output JSON")
	sourceMetadata := flags.String("source-metadata", "", "authenticated tap source metadata JSON")
	tapRoot := flags.String("tap-root", "", "read-only clean tap source root")
	output := flags.String("output", "", "strict validated extracted tap JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("positional arguments are unsupported")
	}
	return catalogextractor.ValidateExtractedFile(*input, *sourceMetadata, *tapRoot, *output)
}

func runSourceMetadata(args []string) error {
	flags := flag.NewFlagSet("dalec-homebrew-catalog-extractor source-metadata", flag.ContinueOnError)
	tap := flags.String("tap", "", "canonical owner/tap identity")
	repository := flags.String("repository", "", "default public GitHub repository")
	tapRoot := flags.String("tap-root", "", "read-only Git checkout root")
	output := flags.String("output", "", "validated tap source metadata JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("positional arguments are unsupported")
	}
	return catalogextractor.WriteSourceMetadata(context.Background(), *tap, *repository, *tapRoot, *output)
}

func runExtract(args []string) error {
	flags := flag.NewFlagSet("dalec-homebrew-catalog-extractor extract", flag.ContinueOnError)
	tap := flags.String("tap", "", "canonical owner/tap identity")
	repository := flags.String("repository", "", "default public GitHub repository")
	tapRoot := flags.String("tap-root", "", "read-only canonical Homebrew tap root")
	sourceMetadata := flags.String("source-metadata", "", "verified tap source metadata JSON")
	homebrewCommit := flags.String("homebrew-commit", "", "release-pinned Homebrew commit")
	output := flags.String("output", "", "validated extracted tap JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("positional arguments are unsupported")
	}
	return catalogextractor.ExtractFile(*tap, *repository, *tapRoot, *sourceMetadata, *homebrewCommit, *output)
}
