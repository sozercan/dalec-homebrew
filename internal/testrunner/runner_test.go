package testrunner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunnerPreservesDirEnvironmentStdinAndSequentialState(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.txt")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	plan := Plan{Test: TestSpec{
		Name: "complete",
		Dir:  dir,
		Env: map[string]string{
			"TEST":       "test-value",
			"OVERRIDE":   "test-level",
			"EXPECT_DIR": dir,
		},
		Steps: []TestStep{
			{
				Command: `printf '%s|%s|%s|' "$TEST" "$STEP" "$OVERRIDE"; cat; printf warn >&2; umask 022; printf state > state.txt; chmod 0644 state.txt`,
				Env: map[string]string{
					"STEP":     "step-value",
					"OVERRIDE": "step-level",
				},
				Stdin:  "input\x00data",
				Stdout: CheckOutput{Equals: "test-value|step-value|step-level|input\x00data"},
				Stderr: CheckOutput{Equals: "warn"},
			},
			{
				Command: `test "$PWD" = "$EXPECT_DIR"; test "${STEP-unset}" = unset; test "$(cat state.txt)" = state; printf second`,
				Stdout:  CheckOutput{Equals: "second"},
				Stderr:  CheckOutput{Empty: true},
			},
		},
		Files: map[string]FileCheckOutput{
			stateFile: {CheckOutput: CheckOutput{Equals: "state"}, Permissions: 0o644},
		},
	}}

	runner := Runner{
		Stdout:  &stdout,
		Stderr:  &stderr,
		Environ: []string{"PATH=/usr/bin:/bin", "OVERRIDE=base-level"},
	}
	if err := runner.Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "test-value|step-value|step-level|input\x00datasecond"; got != want {
		t.Fatalf("stdout=%q, want %q", got, want)
	}
	if got, want := stderr.String(), "warn"; got != want {
		t.Fatalf("stderr=%q, want %q", got, want)
	}
}

func TestRunnerUsesBinSh(t *testing.T) {
	plan := Plan{Test: TestSpec{
		Name: "shell pipeline",
		Steps: []TestStep{{
			Command: `printf abc | tr a-z A-Z`,
			Stdout:  CheckOutput{Equals: "ABC"},
		}},
	}}
	if err := (Runner{Environ: []string{"PATH=/usr/bin:/bin"}}).Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRequiresZeroExitAndStopsImmediately(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "should-not-exist")
	plan := Plan{Test: TestSpec{
		Name: "failure",
		Env:  map[string]string{"MARKER": marker},
		Steps: []TestStep{
			{Command: `exit 7`},
			{Command: `touch "$MARKER"`},
		},
	}}

	err := (Runner{Environ: []string{"PATH=/usr/bin:/bin"}}).Run(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "status 7") || !strings.Contains(err.Error(), "step 1") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("later step ran: %v", statErr)
	}
}

func TestRunnerReportsOutputAndFileCheckFailures(t *testing.T) {
	t.Run("stdout", func(t *testing.T) {
		err := (Runner{}).Run(context.Background(), Plan{Test: TestSpec{
			Name:  "stdout",
			Steps: []TestStep{{Command: `printf actual`, Stdout: CheckOutput{Equals: "expected"}}},
		}})
		if err == nil || !strings.Contains(err.Error(), "stdout") || !strings.Contains(err.Error(), "equals") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("file", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		err := (Runner{}).Run(context.Background(), Plan{Test: TestSpec{
			Name:  "file",
			Files: map[string]FileCheckOutput{missing: {}},
		}})
		if err == nil || !strings.Contains(err.Error(), "expected path to exist") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestRunnerValidatesWholePlanBeforeExecuting(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	plan := Plan{Test: TestSpec{
		Name: "invalid",
		Env:  map[string]string{"MARKER": marker},
		Steps: []TestStep{
			{Command: `touch "$MARKER"`},
			{Command: "true", Stdout: CheckOutput{Matches: []string{"["}}},
		},
	}}

	err := (Runner{Environ: []string{"PATH=/usr/bin:/bin"}}).Run(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "invalid regular expression") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("plan executed before validation completed: %v", statErr)
	}
}

func TestRunnerHonorsContextCancellation(t *testing.T) {
	t.Run("before execution", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := (Runner{}).Run(ctx, Plan{Test: TestSpec{Name: "cancelled", Steps: []TestStep{{Command: "true"}}}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("during command", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := (Runner{}).Run(ctx, Plan{Test: TestSpec{Name: "cancelled", Steps: []TestStep{{Command: "while :; do :; done"}}}})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestMergeEnvUsesLastLayerAndStableOrder(t *testing.T) {
	got := mergeEnv(
		[]string{"Z=base", "A=base", "MALFORMED", "A=last-base"},
		map[string]string{"Z": "test", "B": "test"},
		map[string]string{"Z": "step", "C": "step"},
	)
	want := []string{"A=last-base", "B=test", "C=step", "Z=step"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRunnerBoundsCapturedOutput(t *testing.T) {
	plan := Plan{SchemaVersion: PlanSchemaVersion, Test: TestSpec{Name: "bounded", Steps: []TestStep{{Command: "printf 1234567890"}}}}
	err := (Runner{Stdout: io.Discard, Stderr: io.Discard, MaxOutputBytes: 4}).Run(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunnerDoesNotWaitForBackgroundPipeHolders(t *testing.T) {
	plan := Plan{SchemaVersion: PlanSchemaVersion, Test: TestSpec{Name: "background", Steps: []TestStep{{Command: "sleep 60 &"}}}}
	start := time.Now()
	err := (Runner{Stdout: io.Discard, Stderr: io.Discard, WaitDelay: 50 * time.Millisecond, StepTimeout: time.Second}).Run(context.Background(), plan)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("runner blocked on descendant: %v", time.Since(start))
	}
	if err == nil {
		t.Fatal("expected wait-delay failure for background pipe holder")
	}
}
