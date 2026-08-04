// Package catalogextractor validates and canonicalizes catalog documents
// emitted by the pinned Homebrew/Ruby evaluation adapter inside the isolated
// extractor component.
package catalogextractor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

const (
	DefaultRubyScript  = "/usr/local/libexec/dalec-homebrew-catalog-extract.rb"
	maxExtractorStderr = 1 << 20
)

// PinnedHomebrewCommit is populated with -X in release extractor builds. The
// BuildKit generator passes the request commit into every offline extraction so
// an authorized but misconfigured extractor cannot evaluate a different
// Homebrew codebase.
var PinnedHomebrewCommit string

func Canonicalize(reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("nil catalog input")
	}
	data, err := io.ReadAll(io.LimitReader(reader, catalog.MaxCatalogDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > catalog.MaxCatalogDocumentBytes {
		return nil, fmt.Errorf("catalog input exceeds %d bytes", catalog.MaxCatalogDocumentBytes)
	}
	document, err := catalog.DecodeTapCatalog(data)
	if err != nil {
		return nil, err
	}
	return catalog.CanonicalTapCatalog(document)
}

func CanonicalizeFile(input, output string) error {
	if input == "" || output == "" {
		return errors.New("input and output paths are required")
	}
	in, err := os.Open(input)
	if err != nil {
		return err
	}
	defer in.Close()
	data, err := Canonicalize(in)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(output)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

// ExtractFile executes the pinned Homebrew/Ruby adapter. The enclosing
// BuildKit exec must mount tapRoot read-only at its canonical Homebrew Tap path
// and disable networking. The Go wrapper bounds output and validates its strict
// schema before publishing it to the generator.
func ExtractFile(tapID, repository, tapRoot, sourceMetadata, homebrewCommit, output string) error {
	if tapID == "" || repository == "" || tapRoot == "" || sourceMetadata == "" || homebrewCommit == "" || output == "" {
		return errors.New("tap, repository, tap root, source metadata, Homebrew commit, and output are required")
	}
	if PinnedHomebrewCommit == "" || homebrewCommit != PinnedHomebrewCommit {
		return fmt.Errorf("requested Homebrew commit %q does not match extractor %q", homebrewCommit, PinnedHomebrewCommit)
	}
	tap, err := catalog.ParseTapID(tapID)
	if err != nil || tap.IsCore() {
		return fmt.Errorf("invalid non-core tap %q", tapID)
	}
	if repository != tap.DefaultGitHubRepository() {
		return fmt.Errorf("repository %q does not match %q", repository, tap.DefaultGitHubRepository())
	}
	rootInfo, err := os.Lstat(tapRoot)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("tap root is not a real directory")
	}
	sourceData, err := os.ReadFile(sourceMetadata)
	if err != nil {
		return err
	}
	source, err := DecodeSourceMetadata(sourceData)
	if err != nil {
		return fmt.Errorf("validate tap source metadata: %w", err)
	}
	if source.Tap.ID != tap || source.Tap.Repository != repository {
		return errors.New("tap source metadata changed requested identity")
	}
	ruby, err := portableRuby()
	if err != nil {
		return err
	}
	script := os.Getenv("DALEC_HOMEBREW_CATALOG_EXTRACT_SCRIPT")
	if script == "" {
		script = DefaultRubyScript
	}
	if info, err := os.Stat(script); err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("extract script is not a regular file")
		}
		return fmt.Errorf("catalog extract script: %w", err)
	}
	command := exec.Command(ruby, "-I", filepath.Join(os.Getenv("HOMEBREW_REPOSITORY"), "Library", "Homebrew"), script, tapID, repository, tapRoot, sourceMetadata)
	command.Env = append(os.Environ(),
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_NO_ANALYTICS=1",
		"HOMEBREW_NO_INSTALL_FROM_API=1",
		"HOMEBREW_NO_ENV_HINTS=1",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &limitedWriter{writer: &stdout, remaining: catalog.MaxCatalogDocumentBytes + 1}
	command.Stderr = &limitedWriter{writer: &stderr, remaining: maxExtractorStderr}
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("evaluate tap Formulae: %s", message)
	}
	if int64(stdout.Len()) > catalog.MaxCatalogDocumentBytes {
		return fmt.Errorf("extracted tap exceeds %d bytes", catalog.MaxCatalogDocumentBytes)
	}
	extracted, err := DecodeExtractedTap(stdout.Bytes())
	if err != nil {
		return fmt.Errorf("validate extracted tap: %w", err)
	}
	if extracted.Tap.ID != tap || extracted.Tap.Repository != repository {
		return errors.New("extractor output changed requested tap identity")
	}
	data, err := jsonMarshalExtracted(extracted)
	if err != nil {
		return err
	}
	return writeExclusive(output, data)
}

// ValidateExtractedFile is the trusted post-evaluation publication step. It is
// run in a separate BuildKit exec after the Formula process tree has been
// quiesced, with the evaluator output mounted read-only.
func ValidateExtractedFile(input, sourceMetadata, tapRoot, output string) error {
	if input == "" || sourceMetadata == "" || tapRoot == "" || output == "" {
		return errors.New("extracted input, source metadata, tap root, and output are required")
	}
	data, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	extracted, err := DecodeExtractedTap(data)
	if err != nil {
		return err
	}
	sourceData, err := os.ReadFile(sourceMetadata)
	if err != nil {
		return err
	}
	source, err := DecodeSourceMetadata(sourceData)
	if err != nil {
		return err
	}
	if extracted.Tap != source.Tap {
		return errors.New("evaluator output changed authenticated tap source identity")
	}
	root, err := filepath.EvalSymlinks(tapRoot)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	seenPaths := make(map[string]struct{}, len(extracted.Formulae))
	for _, formula := range extracted.Formulae {
		if _, duplicate := seenPaths[formula.SourcePath]; duplicate {
			return fmt.Errorf("duplicate extracted Formula source path %q", formula.SourcePath)
		}
		seenPaths[formula.SourcePath] = struct{}{}
		candidate := filepath.Join(root, filepath.FromSlash(formula.SourcePath))
		info, err := os.Lstat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("extracted Formula source %q is not a real regular file", formula.SourcePath)
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("extracted Formula source %q escapes the tap", formula.SourcePath)
		}
		file, err := os.Open(candidate)
		if err != nil {
			return err
		}
		hash := sha256.New()
		count, copyErr := io.Copy(hash, io.LimitReader(file, 4<<20+1))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
		if count > 4<<20 {
			return fmt.Errorf("extracted Formula source %q exceeds 4 MiB", formula.SourcePath)
		}
		actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if actual != formula.SourceDigest {
			return fmt.Errorf("extracted Formula source %q digest %s does not match %s", formula.SourcePath, formula.SourceDigest, actual)
		}
	}
	canonical, err := jsonMarshalExtracted(extracted)
	if err != nil {
		return err
	}
	return writeExclusive(output, canonical)
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		if w.remaining > 0 {
			_, _ = w.writer.Write(data[:w.remaining])
			w.remaining = 0
		}
		return len(data), nil
	}
	w.remaining -= int64(len(data))
	return w.writer.Write(data)
}

func portableRuby() (string, error) {
	if configured := os.Getenv("DALEC_HOMEBREW_PORTABLE_RUBY"); configured != "" {
		if info, err := os.Stat(configured); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return configured, nil
		}
		return "", errors.New("configured portable Ruby is unavailable")
	}
	repository := os.Getenv("HOMEBREW_REPOSITORY")
	if repository == "" {
		repository = "/home/linuxbrew/.linuxbrew/Homebrew"
	}
	matches, err := filepath.Glob(filepath.Join(repository, "Library", "Homebrew", "vendor", "portable-ruby", "*", "bin", "ruby"))
	if err != nil {
		return "", err
	}
	for _, candidate := range matches {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("pinned portable Ruby is unavailable")
}

func jsonMarshalExtracted(extracted *ExtractedTap) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(extracted); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func MarshalExtractedTap(extracted *ExtractedTap) ([]byte, error) {
	if extracted == nil {
		return nil, errors.New("nil extracted tap")
	}
	data, err := jsonMarshalExtracted(extracted)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeExtractedTap(data); err != nil {
		return nil, err
	}
	return data, nil
}

func writeExclusive(output string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(output)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}
