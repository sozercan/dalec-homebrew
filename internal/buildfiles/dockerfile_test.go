package buildfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	// The go command runs tests from the package directory. Avoid runtime.Caller:
	// -trimpath rewrites caller filenames to module paths, not filesystem paths.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func dockerfilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), "Dockerfile")
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

func TestMaterializerIsFlattenedAfterAddingHelpers(t *testing.T) {
	data := dockerfileText(t)
	rootfs := dockerfileStage(t, data, "FROM ${RUNTIME_BASE} AS materializer-rootfs")
	for _, want := range []string{
		"COPY --chmod=0555 --from=helper-build /out/dalec-homebrew-materializer",
		"COPY --chmod=0555 --from=helper-build /out/dalec-homebrew-test-runner",
		"COPY --chmod=0444 internal/materializer/pour.rb",
		"find / -xdev",
		"materializer-hardlinks.tsv",
	} {
		if !strings.Contains(rootfs, want) {
			t.Fatalf("materializer-rootfs stage is missing %q", want)
		}
	}
	stage := dockerfileStage(t, data, "FROM scratch AS materializer")
	if !strings.Contains(stage, "COPY --from=materializer-rootfs / /") {
		t.Fatal("materializer must flatten its completed rootfs")
	}
	if got := strings.Count(stage, "\nCOPY "); got != 1 {
		t.Fatalf("materializer stage has %d COPY instructions, want 1", got)
	}
	for _, forbidden := range []string{"dalec-homebrew-record-verify", "homebrew-1.pub", "dalec-homebrew-snapshot-proxy", "dalec-homebrew-runtime-base-evidence"} {
		if strings.Contains(stage, forbidden) {
			t.Fatalf("materializer stage unexpectedly contains %q", forbidden)
		}
	}
}

func TestRuntimeBaseIsPinnedChiseledNoble(t *testing.T) {
	data := dockerfileText(t)
	stage := dockerfileStage(t, data, "FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS runtime-base-rootfs")
	if strings.Contains(stage, "apt-get") {
		t.Fatal("runtime-base-rootfs must not run apt")
	}
	for _, want := range []string{
		"CHISEL_VERSION", "CHISEL_RELEASES_COMMIT", "CHISEL_RELEASES_SHA256",
		"CHISEL_AMD64_SHA256", "CHISEL_ARM64_SHA256", "snapshot-proxy",
		"chisel cut --release /tmp/chisel-release", "base-files_release-info",
		"libc6_gconv", "perl-base_modules", "gzip_scripts", "hostname_bins", "ncurses-bin_bins",
		"type=tmpfs,target=/root/.cache/chisel", "runtime-base-artifacts.tsv",
		"runtime-base-packages.tsv", "runtime-base-chisel.manifest.wall", "dalec-homebrew-runtime-base-evidence",
		"test ! -e /rootfs/usr/bin/apt", "test ! -e /rootfs/var/lib/dpkg/status",
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("runtime-base-rootfs is missing %q", want)
		}
	}
	finalStage := dockerfileStage(t, data, "FROM scratch AS runtime-base")
	if got := strings.Count(finalStage, "\nCOPY "); got != 1 {
		t.Fatalf("runtime-base stage has %d COPY instructions, want 1", got)
	}
	if !strings.Contains(finalStage, "COPY --from=runtime-base-rootfs /rootfs/ /") {
		t.Fatal("runtime-base must copy only the completed Chisel rootfs")
	}
}

func TestMaterializerUsesFullUbuntuButChiselEvidence(t *testing.T) {
	stage := dockerfileStage(t, dockerfileText(t), "FROM ${RUNTIME_BASE} AS materializer-rootfs")
	for _, want := range []string{
		"--mount=from=runtime-base-rootfs,source=/rootfs,target=/run/runtime-base,ro",
		"/run/runtime-base/usr/share/dalec-homebrew/runtime-base-packages.tsv",
		"path-include=/usr/share/doc/*/copyright", "apt-get install -y --no-install-recommends",
		"HOMEBREW_SYSTEM_ENV_TAKES_PRIORITY=1", "HOMEBREW_BASH_COMMAND=",
		"runtime_loader=/lib64/ld-linux-x86-64.so.2", "runtime_loader=/lib/ld-linux-aarch64.so.1",
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("materializer stage is missing %q", want)
		}
	}
}
