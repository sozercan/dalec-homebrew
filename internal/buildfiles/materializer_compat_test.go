package buildfiles

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func materializerCompatPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), "scripts", "materializer-compat.sh")
}

func TestMaterializerCompatibilityProbeContract(t *testing.T) {
	path := materializerCompatPath(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("materializer compatibility probe is not executable")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, want := range []string{
		"2fe24afa0fd4034a66340b61fbe952ec7628d5eb30358191730c9ba2b6a1d421",
		"01a46dbd217ab6379da50f9285da81ebaf0928c3457fbb04542b745bc80cb27f",
		"b97b976075f287a235262dd8d0b8ca3eecb6a6c6ddeff3d5ebb7c3123b20bc56",
		"go run ./cmd/resolve",
		"verify-bottle",
		`[[ "$HOMEBREW_SHA256" == "${DIGEST#sha256:}" ]]`,
		"--network none",
		"--user linuxbrew",
		"--cap-drop ALL",
		"readonly",
		"dalec-homebrew-pour.rb",
		"lib/locale/locale-archive",
		"built_as_bottle",
		"poured_from_bottle",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("materializer compatibility probe is missing %q", want)
		}
	}
	for _, forbidden := range []string{"raw.githubusercontent.com", "formulae.brew.sh/api/formula/", "--network host", "tar -x"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("materializer compatibility probe contains untrusted or unsafe input %q", forbidden)
		}
	}
}

func TestMaterializerCompatibilityFixtureRecords(t *testing.T) {
	tests := []struct {
		arch     string
		filename string
		digest   string
		size     int64
	}{
		{
			arch:     "amd64",
			filename: "glibc--2.39_1.x86_64_linux.bottle.2.tar.gz",
			digest:   "sha256:2fe24afa0fd4034a66340b61fbe952ec7628d5eb30358191730c9ba2b6a1d421",
			size:     14716331,
		},
		{
			arch:     "arm64",
			filename: "glibc--2.39_1.arm64_linux.bottle.2.tar.gz",
			digest:   "sha256:01a46dbd217ab6379da50f9285da81ebaf0928c3457fbb04542b745bc80cb27f",
			size:     13869188,
		},
	}
	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			path := filepath.Join(repositoryRoot(t), "scripts", "testdata", "materializer-compat", "glibc-2.39_1-"+tt.arch+"-resolution.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			record, err := resolution.Decode(data)
			if err != nil {
				t.Fatalf("decode compatibility fixture: %v", err)
			}
			if record.Input.Platform.OS != "linux" || record.Input.Platform.Architecture != tt.arch {
				t.Fatalf("fixture platform = %s/%s", record.Input.Platform.OS, record.Input.Platform.Architecture)
			}
			if len(record.InstallOrder) != 2 || record.InstallOrder[0] != "linux-headers@6.8" || record.InstallOrder[1] != "glibc" {
				t.Fatalf("fixture install order = %v", record.InstallOrder)
			}
			var glibc *resolution.Node
			for i := range record.Nodes {
				if record.Nodes[i].Name == "glibc" {
					glibc = &record.Nodes[i]
					break
				}
			}
			if glibc == nil {
				t.Fatal("fixture has no glibc node")
			}
			if glibc.PkgVersion != "2.39_1" || glibc.Bottle.Filename != tt.filename || glibc.Bottle.Layer.Digest != tt.digest || glibc.Bottle.Layer.Size != tt.size {
				t.Fatalf("fixture glibc identity = version %q, filename %q, digest %q, size %d", glibc.PkgVersion, glibc.Bottle.Filename, glibc.Bottle.Layer.Digest, glibc.Bottle.Layer.Size)
			}
			if glibc.Bottle.HomebrewSHA256 != strings.TrimPrefix(tt.digest, "sha256:") || glibc.UpstreamFormulaID != "homebrew/core/glibc" {
				t.Fatalf("fixture glibc trust binding = checksum %q, Formula %q", glibc.Bottle.HomebrewSHA256, glibc.UpstreamFormulaID)
			}
		})
	}
}

func TestMaterializerCompatibilityProbeRejectsUnsupportedPlatform(t *testing.T) {
	cmd := exec.Command("bash", materializerCompatPath(t), "--image", "example.invalid/materializer:test", "--platform", "linux/s390x")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("unsupported platform accepted\n%s", output)
	}
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 64 {
		t.Fatalf("unsupported platform exit = %v, want 64\n%s", err, output)
	}
	if !strings.Contains(string(output), "unsupported --platform") {
		t.Fatalf("unexpected error: %s", output)
	}
}

func TestMaterializerCompatibilityProbeIsWiredBeforeReleaseIntegration(t *testing.T) {
	ci := workflowYAML(t, "ci.yml")
	qemuWith := yamlMappingValue(t,
		workflowStepByName(t, ci, "dockerfile", "Set up QEMU"), "with")
	if got := yamlScalarValue(t, yamlMappingValue(t, qemuWith, "platforms")); got != "arm64" {
		t.Fatalf("CI QEMU platforms = %q, want arm64", got)
	}

	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		arch := strings.TrimPrefix(platform, "linux/")
		buildStep := workflowStepByName(t, ci, "dockerfile", "Build "+arch+" materializer compatibility image")
		with := yamlMappingValue(t, buildStep, "with")
		if got := yamlScalarValue(t, yamlMappingValue(t, with, "target")); got != "materializer" {
			t.Fatalf("CI %s compatibility target = %q, want materializer", arch, got)
		}
		if got := yamlScalarValue(t, yamlMappingValue(t, with, "platforms")); got != platform {
			t.Fatalf("CI %s compatibility platform = %q, want %s", arch, got, platform)
		}
		if got := yamlScalarValue(t, yamlMappingValue(t, with, "load")); got != "true" {
			t.Fatalf("CI %s compatibility load = %q, want true", arch, got)
		}

		ciProbe := yamlScalarValue(t, yamlMappingValue(t,
			workflowStepByName(t, ci, "dockerfile", "Check "+arch+" pinned Homebrew compatibility"), "run"))
		if strings.Count(ciProbe, "./scripts/materializer-compat.sh") != 2 || strings.Count(ciProbe, "--current") != 1 {
			t.Fatalf("CI must run exactly one fixed and one current %s materializer compatibility probe:\n%s", arch, ciProbe)
		}
	}

	release := workflowYAML(t, "release.yml")
	releaseBuild := yamlScalarValue(t, yamlMappingValue(t,
		workflowStepByName(t, release, "build", "Build children and assemble component indexes"), "run"))
	for _, want := range []string{
		`materializer_ref="$materializer_repo@${!materializer_var}"`,
		`--image "$materializer_ref"`,
		`--platform "$platform"`,
		"--current",
	} {
		if !strings.Contains(releaseBuild, want) {
			t.Fatalf("release build does not run compatibility probe with %q", want)
		}
	}
	if strings.Count(releaseBuild, "./scripts/materializer-compat.sh") != 2 || strings.Count(releaseBuild, "--current") != 1 {
		t.Fatal("release build must run exactly one fixed and one current compatibility probe")
	}
}
