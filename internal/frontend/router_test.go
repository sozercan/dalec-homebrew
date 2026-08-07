package frontend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	bktargets "github.com/moby/buildkit/frontend/subrequests/targets"
)

type buildOptsClient struct {
	gwclient.Client
	opts gwclient.BuildOpts
}

func (c *buildOptsClient) BuildOpts() gwclient.BuildOpts {
	return c.opts
}

func clientWithOpts(opts map[string]string) gwclient.Client {
	return &buildOptsClient{opts: gwclient.BuildOpts{Opts: opts}}
}

func TestHandlerAdvertisesOnlyImageRoute(t *testing.T) {
	handler := newHandler(t.Context(), func(context.Context, gwclient.Client) (*gwclient.Result, error) {
		t.Fatal("image handler called for target-list subrequest")
		return nil, nil
	})

	result, err := handler(t.Context(), clientWithOpts(map[string]string{
		requestIDOption: bktargets.RequestTargets,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var targets bktargets.List
	if err := json.Unmarshal(result.Metadata["result.json"], &targets); err != nil {
		t.Fatal(err)
	}
	if len(targets.Targets) != 1 || targets.Targets[0].Name != imageRoute {
		t.Fatalf("targets=%+v, want only %q", targets.Targets, imageRoute)
	}
}

func TestHandlerPreservesDirectInvocation(t *testing.T) {
	calls := 0
	handler := newHandler(t.Context(), func(_ context.Context, client gwclient.Client) (*gwclient.Result, error) {
		calls++
		if got := client.BuildOpts().Opts[targetOption]; got != "production" {
			t.Fatalf("target=%q, want production", got)
		}
		return gwclient.NewResult(), nil
	})

	if _, err := handler(t.Context(), clientWithOpts(map[string]string{targetOption: "production"})); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("image handler calls=%d, want 1", calls)
	}
}

func TestHandlerRoutesForwardedHomebrewImage(t *testing.T) {
	calls := 0
	handler := newHandler(t.Context(), func(_ context.Context, client gwclient.Client) (*gwclient.Result, error) {
		calls++
		if got := client.BuildOpts().Opts[targetOption]; got != imageRoute {
			t.Fatalf("target=%q, want %q", got, imageRoute)
		}
		return gwclient.NewResult(), nil
	})

	if _, err := handler(t.Context(), clientWithOpts(map[string]string{
		"dalec.target": forwardedTargetKey,
		targetOption:   imageRoute,
	})); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("image handler calls=%d, want 1", calls)
	}
}

func TestHandlerRejectsInvalidForwardedRoutes(t *testing.T) {
	tests := []struct {
		name string
		opts map[string]string
		want string
	}{
		{
			name: "bare target",
			opts: map[string]string{"dalec.target": forwardedTargetKey},
			want: "no such handler",
		},
		{
			name: "unknown child target",
			opts: map[string]string{"dalec.target": forwardedTargetKey, targetOption: "debug"},
			want: "no such handler",
		},
		{
			name: "nested child target",
			opts: map[string]string{"dalec.target": forwardedTargetKey, targetOption: imageRoute + "/debug"},
			want: "must be exactly",
		},
		{
			name: "wrong spec target",
			opts: map[string]string{"dalec.target": "other", targetOption: imageRoute},
			want: `must be "homebrew"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			handler := newHandler(t.Context(), func(context.Context, gwclient.Client) (*gwclient.Result, error) {
				calls++
				return gwclient.NewResult(), nil
			})
			_, err := handler(t.Context(), clientWithOpts(test.opts))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			if calls != 0 {
				t.Fatalf("image handler calls=%d, want 0", calls)
			}
		})
	}
}
