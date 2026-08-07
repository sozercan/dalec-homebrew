package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/materializer"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimecheck"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dalec-homebrew-materializer:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dalec-homebrew-materializer materialize [flags]")
	}
	switch args[0] {
	case "materialize":
		return materialize(ctx, args[1:])
	case "prepare-v2":
		return prepareV2(ctx, args[1:])
	case "install-one-v2":
		return installOneV2(ctx, args[1:])
	case "finalize-v2":
		return finalizeV2(ctx, args[1:])
	case "verify-bottle":
		return verifyBottle(args[1:])
	case "verify-runtime":
		return verifyRuntime(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func materialize(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("materialize", flag.ContinueOnError)
	recordPath := flags.String("resolution", "", "resolution record")
	bottles := flags.String("bottles", "", "verified bottle directory")
	output := flags.String("output", "", "output root")
	prefix := flags.String("prefix", materializer.DefaultPrefix, "Homebrew prefix")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *recordPath == "" || *bottles == "" || *output == "" {
		return fmt.Errorf("--resolution, --bottles, and --output are required")
	}
	data, err := os.ReadFile(*recordPath)
	if err != nil {
		return err
	}
	record, err := resolution.Decode(data)
	if err != nil {
		return err
	}
	_, err = materializer.Materialize(ctx, materializer.MaterializeConfig{Record: record, BottlesDir: *bottles, OutputRoot: *output, Prefix: *prefix})
	return err
}

func prepareV2(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("prepare-v2", flag.ContinueOnError)
	recordPath := flags.String("resolution", "", "V2 resolution record")
	bottles := flags.String("bottles", "", "fetched bottle directory")
	fetchEvidence := flags.String("fetch-evidence", "", "HTTPS fetch evidence directory")
	prefix := flags.String("prefix", materializer.DefaultPrefix, "seeded Homebrew prefix")
	preparedRoot := flags.String("prepared-root", "", "prepared state root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *recordPath == "" || *bottles == "" || *preparedRoot == "" {
		return fmt.Errorf("--resolution, --bottles, and --prepared-root are required")
	}
	data, err := os.ReadFile(*recordPath)
	if err != nil {
		return err
	}
	record, err := resolution.DecodeV2(data)
	if err != nil {
		return err
	}
	_, err = materializer.PrepareV2(ctx, materializer.PrepareV2Config{Record: record, BottlesDir: *bottles, FetchEvidenceDir: *fetchEvidence, Prefix: *prefix, PreparedRoot: *preparedRoot})
	return err
}

func installOneV2(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("install-one-v2", flag.ContinueOnError)
	recordPath := flags.String("resolution", "", "V2 resolution record")
	id := flags.String("id", "", "canonical Formula ID")
	bottlePath := flags.String("bottle", "", "prepared verified bottle")
	prefix := flags.String("prefix", materializer.DefaultPrefix, "persistent Homebrew prefix")
	homebrewConfig := flags.String("homebrew-config", "", "read-only Homebrew user config")
	preparation := flags.String("preparation", "", "preparation evidence JSON")
	evidence := flags.String("evidence", "", "install delta evidence path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *recordPath == "" || *id == "" || *bottlePath == "" || *homebrewConfig == "" || *preparation == "" || *evidence == "" {
		return fmt.Errorf("--resolution, --id, --bottle, --homebrew-config, --preparation, and --evidence are required")
	}
	data, err := os.ReadFile(*recordPath)
	if err != nil {
		return err
	}
	record, err := resolution.DecodeV2(data)
	if err != nil {
		return err
	}
	_, err = materializer.InstallOneV2(ctx, materializer.InstallOneV2Config{Record: record, ID: resolution.FormulaID(*id), BottlePath: *bottlePath, Prefix: *prefix, HomebrewConfig: *homebrewConfig, PreparationEvidence: *preparation, EvidencePath: *evidence})
	return err
}

func finalizeV2(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("finalize-v2", flag.ContinueOnError)
	recordPath := flags.String("resolution", "", "V2 resolution record")
	prefix := flags.String("prefix", materializer.DefaultPrefix, "installed Homebrew prefix")
	output := flags.String("output", "", "runtime overlay root")
	preparation := flags.String("preparation", "", "preparation evidence JSON")
	installEvidence := flags.String("install-evidence", "", "install delta evidence directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *recordPath == "" || *output == "" || *preparation == "" || *installEvidence == "" {
		return fmt.Errorf("--resolution, --output, --preparation, and --install-evidence are required")
	}
	data, err := os.ReadFile(*recordPath)
	if err != nil {
		return err
	}
	record, err := resolution.DecodeV2(data)
	if err != nil {
		return err
	}
	_, err = materializer.FinalizeV2(ctx, materializer.FinalizeV2Config{Record: record, Prefix: *prefix, OutputRoot: *output, PreparationEvidence: *preparation, InstallEvidenceDir: *installEvidence})
	return err
}

func verifyBottle(args []string) error {
	flags := flag.NewFlagSet("verify-bottle", flag.ContinueOnError)
	recordPath := flags.String("resolution", "", "resolution record")
	name := flags.String("name", "", "canonical Formula name or V2 Formula ID")
	filename := flags.String("file", "", "bottle file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *recordPath == "" || *name == "" || *filename == "" {
		return fmt.Errorf("--resolution, --name, and --file are required")
	}
	data, err := os.ReadFile(*recordPath)
	if err != nil {
		return err
	}
	schema, err := resolution.SchemaVersionOf(data)
	if err != nil {
		return err
	}
	f, err := os.Open(*filename)
	if err != nil {
		return err
	}
	defer f.Close()
	var result *bottle.Result
	switch schema {
	case resolution.SchemaVersionV1:
		record, err := resolution.Decode(data)
		if err != nil {
			return err
		}
		var node *resolution.Node
		for i := range record.Nodes {
			if record.Nodes[i].Name == *name {
				node = &record.Nodes[i]
				break
			}
		}
		if node == nil {
			return fmt.Errorf("Formula %q is not in resolution", *name)
		}
		result, err = bottle.VerifyNode(f, *node, bottle.Options{})
	case resolution.SchemaVersionV2:
		record, err := resolution.DecodeV2(data)
		if err != nil {
			return err
		}
		var node *resolution.NodeV2
		for i := range record.Nodes {
			if record.Nodes[i].ID.String() == *name {
				node = &record.Nodes[i]
				break
			}
		}
		if node == nil {
			return fmt.Errorf("Formula ID %q is not in V2 resolution", *name)
		}
		result, err = bottle.VerifyNodeV2(f, *node, record.Nodes, bottle.Options{})
	default:
		return fmt.Errorf("unsupported resolution schema %q", schema)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func verifyRuntime(args []string) error {
	flags := flag.NewFlagSet("verify-runtime", flag.ContinueOnError)
	recordPath := flags.String("resolution", "", "resolution record")
	root := flags.String("root", "/", "runtime root")
	prefix := flags.String("prefix", materializer.DefaultPrefix, "Homebrew prefix")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *recordPath == "" {
		return fmt.Errorf("--resolution is required")
	}
	data, err := os.ReadFile(*recordPath)
	if err != nil {
		return err
	}
	schema, err := resolution.SchemaVersionOf(data)
	if err != nil {
		return err
	}
	switch schema {
	case resolution.SchemaVersionV1:
		record, err := resolution.Decode(data)
		if err != nil {
			return err
		}
		if err := resolution.ValidateForMaterialization(record); err != nil {
			return err
		}
		return runtimecheck.Verify(runtimecheck.Options{Root: *root, Prefix: *prefix, LogicalPrefix: *prefix, Arch: record.Input.Platform.Architecture, CPUBaseline: record.Runtime.CPUBaseline, SearchPATH: record.Runtime.GeneratedPATH})
	case resolution.SchemaVersionV2:
		record, err := resolution.DecodeV2(data)
		if err != nil {
			return err
		}
		return runtimecheck.Verify(runtimecheck.Options{Root: *root, Prefix: *prefix, LogicalPrefix: *prefix, Arch: record.Input.Platform.Architecture, CPUBaseline: record.Runtime.CPUBaseline, SearchPATH: record.Runtime.GeneratedPATH})
	default:
		return fmt.Errorf("unsupported resolution schema %q", schema)
	}
}
