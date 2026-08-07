package cataloggenerator

import (
	"bytes"
	"context"
	"strings"
	"testing"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/solver/pb"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogextractor"
)

func TestExtractionGraphSeparatesNetworkedGitFromOfflineEvaluation(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	extractor := &BuildKitExtractor{extractorRef: "ghcr.io/example/catalog-extractor@" + digest, homebrewCommit: strings.Repeat("b", 40)}
	tap, _ := catalog.ParseTapID("acme/tools")
	state, err := extractor.extractionState(tap)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := state.Marshal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var foundGit, foundTapMount, foundSourceMetadataMount, foundTrustMount, foundTrustEnv, foundGitRemoval bool
	execCount := 0
	for _, raw := range definition.Def {
		var op pb.Op
		if err := op.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if source := op.GetSource(); source != nil {
			if strings.Contains(source.Identifier, "github.com/acme/homebrew-tools.git") {
				foundGit = true
				if source.Identifier != "git://github.com/acme/homebrew-tools.git" {
					t.Fatalf("public Git source unexpectedly selects a tap ref: %q", source.Identifier)
				}
				if source.Attrs[pb.AttrKeepGitDir] != "true" {
					t.Fatal("Git source does not retain exact commit metadata")
				}
				if source.Attrs[pb.AttrAuthTokenSecret] != "" || source.Attrs[pb.AttrAuthHeaderSecret] != "" {
					t.Fatalf("public Git source requests authentication secrets: %#v", source.Attrs)
				}
				if source.Attrs[pb.AttrGitSkipSubmodules] != "true" {
					t.Fatal("public Git source does not disable unbound submodules")
				}
			}
		}
		if file := op.GetFile(); file != nil && strings.Contains(file.String(), ".git") {
			foundGitRemoval = true
		}
		if exec := op.GetExec(); exec != nil {
			execCount++
			if exec.Network != pb.NetMode_NONE || exec.Security != pb.SecurityMode_SANDBOX {
				t.Fatalf("extractor exec network/security = %s/%s", exec.Network, exec.Security)
			}
			if len(exec.Secretenv) != 0 || len(exec.CdiDevices) != 0 {
				t.Fatal("extractor exec carries secret environment or devices")
			}
			for _, env := range exec.Meta.Env {
				if env == "HOMEBREW_USER_CONFIG_HOME=/input/config" {
					foundTrustEnv = true
				}
			}
			for _, mount := range exec.Mounts {
				if mount.MountType == pb.MountType_SECRET || mount.MountType == pb.MountType_SSH {
					t.Fatalf("extractor exec contains forbidden mount type %s", mount.MountType)
				}
				if mount.Dest == "/home/linuxbrew/.linuxbrew/Homebrew/Library/Taps/acme/homebrew-tools" {
					foundTapMount = true
					if !mount.Readonly {
						t.Fatal("tap source mount is writable")
					}
				}
				if mount.Dest == "/input/source" && mount.Readonly {
					foundSourceMetadataMount = true
				}
				if mount.Dest == "/input/config" && mount.Readonly {
					foundTrustMount = true
				}
			}
		}
	}
	if !foundGit || execCount != 3 || !foundTapMount || !foundSourceMetadataMount || !foundTrustMount || !foundTrustEnv || !foundGitRemoval {
		t.Fatalf("graph coverage git=%v execs=%d tap_mount=%v source_metadata=%v trust_mount=%v trust_env=%v git_removal=%v", foundGit, execCount, foundTapMount, foundSourceMetadataMount, foundTrustMount, foundTrustEnv, foundGitRemoval)
	}
}

func TestVerifyExtractedTapRequiresRequestedDefaultRepository(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	valid := &catalogextractor.ExtractedTap{Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository()}}
	if err := verifyExtractedTap(valid, tap); err != nil {
		t.Fatal(err)
	}
	for name, extracted := range map[string]*catalogextractor.ExtractedTap{
		"nil":        nil,
		"wrong tap":  {Tap: catalog.TapSource{ID: "other/tools", Repository: tap.DefaultGitHubRepository()}},
		"wrong repo": {Tap: catalog.TapSource{ID: tap, Repository: "https://github.com/other/homebrew-tools"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyExtractedTap(extracted, tap); err == nil {
				t.Fatal("invalid extracted tap identity accepted")
			}
		})
	}
}

func TestAppendSolveLogsIsBounded(t *testing.T) {
	var output bytes.Buffer
	appendSolveLogs(&output, &bkclient.SolveStatus{Logs: []*bkclient.VertexLog{{Data: bytes.Repeat([]byte{'x'}, maxExtractionSolveLogBytes+1)}}})
	if output.Len() != maxExtractionSolveLogBytes {
		t.Fatalf("log size=%d want=%d", output.Len(), maxExtractionSolveLogBytes)
	}
	appendSolveLogs(&output, &bkclient.SolveStatus{Logs: []*bkclient.VertexLog{{Data: []byte("ignored")}}})
	if output.Len() != maxExtractionSolveLogBytes {
		t.Fatal("bounded log grew after reaching the limit")
	}
}
