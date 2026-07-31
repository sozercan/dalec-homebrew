package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sozercan/dalec-homebrew/internal/runtimebase"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "listen address")
	ready := flag.String("ready-file", "", "file created after listening")
	snapshot := flag.String("snapshot", "", "Ubuntu snapshot timestamp")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runtimebase.ServeSnapshotProxy(ctx, *listen, *ready, *snapshot); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
