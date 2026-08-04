package catalogextractor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

const (
	SourceMetadataSchemaVersion       = "dalec-homebrew-tap-source/v1"
	maxTreeListingBytes         int64 = 64 << 20
	maxTapArchiveBytes          int64 = 1 << 30
	maxTapEntries                     = 250_000
)

type SourceMetadata struct {
	SchemaVersion string            `json:"schema_version"`
	Tap           catalog.TapSource `json:"tap"`
}

func WriteSourceMetadata(ctx context.Context, tapID, repository, tapRoot, output string) error {
	if ctx == nil || tapID == "" || repository == "" || tapRoot == "" || output == "" {
		return errors.New("context, tap, repository, tap root, and output are required")
	}
	tap, err := catalog.ParseTapID(tapID)
	if err != nil || tap.IsCore() || repository != tap.DefaultGitHubRepository() {
		return fmt.Errorf("invalid public tap source %q", tapID)
	}
	if info, err := os.Lstat(tapRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("tap root is not a real directory")
		}
		return err
	}
	commitBytes, err := gitOutput(ctx, maxTreeListingBytes, tapRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(string(commitBytes))
	if !validCommit(commit) {
		return errors.New("resolved tap HEAD is not a lowercase 40-character commit")
	}
	tree, err := gitOutput(ctx, maxTreeListingBytes, tapRoot, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	if err != nil {
		return err
	}
	if entries := bytes.Count(tree, []byte{0}); entries > maxTapEntries {
		return fmt.Errorf("tap tree contains %d entries, maximum is %d", entries, maxTapEntries)
	}
	treeSum := sha256.Sum256(tree)
	archiveDigest, err := gitArchiveDigest(ctx, tapRoot)
	if err != nil {
		return err
	}
	metadata := SourceMetadata{SchemaVersion: SourceMetadataSchemaVersion, Tap: catalog.TapSource{ID: tap, Repository: repository, Commit: commit, TreeDigest: "sha256:" + hex.EncodeToString(treeSum[:]), ArchiveDigest: archiveDigest}}
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return writeExclusive(output, data)
}

func DecodeSourceMetadata(data []byte) (*SourceMetadata, error) {
	if len(data) == 0 || int64(len(data)) > 1<<20 {
		return nil, errors.New("tap source metadata size is outside 1..1MiB")
	}
	if err := validateUniqueJSON(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var value SourceMetadata
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple tap source metadata values")
		}
		return nil, err
	}
	if value.SchemaVersion != SourceMetadataSchemaVersion {
		return nil, fmt.Errorf("unsupported tap source metadata schema %q", value.SchemaVersion)
	}
	probe := &catalog.TapCatalog{SchemaVersion: catalog.TapCatalogSchemaVersion, Tap: value.Tap, PublishedAt: catalogValidationTime, Sequence: 1, Formulae: []catalog.Formula{{ID: catalog.FormulaID(string(value.Tap.ID) + "/probe"), Name: "probe", HomebrewFullName: string(value.Tap.ID) + "/probe", SourcePath: "Formula/probe.rb", SourceDigest: value.Tap.TreeDigest, StableVersion: "1"}}}
	if err := catalog.ValidateTapCatalog(probe); err != nil {
		return nil, fmt.Errorf("tap source metadata: %w", err)
	}
	return &value, nil
}

func gitOutput(ctx context.Context, limit int64, tapRoot string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "/usr/bin/git", append([]string{"-C", tapRoot}, args...)...)
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	var stdout, stderr bytes.Buffer
	command.Stdout = &limitedWriter{writer: &stdout, remaining: limit + 1}
	command.Stderr = &limitedWriter{writer: &stderr, remaining: 1 << 20}
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s failed: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	if int64(stdout.Len()) > limit {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", args[0], limit)
	}
	return stdout.Bytes(), nil
}

func gitArchiveDigest(ctx context.Context, tapRoot string) (string, error) {
	command := exec.CommandContext(ctx, "/usr/bin/git", "-C", tapRoot, "archive", "--format=tar", "HEAD")
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	command.Stderr = &limitedWriter{writer: &stderr, remaining: 1 << 20}
	if err := command.Start(); err != nil {
		return "", err
	}
	digest := sha256.New()
	limited := &io.LimitedReader{R: stdout, N: maxTapArchiveBytes + 1}
	count, copyErr := io.Copy(digest, limited)
	if copyErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return "", copyErr
	}
	if count > maxTapArchiveBytes {
		_ = command.Process.Kill()
		_ = command.Wait()
		return "", fmt.Errorf("tap archive exceeds %d bytes", maxTapArchiveBytes)
	}
	waitErr := command.Wait()
	if waitErr != nil {
		return "", fmt.Errorf("git archive failed: %s", strings.TrimSpace(stderr.String()))
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func validCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range []byte(value) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
