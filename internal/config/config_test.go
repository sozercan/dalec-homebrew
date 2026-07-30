package config

import (
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

func TestRequiresPinnedComponents(t *testing.T) {
	_, err := FromBuildOpts(map[string]string{})
	if err == nil {
		t.Fatal("expected missing component error")
	}
}

func TestFromBuildOpts(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg, err := FromBuildOpts(map[string]string{
		"build-arg:DALEC_HOMEBREW_RUNTIME_BASE": "example/base@" + d,
		"build-arg:DALEC_HOMEBREW_MATERIALIZER": "example/materializer@" + d,
		"build-arg:DALEC_HOMEBREW_FRONTEND_REF": "example/frontend@" + d,
		"build-arg:DALEC_HOMEBREW_COMMIT":       "deadbeef",
		"build-arg:DALEC_HOMEBREW_KEYS_DIGEST":  metadata.DefaultKeySetDigest(),
		"build-arg:DALEC_SKIP_TESTS":            "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SkipTests {
		t.Fatal("skip tests not parsed")
	}
}

func TestRejectsMismatchedVerificationKeyDigest(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err := FromBuildOpts(map[string]string{
		"build-arg:DALEC_HOMEBREW_RUNTIME_BASE": "example/base@" + d,
		"build-arg:DALEC_HOMEBREW_MATERIALIZER": "example/materializer@" + d,
		"build-arg:DALEC_HOMEBREW_COMMIT":       "deadbeef",
		"build-arg:DALEC_HOMEBREW_KEYS_DIGEST":  d,
	})
	if err == nil {
		t.Fatal("expected embedded key digest mismatch")
	}
}

func TestReleaseBoundFrontendCannotSkipTests(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldBase, oldMat, oldCommit, oldKeys := RuntimeBaseRef, MaterializerRef, HomebrewCommit, VerificationKeysDigest
	defer func() {
		RuntimeBaseRef, MaterializerRef, HomebrewCommit, VerificationKeysDigest = oldBase, oldMat, oldCommit, oldKeys
	}()
	RuntimeBaseRef = "example/base@" + d
	MaterializerRef = "example/materializer@" + d
	HomebrewCommit = "0123456789abcdef0123456789abcdef01234567"
	VerificationKeysDigest = metadata.DefaultKeySetDigest()
	_, err := FromBuildOpts(map[string]string{
		"source":                     "example/frontend@" + d,
		"build-arg:DALEC_SKIP_TESTS": "1",
	})
	if err == nil {
		t.Fatal("expected release-bound test bypass rejection")
	}
}

func TestReleaseBindingsCannotBeOverridden(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	other := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oldBase, oldMat, oldCommit, oldKeys, oldRuby := RuntimeBaseRef, MaterializerRef, HomebrewCommit, VerificationKeysDigest, PortableRubyVersion
	defer func() {
		RuntimeBaseRef, MaterializerRef, HomebrewCommit, VerificationKeysDigest, PortableRubyVersion = oldBase, oldMat, oldCommit, oldKeys, oldRuby
	}()
	RuntimeBaseRef = "example/base@" + d
	MaterializerRef = "example/materializer@" + d
	HomebrewCommit = "0123456789abcdef0123456789abcdef01234567"
	VerificationKeysDigest = d
	PortableRubyVersion = "4.0.6"
	_, err := FromBuildOpts(map[string]string{"source": "example/frontend@" + d, "build-arg:DALEC_HOMEBREW_RUNTIME_BASE": "example/other@" + other, "build-arg:DALEC_HOMEBREW_COMMIT": "ffffffffffffffffffffffffffffffffffffffff"})
	if err == nil {
		t.Fatal("expected release binding mismatch")
	}
}

func TestGatewaySourceMustBePinned(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err := FromBuildOpts(map[string]string{"source": "example/frontend:latest", "build-arg:DALEC_HOMEBREW_RUNTIME_BASE": "example/base@" + d, "build-arg:DALEC_HOMEBREW_MATERIALIZER": "example/materializer@" + d, "build-arg:DALEC_HOMEBREW_COMMIT": "0123456789abcdef0123456789abcdef01234567", "build-arg:DALEC_HOMEBREW_KEYS_DIGEST": d})
	if err == nil {
		t.Fatal("expected mutable source rejection")
	}
}
