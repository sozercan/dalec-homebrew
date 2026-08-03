package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sozercan/dalec-homebrew/internal/release"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type componentOptions struct {
	index string
	amd64 string
	arm64 string
}

type options struct {
	frontend               componentOptions
	runtimeBase            componentOptions
	materializer           componentOptions
	homebrewCommit         string
	portableRubyVersion    string
	verificationKeysDigest string
	dalecModule            string
	buildKitModule         string
	output                 string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "dalec-homebrew-release-manifest:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("dalec-homebrew-release-manifest", flag.ContinueOnError)
	flags.SetOutput(stderr)

	bindComponentFlags(flags, "frontend", &opts.frontend)
	bindComponentFlags(flags, "runtime-base", &opts.runtimeBase)
	bindComponentFlags(flags, "materializer", &opts.materializer)
	flags.StringVar(&opts.homebrewCommit, "homebrew-commit", "", "immutable Homebrew commit")
	flags.StringVar(&opts.portableRubyVersion, "portable-ruby-version", "", "portable Ruby version")
	flags.StringVar(&opts.verificationKeysDigest, "verification-keys-digest", "", "sha256 digest of the Homebrew verification key set")
	flags.StringVar(&opts.dalecModule, "dalec-module", "", "Dalec Go module version")
	flags.StringVar(&opts.buildKitModule, "buildkit-module", "", "BuildKit Go module version")
	flags.StringVar(&opts.output, "output", "-", "output path, or - for stdout")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}

	manifest := &release.Manifest{
		SchemaVersion:          release.SchemaVersion,
		PolicyVersion:          resolution.PolicyVersion,
		Frontend:               opts.frontend.component(),
		RuntimeBase:            opts.runtimeBase.component(),
		Materializer:           opts.materializer.component(),
		HomebrewCommit:         opts.homebrewCommit,
		PortableRubyVersion:    opts.portableRubyVersion,
		VerificationKeysDigest: opts.verificationKeysDigest,
		DalecModule:            opts.dalecModule,
		BuildKitModule:         opts.buildKitModule,
	}
	data, err := release.Canonical(manifest)
	if err != nil {
		return fmt.Errorf("canonicalize component manifest: %w", err)
	}
	if opts.output == "-" {
		if _, err := stdout.Write(data); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(opts.output, data, 0o644); err != nil {
		return fmt.Errorf("write output %q: %w", opts.output, err)
	}
	return nil
}

func bindComponentFlags(flags *flag.FlagSet, name string, opts *componentOptions) {
	flags.StringVar(&opts.index, name+"-index", "", "digest-pinned "+name+" multi-platform index ref")
	flags.StringVar(&opts.amd64, name+"-amd64", "", "digest-pinned "+name+" linux/amd64 child ref")
	flags.StringVar(&opts.arm64, name+"-arm64", "", "digest-pinned "+name+" linux/arm64 child ref")
}

func (opts componentOptions) component() release.Component {
	return release.Component{
		Index: opts.index,
		Platforms: []release.PlatformRef{
			{
				Platform: resolution.Platform{OS: "linux", Architecture: "amd64"},
				Ref:      opts.amd64,
			},
			{
				Platform: resolution.Platform{OS: "linux", Architecture: "arm64"},
				Ref:      opts.arm64,
			},
		},
	}
}
