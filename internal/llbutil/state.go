package llbutil

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func ResolutionState(record *resolution.Record) (llb.State, []byte, error) {
	data, err := resolution.Canonical(record)
	if err != nil {
		return llb.Scratch(), nil, err
	}
	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	state := llb.Scratch().File(llb.Mkfile("/resolution.json", 0o444, data, llb.WithCreatedTime(epoch)))
	return state, data, nil
}

func BottleState(record *resolution.Record) (llb.State, error) {
	if record == nil {
		return llb.Scratch(), fmt.Errorf("nil resolution")
	}
	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	state := llb.Scratch()
	seen := map[string]struct{}{}
	for _, name := range record.InstallOrder {
		node, ok := findNode(record.Nodes, name)
		if !ok {
			return llb.Scratch(), fmt.Errorf("install node %q is missing", name)
		}
		if _, duplicate := seen[node.Bottle.Filename]; duplicate {
			return llb.Scratch(), fmt.Errorf("duplicate bottle filename %q", node.Bottle.Filename)
		}
		seen[node.Bottle.Filename] = struct{}{}
		blob := llb.ImageBlob(node.Bottle.Repository+"@"+node.Bottle.Layer.Digest, llb.Filename(node.Bottle.Filename), llb.Chmod(0o444))
		state = state.File(llb.Copy(blob, "/"+node.Bottle.Filename, "/"+node.Bottle.Filename, &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch}))
	}
	return state, nil
}

func Materialize(materializerRef string, platform ocispec.Platform, record *resolution.Record) (llb.State, error) {
	if materializerRef == "" {
		return llb.Scratch(), fmt.Errorf("empty materializer reference")
	}
	bottles, err := BottleState(record)
	if err != nil {
		return llb.Scratch(), err
	}
	recordState, _, err := ResolutionState(record)
	if err != nil {
		return llb.Scratch(), err
	}
	worker := llb.Image(materializerRef, llb.Platform(platform))
	run := worker.Run(
		llb.Args([]string{"/usr/local/bin/dalec-homebrew-materializer", "materialize", "--resolution", "/run/dalec-homebrew/input/resolution.json", "--bottles", "/run/dalec-homebrew/bottles", "--output", "/out"}),
		llb.User("root"), llb.Network(llb.NetModeNone),
		llb.AddMount("/run/dalec-homebrew/input", recordState, llb.Readonly),
		llb.AddMount("/run/dalec-homebrew/bottles", bottles, llb.Readonly),
		llb.AddMount("/out", llb.Scratch()),
		llb.WithLinuxResources(llb.LinuxResources{Memory: 8 << 30, MemorySwap: 8 << 30, CPUQuota: 400000, CPUPeriod: 100000}),
	)
	return run.GetMount("/out"), nil
}

func AssembleRuntime(runtimeBaseRef string, platform ocispec.Platform, materialized llb.State, record *resolution.Record) (llb.State, error) {
	if runtimeBaseRef == "" {
		return llb.Scratch(), fmt.Errorf("empty runtime base reference")
	}
	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	base := llb.Image(runtimeBaseRef, llb.Platform(platform))
	// Materializer output is a complete root-relative overlay assembled from an
	// explicit verified inventory. Copying it into the clean base never inherits
	// the materializer image or its Homebrew repository.
	return base.File(llb.Copy(materialized, "/", "/", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true, CreatedTime: &epoch})), nil
}

func Solve(ctx context.Context, client gwclient.Client, state llb.State, platform ocispec.Platform) (gwclient.Reference, error) {
	def, err := state.Marshal(ctx, llb.Platform(platform))
	if err != nil {
		return nil, err
	}
	result, err := client.Solve(ctx, gwclient.SolveRequest{Definition: def.ToPB()})
	if err != nil {
		return nil, err
	}
	return result.SingleRef()
}

func findNode(nodes []resolution.Node, name string) (resolution.Node, bool) {
	for _, node := range nodes {
		if node.Name == name {
			return node, true
		}
	}
	return resolution.Node{}, false
}

// ApplyExecutionConfig makes the LLB execution state match the exported OCI
// environment, directory and user. Entrypoint/Cmd remain image metadata.
func ApplyExecutionConfig(state llb.State, image *resolutionExecutionConfig) llb.State {
	if image == nil {
		return state
	}
	for _, entry := range image.Env {
		if key, value, ok := cutEnv(entry); ok {
			state = state.AddEnv(key, value)
		}
	}
	if image.WorkingDir != "" {
		state = state.File(llb.Mkdir(image.WorkingDir, 0o755, llb.WithParents(true))).Dir(image.WorkingDir)
	}
	if image.User != "" {
		state = state.User(image.User)
	}
	return state
}

// resolutionExecutionConfig is intentionally tiny so callers do not have to
// couple this helper to one image-spec package alias.
type resolutionExecutionConfig struct {
	Env        []string
	WorkingDir string
	User       string
}

func ExecutionConfig(env []string, workingDir, user string) *resolutionExecutionConfig {
	return &resolutionExecutionConfig{Env: append([]string(nil), env...), WorkingDir: workingDir, User: user}
}

func cutEnv(value string) (string, string, bool) {
	for i := 0; i < len(value); i++ {
		if value[i] == '=' && i > 0 {
			return value[:i], value[i+1:], true
		}
	}
	return "", "", false
}
