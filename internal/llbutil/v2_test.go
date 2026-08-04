package llbutil

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestPreparePrefixSeedUsesStateRoot(t *testing.T) {
	data, err := os.ReadFile("v2.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `llb.Copy(worker, DefaultHomebrewPrefixV2, "/"`) {
		t.Fatal("V2 prefix seed is not copied to the state root")
	}
	if strings.Contains(text, `llb.SourcePath("/prefix")`) {
		t.Fatal("V2 prefix seed retains a nested /prefix source path")
	}
}

func TestLinuxbrewWritableScratchV2IsPrivateAndOwnedByInstaller(t *testing.T) {
	state := linuxbrewWritableScratchV2()
	definition, err := state.Marshal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range definition.Def {
		var op pb.Op
		if err := op.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		file := op.GetFile()
		if file == nil {
			continue
		}
		for _, action := range file.Actions {
			directory := action.GetMkdir()
			if directory == nil || directory.Path != "/data" {
				continue
			}
			found = true
			if directory.Mode != 0o700 {
				t.Fatalf("writable scratch mode=%#o, want 0700", directory.Mode)
			}
			if directory.Owner == nil || directory.Owner.User.GetByID() != 1000 || directory.Owner.Group.GetByID() != 1000 {
				t.Fatalf("writable scratch owner=%v, want 1000:1000", directory.Owner)
			}
		}
	}
	if !found {
		t.Fatal("writable scratch does not create /data")
	}
}

func TestInstallPreparedV2MountsInstallerWritableScratch(t *testing.T) {
	data, err := os.ReadFile("v2.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, destination := range []string{"/home/linuxbrew/.cache", "/tmp", "/var/tmp"} {
		want := `llb.AddMount("` + destination + `", `
		start := strings.Index(text, want)
		if start < 0 {
			t.Fatalf("V2 install does not mount %s", destination)
		}
		line := text[start:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		if !strings.Contains(line, `llb.SourcePath("/data")`) || strings.Contains(line, "llb.Scratch()") {
			t.Fatalf("V2 install mount %s is not backed by installer-owned scratch: %s", destination, line)
		}
	}
}

func TestEnsurePreparedPrefixDirectoriesV2CreatesRootRelativeStructure(t *testing.T) {
	state := ensurePreparedPrefixDirectoriesV2(llb.Scratch())
	definition, err := state.Marshal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operations := ""
	for _, raw := range definition.Def {
		var op pb.Op
		if err := op.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if file := op.GetFile(); file != nil {
			operations += file.String()
		}
	}
	for _, directory := range []string{"/Cellar", "/opt", "/var"} {
		if !strings.Contains(operations, directory) {
			t.Fatalf("prepared prefix state is missing %s: %s", directory, operations)
		}
	}
	if strings.Contains(operations, "/prefix/Cellar") {
		t.Fatalf("prepared prefix directories were nested under /prefix: %s", operations)
	}
}

func TestFetchRequestV2BindsSignedTransport(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	node := resolution.NodeV2{ID: "acme/tools/widget", Bottle: resolution.BottleV2{Filename: "widget.tgz", Transport: resolution.BottleTransport{HTTPS: &resolution.HTTPSTransport{URL: "https://bottles.example/widget.tgz", ExpectedSize: 7, SHA256: d, Filename: "widget.tgz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: resolution.HTTPSFetchPolicyVersionV1}}}}
	request, err := fetchRequestV2(node)
	if err != nil {
		t.Fatal(err)
	}
	if request.ArtifactID != node.ID.String() || request.SHA256 != strings.TrimPrefix(d, "sha256:") || request.FetchPolicyVersion != fetcher.FetchPolicyVersion {
		t.Fatalf("request=%+v", request)
	}
}

func TestFetchRequestV2RejectsFilenameMismatch(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	node := resolution.NodeV2{ID: "acme/tools/widget", Bottle: resolution.BottleV2{Filename: "widget.tgz", Transport: resolution.BottleTransport{HTTPS: &resolution.HTTPSTransport{URL: "https://bottles.example/widget.tgz", ExpectedSize: 7, SHA256: d, Filename: "other.tgz", AllowedRedirectHosts: []string{"bottles.example"}, FetchPolicyVersion: resolution.HTTPSFetchPolicyVersionV1}}}}
	if _, err := fetchRequestV2(node); err == nil {
		t.Fatal("filename mismatch accepted")
	}
}

func TestV2LLBBindingsRejectComponentAndPlatformMismatch(t *testing.T) {
	record := &resolution.RecordV2{Input: resolution.Input{Platform: resolution.Platform{OS: "linux", Architecture: "amd64"}}, Components: resolution.ComponentsV2{BottleFetcherRef: "example/fetcher@sha256:" + strings.Repeat("a", 64)}}
	if _, err := BottleStatesV2("example/other@sha256:"+strings.Repeat("b", 64), ocispec.Platform{OS: "linux", Architecture: "amd64"}, record); err == nil {
		t.Fatal("mismatched fetcher reference accepted")
	}
	if err := validateV2PlatformBinding(record, ocispec.Platform{OS: "linux", Architecture: "arm64"}); err == nil {
		t.Fatal("mismatched platform accepted")
	}
}
