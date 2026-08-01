package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/release"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestRunWritesCanonicalManifestToStdout(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run(validArgs(), &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	want := canonicalManifest(t)
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("stdout mismatch\n got: %s\nwant: %s", stdout.Bytes(), want)
	}

	manifest, err := release.Decode(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("decode generated manifest: %v", err)
	}
	for name, component := range map[string]release.Component{
		"frontend":     manifest.Frontend,
		"runtime base": manifest.RuntimeBase,
		"materializer": manifest.Materializer,
	} {
		if got := component.Platforms[0].Platform.Architecture; got != "amd64" {
			t.Errorf("%s first platform = %q, want amd64", name, got)
		}
		if got := component.Platforms[1].Platform.Architecture; got != "arm64" {
			t.Errorf("%s second platform = %q, want arm64", name, got)
		}
	}
}

func TestRunWritesCanonicalManifestToFile(t *testing.T) {
	output := filepath.Join(t.TempDir(), "components.json")
	args := append(validArgs(), "--output", output)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := canonicalManifest(t)
	if !bytes.Equal(got, want) {
		t.Fatalf("file mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestRunRequiresEveryManifestInput(t *testing.T) {
	for _, name := range []string{
		"frontend-index",
		"frontend-amd64",
		"frontend-arm64",
		"runtime-base-index",
		"runtime-base-amd64",
		"runtime-base-arm64",
		"materializer-index",
		"materializer-amd64",
		"materializer-arm64",
		"homebrew-commit",
		"portable-ruby-version",
		"verification-keys-digest",
		"dalec-module",
		"buildkit-module",
	} {
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := run(withoutFlag(validArgs(), "--"+name), &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "--"+name+" is required") {
				t.Fatalf("err = %v", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
}

func TestRunCanonicalizesBeforeOpeningOutput(t *testing.T) {
	output := filepath.Join(t.TempDir(), "components.json")
	const sentinel = "existing release manifest"
	if err := os.WriteFile(output, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	args := setFlag(validArgs(), "--materializer-arm64", pinned("ghcr.io/example/other", '9'))
	args = append(args, "--output", output)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "materializer linux/arm64 child uses a different repository") {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	got, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != sentinel {
		t.Fatalf("invalid manifest changed output to %q", got)
	}
}

func validArgs() []string {
	return []string{
		"--frontend-index", pinned("ghcr.io/example/frontend", '1'),
		"--frontend-amd64", pinned("ghcr.io/example/frontend", '2'),
		"--frontend-arm64", pinned("ghcr.io/example/frontend", '3'),
		"--runtime-base-index", pinned("ghcr.io/example/runtime-base", '4'),
		"--runtime-base-amd64", pinned("ghcr.io/example/runtime-base", '5'),
		"--runtime-base-arm64", pinned("ghcr.io/example/runtime-base", '6'),
		"--materializer-index", pinned("ghcr.io/example/materializer", '7'),
		"--materializer-amd64", pinned("ghcr.io/example/materializer", '8'),
		"--materializer-arm64", pinned("ghcr.io/example/materializer", '9'),
		"--homebrew-commit", strings.Repeat("b", 40),
		"--portable-ruby-version", "4.0.6",
		"--verification-keys-digest", "sha256:" + strings.Repeat("a", 64),
		"--dalec-module", "v0.21.5",
		"--buildkit-module", "v0.31.2",
	}
}

func canonicalManifest(t *testing.T) []byte {
	t.Helper()
	manifest := &release.Manifest{
		SchemaVersion:          release.SchemaVersion,
		PolicyVersion:          resolution.PolicyVersion,
		Frontend:               testComponent("ghcr.io/example/frontend", '1', '2', '3'),
		RuntimeBase:            testComponent("ghcr.io/example/runtime-base", '4', '5', '6'),
		Materializer:           testComponent("ghcr.io/example/materializer", '7', '8', '9'),
		HomebrewCommit:         strings.Repeat("b", 40),
		PortableRubyVersion:    "4.0.6",
		VerificationKeysDigest: "sha256:" + strings.Repeat("a", 64),
		DalecModule:            "v0.21.5",
		BuildKitModule:         "v0.31.2",
	}
	data, err := release.Canonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func testComponent(repo string, index, amd64, arm64 rune) release.Component {
	return release.Component{
		Index: pinned(repo, index),
		Platforms: []release.PlatformRef{
			{Platform: resolution.Platform{OS: "linux", Architecture: "amd64"}, Ref: pinned(repo, amd64)},
			{Platform: resolution.Platform{OS: "linux", Architecture: "arm64"}, Ref: pinned(repo, arm64)},
		},
	}
}

func pinned(repo string, fill rune) string {
	return repo + "@sha256:" + strings.Repeat(string(fill), 64)
}

func withoutFlag(args []string, name string) []string {
	for i := 0; i < len(args); i += 2 {
		if args[i] == name {
			return append(append([]string(nil), args[:i]...), args[i+2:]...)
		}
	}
	return append([]string(nil), args...)
}

func setFlag(args []string, name, value string) []string {
	args = append([]string(nil), args...)
	for i := 0; i < len(args); i += 2 {
		if args[i] == name {
			args[i+1] = value
			return args
		}
	}
	return args
}
