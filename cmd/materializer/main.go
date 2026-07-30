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

func verifyBottle(args []string) error {
	flags := flag.NewFlagSet("verify-bottle", flag.ContinueOnError)
	recordPath := flags.String("resolution", "", "resolution record")
	name := flags.String("name", "", "canonical Formula name")
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
	f, err := os.Open(*filename)
	if err != nil {
		return err
	}
	defer f.Close()
	result, err := bottle.VerifyNode(f, *node, bottle.Options{})
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
	record, err := resolution.Decode(data)
	if err != nil {
		return err
	}
	if err := resolution.ValidateForMaterialization(record); err != nil {
		return err
	}
	return runtimecheck.Verify(runtimecheck.Options{Root: *root, Prefix: *prefix, LogicalPrefix: *prefix, Arch: record.Input.Platform.Architecture, CPUBaseline: record.Runtime.CPUBaseline, SearchPATH: record.Runtime.GeneratedPATH})
}
