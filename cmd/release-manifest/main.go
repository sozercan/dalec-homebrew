package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/release"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type componentOptions struct {
	index string
	amd64 string
	arm64 string
}

type options struct {
	schemaVersion                     string
	frontend                          componentOptions
	runtimeBase                       componentOptions
	materializer                      componentOptions
	bottleFetcher                     componentOptions
	catalogExtractor                  componentOptions
	homebrewCommit                    string
	portableRubyVersion               string
	verificationKeysDigest            string
	metadataBundleDigest              string
	dalecModule                       string
	buildKitModule                    string
	tapPolicyDigest                   string
	executableRuntimePolicyDigest     string
	supportedCatalogPolicyVersions    string
	supportedFetchPolicyVersions      string
	supportedProvenancePolicyVersions string
	output                            string
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

	flags.StringVar(&opts.schemaVersion, "schema-version", "v1", "component manifest schema: v1 or v2")
	bindComponentFlags(flags, "frontend", &opts.frontend)
	bindComponentFlags(flags, "runtime-base", &opts.runtimeBase)
	bindComponentFlags(flags, "materializer", &opts.materializer)
	bindComponentFlags(flags, "bottle-fetcher", &opts.bottleFetcher)
	bindComponentFlags(flags, "catalog-extractor", &opts.catalogExtractor)
	flags.StringVar(&opts.homebrewCommit, "homebrew-commit", "", "immutable Homebrew commit")
	flags.StringVar(&opts.portableRubyVersion, "portable-ruby-version", "", "portable Ruby version")
	flags.StringVar(&opts.verificationKeysDigest, "verification-keys-digest", "", "sha256 digest of the Homebrew verification key set")
	flags.StringVar(&opts.metadataBundleDigest, "metadata-bundle-digest", "", "sha256 digest of the authenticated Homebrew metadata bundle manifest")
	flags.StringVar(&opts.dalecModule, "dalec-module", "", "Dalec Go module version")
	flags.StringVar(&opts.buildKitModule, "buildkit-module", "", "BuildKit Go module version")
	flags.StringVar(&opts.tapPolicyDigest, "tap-policy-digest", "", "V2 embedded tap-policy sha256 digest")
	flags.StringVar(&opts.executableRuntimePolicyDigest, "executable-runtime-policy-digest", "", "V2 executable-runtime-policy sha256 digest")
	flags.StringVar(&opts.supportedCatalogPolicyVersions, "supported-catalog-policy-versions", "", "comma-separated V2 catalog policy versions")
	flags.StringVar(&opts.supportedFetchPolicyVersions, "supported-fetch-policy-versions", "", "comma-separated V2 fetch policy versions")
	flags.StringVar(&opts.supportedProvenancePolicyVersions, "supported-provenance-policy-versions", "", "comma-separated V2 provenance policy versions")
	flags.StringVar(&opts.output, "output", "-", "output path, or - for stdout")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}

	schemaVersion, err := parseSchemaVersion(opts.schemaVersion)
	if err != nil {
		return err
	}
	manifest := &release.Manifest{
		SchemaVersion:          schemaVersion,
		Frontend:               opts.frontend.component(),
		RuntimeBase:            opts.runtimeBase.component(),
		Materializer:           opts.materializer.component(),
		HomebrewCommit:         opts.homebrewCommit,
		PortableRubyVersion:    opts.portableRubyVersion,
		VerificationKeysDigest: opts.verificationKeysDigest,
		MetadataBundleDigest:   opts.metadataBundleDigest,
		DalecModule:            opts.dalecModule,
		BuildKitModule:         opts.buildKitModule,
	}
	switch schemaVersion {
	case release.SchemaVersionV1:
		if opts.hasV2Inputs() {
			return errors.New("V2 component and policy flags require --schema-version=v2")
		}
		manifest.PolicyVersion = resolution.PolicyVersion
	case release.SchemaVersionV2:
		bottleFetcher := opts.bottleFetcher.component()
		catalogExtractor := opts.catalogExtractor.component()
		manifest.PolicyVersion = release.RuntimePolicyVersionV2
		manifest.BottleFetcher = &bottleFetcher
		manifest.CatalogExtractor = &catalogExtractor
		manifest.TapPolicyDigest = opts.tapPolicyDigest
		manifest.ExecutableRuntimePolicyDigest = opts.executableRuntimePolicyDigest
		manifest.SupportedCatalogPolicyVersions = splitPolicyVersions(opts.supportedCatalogPolicyVersions)
		manifest.SupportedFetchPolicyVersions = splitPolicyVersions(opts.supportedFetchPolicyVersions)
		manifest.SupportedProvenancePolicyVersions = splitPolicyVersions(opts.supportedProvenancePolicyVersions)
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

func parseSchemaVersion(value string) (string, error) {
	switch value {
	case "v1", release.SchemaVersionV1:
		return release.SchemaVersionV1, nil
	case "v2", release.SchemaVersionV2:
		return release.SchemaVersionV2, nil
	default:
		return "", fmt.Errorf("unsupported --schema-version %q: expected v1 or v2", value)
	}
}

func splitPolicyVersions(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func (opts options) hasV2Inputs() bool {
	return opts.bottleFetcher.configured() ||
		opts.catalogExtractor.configured() ||
		opts.metadataBundleDigest != "" ||
		opts.tapPolicyDigest != "" ||
		opts.executableRuntimePolicyDigest != "" ||
		opts.supportedCatalogPolicyVersions != "" ||
		opts.supportedFetchPolicyVersions != "" ||
		opts.supportedProvenancePolicyVersions != ""
}

func bindComponentFlags(flags *flag.FlagSet, name string, opts *componentOptions) {
	flags.StringVar(&opts.index, name+"-index", "", "digest-pinned "+name+" multi-platform index ref")
	flags.StringVar(&opts.amd64, name+"-amd64", "", "digest-pinned "+name+" linux/amd64 child ref")
	flags.StringVar(&opts.arm64, name+"-arm64", "", "digest-pinned "+name+" linux/arm64 child ref")
}

func (opts componentOptions) configured() bool {
	return opts.index != "" || opts.amd64 != "" || opts.arm64 != ""
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
