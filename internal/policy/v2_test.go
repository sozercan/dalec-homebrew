package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func TestV2FormulaCapabilitiesDoNotUseShortNames(t *testing.T) {
	caps, ok, err := V2FormulaCapabilities("homebrew/core/node")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(caps.GeneratedGlobalPaths) == 0 {
		t.Fatalf("core node capabilities=%+v ok=%v", caps, ok)
	}
	for _, id := range []string{"acme/tools/node", "node"} {
		caps, ok, err := V2FormulaCapabilities(id)
		if err != nil {
			t.Fatal(err)
		}
		if ok || len(caps.GeneratedGlobalPaths) != 0 {
			t.Fatalf("spoof %q received capabilities %+v", id, caps)
		}
	}
}

func TestV2FormulaCapabilitiesBindIssue18RulesToExactCoreIDs(t *testing.T) {
	for id, rules := range map[string][]string{
		"homebrew/core/certifi": {"certifi-shared-ca-link-v1"},
		"homebrew/core/libpsl":  {"optional-libpsl-tooling", "runtime-aux-libpsl"},
	} {
		caps, ok, err := V2FormulaCapabilities(id)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("missing capabilities for %s", id)
		}
		for _, rule := range rules {
			if !slices.Contains(caps.Rules, rule) {
				t.Fatalf("capabilities for %s lack %q: %+v", id, rule, caps)
			}
		}
	}
	for _, id := range []string{"certifi", "libpsl", "acme/tools/certifi", "acme/tools/libpsl"} {
		caps, ok, err := V2FormulaCapabilities(id)
		if err != nil {
			t.Fatal(err)
		}
		if ok || len(caps.Rules) != 0 {
			t.Fatalf("spoof %q received capabilities %+v", id, caps)
		}
	}
}

func TestV2RuntimePolicyBindings(t *testing.T) {
	p, err := V2RuntimePolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.ResolverPolicyVersion != policyv2.ResolverPolicyVersion {
		t.Fatalf("resolver policy=%q", p.ResolverPolicyVersion)
	}
	if digest, err := V2RuntimePolicyDigest(); err != nil || digest == "" {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	profile := p.MinimalRuntimeProfile()
	if profile.Name != policyv2.RuntimeProfileMinimalV1 || !slices.Equal(profile.Rules, policyv2.MinimalV1RuntimePruneRules()) {
		t.Fatalf("minimal runtime profile=%+v", profile)
	}
}

func TestBindRuntimePolicyV2MinimalOnly(t *testing.T) {
	implicitMinimal := testRuntimePolicyRecordV2(t)
	implicitAllow, err := BindRuntimePolicyV2(implicitMinimal)
	if err != nil {
		t.Fatal(err)
	}
	if implicitMinimal.Runtime.Profile != policyv2.RuntimeProfileMinimalV1 || implicitAllow.PruningProfile != policyv2.RuntimeProfileMinimalV1 {
		t.Fatalf("implicit minimal binding = profile %q allowlist %q", implicitMinimal.Runtime.Profile, implicitAllow.PruningProfile)
	}
	if !slices.Equal(implicitAllow.PruningRules, policyv2.MinimalV1RuntimePruneRules()) {
		t.Fatalf("minimal pruning rules=%v", implicitAllow.PruningRules)
	}

	explicitMinimal := testRuntimePolicyRecordV2(t)
	explicitMinimal.Runtime.Profile = policyv2.RuntimeProfileMinimalV1
	explicitAllow, err := BindRuntimePolicyV2(explicitMinimal)
	if err != nil {
		t.Fatal(err)
	}
	if explicitAllow.PruningProfile != policyv2.RuntimeProfileMinimalV1 {
		t.Fatalf("explicit minimal allowlist profile = %q", explicitAllow.PruningProfile)
	}
	if implicitMinimal.PruningPolicyDigest != explicitMinimal.PruningPolicyDigest {
		t.Fatalf("implicit and explicit minimal policy digests differ: %s != %s", implicitMinimal.PruningPolicyDigest, explicitMinimal.PruningPolicyDigest)
	}
	implicitDigest, err := resolution.DigestV2(implicitMinimal)
	if err != nil {
		t.Fatal(err)
	}
	explicitDigest, err := resolution.DigestV2(explicitMinimal)
	if err != nil {
		t.Fatal(err)
	}
	if implicitDigest != explicitDigest {
		t.Fatalf("implicit and explicit minimal resolution digests differ: %s != %s", implicitDigest, explicitDigest)
	}

	implicitMinimal.Runtime.Profile = "standard-v1"
	if _, err := BindRuntimePolicyV2(implicitMinimal); err == nil || !strings.Contains(err.Error(), "unsupported V2 runtime profile") {
		t.Fatalf("BindRuntimePolicyV2() error = %v, want in-memory profile tamper rejection", err)
	}
}

func TestVerifyRuntimePolicyV2InfersDecodedMinimalProfile(t *testing.T) {
	record := testRuntimePolicyRecordV2(t)
	record.Runtime.Profile = policyv2.RuntimeProfileMinimalV1
	if _, err := BindRuntimePolicyV2(record); err != nil {
		t.Fatal(err)
	}
	canonical, err := resolution.CanonicalV2(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte(`"profile"`)) {
		t.Fatalf("runtime profile leaked into V2 resolution: %s", canonical)
	}
	decoded, err := resolution.DecodeV2(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Runtime.Profile != "" {
		t.Fatalf("decoded in-memory profile = %q, want empty", decoded.Runtime.Profile)
	}
	allow, err := VerifyRuntimePolicyV2(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if allow.PruningProfile != policyv2.RuntimeProfileMinimalV1 {
		t.Fatalf("inferred pruning profile = %q, want %q", allow.PruningProfile, policyv2.RuntimeProfileMinimalV1)
	}
	if decoded.Runtime.Profile != "" {
		t.Fatalf("verification mutated decoded profile to %q", decoded.Runtime.Profile)
	}
}

func TestRuntimeAllowlistV2UsesFullFormulaIDsForCapabilities(t *testing.T) {
	record := &resolution.RecordV2{Nodes: []resolution.NodeV2{{ID: "acme/tools/node", Tap: "acme/tools", Name: "node", HomebrewFullName: "acme/tools/node"}}}
	allow, _, err := RuntimeAllowlistV2(record)
	if err == nil {
		// ProjectV2 validates complete records; use the exact capability helper to
		// make the spoof assertion independently of unrelated record fields.
		for _, rule := range allow.Owners {
			if rule.Path == nodeNPMRuntimePath {
				t.Fatalf("non-core node received npm capability: %+v", rule)
			}
		}
	}
	caps, ok, err := V2FormulaCapabilities("acme/tools/node")
	if err != nil {
		t.Fatal(err)
	}
	if ok || len(caps.GeneratedGlobalPaths) != 0 {
		t.Fatalf("non-core node capabilities=%+v ok=%v", caps, ok)
	}
}

func TestVerifyRuntimePolicyV2RequiresExactBindingsWithoutMutation(t *testing.T) {
	bound := testRuntimePolicyRecordV2(t)
	if _, err := BindRuntimePolicyV2(bound); err != nil {
		t.Fatal(err)
	}
	if len(bound.Runtime.WritablePaths) == 0 || bound.PruningPolicyDigest == "" {
		t.Fatalf("BindRuntimePolicyV2() did not populate bindings: runtime=%+v pruning=%q", bound.Runtime, bound.PruningPolicyDigest)
	}
	assertVerifyRuntimePolicyV2DoesNotMutate(t, bound, "")

	for name, test := range map[string]struct {
		mutate func(*resolution.RecordV2)
		want   string
	}{
		"missing writable paths": {
			mutate: func(record *resolution.RecordV2) { record.Runtime.WritablePaths = nil },
			want:   "runtime writable paths are missing",
		},
		"mismatched writable paths": {
			mutate: func(record *resolution.RecordV2) {
				record.Runtime.WritablePaths = []string{"/home/linuxbrew/.linuxbrew/var/not-hello"}
			},
			want: "runtime writable paths do not match V2 policy",
		},
		"missing pruning policy digest": {
			mutate: func(record *resolution.RecordV2) { record.PruningPolicyDigest = "" },
			want:   "pruning policy digest is missing",
		},
		"mismatched pruning policy digest": {
			mutate: func(record *resolution.RecordV2) {
				record.PruningPolicyDigest = "sha256:" + strings.Repeat("f", 64)
			},
			want: "does not match minimal runtime policy",
		},
		"mismatched tap policy digest": {
			mutate: func(record *resolution.RecordV2) {
				record.Components.TapPolicyDigest = "sha256:" + strings.Repeat("f", 64)
			},
			want: "tap policy digest",
		},
		"extra catalog policy version": {
			mutate: func(record *resolution.RecordV2) {
				record.Components.SupportedCatalogPolicyVersions = append(record.Components.SupportedCatalogPolicyVersions, "unexpected")
			},
			want: "supported catalog policy versions",
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := cloneRuntimePolicyRecordV2(t, bound)
			test.mutate(record)
			assertVerifyRuntimePolicyV2DoesNotMutate(t, record, test.want)
		})
	}
}

func assertVerifyRuntimePolicyV2DoesNotMutate(t *testing.T, record *resolution.RecordV2, wantError string) {
	t.Helper()
	profile := record.Runtime.Profile
	before, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	_, verifyErr := VerifyRuntimePolicyV2(record)
	if wantError == "" {
		if verifyErr != nil {
			t.Fatal(verifyErr)
		}
	} else if verifyErr == nil || !strings.Contains(verifyErr.Error(), wantError) {
		t.Fatalf("VerifyRuntimePolicyV2() error = %v, want %q", verifyErr, wantError)
	}
	after, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("VerifyRuntimePolicyV2() mutated record\nbefore: %s\n after: %s", before, after)
	}
	if record.Runtime.Profile != profile {
		t.Fatalf("VerifyRuntimePolicyV2() mutated in-memory profile from %q to %q", profile, record.Runtime.Profile)
	}
}

func cloneRuntimePolicyRecordV2(t *testing.T, record *resolution.RecordV2) *resolution.RecordV2 {
	t.Helper()
	data, err := resolution.CanonicalV2(record)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := resolution.DecodeV2(data)
	if err != nil {
		t.Fatal(err)
	}
	return clone
}

func testRuntimePolicyRecordV2(t *testing.T) *resolution.RecordV2 {
	t.Helper()
	executablePolicyDigest, err := V2RuntimePolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	tapPolicyDigest, err := V2TapPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Unix(1_800_000_000, 0).UTC()
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	digestC := "sha256:" + strings.Repeat("c", 64)
	descriptor := func(value string) resolution.Descriptor {
		return resolution.Descriptor{Digest: value, Size: 1, MediaType: "application/test"}
	}
	manifest := descriptor(digestA)
	manifest.Platform = &resolution.Platform{OS: "linux", Architecture: "amd64"}
	sequence := uint64(generatedAt.Unix())
	return &resolution.RecordV2{
		SchemaVersion: resolution.SchemaVersionV2,
		PolicyVersion: resolution.PolicyVersionV2,
		Input: resolution.Input{
			DalecSpecDigest: digestA,
			Platform:        resolution.Platform{OS: "linux", Architecture: "amd64"},
		},
		MetadataSources: []resolution.MetadataSource{{
			Tap:               "homebrew/core",
			Commit:            strings.Repeat("1", 40),
			Signer:            resolution.Signature{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true},
			Documents:         []resolution.MetadataDocument{{Name: "formula", Digest: digestA, EnvelopeDigest: digestB}, {Name: "migrations", Digest: digestC, EnvelopeDigest: digestB}},
			GeneratedAt:       generatedAt,
			GeneratedAtSource: resolution.CoreGeneratedAtSignedPayload,
			FetchedAt:         generatedAt.Add(time.Minute),
			Sequence:          sequence,
			Rollback:          resolution.RollbackEvidence{Policy: resolution.CoreMetadataRollbackPolicyV1, SequenceFloor: sequence - 1, StateDigest: digestC},
		}},
		ResolvedAt:      generatedAt.Add(2 * time.Minute),
		SourceDateEpoch: generatedAt.Unix(),
		Requested:       []resolution.RequestedRootV2{{Requested: "hello", ID: "homebrew/core/hello"}},
		Nodes: []resolution.NodeV2{{
			ID:               "homebrew/core/hello",
			Tap:              "homebrew/core",
			Name:             "hello",
			HomebrewFullName: "homebrew/core/hello",
			FormulaVersion:   "1.0",
			PkgVersion:       "1.0",
			Bottle: resolution.BottleV2{
				Tag:                        "x86_64_linux",
				Filename:                   "hello--1.0.x86_64_linux.bottle.tar.gz",
				Size:                       1,
				SHA256:                     digestB,
				Cellar:                     "/home/linuxbrew/.linuxbrew/Cellar",
				CurrentFormulaSourceDigest: digestA,
				Verification:               resolution.BottleVerificationV2{PolicyVersion: resolution.CoreBottleVerificationDeferredV1},
				Tab:                        resolution.BottleTabV2{Arch: "x86_64"},
				Transport: resolution.BottleTransport{OCI: &resolution.OCITransport{
					Registry:   "ghcr.io",
					Repository: "homebrew/core/hello",
					Index:      descriptor(digestA),
					Manifest:   manifest,
					Config:     descriptor(digestC),
					Layer:      descriptor(digestB),
				}},
			},
			Provenance: resolution.Provenance{Waiver: &resolution.ProvenanceWaiver{Policy: resolution.CoreProvenanceWaiverPolicyV1}},
		}},
		InstallOrder: []resolution.FormulaID{"homebrew/core/hello"},
		Components: resolution.ComponentsV2{
			FrontendIndexRef:                  "ghcr.io/example/frontend@" + digestC,
			FrontendRef:                       "ghcr.io/example/frontend@" + digestA,
			RuntimeBaseRef:                    "ghcr.io/example/runtime-base@" + digestC,
			MaterializerRef:                   "ghcr.io/example/materializer@" + digestA,
			BottleFetcherRef:                  "ghcr.io/example/bottle-fetcher@" + digestC,
			CatalogServiceOrigin:              "https://catalog.example.test",
			IngestionJWSKeyPolicyDigest:       digestA,
			TapPolicyDigest:                   tapPolicyDigest,
			ExecutableRuntimePolicyDigest:     executablePolicyDigest,
			HomebrewCommit:                    strings.Repeat("1", 40),
			RubyRuntime:                       "4.0.6",
			VerificationKeys:                  digestC,
			DalecModule:                       "github.com/project-dalec/dalec@v0.21.5",
			BuildKitModule:                    "github.com/moby/buildkit@v0.31.2",
			SupportedCatalogPolicyVersions:    []string{"tap-catalog-v1"},
			SupportedFetchPolicyVersions:      []string{resolution.HTTPSFetchPolicyVersionV1},
			SupportedProvenancePolicyVersions: []string{resolution.CoreProvenanceWaiverPolicyV1, resolution.HTTPSBottleSourceWaiverPolicyV1, resolution.ProvenanceWaiverPolicyV1, resolution.PrebuiltProvenanceWaiverPolicyV1, resolution.VerifiedProvenancePolicyV1},
		},
		Runtime: resolution.RuntimePolicy{User: "linuxbrew", UID: 1000, GID: 1000, CPUBaseline: "core2"},
	}
}

func TestV2RuntimeWritablePathsAreCanonicalized(t *testing.T) {
	data, err := os.ReadFile("v2.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "slices.Sort(writable)"); got != 1 {
		t.Fatalf("V2 writable paths are sorted at %d policy boundaries, want one shared boundary", got)
	}
}
