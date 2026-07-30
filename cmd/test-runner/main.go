package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sozercan/dalec-homebrew/internal/testrunner"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: dalec-homebrew-test-runner <plan.json>")
		fmt.Fprintln(stderr, "       dalec-homebrew-test-runner -  # read the plan from stdin")
		return 2
	}

	reader := stdin
	var closePlan func() error
	if args[0] != "-" {
		file, err := os.Open(args[0])
		if err != nil {
			fmt.Fprintf(stderr, "dalec-homebrew-test-runner: open plan: %v\n", err)
			return 1
		}
		reader = file
		closePlan = file.Close
	}
	if closePlan != nil {
		defer closePlan() // The read happens synchronously before run returns.
	}

	plan, err := testrunner.DecodePlan(reader)
	if err != nil {
		fmt.Fprintf(stderr, "dalec-homebrew-test-runner: %v\n", err)
		return 1
	}
	if err := (testrunner.Runner{Stdout: stdout, Stderr: stderr}).Run(ctx, plan); err != nil {
		fmt.Fprintf(stderr, "dalec-homebrew-test-runner: %v\n", err)
		return 1
	}
	return 0
}
