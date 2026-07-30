package frontend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/moby/buildkit/client/llb"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/project-dalec/dalec"
	"github.com/sozercan/dalec-homebrew/internal/llbutil"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/testrunner"
)

func AddTests(state llb.State, materializerRef string, platform ocispec.Platform, tests []*dalec.TestSpec, env []string, dir, user string, epochSeconds int64) (llb.State, error) {
	plans, err := testrunner.NewPlans(tests)
	if err != nil {
		return llb.Scratch(), err
	}
	if len(plans) == 0 {
		return state, nil
	}
	epoch := time.Unix(epochSeconds, 0).UTC()
	toolImage := llb.Image(materializerRef, llb.Platform(platform))
	tool := llb.Scratch().File(llb.Copy(toolImage, "/usr/local/bin/dalec-homebrew-test-runner", "/dalec-homebrew-test-runner", &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch}))
	branches := make([]llb.State, 0, len(plans))
	h := sha256.New()
	for i, plan := range plans {
		data, err := json.Marshal(plan)
		if err != nil {
			return llb.Scratch(), err
		}
		_, _ = h.Write(data)
		planState := llb.Scratch().File(llb.Mkfile("/plan.json", 0o444, data, llb.WithCreatedTime(epoch)))
		testState := state.
			File(llb.Copy(tool, "/dalec-homebrew-test-runner", "/__dalec_homebrew/test-runner", &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch})).
			File(llb.Copy(planState, "/plan.json", fmt.Sprintf("/__dalec_homebrew/test-%03d.json", i), &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch}))
		for _, entry := range env {
			if key, value, ok := cutEnv(entry); ok {
				testState = testState.AddEnv(key, value)
			}
		}
		if dir != "" {
			testState = testState.Dir(dir)
		}
		if user != "" {
			testState = testState.User(user)
		}
		run := testState.Run(
			llb.Args([]string{"/__dalec_homebrew/test-runner", fmt.Sprintf("/__dalec_homebrew/test-%03d.json", i)}),
			llb.Network(llb.NetModeNone), llb.User(user),
			llb.WithLinuxResources(llb.LinuxResources{Memory: 2 << 30, MemorySwap: 2 << 30, CPUQuota: 200000, CPUPeriod: 100000}),
		)
		branches = append(branches, run.Root())
	}
	id := "runtime-tests/" + hex.EncodeToString(h.Sum(nil))
	return state.Requires(id, branches...), nil
}

func cutEnv(entry string) (string, string, bool) {
	for i := 1; i < len(entry); i++ {
		if entry[i] == '=' {
			return entry[:i], entry[i+1:], true
		}
	}
	return "", "", false
}

func AddRuntimeVerification(state llb.State, materializerRef string, platform ocispec.Platform, record *resolution.Record) (llb.State, error) {
	if record == nil {
		return llb.Scratch(), fmt.Errorf("nil resolution for runtime verification")
	}
	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	toolImage := llb.Image(materializerRef, llb.Platform(platform))
	tool := llb.Scratch().File(llb.Copy(toolImage, "/usr/local/bin/dalec-homebrew-materializer", "/dalec-homebrew-materializer", &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch}))
	recordState, data, err := llbutil.ResolutionState(record)
	if err != nil {
		return llb.Scratch(), err
	}
	branch := state.File(llb.Copy(tool, "/dalec-homebrew-materializer", "/__dalec_homebrew/materializer", &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch})).File(llb.Copy(recordState, "/resolution.json", "/__dalec_homebrew/resolution.json", &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch}))
	run := branch.Run(llb.Args([]string{"/__dalec_homebrew/materializer", "verify-runtime", "--resolution", "/__dalec_homebrew/resolution.json", "--root", "/", "--prefix", "/home/linuxbrew/.linuxbrew"}), llb.Network(llb.NetModeNone), llb.User("root"), llb.Dir("/"), llb.WithLinuxResources(llb.LinuxResources{Memory: 2 << 30, MemorySwap: 2 << 30, CPUQuota: 200000, CPUPeriod: 100000}))
	sum := sha256.Sum256(data)
	return state.Requires("runtime-verification/"+hex.EncodeToString(sum[:]), run.Root()), nil
}
