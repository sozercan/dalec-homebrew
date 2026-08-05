package frontend

import (
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
