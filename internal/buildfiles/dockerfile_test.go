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
	start := strings.Index(data, marker+"\n")
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
	for _, forbidden := range []string{"FRONTEND_TEST_CA_CERTIFICATE_BASE64", "frontend-test-ca-bundle", "/out/test-ca.crt"} {
		if strings.Contains(stage, forbidden) {
			t.Fatalf("production frontend stage unexpectedly contains test-only CA input %q", forbidden)
		}
	}
}

func TestFrontendTestCATargetReusesProductionBinaryAndIsolatesTrustBundle(t *testing.T) {
	data := dockerfileText(t)
	stage := dockerfileStage(t, data, "FROM scratch AS frontend-test-ca")
	for _, want := range []string{
		"COPY --from=frontend-test-ca-bundle /out/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
		"COPY --from=frontend-build /out/dalec-homebrew-frontend /dalec-homebrew-frontend",
		`moby.buildkit.frontend.caps="moby.buildkit.frontend.inputs"`,
		`ENTRYPOINT ["/dalec-homebrew-frontend"]`,
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("test-only frontend stage is missing %q", want)
		}
	}
	if got := strings.Count(stage, "\nCOPY "); got != 2 {
		t.Fatalf("test-only frontend stage has %d COPY instructions, want 2", got)
	}
	if strings.Contains(stage, "ARG FRONTEND_TEST_CA_CERTIFICATE_BASE64") {
		t.Fatal("test-only CA input must be consumed by the bundle stage, not retained in the final frontend stage")
	}
	lastStage := data[strings.LastIndex(data, "\nFROM ")+1:]
	if !strings.HasPrefix(lastStage, "FROM scratch AS frontend\n") {
		t.Fatal("production frontend must remain the Dockerfile's default final target")
	}
}

func TestFrontendTestCABundleRequiresNonEmptyBase64Certificate(t *testing.T) {
	stage := dockerfileStage(t, dockerfileText(t), "FROM ca-bundle AS frontend-test-ca-bundle")
	for _, want := range []string{
		"ARG FRONTEND_TEST_CA_CERTIFICATE_BASE64",
		`test -n "${FRONTEND_TEST_CA_CERTIFICATE_BASE64}"`,
		`printf '%s' "${FRONTEND_TEST_CA_CERTIFICATE_BASE64}" | base64 --decode > /out/test-ca.crt`,
		"test -s /out/test-ca.crt",
		"cat /etc/ssl/certs/ca-certificates.crt > /out/ca-certificates.crt",
		"cat /out/test-ca.crt >> /out/ca-certificates.crt",
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("test-only CA bundle stage is missing validation or append step %q", want)
		}
	}
	if strings.Contains(stage, "ARG FRONTEND_TEST_CA_CERTIFICATE_BASE64=") {
		t.Fatal("test-only CA certificate build argument must not have a default value")
	}
	requireBefore := func(first, second string) {
		t.Helper()
		firstIndex := strings.Index(stage, first)
		secondIndex := strings.Index(stage, second)
		if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
			t.Fatalf("test-only CA bundle stage must place %q before %q", first, second)
		}
	}
	requireBefore(`test -n "${FRONTEND_TEST_CA_CERTIFICATE_BASE64}"`, "base64 --decode")
	requireBefore("base64 --decode", "test -s /out/test-ca.crt")
	requireBefore("test -s /out/test-ca.crt", "cat /out/test-ca.crt >> /out/ca-certificates.crt")
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

func TestV2ComponentImagesAreExplicitAndMinimal(t *testing.T) {
	data := dockerfileText(t)
	helper := dockerfileStage(t, data, "FROM go-source AS helper-build")
	if !strings.Contains(helper, "internal/catalogextractor.PinnedHomebrewCommit=${HOMEBREW_COMMIT}") {
		t.Fatal("catalog extractor binary is not bound to the release Homebrew commit")
	}
	for _, binding := range []string{
		"internal/config.MaterializerV2BindingsRequired=1",
		"internal/config.IngestionJWSKeyPolicyDigest=${INGESTION_JWS_KEY_POLICY_DIGEST}",
		"internal/config.TapPolicyDigest=${TAP_POLICY_DIGEST}",
		"internal/config.ExecutableRuntimePolicyDigest=${EXECUTABLE_RUNTIME_POLICY_DIGEST}",
		"internal/config.SupportedCatalogPolicyVersions=${SUPPORTED_CATALOG_POLICY_VERSIONS}",
		"internal/config.SupportedFetchPolicyVersions=${SUPPORTED_FETCH_POLICY_VERSIONS}",
		"internal/config.SupportedProvenancePolicyVersions=${SUPPORTED_PROVENANCE_POLICY_VERSIONS}",
	} {
		if !strings.Contains(helper, binding) {
			t.Fatalf("materializer build is missing release policy binding %q", binding)
		}
	}
	fetcher := dockerfileStage(t, data, "FROM scratch AS bottle-fetcher")
	for _, want := range []string{
		"COPY --from=ca-bundle /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
		"COPY --from=helper-build /out/dalec-homebrew-bottle-fetcher /dalec-homebrew-bottle-fetcher",
		`ENTRYPOINT ["/dalec-homebrew-bottle-fetcher"]`,
	} {
		if !strings.Contains(fetcher, want) {
			t.Fatalf("bottle-fetcher stage is missing %q", want)
		}
	}
	if got := strings.Count(fetcher, "\nCOPY "); got != 2 {
		t.Fatalf("bottle-fetcher stage has %d COPY instructions, want 2", got)
	}

	service := dockerfileStage(t, data, "FROM scratch AS catalog-service")
	for _, want := range []string{
		"COPY --from=ca-bundle /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
		"COPY --from=helper-build /out/dalec-homebrew-catalog-service /dalec-homebrew-catalog-service",
		"COPY --from=helper-build --chown=65532:65532 /out/catalog-service-rootfs/ /",
		`ENTRYPOINT ["/dalec-homebrew-catalog-service"]`,
	} {
		if !strings.Contains(service, want) {
			t.Fatalf("catalog-service stage is missing %q", want)
		}
	}
	if got := strings.Count(service, "\nCOPY "); got != 3 {
		t.Fatalf("catalog-service stage has %d COPY instructions, want 3", got)
	}

	extractorRoot := dockerfileStage(t, data, "FROM materializer-rootfs AS catalog-extractor-rootfs")
	if !strings.Contains(extractorRoot, "COPY --chmod=0444 internal/catalogextractor/extract.rb /usr/local/libexec/dalec-homebrew-catalog-extract.rb") {
		t.Fatal("catalog-extractor rootfs is missing the pinned Ruby extraction adapter")
	}
	extractor := dockerfileStage(t, data, "FROM scratch AS catalog-extractor")
	if !strings.Contains(extractor, "COPY --from=catalog-extractor-rootfs / /") || !strings.Contains(extractor, `ENTRYPOINT ["/usr/local/bin/dalec-homebrew-catalog-extractor"]`) || !strings.Contains(extractor, "HOMEBREW_REQUIRE_TAP_TRUST=1") {
		t.Fatal("catalog-extractor stage does not bind the isolated Homebrew/Ruby extractor rootfs")
	}
}

func TestFrontendBuildBindsCompleteNonCoreTuple(t *testing.T) {
	stage := dockerfileStage(t, dockerfileText(t), "FROM go-source AS frontend-build")
	for _, want := range []string{
		"internal/config.BottleFetcherRef=${BOTTLE_FETCHER_REF}",
		"internal/config.CatalogServiceOrigin=${CATALOG_SERVICE_ORIGIN}",
		"internal/config.IngestionJWSKeyPolicyDigest=${INGESTION_JWS_KEY_POLICY_DIGEST}",
		"internal/config.IngestionJWSKeyPolicyBase64=${INGESTION_JWS_KEY_POLICY_BASE64}",
		"internal/config.TapPolicyDigest=${TAP_POLICY_DIGEST}",
		"internal/config.ExecutableRuntimePolicyDigest=${EXECUTABLE_RUNTIME_POLICY_DIGEST}",
		"internal/config.SupportedCatalogPolicyVersions=${SUPPORTED_CATALOG_POLICY_VERSIONS}",
		"internal/config.SupportedFetchPolicyVersions=${SUPPORTED_FETCH_POLICY_VERSIONS}",
		"internal/config.SupportedProvenancePolicyVersions=${SUPPORTED_PROVENANCE_POLICY_VERSIONS}",
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("frontend build stage is missing %q", want)
		}
	}
}
