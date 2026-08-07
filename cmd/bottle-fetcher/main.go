package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sozercan/dalec-homebrew/internal/fetcher"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "dalec-homebrew-bottle-fetcher:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader) error {
	flags := flag.NewFlagSet("dalec-homebrew-bottle-fetcher", flag.ContinueOnError)
	requestPath := flags.String("request", "", "signed fetch request JSON path, or - for stdin")
	outputPath := flags.String("output", "", "verified bottle output path")
	evidencePath := flags.String("evidence", "", "fetch evidence output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not supported")
	}
	if *requestPath == "" || *outputPath == "" || *evidencePath == "" {
		return fmt.Errorf("--request, --output, and --evidence are required")
	}

	reader := stdin
	var closeRequest func() error
	if *requestPath != "-" {
		file, err := os.Open(*requestPath)
		if err != nil {
			return fmt.Errorf("open fetch request: %w", err)
		}
		reader = file
		closeRequest = file.Close
	}
	if closeRequest != nil {
		defer closeRequest()
	}
	request, err := fetcher.DecodeRequest(reader)
	if err != nil {
		return err
	}
	client, err := fetcher.New(fetcher.Config{})
	if err != nil {
		return err
	}
	_, err = client.FetchToFiles(ctx, request, *outputPath, *evidencePath)
	return err
}
