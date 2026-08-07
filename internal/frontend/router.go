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
	targetOption       = "target"
)

// NewHandler returns the child gateway entrypoint for upstream-Dalec-forwarded
// invocations and subrequests. It advertises only the image route.
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

	return router.Handle
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
