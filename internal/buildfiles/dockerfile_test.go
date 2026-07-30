package buildfiles

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

func dockerfilePath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../..", "Dockerfile"))
}

func dockerfileText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(dockerfilePath(t))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func dockerfileStage(t *testing.T, data, marker string) string {
	t.Helper()
	start := strings.Index(data, marker)
	if start < 0 {
		t.Fatalf("Dockerfile stage marker %q is absent", marker)
	}
	stage := data[start+len(marker):]
	if next := strings.Index(stage, "\nFROM "); next >= 0 {
		stage = stage[:next]
	}
	return stage
}

func TestDockerfileParses(t *testing.T) {
	f, err := os.Open(dockerfilePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := parser.Parse(f); err != nil {
		t.Fatal(err)
	}
}

func TestFrontendImageContainsOnlyGatewayAndCABundle(t *testing.T) {
	stage := dockerfileStage(t, dockerfileText(t), "FROM scratch AS frontend")
	for _, want := range []string{
		"COPY --from=ca-bundle /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
		"COPY --from=frontend-build /out/dalec-homebrew-frontend /dalec-homebrew-frontend",
		`moby.buildkit.frontend.caps="moby.buildkit.frontend.inputs"`,
		`ENTRYPOINT ["/dalec-homebrew-frontend"]`,
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("frontend stage is missing %q", want)
		}
	}
	if got := strings.Count(stage, "\nCOPY "); got != 2 {
		t.Fatalf("frontend stage has %d COPY instructions, want 2", got)
	}
	for _, forbidden := range []string{"test-runner", "materializer", "record-verify", "release-verify", "homebrew-1.pub"} {
		if strings.Contains(stage, forbidden) {
			t.Fatalf("frontend stage unexpectedly contains %q", forbidden)
		}
	}
}

func TestMaterializerAvoidsRedundantHelperLayers(t *testing.T) {
	stage := dockerfileStage(t, dockerfileText(t), "FROM runtime-base AS materializer")
	for _, want := range []string{
		"install -d -o root -g root -m 0755 /usr/local/libexec",
		"COPY --chmod=0555 --from=helper-build /out/dalec-homebrew-materializer",
		"COPY --chmod=0555 --from=helper-build /out/dalec-homebrew-test-runner",
		"COPY --chmod=0444 internal/materializer/pour.rb",
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("materializer stage is missing %q", want)
		}
	}
	for _, forbidden := range []string{"dalec-homebrew-record-verify", "homebrew-1.pub", "RUN chmod 0555", "RUN chmod 0444"} {
		if strings.Contains(stage, forbidden) {
			t.Fatalf("materializer stage unexpectedly contains %q", forbidden)
		}
	}
}

func TestRuntimeBaseDoesNotInstallRedundantUbuntuPackages(t *testing.T) {
	stage := dockerfileStage(t, dockerfileText(t), "FROM ${RUNTIME_BASE} AS runtime-base-rootfs")
	if strings.Contains(stage, "apt-get") {
		t.Fatal("runtime-base-rootfs must not run apt; the pinned CA runtime files are copied from ca-bundle")
	}
	for _, want := range []string{
		"runtime-base-artifacts.tsv",
		"runtime-base-packages.tsv",
		"path-include=/usr/share/doc/*/copyright",
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("runtime-base-rootfs is missing %q", want)
		}
	}
}
