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
					t.Fatalf("unpinned Git source identifier=%q", source.Identifier)
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

func TestExtractionGraphSelectsPinnedTapCommit(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	commit := strings.Repeat("c", 40)
	tap, _ := catalog.ParseTapID("acme/tools")
	extractor := &BuildKitExtractor{
		extractorRef:   "ghcr.io/example/catalog-extractor@" + digest,
		homebrewCommit: strings.Repeat("b", 40),
		tapCommits:     map[catalog.TapID]string{tap: commit},
	}
	state, err := extractor.extractionState(tap)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := state.Marshal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "git://github.com/acme/homebrew-tools.git#" + commit
	for _, raw := range definition.Def {
		var op pb.Op
		if err := op.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if source := op.GetSource(); source != nil && strings.Contains(source.Identifier, "github.com/acme/homebrew-tools.git") {
			if source.Identifier != want {
				t.Fatalf("Git source identifier=%q want=%q", source.Identifier, want)
			}
			return
		}
	}
	t.Fatal("pinned Git source not found")
}

func TestBuildKitExtractorVerifiesPinnedTapCommit(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	commit := strings.Repeat("c", 40)
	extracted := &catalogextractor.ExtractedTap{Tap: catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: commit}}
	if err := (&BuildKitExtractor{}).verifyExtractedTap(tap, extracted); err != nil {
		t.Fatalf("unpinned verification failed: %v", err)
	}
	extractor := &BuildKitExtractor{tapCommits: map[catalog.TapID]string{tap: commit}}
	if err := extractor.verifyExtractedTap(tap, extracted); err != nil {
		t.Fatalf("matching pin verification failed: %v", err)
	}
	extracted.Tap.Commit = strings.Repeat("d", 40)
	if err := extractor.verifyExtractedTap(tap, extracted); err == nil || !strings.Contains(err.Error(), "does not match requested pin") {
		t.Fatalf("mismatched pin error=%v", err)
	}
}

func TestNewBuildKitExtractorCopiesTapCommits(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	commit := strings.Repeat("c", 40)
	pins := map[catalog.TapID]string{tap: commit}
	extractor, err := NewBuildKitExtractor(context.Background(), BuildKitExtractorConfig{
		Address:        "unix:///definitely-missing-buildkit.sock",
		ExtractorRef:   "ghcr.io/example/catalog-extractor@sha256:" + strings.Repeat("a", 64),
		HomebrewCommit: strings.Repeat("b", 40),
		TapCommits:     pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := extractor.Close(); err != nil {
			t.Errorf("close extractor: %v", err)
		}
	})
	pins[tap] = strings.Repeat("d", 40)
	if got := extractor.tapCommits[tap]; got != commit {
		t.Fatalf("copied pin=%q want=%q", got, commit)
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
