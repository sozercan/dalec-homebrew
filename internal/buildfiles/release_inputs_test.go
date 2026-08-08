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
		HomebrewArchiveSHA256  string            `json:"homebrew_archive_sha256"`
		VerificationKeysDigest string            `json:"verification_keys_digest"`
		DalecModule            string            `json:"dalec_module"`
		BuildKitModule         string            `json:"buildkit_module"`
		DalecFrontend          struct {
			SchemaVersion string `json:"schema_version"`
			Module        struct {
				Path    string `json:"path"`
				Version string `json:"version"`
			} `json:"module"`
			Route     string            `json:"route"`
			Index     string            `json:"index"`
			Platforms map[string]string `json:"platforms"`
		} `json:"dalec_frontend"`
	}
	if err := json.Unmarshal(stdout, &inputs); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if inputs.SchemaVersion != "dalec-homebrew-release-inputs/v2" {
		t.Fatalf("schema_version = %q", inputs.SchemaVersion)
	}
	if inputs.UbuntuBase["linux/amd64"] != testRuntimeBaseAMD64 || inputs.UbuntuBase["linux/arm64"] != testRuntimeBaseARM64 {
		t.Fatalf("ubuntu_base = %#v", inputs.UbuntuBase)
	}
	if inputs.HomebrewCommit != testHomebrewCommit {
		t.Fatalf("homebrew_commit = %q", inputs.HomebrewCommit)
	}
	if inputs.HomebrewArchiveSHA256 != testHomebrewArchiveSHA256 {
		t.Fatalf("homebrew_archive_sha256 = %q", inputs.HomebrewArchiveSHA256)
	}
	if inputs.VerificationKeysDigest != testVerificationKeysDigest {
		t.Fatalf("verification_keys_digest = %q", inputs.VerificationKeysDigest)
	}
	if inputs.DalecModule != testDalecVersion || inputs.BuildKitModule != "v0.31.2" {
		t.Fatalf("module versions = %q, %q", inputs.DalecModule, inputs.BuildKitModule)
	}
	if inputs.DalecFrontend.SchemaVersion != "dalec-homebrew-dalec-frontend/v1" ||
		inputs.DalecFrontend.Module.Path != "github.com/project-dalec/dalec" ||
		inputs.DalecFrontend.Module.Version != testDalecFrontendVersion ||
		inputs.DalecFrontend.Route != testDalecRoute ||
		inputs.DalecFrontend.Index != testDalecIndex ||
		inputs.DalecFrontend.Platforms["linux/amd64"] != testDalecAMD64 ||
		inputs.DalecFrontend.Platforms["linux/arm64"] != testDalecARM64 {
		t.Fatalf("dalec_frontend = %#v", inputs.DalecFrontend)
	}
}

func TestReleaseInputsRejectsUnsafeFixtures(t *testing.T) {
	setAMD64Base := func(f *bakeFixture, ref string) {
		f.Target["runtime-base-amd64"].Args["RUNTIME_BASE"] = ref
		f.Target["materializer-amd64"].Args["RUNTIME_BASE"] = ref
		f.Target["catalog-extractor-amd64"].Args["RUNTIME_BASE"] = ref
	}
	tests := []struct {
		name               string
		mutate             func(*bakeFixture)
		replacedModule     string
		dalecModuleVersion string
		want               string
	}{
		{
			name: "conflicting epochs",
			mutate: func(f *bakeFixture) {
				f.Target["materializer-arm64"].Args["SOURCE_DATE_EPOCH"] = "1781049601"
			},
			want: "release targets use conflicting SOURCE_DATE_EPOCH values",
		},
		{
			name: "missing bottle fetcher target",
			mutate: func(f *bakeFixture) {
				delete(f.Target, "bottle-fetcher-arm64")
			},
			want: "release bake target is missing: bottle-fetcher-arm64",
		},
		{
			name: "miswired catalog extractor target",
			mutate: func(f *bakeFixture) {
				f.Target["catalog-extractor-amd64"].Target = "frontend"
			},
			want: "catalog-extractor-amd64 must use context ., Dockerfile, target catalog-extractor",
		},
		{
			name: "frontend helper binding default",
			mutate: func(f *bakeFixture) {
				f.Target["frontend"].Args["BOTTLE_FETCHER_REF"] = "registry.example/fetcher@sha256:" + strings.Repeat("a", 64)
			},
			want: "default frontend BOTTLE_FETCHER_REF must be empty",
		},
		{
			name: "materializer policy binding default",
			mutate: func(f *bakeFixture) {
				f.Target["materializer-amd64"].Args["TAP_POLICY_DIGEST"] = "sha256:" + strings.Repeat("a", 64)
			},
			want: "default materializer-amd64 TAP_POLICY_DIGEST must be empty",
		},
		{
			name: "catalog extractor runtime base mismatch",
			mutate: func(f *bakeFixture) {
				f.Target["catalog-extractor-amd64"].Args["RUNTIME_BASE"] = "docker.io/library/ubuntu@sha256:" + strings.Repeat("c", 64)
			},
			want: "catalog-extractor-amd64 Ubuntu base differs from runtime-base-amd64",
		},
		{
			name: "mutable runtime base",
			mutate: func(f *bakeFixture) {
				setAMD64Base(f, "docker.io/library/ubuntu:24.04")
			},
			want: "runtime-base-amd64 must be a digest-pinned OCI reference",
		},
		{
			name: "runtime base with scheme",
			mutate: func(f *bakeFixture) {
				setAMD64Base(f, "https://"+testRuntimeBaseAMD64)
			},
			want: "runtime-base-amd64 must be a digest-pinned OCI reference",
		},
		{
			name: "malformed runtime base",
			mutate: func(f *bakeFixture) {
				setAMD64Base(f, "docker.io//ubuntu@sha256:"+strings.Repeat("a", 64))
			},
			want: "runtime-base-amd64 must be a digest-pinned OCI reference",
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
				f.Target["frontend"].Dockerfile = "Dockerfile.release"
			},
			want: "frontend must use context ., Dockerfile, target frontend",
		},
		{
			name: "unexpected staging tag",
			mutate: func(f *bakeFixture) {
				f.Target["frontend"].Tags = []string{"registry.example/other:latest"}
			},
			want: "frontend must use context ., Dockerfile, target frontend, platforms",
		},
		{
			name: "target output",
			mutate: func(f *bakeFixture) {
				f.Target["runtime-base-amd64"].Output = []string{"type=registry"}
			},
			want: "runtime-base-amd64 must not set output",
		},
		{
			name: "target cache exporter",
			mutate: func(f *bakeFixture) {
				f.Target["runtime-base-amd64"].CacheTo = []string{"type=registry,ref=registry.example/cache:latest"}
			},
			want: "runtime-base-amd64 must not set cache-to",
		},
		{
			name: "target secret",
			mutate: func(f *bakeFixture) {
				f.Target["runtime-base-arm64"].Secret = []string{"id=token"}
			},
			want: "runtime-base-arm64 must not set secret",
		},
		{
			name: "target ssh",
			mutate: func(f *bakeFixture) {
				f.Target["materializer-amd64"].SSH = []string{"default"}
			},
			want: "materializer-amd64 must not set ssh",
		},
		{
			name: "target network",
			mutate: func(f *bakeFixture) {
				f.Target["materializer-arm64"].Network = "host"
			},
			want: "materializer-arm64 must not set network",
		},
		{
			name: "target entitlements",
			mutate: func(f *bakeFixture) {
				f.Target["frontend"].Entitlements = []string{"network.host"}
			},
			want: "frontend must not set entitlements",
		},
		{
			name: "target attestations",
			mutate: func(f *bakeFixture) {
				f.Target["frontend"].Attest = []string{"type=provenance"}
			},
			want: "frontend must not set attest",
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
			dalecModuleVersion := tt.dalecModuleVersion
			if dalecModuleVersion == "" {
				dalecModuleVersion = testDalecVersion
			}
			stdout, stderr, err := runReleaseInputsWithDalecVersion(t, fixture, tt.replacedModule, dalecModuleVersion)
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
	testRegistry               = "registry.example/release"
	testVersion                = "v1.2.3-rc.1"
	testSourceDateEpoch        = "1781049600"
	testRuntimeBaseAMD64       = "docker.io/library/ubuntu@sha256:52df9b1ee71626e0088f7d400d5c6b5f7bb916f8f0c82b474289a4ece6cf3faf"
	testRuntimeBaseARM64       = "docker.io/library/ubuntu@sha256:7f622ca8766bccb22f04242ecb6f19f770b2f08827dc4b8c707de5e78a6da7ab"
	testHomebrewCommit         = "935053a12d38d62e59c467bf7f0f50dbc11cbcb6"
	testHomebrewArchiveSHA256  = "09eafcf099e344f5c1a4040992a2e1add3789e9b553b9141ab14df9f727f8c6b"
	testVerificationKeysDigest = "sha256:ef2d2c9e0219d485df9f07fff7b037feadc36c93085be9ffefb1390f31a3de1d"
	testDalecVersion           = "v0.21.5-0.20260728234020-5fa2c46d716b"
	testDalecFrontendVersion   = "v0.21.5"
	testDalecRoute             = "homebrew/image"
	testDalecIndex             = "ghcr.io/project-dalec/dalec/frontend@sha256:37f3a2ab5b7e65b3f8c5cb4e79f9f184f8d2b7e7d3f328041d7d22d160805c8c"
	testDalecAMD64             = "ghcr.io/project-dalec/dalec/frontend@sha256:4ce1cda772259b27a37a304ed9b30f3f06a3d776e1468afea4b05e8bdfa24d46"
	testDalecARM64             = "ghcr.io/project-dalec/dalec/frontend@sha256:ebb7d748011880b9bd6d430257831d3eb5e8ed1d1814ebeef1fcf182daff171e"
)

type bakeFixture struct {
	Target map[string]*bakeTarget `json:"target"`
}

type bakeTarget struct {
	Target       string            `json:"target"`
	Context      string            `json:"context"`
	Dockerfile   string            `json:"dockerfile"`
	Platforms    []string          `json:"platforms"`
	Args         map[string]string `json:"args"`
	Tags         []string          `json:"tags"`
	Output       any               `json:"output,omitempty"`
	CacheTo      any               `json:"cache-to,omitempty"`
	Secret       any               `json:"secret,omitempty"`
	SSH          any               `json:"ssh,omitempty"`
	Network      any               `json:"network,omitempty"`
	Entitlements any               `json:"entitlements,omitempty"`
	Attest       any               `json:"attest,omitempty"`
}

func validBakeFixture() bakeFixture {
	child := func(target, platform, runtimeBase, tag string) *bakeTarget {
		args := map[string]string{"SOURCE_DATE_EPOCH": testSourceDateEpoch}
		if runtimeBase != "" {
			args["RUNTIME_BASE"] = runtimeBase
		}
		return &bakeTarget{
			Target:     target,
			Context:    ".",
			Dockerfile: "Dockerfile",
			Platforms:  []string{platform},
			Args:       args,
			Tags:       []string{tag},
		}
	}
	return bakeFixture{Target: map[string]*bakeTarget{
		"runtime-base-amd64":      child("runtime-base", "linux/amd64", testRuntimeBaseAMD64, testRegistry+"/dalec-homebrew-runtime-base:"+testVersion+"-amd64"),
		"runtime-base-arm64":      child("runtime-base", "linux/arm64", testRuntimeBaseARM64, testRegistry+"/dalec-homebrew-runtime-base:"+testVersion+"-arm64"),
		"bottle-fetcher-amd64":    child("bottle-fetcher", "linux/amd64", "", testRegistry+"/dalec-homebrew-bottle-fetcher:"+testVersion+"-amd64"),
		"bottle-fetcher-arm64":    child("bottle-fetcher", "linux/arm64", "", testRegistry+"/dalec-homebrew-bottle-fetcher:"+testVersion+"-arm64"),
		"catalog-extractor-amd64": child("catalog-extractor", "linux/amd64", testRuntimeBaseAMD64, testRegistry+"/dalec-homebrew-catalog-extractor:"+testVersion+"-amd64"),
		"catalog-extractor-arm64": child("catalog-extractor", "linux/arm64", testRuntimeBaseARM64, testRegistry+"/dalec-homebrew-catalog-extractor:"+testVersion+"-arm64"),
		"materializer-amd64":      child("materializer", "linux/amd64", testRuntimeBaseAMD64, testRegistry+"/dalec-homebrew-materializer:"+testVersion+"-amd64"),
		"materializer-arm64":      child("materializer", "linux/arm64", testRuntimeBaseARM64, testRegistry+"/dalec-homebrew-materializer:"+testVersion+"-arm64"),
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
			Tags: []string{testRegistry + "/dalec-homebrew:" + testVersion},
		},
	}}
}

func runReleaseInputs(t *testing.T, fixture bakeFixture, replacedModule string) ([]byte, []byte, error) {
	t.Helper()
	return runReleaseInputsWithDalecVersion(t, fixture, replacedModule, testDalecVersion)
}

func runReleaseInputsWithDalecVersion(t *testing.T, fixture bakeFixture, replacedModule, dalecModuleVersion string) ([]byte, []byte, error) {
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

	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goCache := goEnv(t, realGo, "GOCACHE")
	goModCache := goEnv(t, realGo, "GOMODCACHE")

	bin := filepath.Join(temp, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
expected="buildx bake --print --var REGISTRY=$REGISTRY --var VERSION=$VERSION runtime-base-amd64 runtime-base-arm64 bottle-fetcher-amd64 bottle-fetcher-arm64 catalog-extractor-amd64 catalog-extractor-arm64 materializer-amd64 materializer-arm64 frontend"
[[ "$*" == "$expected" ]] || {
  echo "unexpected docker command: $*" >&2
  exit 1
}
cat "$RELEASE_INPUTS_BAKE_FIXTURE"
`)
	writeExecutable(t, filepath.Join(bin, "go"), `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2" == "run ./cmd/live-input-verify" ]]; then
  exec "$RELEASE_INPUTS_REAL_GO" "$@"
fi
[[ "$1 $2 $3" == "list -m -json" && $# == 4 ]] || {
  echo "unexpected go command: $*" >&2
  exit 1
}
module=$4
case "$module" in
  github.com/project-dalec/dalec) version=$RELEASE_INPUTS_DALEC_MODULE_VERSION ;;
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
		"GOCACHE="+goCache,
		"GOMODCACHE="+goModCache,
		"BUILDX_BAKE_FILE=",
		"BUILDX_BAKE_PATH_SEPARATOR=",
		"REGISTRY="+testRegistry,
		"VERSION="+testVersion,
		"RELEASE_INPUTS_BAKE_FIXTURE="+fixturePath,
		"RELEASE_INPUTS_REAL_GO="+realGo,
		"RELEASE_INPUTS_REPLACED_MODULE="+replacedModule,
		"RELEASE_INPUTS_DALEC_MODULE_VERSION="+dalecModuleVersion,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return append([]byte(nil), stdout.Bytes()...), append([]byte(nil), stderr.Bytes()...), err
}

func goEnv(t *testing.T, goBinary, name string) string {
	t.Helper()
	cmd := exec.Command(goBinary, "env", name)
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env %s: %v", name, err)
	}
	return strings.TrimSpace(string(output))
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}
