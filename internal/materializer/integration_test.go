package materializer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type installRunner struct {
	t                 *testing.T
	prefix, sourceDir string
	epoch             time.Time
	called            bool
}

func (r *installRunner) Run(_ context.Context, c Command) error {
	r.called = true
	stagedFormula := filepath.Join(r.prefix, filepath.FromSlash(coreTapFormulaRoot), "h", "hello.rb")
	data, err := os.ReadFile(stagedFormula)
	if err != nil {
		r.t.Fatalf("verified Formula was not staged before pour: %v", err)
	}
	if string(data) != "class Hello < Formula\nend\n" {
		r.t.Fatalf("staged Formula changed: %q", data)
	}
	info, err := os.Stat(stagedFormula)
	if err != nil {
		r.t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 || !info.ModTime().Equal(r.epoch) {
		r.t.Fatalf("staged Formula metadata = mode %o mtime %s", info.Mode().Perm(), info.ModTime())
	}
	wantBrew := filepath.Join(r.prefix, filepath.FromSlash(protectedHomebrewBrew))
	if c.Path != wantBrew {
		r.t.Fatalf("brew path = %q, want protected path %q", c.Path, wantBrew)
	}
	if len(c.Args) != 3 || strings.Join(c.Args, " ") != "ruby "+pourScriptPath+" "+filepath.Join(filepath.Dir(c.Args[2]), "hello--1.x86_64_linux.bottle.tar.gz") {
		r.t.Fatalf("args=%v", c.Args)
	}
	if filepath.Dir(c.Args[2]) == r.sourceDir {
		r.t.Fatal("brew was given the mutable source bottle path")
	}
	env := strings.Join(c.Env, "\n")
	for _, want := range []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_FROM_API=1", "HOMEBREW_NO_ANALYTICS=1"} {
		if !strings.Contains(env, want) {
			r.t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(env, "HOMEBREW_DEVELOPER=") {
		r.t.Fatal("developer mode must not be enabled for deterministic bottle pouring")
	}
	keg := filepath.Join(r.prefix, "Cellar/hello/1")
	if err := os.MkdirAll(filepath.Join(keg, "bin"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(keg, "bin/hello"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(keg, ".brew"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(keg, ".brew/hello.rb"), []byte("class Hello < Formula\nend\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(keg, "INSTALL_RECEIPT.json"), []byte(`{"built_as_bottle":true,"poured_from_bottle":true,"arch":"x86_64","runtime_dependencies":[],"source":{"spec":"stable","tap":"homebrew/core","versions":{"stable":"1","version_scheme":0}}}`), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.prefix, "opt"), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(keg, filepath.Join(r.prefix, "opt/hello")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.prefix, "bin"), 0o755); err != nil {
		return err
	}
	return os.Symlink("../Cellar/hello/1/bin/hello", filepath.Join(r.prefix, "bin/hello"))
}

func TestInstallVerifiesBeforeOfflineBottleCommand(t *testing.T) {
	prefix := formulaTapFixture(t)
	bottles := t.TempDir()
	for _, dir := range []string{"bin", "etc", "var", "Cellar", "opt"} {
		if err := os.MkdirAll(filepath.Join(prefix, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../Homebrew/bin/brew", filepath.Join(prefix, "bin/brew")); err != nil {
		t.Fatal(err)
	}
	archive := testBottle(t)
	sum := sha256.Sum256(archive)
	hexsum := hex.EncodeToString(sum[:])
	filename := "hello--1.x86_64_linux.bottle.tar.gz"
	if err := os.WriteFile(filepath.Join(bottles, filename), archive, 0o444); err != nil {
		t.Fatal(err)
	}
	tm := time.Unix(1_800_000_000, 0).UTC()
	d := "sha256:" + strings.Repeat("a", 64)
	desc := resolution.Descriptor{Digest: d, Size: 1, MediaType: "application/test"}
	record := &resolution.Record{SchemaVersion: resolution.SchemaVersion, PolicyVersion: resolution.PolicyVersion, Input: resolution.Input{DalecSpecDigest: d, Platform: resolution.Platform{OS: "linux", Architecture: "amd64"}}, Metadata: resolution.MetadataSnapshot{Digest: d, FormulaDigest: d, MigrationDigest: d, FormulaEnvelopeDigest: d, MigrationEnvelopeDigest: d, FormulaFreshnessSource: "signed-payload", MigrationFreshnessSource: "signed-payload", GeneratedAt: tm, FetchedAt: tm, FormulaURL: "https://example/formula", MigrationURL: "https://example/migrations", Signatures: []resolution.Signature{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}}, FormulaSignatures: []resolution.Signature{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}}, MigrationSignatures: []resolution.Signature{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}}}, ResolvedAt: tm, SourceDateEpoch: tm.Unix(), Requested: []resolution.RequestedRoot{{Requested: "hello", Canonical: "hello"}}, Nodes: []resolution.Node{{Name: "hello", FullName: "homebrew/core/hello", FormulaVersion: "1", PkgVersion: "1", License: "MIT", Bottle: resolution.Bottle{Tag: "x86_64_linux", Filename: filename, Repository: "ghcr.io/homebrew/core/hello", Index: desc, Manifest: func() resolution.Descriptor {
		value := desc
		value.Platform = &resolution.Platform{OS: "linux", Architecture: "amd64"}
		return value
	}(), Config: desc, Layer: resolution.Descriptor{Digest: "sha256:" + hexsum, Size: int64(len(archive)), MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"}, HomebrewSHA256: hexsum, Tab: resolution.BottleTab{HomebrewVersion: "6", Arch: "x86_64"}}}}, InstallOrder: []string{"hello"}, Components: resolution.Components{FrontendRef: "ghcr.io/x/f@" + d, RuntimeBaseRef: "ghcr.io/x/b@" + d, MaterializerRef: "ghcr.io/x/m@" + d, HomebrewCommit: strings.Repeat("a", 40), RubyRuntime: "portable-ruby-4.0.6", VerificationKeys: d, DalecModule: "v1", BuildKitModule: "v1"}, Runtime: resolution.RuntimePolicy{User: "linuxbrew", UID: 1000, GID: 1000, CPUBaseline: "core2"}, AttestationPolicy: resolution.AttestationPolicy{Waiver: "homebrew-jws-and-verified-oci-chain-v1"}}
	if _, err := policy.BindRuntimePolicy(record); err != nil {
		t.Fatal(err)
	}
	runner := &installRunner{t: t, prefix: prefix, sourceDir: bottles, epoch: tm}
	tapOptions := formulaTapTestOptions()
	evidence, err := Install(context.Background(), Config{
		Record: record, BottlesDir: bottles, Prefix: prefix, Runner: runner,
		formulaTapUID: tapOptions.ownerUID, formulaTapGID: tapOptions.ownerGID,
		formulaTapRuntimeUID: tapOptions.runtimeUID, formulaTapRuntimeGID: tapOptions.runtimeGID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !runner.called || len(evidence.VerifiedBottles) != 1 || len(evidence.StagedFormulae) != 1 || len(evidence.InstallDeltas) != 1 {
		t.Fatalf("runner=%v evidence=%+v", runner.called, evidence)
	}
	if evidence.VerifiedBottles[0].FormulaSource != nil {
		t.Fatal("transient Formula source was retained in serialized evidence")
	}
	staged := evidence.StagedFormulae[0]
	if staged.Formula != "hello" || staged.TapPath != "Formula/h/hello.rb" || staged.BottlePath != "hello/1/.brew/hello.rb" {
		t.Fatalf("staged Formula evidence = %#v", staged)
	}
}

func testBottle(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	files := map[string][]byte{"hello/1/.brew/hello.rb": []byte("class Hello < Formula\nend\n"), "hello/1/bin/hello": []byte("#!/bin/sh\n")}
	for name, data := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, "/hello") && strings.Contains(name, "/bin/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
