package buildfiles

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseInputsFixture(t *testing.T) {
	stdout, stderr, err := runReleaseInputs(t, validBakeFixture(), "")
	if err != nil {
		t.Fatalf("release-inputs.sh: %v\nstderr: %s", err, stderr)
	}
	if len(stderr) != 0 {
		t.Fatalf("stderr = %q", stderr)
	}

	var inputs struct {
		SchemaVersion          string            `json:"schema_version"`
		UbuntuBase             map[string]string `json:"ubuntu_base"`
		HomebrewCommit         string            `json:"homebrew_commit"`
		VerificationKeysDigest string            `json:"verification_keys_digest"`
		DalecModule            string            `json:"dalec_module"`
		BuildKitModule         string            `json:"buildkit_module"`
	}
	if err := json.Unmarshal(stdout, &inputs); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if inputs.SchemaVersion != "dalec-homebrew-release-inputs/v1" {
		t.Fatalf("schema_version = %q", inputs.SchemaVersion)
	}
	if inputs.UbuntuBase["linux/amd64"] != testRuntimeBaseAMD64 || inputs.UbuntuBase["linux/arm64"] != testRuntimeBaseARM64 {
		t.Fatalf("ubuntu_base = %#v", inputs.UbuntuBase)
	}
	if inputs.HomebrewCommit != testHomebrewCommit {
		t.Fatalf("homebrew_commit = %q", inputs.HomebrewCommit)
	}
	if inputs.VerificationKeysDigest != testVerificationKeysDigest {
		t.Fatalf("verification_keys_digest = %q", inputs.VerificationKeysDigest)
	}
	if inputs.DalecModule != "v0.21.5-0.20260728234020-5fa2c46d716b" || inputs.BuildKitModule != "v0.31.2" {
		t.Fatalf("module versions = %q, %q", inputs.DalecModule, inputs.BuildKitModule)
	}
}

func TestReleaseInputsRejectsUnsafeFixtures(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*bakeFixture)
		replacedModule string
		want           string
	}{
		{
			name: "conflicting epochs",
			mutate: func(f *bakeFixture) {
				f.Target["materializer-arm64"].Args["SOURCE_DATE_EPOCH"] = "1781049601"
			},
			want: "release targets use conflicting SOURCE_DATE_EPOCH values",
		},
		{
			name: "mutable runtime base",
			mutate: func(f *bakeFixture) {
				f.Target["runtime-base-amd64"].Args["RUNTIME_BASE"] = "docker.io/library/ubuntu:24.04"
				f.Target["materializer-amd64"].Args["RUNTIME_BASE"] = "docker.io/library/ubuntu:24.04"
			},
			want: "runtime base is not digest pinned",
		},
		{
			name: "target argument override",
			mutate: func(f *bakeFixture) {
				f.Target["runtime-base-amd64"].Args["HOMEBREW_COMMIT"] = strings.Repeat("c", 40)
			},
			want: "runtime-base-amd64 overrides HOMEBREW_COMMIT with an unrecorded release value",
		},
		{
			name: "alternate Dockerfile",
			mutate: func(f *bakeFixture) {
				target := f.Target["frontend"]
				target.Dockerfile = "Dockerfile.release"
				f.Target["frontend"] = target
			},
			want: "frontend must use context ., Dockerfile, target frontend",
		},
		{
			name:           "module replacement",
			replacedModule: "github.com/project-dalec/dalec",
			want:           "release module github.com/project-dalec/dalec must not be replaced",
		},
		{
			name:           "BuildKit module replacement",
			replacedModule: "github.com/moby/buildkit",
			want:           "release module github.com/moby/buildkit must not be replaced",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := validBakeFixture()
			if tt.mutate != nil {
				tt.mutate(&fixture)
			}
			stdout, stderr, err := runReleaseInputs(t, fixture, tt.replacedModule)
			if err == nil {
				t.Fatalf("release-inputs.sh succeeded\nstdout: %s", stdout)
			}
			if len(stdout) != 0 {
				t.Fatalf("stdout = %q, want no partial release inputs", stdout)
			}
			if !strings.Contains(string(stderr), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.want)
			}
		})
	}
}

const (
	testSourceDateEpoch        = "1781049600"
	testRuntimeBaseAMD64       = "docker.io/library/ubuntu@sha256:52df9b1ee71626e0088f7d400d5c6b5f7bb916f8f0c82b474289a4ece6cf3faf"
	testRuntimeBaseARM64       = "docker.io/library/ubuntu@sha256:7f622ca8766bccb22f04242ecb6f19f770b2f08827dc4b8c707de5e78a6da7ab"
	testHomebrewCommit         = "77d90328ca2f63ff4ec1f67de0ade5632f5d2335"
	testVerificationKeysDigest = "sha256:ef2d2c9e0219d485df9f07fff7b037feadc36c93085be9ffefb1390f31a3de1d"
)

type bakeFixture struct {
	Target map[string]bakeTarget `json:"target"`
}

type bakeTarget struct {
	Target     string            `json:"target"`
	Context    string            `json:"context"`
	Dockerfile string            `json:"dockerfile"`
	Platforms  []string          `json:"platforms"`
	Args       map[string]string `json:"args"`
}

func validBakeFixture() bakeFixture {
	child := func(target, platform, runtimeBase string) bakeTarget {
		args := map[string]string{"SOURCE_DATE_EPOCH": testSourceDateEpoch}
		if runtimeBase != "" {
			args["RUNTIME_BASE"] = runtimeBase
		}
		return bakeTarget{Target: target, Context: ".", Dockerfile: "Dockerfile", Platforms: []string{platform}, Args: args}
	}
	return bakeFixture{Target: map[string]bakeTarget{
		"runtime-base-amd64": child("runtime-base", "linux/amd64", testRuntimeBaseAMD64),
		"runtime-base-arm64": child("runtime-base", "linux/arm64", testRuntimeBaseARM64),
		"materializer-amd64": child("materializer", "linux/amd64", testRuntimeBaseAMD64),
		"materializer-arm64": child("materializer", "linux/arm64", testRuntimeBaseARM64),
		"frontend": {
			Target:     "frontend",
			Context:    ".",
			Dockerfile: "Dockerfile",
			Platforms:  []string{"linux/amd64", "linux/arm64"},
			Args: map[string]string{
				"FRONTEND_REF":      "",
				"MATERIALIZER_REF":  "",
				"RUNTIME_BASE_REF":  "",
				"SOURCE_DATE_EPOCH": testSourceDateEpoch,
			},
		},
	}}
}

func runReleaseInputs(t *testing.T, fixture bakeFixture, replacedModule string) ([]byte, []byte, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release-inputs.sh requires Bash")
	}

	root := repositoryRoot(t)
	temp := t.TempDir()
	fixturePath := filepath.Join(temp, "bake.json")
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(temp, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == "buildx bake --print runtime-base-amd64 runtime-base-arm64 materializer-amd64 materializer-arm64 frontend" ]] || {
  echo "unexpected docker command: $*" >&2
  exit 1
}
cat "$RELEASE_INPUTS_BAKE_FIXTURE"
`)
	writeExecutable(t, filepath.Join(bin, "go"), `#!/usr/bin/env bash
set -euo pipefail
[[ "$1 $2 $3" == "list -m -json" && $# == 4 ]] || {
  echo "unexpected go command: $*" >&2
  exit 1
}
module=$4
case "$module" in
  github.com/project-dalec/dalec) version=v0.21.5-0.20260728234020-5fa2c46d716b ;;
  github.com/moby/buildkit) version=v0.31.2 ;;
  *) echo "unexpected module: $module" >&2; exit 1 ;;
esac
if [[ "${RELEASE_INPUTS_REPLACED_MODULE:-}" == "$module" ]]; then
  printf '{"Path":"%s","Version":"%s","Replace":{"Path":"../local"}}\n' "$module" "$version"
else
  printf '{"Path":"%s","Version":"%s"}\n' "$module" "$version"
fi
`)

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "release-inputs.sh"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+temp,
		"TMPDIR="+temp,
		"LC_ALL=C",
		"BASH_ENV=",
		"ENV=",
		"GOFLAGS=",
		"GOWORK=off",
		"BUILDX_BAKE_FILE=",
		"BUILDX_BAKE_PATH_SEPARATOR=",
		"RELEASE_INPUTS_BAKE_FIXTURE="+fixturePath,
		"RELEASE_INPUTS_REPLACED_MODULE="+replacedModule,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return append([]byte(nil), stdout.Bytes()...), append([]byte(nil), stderr.Bytes()...), err
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}
