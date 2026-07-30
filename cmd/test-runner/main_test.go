package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/testrunner"
)

func TestRunReadsPlanFromStdin(t *testing.T) {
	plan := testrunner.Plan{Test: testrunner.TestSpec{
		Name: "ok",
		Steps: []testrunner.TestStep{{
			Command: `printf output; printf warning >&2`,
			Stdout:  testrunner.CheckOutput{Equals: "output"},
			Stderr:  testrunner.CheckOutput{Equals: "warning"},
		}},
	}}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"-"}, bytes.NewReader(data), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "output" || stderr.String() != "warning" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunReadsPlanFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(`{"test":{"name":"ok","steps":[{"command":"printf file","stdout":{"equals":"file"}}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(context.Background(), []string{path}, strings.NewReader("unused"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "file" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestRunUsageAndPlanErrors(t *testing.T) {
	t.Run("usage", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := run(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "usage:") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("unknown mounts", func(t *testing.T) {
		var stderr bytes.Buffer
		code := run(context.Background(), []string{"-"}, strings.NewReader(`{"test":{"name":"bad","mounts":[]}}`), &bytes.Buffer{}, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), `unknown field "mounts"`) {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("missing file", func(t *testing.T) {
		var stderr bytes.Buffer
		code := run(context.Background(), []string{filepath.Join(t.TempDir(), "missing.json")}, strings.NewReader(""), &bytes.Buffer{}, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "open plan") {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	})
}

func TestRunReportsTestFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{"-"},
		strings.NewReader(`{"test":{"name":"fails","steps":[{"command":"exit 9"}]}}`),
		&stdout,
		&stderr,
	)
	if code != 1 || !strings.Contains(stderr.String(), "status 9") || !strings.Contains(stderr.String(), `test "fails"`) {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
