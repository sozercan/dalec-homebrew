package frontend

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/project-dalec/dalec"
	dalecfrontend "github.com/project-dalec/dalec/frontend"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestDalecLoadOptionsAllowFrontendOwnedBuildArgs(t *testing.T) {
	args := make(map[string]string)
	for _, name := range config.BuildArgNames() {
		args[name] = "test-value"
	}
	if err := (&dalec.Spec{}).SubstituteArgs(args); err == nil || !strings.Contains(err.Error(), "unknown arg") {
		t.Fatalf("frontend-owned args were unexpectedly accepted without load options: %v", err)
	}

	loadConfig := dalecfrontend.LoadConfig{}
	for _, option := range dalecLoadOptions() {
		option(&loadConfig)
	}
	if err := (&dalec.Spec{}).SubstituteArgs(args, loadConfig.SubstituteOpts...); err != nil {
		t.Fatalf("frontend-owned args were rejected: %v", err)
	}

	args["DALEC_HOMEBREW_UNKNOWN"] = "value"
	if err := (&dalec.Spec{}).SubstituteArgs(args, loadConfig.SubstituteOpts...); err == nil || !strings.Contains(err.Error(), `unknown arg: "DALEC_HOMEBREW_UNKNOWN"`) {
		t.Fatalf("unknown frontend arg error = %v", err)
	}
}

func TestMarshalEffectiveSpecPreservesFrontendMetadata(t *testing.T) {
	frontendRef := "ghcr.io/example/dalec-homebrew@sha256:" + strings.Repeat("a", 64)
	spec := &dalec.Spec{Targets: map[string]dalec.Target{
		"homebrew": {Frontend: &dalec.Frontend{Image: frontendRef}},
	}}

	effective, err := marshalEffectiveSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		SchemaVersion string     `json:"schema_version"`
		DalecSpec     dalec.Spec `json:"dalec_spec"`
	}
	if err := json.Unmarshal(effective, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != "dalec-homebrew-effective-input/v1" {
		t.Fatalf("effective envelope=%+v", decoded)
	}
	if got := decoded.DalecSpec.Targets["homebrew"].Frontend; got == nil || got.Image != frontendRef {
		t.Fatalf("frontend metadata=%+v, want image %q", got, frontendRef)
	}

	target := spec.Targets["homebrew"]
	target.Frontend = nil
	spec.Targets["homebrew"] = target
	withoutFrontend, err := marshalEffectiveSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if sha256Hex(effective) == sha256Hex(withoutFrontend) {
		t.Fatal("effective spec digest did not bind frontend routing metadata")
	}
	firstOrder, err := marshalEffectiveSpec(&dalec.Spec{Dependencies: &dalec.PackageDependencies{Runtime: dalec.PackageDependencyList{
		"zlib":  {},
		"hello": {},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	secondOrder, err := marshalEffectiveSpec(&dalec.Spec{Dependencies: &dalec.PackageDependencies{Runtime: dalec.PackageDependencyList{
		"hello": {},
		"zlib":  {},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if sha256Hex(firstOrder) != sha256Hex(secondOrder) {
		t.Fatal("effective spec digest depends on Go map insertion order")
	}
}

func TestMetadataBaseURL(t *testing.T) {
	got, err := metadataBaseURL("https://formulae.brew.sh/api/formula.jws.json", "https://formulae.brew.sh/api/formula_tap_migrations.jws.json")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://formulae.brew.sh/api/" {
		t.Fatalf("got %q", got)
	}
	if _, err := metadataBaseURL("https://a/formula.jws.json", "https://b/formula_tap_migrations.jws.json"); err == nil {
		t.Fatal("expected error")
	}
}
func TestCrossPlatformRootVersions(t *testing.T) {
	record := func(arch, version string) *resolution.Record {
		return &resolution.Record{Input: resolution.Input{Platform: resolution.Platform{OS: "linux", Architecture: arch}}, Requested: []resolution.RequestedRoot{{Requested: "x", Canonical: "x"}}, Nodes: []resolution.Node{{Name: "x", PkgVersion: version}}}
	}
	if err := validateCrossPlatformRoots([]*resolution.Record{record("amd64", "1"), record("arm64", "1")}); err != nil {
		t.Fatal(err)
	}
	armOnly := &resolution.Record{Input: resolution.Input{Platform: resolution.Platform{OS: "linux", Architecture: "arm64"}}, Requested: []resolution.RequestedRoot{{Requested: "y", Canonical: "y"}}, Nodes: []resolution.Node{{Name: "y", PkgVersion: "1"}}}
	if err := validateCrossPlatformRoots([]*resolution.Record{record("amd64", "1"), armOnly}); err != nil {
		t.Fatalf("arch-filtered roots should be independent: %v", err)
	}
	if err := validateCrossPlatformRoots([]*resolution.Record{record("amd64", "1"), record("arm64", "2")}); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("err=%v", err)
	}
}

func TestCrossPlatformV2RootVersionsUseFormulaID(t *testing.T) {
	record := func(arch, version string) *resolution.RecordV2 {
		return &resolution.RecordV2{Input: resolution.Input{Platform: resolution.Platform{OS: "linux", Architecture: arch}}, Requested: []resolution.RequestedRootV2{{Requested: "acme/tools/widget", ID: "acme/tools/widget"}}, Nodes: []resolution.NodeV2{{ID: "acme/tools/widget", Name: "widget", PkgVersion: version}}}
	}
	if err := validateCrossPlatformRootsV2([]*resolution.RecordV2{record("amd64", "1"), record("arm64", "1")}); err != nil {
		t.Fatal(err)
	}
	if err := validateCrossPlatformRootsV2([]*resolution.RecordV2{record("amd64", "1"), record("arm64", "2")}); err == nil {
		t.Fatal("cross-platform V2 root mismatch accepted")
	}
}

func TestCrossPlatformV2RejectsDifferentResolvedIDsForSameRequestedRoot(t *testing.T) {
	left := &resolution.RecordV2{Input: resolution.Input{Platform: resolution.Platform{OS: "linux", Architecture: "amd64"}}, Requested: []resolution.RequestedRootV2{{Requested: "acme/tools/alias", ID: "acme/tools/widget"}}, Nodes: []resolution.NodeV2{{ID: "acme/tools/widget", Name: "widget", PkgVersion: "1"}}}
	right := &resolution.RecordV2{Input: resolution.Input{Platform: resolution.Platform{OS: "linux", Architecture: "arm64"}}, Requested: []resolution.RequestedRootV2{{Requested: "acme/tools/alias", ID: "other/tap/widget"}}, Nodes: []resolution.NodeV2{{ID: "other/tap/widget", Name: "widget", PkgVersion: "1"}}}
	if err := validateCrossPlatformRootsV2([]*resolution.RecordV2{left, right}); err == nil {
		t.Fatal("different resolved IDs accepted for the same requested root")
	}
}
