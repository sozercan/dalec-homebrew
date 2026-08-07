package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	validRefs := []string{
		"--runtime-base-ref", "GHCR.IO/example/runtime-base@" + digest,
		"--materializer-ref", "localhost:5000/example/materializer.v2@" + digest,
		"--frontend-ref", "example/frontend_name:release-1@" + digest,
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "empty"},
		{name: "valid refs and timestamp", args: append(append([]string{}, validRefs...), "--metadata-not-before", "2026-08-02T12:34:56.1-07:00")},
		{name: "valid long fraction", args: []string{"--metadata-not-before", "2026-08-02T12:34:56.1234567890Z"}},
		{name: "valid additional refs", args: []string{"--pinned-ref", "GO_IMAGE=GHCR.IO/example/go@" + digest, "--pinned-ref", "dockerfile-frontend=docker/dockerfile:1.12@" + digest}},
		{name: "additional ref scheme", args: []string{"--pinned-ref", "GO_IMAGE=https://ghcr.io/example/go@" + digest}, want: "GO_IMAGE must be a digest-pinned OCI reference"},
		{name: "malformed additional ref", args: []string{"--pinned-ref", "ghcr.io/example/go@" + digest}, want: "NAME=REFERENCE"},
		{name: "partial refs", args: []string{"--runtime-base-ref", "ghcr.io/example/base@" + digest}, want: "must be set together"},
		{name: "scheme", args: []string{"--runtime-base-ref", "https://ghcr.io/example/base@" + digest, "--materializer-ref", "ghcr.io/example/materializer@" + digest, "--frontend-ref", "ghcr.io/example/frontend@" + digest}, want: "invalid OCI reference"},
		{name: "empty segment", args: []string{"--runtime-base-ref", "ghcr.io//base@" + digest, "--materializer-ref", "ghcr.io/example/materializer@" + digest, "--frontend-ref", "ghcr.io/example/frontend@" + digest}, want: "invalid OCI reference"},
		{name: "malformed tag", args: []string{"--runtime-base-ref", "ghcr.io/example/base@" + digest, "--materializer-ref", "ghcr.io/example/materializer:bad:tag@" + digest, "--frontend-ref", "ghcr.io/example/frontend@" + digest}, want: "invalid OCI reference"},
		{name: "invalid date", args: []string{"--metadata-not-before", "2026-02-31T00:00:00Z"}, want: "valid RFC3339"},
		{name: "invalid hour", args: []string{"--metadata-not-before", "2026-08-02T24:00:00Z"}, want: "valid RFC3339"},
		{name: "invalid offset", args: []string{"--metadata-not-before", "2026-08-02T00:00:00+01:60"}, want: "valid RFC3339"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := run(tt.args, &stdout, io.Discard)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				if stdout.Len() != 0 {
					t.Fatalf("unexpected stdout: %q", stdout.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestDalecFrontendPin(t *testing.T) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	digestC := "sha256:" + strings.Repeat("c", 64)
	valid := dalecFrontendPin{
		SchemaVersion: dalecFrontendPinSchema,
		Module:        dalecModule{Path: dalecModulePath, Version: "v0.21.5"},
		Index:         "ghcr.io/project-dalec/dalec/frontend@" + digestA,
		Platforms: map[string]string{
			"linux/amd64": "ghcr.io/project-dalec/dalec/frontend@" + digestB,
			"linux/arm64": "ghcr.io/project-dalec/dalec/frontend@" + digestC,
		},
		Route: dalecHomebrewRoute,
	}

	writePin := func(t *testing.T, value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "dalec-frontend.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	clone := func() dalecFrontendPin {
		pin := valid
		pin.Platforms = make(map[string]string, len(valid.Platforms))
		for platform, ref := range valid.Platforms {
			pin.Platforms[platform] = ref
		}
		return pin
	}

	t.Run("valid", func(t *testing.T) {
		path := writePin(t, valid)
		var stdout bytes.Buffer
		if err := run([]string{"--dalec-frontend-file", path}, &stdout, io.Discard); err != nil {
			t.Fatalf("run: %v", err)
		}
		var got dalecFrontendSelection
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatalf("decode stdout: %v", err)
		}
		want := dalecFrontendSelection{Index: valid.Index, Route: valid.Route}
		if got != want {
			t.Fatalf("selection = %+v, want %+v", got, want)
		}
	})

	t.Run("explicit platform child", func(t *testing.T) {
		path := writePin(t, valid)
		var stdout bytes.Buffer
		args := []string{
			"--dalec-frontend-file", path,
			"--dalec-frontend-ref", valid.Platforms["linux/amd64"],
			"--dalec-route", valid.Route,
			"--platform", "linux/amd64",
		}
		if err := run(args, &stdout, io.Discard); err != nil {
			t.Fatalf("run: %v", err)
		}
		var got dalecFrontendSelection
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatalf("decode stdout: %v", err)
		}
		if got.Index != valid.Platforms["linux/amd64"] || got.Route != valid.Route {
			t.Fatalf("selection = %+v", got)
		}
	})

	tests := []struct {
		name   string
		mutate func(*dalecFrontendPin)
		want   string
	}{
		{name: "wrong schema", mutate: func(pin *dalecFrontendPin) { pin.SchemaVersion = "v2" }, want: "schema_version must be exactly"},
		{name: "wrong module", mutate: func(pin *dalecFrontendPin) { pin.Module.Path = "example.invalid/dalec" }, want: "module path must be exactly"},
		{name: "mutable index", mutate: func(pin *dalecFrontendPin) { pin.Index = "ghcr.io/project-dalec/dalec/frontend:latest" }, want: "index must be a digest-pinned"},
		{name: "mutable child", mutate: func(pin *dalecFrontendPin) {
			pin.Platforms["linux/amd64"] = "ghcr.io/project-dalec/dalec/frontend:latest"
		}, want: "linux/amd64 child must be a digest-pinned"},
		{name: "different repository", mutate: func(pin *dalecFrontendPin) {
			pin.Platforms["linux/amd64"] = "ghcr.io/example/dalec/frontend@" + digestB
		}, want: "different repository"},
		{name: "missing arm64", mutate: func(pin *dalecFrontendPin) { delete(pin.Platforms, "linux/arm64") }, want: "missing platform \"linux/arm64\""},
		{name: "unsupported platform", mutate: func(pin *dalecFrontendPin) {
			pin.Platforms["linux/s390x"] = "ghcr.io/project-dalec/dalec/frontend@" + digestB
		}, want: "unsupported platform"},
		{name: "same child", mutate: func(pin *dalecFrontendPin) { pin.Platforms["linux/arm64"] = pin.Platforms["linux/amd64"] }, want: "must use different manifests"},
		{name: "wrong route", mutate: func(pin *dalecFrontendPin) { pin.Route = "homebrew/debug" }, want: "route must be exactly \"homebrew/image\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pin := clone()
			tt.mutate(&pin)
			path := writePin(t, pin)
			err := run([]string{"--dalec-frontend-file", path}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}

	t.Run("unknown field", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dalec-frontend.json")
		data := `{"schema_version":"dalec-homebrew-dalec-frontend/v1","module":{"path":"github.com/project-dalec/dalec","version":"v0.21.5"},"index":"` + valid.Index + `","platforms":{"linux/amd64":"` + valid.Platforms["linux/amd64"] + `","linux/arm64":"` + valid.Platforms["linux/arm64"] + `"},"route":"homebrew/image","unexpected":true}`
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		err := run([]string{"--dalec-frontend-file", path}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("err = %v, want unknown field", err)
		}
	})

	t.Run("partial explicit selection", func(t *testing.T) {
		path := writePin(t, valid)
		err := run([]string{"--dalec-frontend-file", path, "--dalec-frontend-ref", valid.Index}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Fatalf("err = %v, want paired input error", err)
		}
	})

	t.Run("explicit selection mismatch", func(t *testing.T) {
		path := writePin(t, valid)
		args := []string{
			"--dalec-frontend-file", path,
			"--dalec-frontend-ref", valid.Platforms["linux/arm64"],
			"--dalec-route", valid.Route,
			"--platform", "linux/amd64",
		}
		err := run(args, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "does not match the release-bound index or linux/amd64 child") {
			t.Fatalf("err = %v, want platform binding error", err)
		}
	})

	t.Run("duplicate field", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dalec-frontend.json")
		data := `{"index":"` + valid.Index + `","index":"` + valid.Index + `","platforms":[],"route":"homebrew/image"}`
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		err := run([]string{"--dalec-frontend-file", path}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), `duplicate JSON member "index"`) {
			t.Fatalf("err = %v, want duplicate member", err)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dalec-frontend.json")
		if err := os.WriteFile(path, bytes.Repeat([]byte(" "), maxDalecFrontendPinBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		err := run([]string{"--dalec-frontend-file", path}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("err = %v, want size error", err)
		}
	})
}

func TestBaseSpecValidation(t *testing.T) {
	writeSpec := func(t *testing.T, data string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "spec.yaml")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "valid",
			data: "# syntax=example/frontend@sha256:" + strings.Repeat("a", 64) + "\ndependencies:\n  runtime:\n    hello: {}\n",
		},
		{
			name: "missing syntax",
			data: "dependencies:\n  runtime:\n    hello: {}\n",
			want: "must start with a # syntax= directive",
		},
		{
			name: "runtime list",
			data: "# syntax=example/frontend@sha256:" + strings.Repeat("a", 64) + "\ndependencies:\n  runtime: [hello, jq]\n",
			want: "dependencies.runtime must use map form",
		},
		{
			name: "missing dependencies",
			data: "# syntax=example/frontend@sha256:" + strings.Repeat("a", 64) + "\nname: example\n",
			want: "dependencies.runtime must use map form",
		},
		{
			name: "missing runtime",
			data: "# syntax=example/frontend@sha256:" + strings.Repeat("a", 64) + "\ndependencies: {}\n",
			want: "dependencies.runtime must use map form",
		},
		{
			name: "runtime scalar",
			data: "# syntax=example/frontend@sha256:" + strings.Repeat("a", 64) + "\ndependencies:\n  runtime: hello\n",
			want: "dependencies.runtime must use map form",
		},
		{
			name: "empty targets",
			data: "# syntax=example/frontend@sha256:" + strings.Repeat("a", 64) + "\ndependencies:\n  runtime:\n    hello: {}\ntargets: {}\n",
			want: "must not define top-level targets",
		},
		{
			name: "quoted targets",
			data: "# syntax=example/frontend@sha256:" + strings.Repeat("a", 64) + "\ndependencies:\n  runtime:\n    hello: {}\n\"targets\": {}\n",
			want: "must not define top-level targets",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeSpec(t, test.data)
			err := run([]string{"--base-spec-file", path}, io.Discard, io.Discard)
			if test.want == "" {
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want substring %q", err, test.want)
			}
		})
	}
}
