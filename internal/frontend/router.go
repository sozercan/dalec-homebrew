package frontend

import (
	"context"
	"fmt"

	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	bktargets "github.com/moby/buildkit/frontend/subrequests/targets"
	dalecfrontend "github.com/project-dalec/dalec/frontend"
)

const (
	forwardedTargetKey = "homebrew"
	imageRoute         = "image"
	requestIDOption    = "requestid"
	targetOption       = "target"
)

// NewHandler returns the gateway entrypoint for direct and upstream-Dalec
// invocations. Direct builds keep their existing target semantics. Forwarded
// builds and subrequests go through Dalec's router, which exposes only the
// provider-owned image route.
func NewHandler(ctx context.Context) gwclient.BuildFunc {
	return newHandler(ctx, Handle)
}

func newHandler(ctx context.Context, imageHandler gwclient.BuildFunc) gwclient.BuildFunc {
	var router dalecfrontend.Router
	router.Add(ctx, dalecfrontend.Route{
		FullPath: imageRoute,
		Handler:  exactForwardedImageHandler(imageHandler),
		Info: dalecfrontend.Target{Target: bktargets.Target{
			Name:        imageRoute,
			Description: "Build a minimal Homebrew runtime image",
		}},
	})

	return func(ctx context.Context, client gwclient.Client) (*gwclient.Result, error) {
		opts := client.BuildOpts().Opts
		if opts[requestIDOption] != "" || dalecfrontend.GetTargetKey(client) != "" {
			return router.Handle(ctx, client)
		}
		return imageHandler(ctx, client)
	}
}

func exactForwardedImageHandler(next gwclient.BuildFunc) gwclient.BuildFunc {
	return func(ctx context.Context, client gwclient.Client) (*gwclient.Result, error) {
		targetKey := dalecfrontend.GetTargetKey(client)
		if targetKey != forwardedTargetKey {
			return nil, fmt.Errorf("forwarded Dalec target must be %q, got %q", forwardedTargetKey, targetKey)
		}
		if target := client.BuildOpts().Opts[targetOption]; target != imageRoute {
			return nil, fmt.Errorf("forwarded child target must be exactly %q, got %q", imageRoute, target)
		}
		return next(ctx, client)
	}
}
