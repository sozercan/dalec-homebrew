package testrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const shellPath = "/bin/sh"

// Runner executes plans in the current filesystem namespace.
//
// A nil Environ inherits os.Environ. A non-nil empty Environ starts commands
// with an empty environment. Nil output writers discard the corresponding
// stream after it has been captured for assertions.
type Runner struct {
	Stdout         io.Writer
	Stderr         io.Writer
	Environ        []string
	MaxOutputBytes int64
	MaxFileBytes   int64
	StepTimeout    time.Duration
	WaitDelay      time.Duration
}

// Run executes a plan with command output connected to the current process.
func Run(ctx context.Context, plan Plan) error {
	return Runner{Stdout: os.Stdout, Stderr: os.Stderr}.Run(ctx, plan)
}

// Run validates the complete plan, then runs its steps and file checks. Each
// command is executed as /bin/sh -c <command> and must exit successfully before
// the next step starts.
func (r Runner) Run(ctx context.Context, plan Plan) error {
	if err := plan.Validate(); err != nil {
		return err
	}

	stdout := r.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := r.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	baseEnv := r.Environ
	if baseEnv == nil {
		baseEnv = os.Environ()
	}

	test := plan.Test
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("before test %q: %w", test.Name, err)
	}
	maxOutput := r.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 16 << 20
	}
	maxFile := r.MaxFileBytes
	if maxFile <= 0 {
		maxFile = 16 << 20
	}
	stepTimeout := r.StepTimeout
	if stepTimeout <= 0 {
		stepTimeout = 10 * time.Minute
	}
	waitDelay := r.WaitDelay
	if waitDelay <= 0 {
		waitDelay = 2 * time.Second
	}
	for stepIndex, step := range test.Steps {
		if err := runStep(ctx, baseEnv, test, step, stdout, stderr, maxOutput, stepTimeout, waitDelay); err != nil {
			return fmt.Errorf("test %q step %d: %w", test.Name, stepIndex+1, err)
		}
	}
	if err := checkFilesLimited(test.Files, maxFile); err != nil {
		return fmt.Errorf("test %q: %w", test.Name, err)
	}
	return nil
}

func runStep(ctx context.Context, baseEnv []string, test TestSpec, step TestStep, stdout, stderr io.Writer, maxOutput int64, timeout, waitDelay time.Duration) error {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(stepCtx, shellPath, "-c", step.Command)
	configureCommand(cmd, waitDelay)
	cmd.Dir = test.Dir
	cmd.Env = mergeEnv(baseEnv, test.Env, step.Env)
	cmd.Stdin = strings.NewReader(step.Stdin)

	capturedStdout := &boundedCapture{dst: stdout, limit: maxOutput}
	capturedStderr := &boundedCapture{dst: stderr, limit: maxOutput}
	cmd.Stdout = capturedStdout
	cmd.Stderr = capturedStderr

	runErr := cmd.Run()
	terminateCommandGroup(cmd)
	if err := runErr; err != nil {
		if ctxErr := stepCtx.Err(); ctxErr != nil {
			return fmt.Errorf("command %q: %w", step.Command, ctxErr)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("command %q exited with status %d", step.Command, exitErr.ExitCode())
		}
		return fmt.Errorf("run command %q: %w", step.Command, err)
	}
	if capturedStdout.exceeded || capturedStderr.exceeded {
		return fmt.Errorf("command %q output exceeds %d bytes per stream", step.Command, maxOutput)
	}
	if err := checkOutput("stdout", capturedStdout.buf.Bytes(), step.Stdout); err != nil {
		return err
	}
	if err := checkOutput("stderr", capturedStderr.buf.Bytes(), step.Stderr); err != nil {
		return err
	}
	return nil
}

func mergeEnv(base []string, layers ...map[string]string) []string {
	env := make(map[string]string, len(base))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	for _, layer := range layers {
		for key, value := range layer {
			env[key] = value
		}
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}
	return result
}

type boundedCapture struct {
	dst      io.Writer
	buf      bytes.Buffer
	limit    int64
	exceeded bool
}

func (w *boundedCapture) Write(p []byte) (int, error) {
	remaining := w.limit - int64(w.buf.Len())
	take := len(p)
	if remaining <= 0 {
		take = 0
	} else if int64(take) > remaining {
		take = int(remaining)
	}
	if take > 0 {
		n, err := w.dst.Write(p[:take])
		if err != nil {
			return n, err
		}
		if n != take {
			return n, io.ErrShortWrite
		}
		_, _ = w.buf.Write(p[:take])
	}
	if take < len(p) {
		w.exceeded = true
	}
	return len(p), nil
}
