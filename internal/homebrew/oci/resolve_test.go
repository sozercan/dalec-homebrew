package oci

import (
	"context"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestResolveTargetSpecificLinuxPlatforms(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		architecture string
		bottleTag    string
	}{
		{name: "amd64", architecture: "amd64", bottleTag: BottleTagX8664Linux},
		{name: "arm64", architecture: "arm64", bottleTag: BottleTagARM64Linux},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target := ocispec.Platform{OS: "linux", Architecture: test.architecture}
			fixture := newRegistryFixture(t, registryFixtureOptions{bottleTag: test.bottleTag, target: target})
			result, err := fixture.client.Resolve(context.Background(), fixture.formula, target)
			if err != nil {
				t.Fatal(err)
			}
			if result.SelectedBottleTag != test.bottleTag {
				t.Fatalf("selected tag = %q, want %q", result.SelectedBottleTag, test.bottleTag)
			}
			if result.Manifest.Platform == nil || result.Manifest.Platform.Architecture != test.architecture || result.Manifest.Platform.OS != "linux" {
				t.Fatalf("selected platform = %#v", result.Manifest.Platform)
			}
		})
	}
}

func TestResolveRejectsWrongPlatform(t *testing.T) {
	t.Parallel()

	wrong := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	fixture := newRegistryFixture(t, registryFixtureOptions{descriptorPlatform: &wrong})
	_, err := fixture.client.Resolve(context.Background(), fixture.formula, fixture.target)
	if err == nil || !strings.Contains(err.Error(), "does not match target") {
		t.Fatalf("expected wrong-platform error, got %v", err)
	}
}

func TestResolveRejectsMutatedDescriptor(t *testing.T) {
	t.Parallel()

	fixture := newRegistryFixture(t, registryFixtureOptions{mutateManifest: true})
	_, err := fixture.client.Resolve(context.Background(), fixture.formula, fixture.target)
	if err == nil || !strings.Contains(err.Error(), "does not match descriptor digest") {
		t.Fatalf("expected mutated-descriptor error, got %v", err)
	}
}

func TestResolveRejectsMalformedTab(t *testing.T) {
	t.Parallel()

	fixture := newRegistryFixture(t, registryFixtureOptions{tab: `{"homebrew_version":`})
	_, err := fixture.client.Resolve(context.Background(), fixture.formula, fixture.target)
	if err == nil || !strings.Contains(err.Error(), "decode sh.brew.tab") {
		t.Fatalf("expected malformed-tab error, got %v", err)
	}
}

func TestResolveUsesAllFallbackAndConvertsToResolutionTypes(t *testing.T) {
	t.Parallel()

	fixture := newRegistryFixture(t, registryFixtureOptions{
		bottleTag:      BottleTagAll,
		configPlatform: ocispec.Platform{OS: "darwin", Architecture: "arm64"},
	})
	result, err := fixture.client.Resolve(context.Background(), fixture.formula, fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	if result.SelectedBottleTag != BottleTagAll {
		t.Fatalf("selected tag = %q", result.SelectedBottleTag)
	}
	if result.Manifest.Platform != nil {
		t.Fatalf("all manifest platform = %#v", result.Manifest.Platform)
	}
	if result.Filename != "fixture--1.2.3_1.all.bottle.3.tar.gz" {
		t.Fatalf("filename = %q", result.Filename)
	}
	if fixture.tokenRequests.Load() != 1 {
		t.Fatalf("token requests = %d, want 1", fixture.tokenRequests.Load())
	}
	if fixture.challengeCount.Load() != 1 {
		t.Fatalf("authentication challenges = %d, want 1", fixture.challengeCount.Load())
	}

	bottle := result.ResolutionBottle()
	if bottle.Repository != "ghcr.io/homebrew/core/fixture" {
		t.Fatalf("resolution repository = %q", bottle.Repository)
	}
	if bottle.Layer.Digest != "sha256:"+result.HomebrewSHA256 {
		t.Fatalf("layer digest = %q", bottle.Layer.Digest)
	}
	node := result.ResolutionNode()
	if node.Name != "fixture" || node.FullName != "homebrew/core/fixture" || node.PkgVersion != "1.2.3_1" {
		t.Fatalf("unexpected node identity: %#v", node)
	}
	if len(node.Dependencies) != 1 || node.Dependencies[0].Name != "dep" || node.Dependencies[0].Minimum != "1.2_1" {
		t.Fatalf("unexpected dependencies: %#v", node.Dependencies)
	}
	if len(node.ExecutablePaths) != 2 || node.ExecutablePaths[0] != "bin/fixture" {
		t.Fatalf("unexpected executable paths: %#v", node.ExecutablePaths)
	}
}

func TestResolveEscapedFormulaNameThroughRegistryFixture(t *testing.T) {
	t.Parallel()

	fixture := newRegistryFixture(t, registryFixtureOptions{formulaName: "tool@1+preview"})
	result, err := fixture.client.Resolve(context.Background(), fixture.formula, fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reference.Repository != "homebrew/core/tool/1xpreview" {
		t.Fatalf("repository = %q", result.Reference.Repository)
	}
	if result.Filename != "tool@1+preview--1.2.3_1.x86_64_linux.bottle.3.tar.gz" {
		t.Fatalf("filename = %q", result.Filename)
	}
}
