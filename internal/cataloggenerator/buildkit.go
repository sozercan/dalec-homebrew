package cataloggenerator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogextractor"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

const (
	extractedTapFilename   = "extracted-tap.json"
	untrustedTapFilename   = "untrusted-extracted-tap.json"
	sourceMetadataFilename = "source.json"
)

type TapExtractor interface {
	Extract(context.Context, catalog.TapID) (*catalogextractor.ExtractedTap, error)
}

type BuildKitExtractorConfig struct {
	Address        string
	ExtractorRef   string
	HomebrewCommit string
	TapCommits     map[catalog.TapID]string
}

// BuildKitExtractor separates the networked public Git source operation from a
// network-disabled, read-only extractor exec on a dedicated BuildKit worker.
type BuildKitExtractor struct {
	client         *bkclient.Client
	extractorRef   string
	homebrewCommit string
	tapCommits     map[catalog.TapID]string
	closeOnce      sync.Once
	closeErr       error
}

func NewBuildKitExtractor(ctx context.Context, cfg BuildKitExtractorConfig) (*BuildKitExtractor, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("BuildKit address is required")
	}
	if err := resolution.ValidatePinnedReference(cfg.ExtractorRef); err != nil {
		return nil, fmt.Errorf("catalog extractor reference: %w", err)
	}
	if len(cfg.HomebrewCommit) != 40 {
		return nil, errors.New("release-pinned Homebrew commit is required")
	}
	tapCommits, err := copyTapCommits(cfg.TapCommits)
	if err != nil {
		return nil, err
	}
	client, err := bkclient.New(ctx, cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("connect dedicated BuildKit worker: %w", err)
	}
	return &BuildKitExtractor{client: client, extractorRef: cfg.ExtractorRef, homebrewCommit: cfg.HomebrewCommit, tapCommits: tapCommits}, nil
}

func (e *BuildKitExtractor) Close() error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		if e.client != nil {
			e.closeErr = e.client.Close()
		}
	})
	return e.closeErr
}

func (e *BuildKitExtractor) Extract(ctx context.Context, tap catalog.TapID) (*catalogextractor.ExtractedTap, error) {
	if e == nil || e.client == nil {
		return nil, errors.New("BuildKit extractor is unavailable")
	}
	if err := tap.Validate(); err != nil || tap.IsCore() {
		return nil, fmt.Errorf("invalid non-core tap %q", tap)
	}
	output, err := e.extractionState(tap)
	if err != nil {
		return nil, err
	}
	definition, err := output.Marshal(ctx)
	if err != nil {
		return nil, fmt.Errorf("marshal tap extraction: %w", err)
	}
	outputDir, err := os.MkdirTemp("", "dalec-homebrew-catalog-extract-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(outputDir)
	status := make(chan *bkclient.SolveStatus)
	statusDone := make(chan struct{})
	var solveLogs bytes.Buffer
	go func() {
		for update := range status {
			appendSolveLogs(&solveLogs, update)
		}
		close(statusDone)
	}()
	_, err = e.client.Solve(ctx, definition, bkclient.SolveOpt{Exports: []bkclient.ExportEntry{{Type: bkclient.ExporterLocal, OutputDir: outputDir}}}, status)
	<-statusDone
	if err != nil {
		message := strings.TrimSpace(solveLogs.String())
		if message != "" {
			return nil, fmt.Errorf("solve isolated tap extraction: %w: %s", err, message)
		}
		return nil, fmt.Errorf("solve isolated tap extraction: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(outputDir, extractedTapFilename))
	if err != nil {
		return nil, fmt.Errorf("read extracted tap: %w", err)
	}
	extracted, err := catalogextractor.DecodeExtractedTap(data)
	if err != nil {
		return nil, fmt.Errorf("decode isolated tap extraction: %w", err)
	}
	if err := e.verifyExtractedTap(tap, extracted); err != nil {
		return nil, err
	}
	return extracted, nil
}

func (e *BuildKitExtractor) verifyExtractedTap(tap catalog.TapID, extracted *catalogextractor.ExtractedTap) error {
	if extracted == nil {
		return errors.New("isolated tap extraction returned no result")
	}
	if extracted.Tap.ID != tap || extracted.Tap.Repository != tap.DefaultGitHubRepository() {
		return errors.New("isolated tap extraction changed requested identity")
	}
	if commit, pinned := e.tapCommits[tap]; pinned && extracted.Tap.Commit != commit {
		return fmt.Errorf("isolated tap extraction commit %q does not match requested pin %q for %s", extracted.Tap.Commit, commit, tap)
	}
	return nil
}

const maxExtractionSolveLogBytes = 1 << 20

func appendSolveLogs(output *bytes.Buffer, status *bkclient.SolveStatus) {
	if output == nil || status == nil || output.Len() >= maxExtractionSolveLogBytes {
		return
	}
	for _, entry := range status.Logs {
		if entry == nil || len(entry.Data) == 0 {
			continue
		}
		remaining := maxExtractionSolveLogBytes - output.Len()
		data := entry.Data
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = output.Write(data)
		if output.Len() >= maxExtractionSolveLogBytes {
			return
		}
	}
}

func linuxbrewWritableScratch() llb.State {
	return llb.Scratch().File(llb.Mkdir("/data", 0o700, llb.WithUIDGID(1000, 1000)))
}

func (e *BuildKitExtractor) extractionState(tap catalog.TapID) (llb.State, error) {
	if e == nil || e.extractorRef == "" || e.homebrewCommit == "" {
		return llb.Scratch(), errors.New("BuildKit extractor is unavailable")
	}
	if err := tap.Validate(); err != nil || tap.IsCore() {
		return llb.Scratch(), fmt.Errorf("invalid non-core tap %q", tap)
	}
	repository := tap.DefaultGitHubRepository()
	gitURL := repository + ".git"
	tapRoot := "/home/linuxbrew/.linuxbrew/Homebrew/Library/Taps/" + tap.Owner() + "/homebrew-" + tap.Name()
	// BuildKit's Git source resolves an unpinned tap's default branch HEAD, or
	// fetches the configured exact commit. Keep .git so the offline extractor can
	// independently bind the observed commit and deterministic tree/archive digests.
	source := llb.Git(gitURL, e.tapCommits[tap], llb.KeepGitDir(), llb.GitSkipSubmodules(), llb.AuthTokenSecret(""), llb.AuthHeaderSecret(""))
	worker := llb.Image(e.extractorRef)
	metadataRun := worker.Run(
		llb.Args([]string{
			"/usr/local/bin/dalec-homebrew-catalog-extractor", "source-metadata",
			"--tap", string(tap),
			"--repository", repository,
			"--tap-root", "/input/tap",
			"--output", "/out/" + sourceMetadataFilename,
		}),
		llb.User("0:0"),
		llb.ReadonlyRootFS(),
		llb.Network(pb.NetMode_NONE),
		llb.AddMount("/input/tap", source, llb.Readonly),
		llb.AddMount("/out", llb.Scratch()),
		llb.AddMount("/tmp", llb.Scratch()),
		llb.AddMount("/var/tmp", llb.Scratch()),
		llb.WithCustomName("bind Homebrew tap source "+string(tap)),
	)
	metadataState := metadataRun.GetMount("/out")
	cleanSource := source.File(llb.Rm("/.git"))
	trustBytes, err := json.Marshal(struct {
		TrustedTaps []string `json:"trustedtaps"`
	}{TrustedTaps: []string{string(tap)}})
	if err != nil {
		return llb.Scratch(), err
	}
	trustState := llb.Scratch().File(llb.Mkfile("/trust.json", 0o444, append(trustBytes, '\n')))
	tmpState := linuxbrewWritableScratch()
	varTmpState := linuxbrewWritableScratch()
	cacheState := linuxbrewWritableScratch()
	evaluator := worker.AddEnv("HOMEBREW_USER_CONFIG_HOME", "/input/config").AddEnv("HOMEBREW_REQUIRE_TAP_TRUST", "1")
	run := evaluator.Run(
		llb.Args([]string{
			"/usr/local/bin/dalec-homebrew-catalog-extractor", "extract",
			"--tap", string(tap),
			"--repository", repository,
			"--tap-root", tapRoot,
			"--source-metadata", "/input/source/" + sourceMetadataFilename,
			"--homebrew-commit", e.homebrewCommit,
			"--output", "/out/" + untrustedTapFilename,
		}),
		llb.User("0:0"),
		llb.ReadonlyRootFS(),
		llb.Network(pb.NetMode_NONE),
		llb.AddMount(tapRoot, cleanSource, llb.Readonly),
		llb.AddMount("/input/source", metadataState, llb.Readonly),
		llb.AddMount("/input/config", trustState, llb.Readonly),
		llb.AddMount("/out", llb.Scratch()),
		llb.AddMount("/tmp", tmpState, llb.SourcePath("/data")),
		llb.AddMount("/var/tmp", varTmpState, llb.SourcePath("/data")),
		llb.AddMount("/home/linuxbrew/.cache", cacheState, llb.SourcePath("/data")),
		llb.WithCustomName("extract Homebrew tap "+string(tap)),
	)
	untrustedState := run.GetMount("/out")
	finalize := worker.Run(
		llb.Args([]string{
			"/usr/local/bin/dalec-homebrew-catalog-extractor", "validate-extracted",
			"--input", "/input/evaluator/" + untrustedTapFilename,
			"--source-metadata", "/input/source/" + sourceMetadataFilename,
			"--tap-root", "/input/tap",
			"--output", "/out/" + extractedTapFilename,
		}),
		llb.User("0:0"),
		llb.ReadonlyRootFS(),
		llb.Network(pb.NetMode_NONE),
		llb.AddMount("/input/evaluator", untrustedState, llb.Readonly),
		llb.AddMount("/input/source", metadataState, llb.Readonly),
		llb.AddMount("/input/tap", cleanSource, llb.Readonly),
		llb.AddMount("/out", llb.Scratch()),
		llb.AddMount("/tmp", llb.Scratch()),
		llb.WithCustomName("validate extracted Homebrew tap "+string(tap)),
	)
	return finalize.GetMount("/out"), nil
}
