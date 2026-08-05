package materializer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestEnsureWritablePrefixDirectoriesV2CreatesAndRejectsUnsafePaths(t *testing.T) {
	prefix := t.TempDir()
	if err := ensureWritablePrefixDirectoriesV2(prefix, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	for _, directory := range writablePrefixDirectoriesV2 {
		info, err := os.Lstat(filepath.Join(prefix, directory))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
			t.Fatalf("directory %s info=%v err=%v", directory, info, err)
		}
	}

	unsafe := filepath.Join(t.TempDir(), "prefix")
	if err := os.Mkdir(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(unsafe, "Cellar")); err != nil {
		t.Fatal(err)
	}
	if err := ensureWritablePrefixDirectoriesV2(unsafe, os.Geteuid(), os.Getegid()); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("unsafe prefix error=%v", err)
	}
}

func TestRejectsTapLocalFormulaHelperLoads(t *testing.T) {
	for _, source := range []string{
		"require_relative \"../lib/helper\"\nclass Widget < Formula; end\n",
		"load(\"./helpers.rb\")\nclass Widget < Formula; end\n",
		"require \"../shared/helper\"\nclass Widget < Formula; end\n",
	} {
		if err := validateStandaloneFormulaSource([]byte(source)); err == nil {
			t.Fatalf("accepted %q", source)
		}
	}
	if err := validateStandaloneFormulaSource([]byte("require \"json\"\nclass Widget < Formula; end\n")); err != nil {
		t.Fatal(err)
	}
}

func TestV2MaterializerPhasesVerifySerializedRuntimePolicy(t *testing.T) {
	bound := materializerRuntimePolicyRecordV2(t)
	if _, err := policy.BindRuntimePolicyV2(bound); err != nil {
		t.Fatal(err)
	}
	setMaterializerCompiledBindings(t, bound)

	phases := map[string]func(*resolution.RecordV2) error{
		"prepare": func(record *resolution.RecordV2) error {
			_, err := PrepareV2(context.Background(), PrepareV2Config{Record: record})
			return err
		},
		"install": func(record *resolution.RecordV2) error {
			_, err := InstallOneV2(context.Background(), InstallOneV2Config{Record: record})
			return err
		},
		"finalize": func(record *resolution.RecordV2) error {
			_, err := FinalizeV2(context.Background(), FinalizeV2Config{
				Record:              record,
				Prefix:              "/prefix",
				OutputRoot:          "/output",
				PreparationEvidence: "/preparation.json",
				InstallEvidenceDir:  "/deltas",
			})
			return err
		},
	}
	bindings := map[string]struct {
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
			want: "does not match policy",
		},
	}

	for phaseName, phase := range phases {
		for bindingName, binding := range bindings {
			t.Run(phaseName+"/"+bindingName, func(t *testing.T) {
				record := cloneMaterializerRuntimePolicyRecordV2(t, bound)
				binding.mutate(record)
				before, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				err = phase(record)
				if err == nil || !strings.Contains(err.Error(), binding.want) {
					t.Fatalf("phase error = %v, want %q", err, binding.want)
				}
				after, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatalf("phase mutated V2 record\nbefore: %s\n after: %s", before, after)
				}
			})
		}
	}
}

func setMaterializerCompiledBindings(t *testing.T, record *resolution.RecordV2) {
	t.Helper()
	old := []string{
		config.BottleFetcherRef, config.CatalogServiceOrigin, config.IngestionJWSKeyPolicyDigest,
		config.TapPolicyDigest, config.ExecutableRuntimePolicyDigest, config.HomebrewCommit,
		config.VerificationKeysDigest, config.PortableRubyVersion, config.SupportedCatalogPolicyVersions,
		config.SupportedFetchPolicyVersions, config.SupportedProvenancePolicyVersions, config.MaterializerV2BindingsRequired,
	}
	config.BottleFetcherRef = record.Components.BottleFetcherRef
	config.CatalogServiceOrigin = record.Components.CatalogServiceOrigin
	config.IngestionJWSKeyPolicyDigest = record.Components.IngestionJWSKeyPolicyDigest
	config.TapPolicyDigest = record.Components.TapPolicyDigest
	config.ExecutableRuntimePolicyDigest = record.Components.ExecutableRuntimePolicyDigest
	config.HomebrewCommit = record.Components.HomebrewCommit
	config.VerificationKeysDigest = record.Components.VerificationKeys
	config.PortableRubyVersion = record.Components.RubyRuntime
	config.SupportedCatalogPolicyVersions = strings.Join(record.Components.SupportedCatalogPolicyVersions, ",")
	config.SupportedFetchPolicyVersions = strings.Join(record.Components.SupportedFetchPolicyVersions, ",")
	config.SupportedProvenancePolicyVersions = strings.Join(record.Components.SupportedProvenancePolicyVersions, ",")
	config.MaterializerV2BindingsRequired = "1"
	t.Cleanup(func() {
		config.BottleFetcherRef, config.CatalogServiceOrigin, config.IngestionJWSKeyPolicyDigest = old[0], old[1], old[2]
		config.TapPolicyDigest, config.ExecutableRuntimePolicyDigest, config.HomebrewCommit = old[3], old[4], old[5]
		config.VerificationKeysDigest, config.PortableRubyVersion, config.SupportedCatalogPolicyVersions = old[6], old[7], old[8]
		config.SupportedFetchPolicyVersions, config.SupportedProvenancePolicyVersions, config.MaterializerV2BindingsRequired = old[9], old[10], old[11]
	})
}

func cloneMaterializerRuntimePolicyRecordV2(t *testing.T, record *resolution.RecordV2) *resolution.RecordV2 {
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

func materializerRuntimePolicyRecordV2(t *testing.T) *resolution.RecordV2 {
	t.Helper()
	executablePolicyDigest, err := policy.V2RuntimePolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	tapPolicyDigest, err := policy.V2TapPolicyDigest()
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
			Tap:         "homebrew/core",
			Commit:      strings.Repeat("1", 40),
			Signer:      resolution.Signature{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true},
			Documents:   []resolution.MetadataDocument{{Name: "formula", Digest: digestA, EnvelopeDigest: digestB}, {Name: "migrations", Digest: digestC, EnvelopeDigest: digestB}},
			GeneratedAt: generatedAt,
			FetchedAt:   generatedAt.Add(time.Minute),
			Sequence:    sequence,
			Rollback:    resolution.RollbackEvidence{Policy: resolution.CoreMetadataRollbackPolicyV1, SequenceFloor: sequence - 1, StateDigest: digestC},
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

func TestVerifyReceiptlessMarkerV2MatchesHTTPSArchive(t *testing.T) {
	node := resolution.NodeV2{ID: "acme/tools/widget", Bottle: resolution.BottleV2{Transport: resolution.BottleTransport{HTTPS: &resolution.HTTPSTransport{}}, Tab: resolution.BottleTabV2{Receiptless: true}}}
	if err := verifyReceiptlessMarkerV2(node, &bottle.Result{}); err != nil {
		t.Fatal(err)
	}
	node.Bottle.Tab.Receiptless = false
	if err := verifyReceiptlessMarkerV2(node, &bottle.Result{}); err == nil {
		t.Fatal("missing receipt accepted without receiptless marker")
	}
	node.Bottle.Tab.Receiptless = false
	if err := verifyReceiptlessMarkerV2(node, &bottle.Result{Receipt: &bottle.ReceiptEvidence{}}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPrebuiltDerivedBottleV2BindsPayload(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	node := resolution.NodeV2{
		ID: "sozercan/repo/a365",
		Bottle: resolution.BottleV2{PrebuiltDerivation: &resolution.PrebuiltDerivationV2{
			Payload: resolution.PrebuiltPayloadEvidenceV2{
				DestinationPath: "bin/a365",
				SHA256:          digest,
				Size:            42,
				DerivedMode:     0o555,
			},
		}},
	}
	valid := &bottle.Result{Inventory: []bottle.InventoryEntry{{KegPath: "bin/a365", Type: bottle.EntryRegular, SHA256: digest, Size: 42, Mode: 0o555}}}
	if err := verifyPrebuiltDerivedBottleV2(node, valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*bottle.Result){
		"receipt":   func(result *bottle.Result) { result.Receipt = &bottle.ReceiptEvidence{} },
		"digest":    func(result *bottle.Result) { result.Inventory[0].SHA256 = "sha256:" + strings.Repeat("b", 64) },
		"mode":      func(result *bottle.Result) { result.Inventory[0].Mode = 0o755 },
		"missing":   func(result *bottle.Result) { result.Inventory = nil },
		"duplicate": func(result *bottle.Result) { result.Inventory = append(result.Inventory, result.Inventory[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *valid
			copy.Inventory = append([]bottle.InventoryEntry(nil), valid.Inventory...)
			mutate(&copy)
			if err := verifyPrebuiltDerivedBottleV2(node, &copy); err == nil {
				t.Fatal("tampered prebuilt derived bottle accepted")
			}
		})
	}
}
