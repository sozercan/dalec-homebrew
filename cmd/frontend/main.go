package main

import (
	"context"
	"fmt"
	"os"

	"github.com/moby/buildkit/frontend/gateway/grpcclient"
	"github.com/moby/buildkit/util/appcontext"
	_ "github.com/moby/buildkit/util/grpcutil/encoding/proto"
	front "github.com/sozercan/dalec-homebrew/internal/frontend"
)

func main() {
	ctx, cancel := context.WithCancel(appcontext.Context())
	defer cancel()
	if err := grpcclient.RunFromEnvironment(ctx, front.NewHandler(ctx)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(70)
	}
}
