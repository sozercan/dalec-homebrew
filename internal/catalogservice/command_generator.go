package catalogservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

const (
	GeneratedSetSchemaVersion         = "dalec-homebrew-catalog-generated-set/v1"
	MaxGeneratorOutputBytes           = catalog.MaxAggregateCatalogBytes + catalog.MaxCatalogSetBytes
	DefaultGeneratorStderrBytes int64 = 64 << 10

	GeneratorExitInvalidTap    = 20
	GeneratorExitMissingBottle = 21
	GeneratorExitPolicy        = 22
	GeneratorExitSignature     = 23
	GeneratorExitUnavailable   = 24
	GeneratorExitTimeout       = 25
)

var errCommandOutputLimit = errors.New("external catalog generator output exceeds limit")

type generatedSetDocument struct {
	SchemaVersion string                   `json:"schema_version"`
	Catalogs      []catalog.TapCatalog     `json:"catalogs"`
	Results       []catalog.PlatformResult `json:"results"`
}

// EncodeGeneratedSet returns the strict JSON document expected from an
// external command generator.
func EncodeGeneratedSet(generated *GeneratedSet) ([]byte, error) {
	if generated == nil {
		return nil, errors.New("generated set is nil")
	}
	document := generatedSetDocument{SchemaVersion: GeneratedSetSchemaVersion, Catalogs: generated.Catalogs, Results: generated.Results}
	data, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxGeneratorOutputBytes {
		return nil, fmt.Errorf("generated set exceeds %d bytes", MaxGeneratorOutputBytes)
	}
	return data, nil
}

// DecodeGeneratedSet strictly decodes one bounded external generator result.
func DecodeGeneratedSet(data []byte) (*GeneratedSet, error) {
	var document generatedSetDocument
	if err := decodeStrictJSON(data, MaxGeneratorOutputBytes, "external generator result", &document); err != nil {
		return nil, err
	}
	if document.SchemaVersion != GeneratedSetSchemaVersion {
		return nil, fmt.Errorf("unsupported generated-set schema_version %q", document.SchemaVersion)
	}
	if document.Catalogs == nil || len(document.Catalogs) == 0 || len(document.Catalogs) > catalog.MaxTaps {
		return nil, errors.New("generated-set catalogs are empty or exceed the tap limit")
	}
	if document.Results == nil || len(document.Results) == 0 || len(document.Results) > 2 {
		return nil, errors.New("generated-set results are empty or exceed the platform limit")
	}
	return &GeneratedSet{Catalogs: document.Catalogs, Results: document.Results}, nil
}

// CommandGeneratorConfig configures direct execution of a trusted generator
// binary. Path must be absolute; no shell is involved.
type CommandGeneratorConfig struct {
	Path           string
	Args           []string
	MaxOutputBytes int64
	MaxStderrBytes int64
}

type CommandGenerator struct {
	path           string
	execPath       string
	cleanupDir     string
	args           []string
	maxOutputBytes int64
	maxStderrBytes int64

	mu        sync.RWMutex
	closeOnce sync.Once
	closeErr  error
	closed    bool
}

// NewCommandGenerator validates a direct executable route.
func NewCommandGenerator(config CommandGeneratorConfig) (*CommandGenerator, error) {
	if !filepath.IsAbs(config.Path) {
		return nil, errors.New("external generator path must be absolute")
	}
	info, err := os.Lstat(config.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect external generator: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("external generator must be a non-writable executable regular non-symlink file")
	}
	if err := validateCommandFileOwner(info); err != nil {
		return nil, err
	}
	for _, argument := range config.Args {
		if bytes.IndexByte([]byte(argument), 0) >= 0 {
			return nil, errors.New("external generator argument contains NUL")
		}
	}
	maxOutput := config.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = MaxGeneratorOutputBytes
	}
	if maxOutput <= 0 || maxOutput > MaxGeneratorOutputBytes {
		return nil, fmt.Errorf("external generator output limit %d is outside 1..%d", maxOutput, MaxGeneratorOutputBytes)
	}
	maxStderr := config.MaxStderrBytes
	if maxStderr == 0 {
		maxStderr = DefaultGeneratorStderrBytes
	}
	if maxStderr <= 0 || maxStderr > DefaultGeneratorStderrBytes {
		return nil, fmt.Errorf("external generator stderr limit %d is outside 1..%d", maxStderr, DefaultGeneratorStderrBytes)
	}
	executable, _, err := openPinnedCommand(config.Path)
	if err != nil {
		return nil, fmt.Errorf("pin external generator: %w", err)
	}
	pinnedInfo, err := executable.Stat()
	if err != nil || !os.SameFile(info, pinnedInfo) {
		_ = executable.Close()
		return nil, errors.New("external generator changed during validation")
	}
	execPath, cleanupDir, err := pinGeneratorExecutable(executable)
	_ = executable.Close()
	if err != nil {
		return nil, err
	}
	return &CommandGenerator{
		path:           config.Path,
		execPath:       execPath,
		cleanupDir:     cleanupDir,
		args:           slices.Clone(config.Args),
		maxOutputBytes: maxOutput,
		maxStderrBytes: maxStderr,
	}, nil
}

func pinGeneratorExecutable(source *os.File) (string, string, error) {
	directory, err := os.MkdirTemp("", "dalec-homebrew-catalog-generator-")
	if err != nil {
		return "", "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", "", err
	}
	path := filepath.Join(directory, "generator")
	destination, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o500)
	if err != nil {
		_ = os.RemoveAll(directory)
		return "", "", err
	}
	if _, err := source.Seek(0, 0); err != nil {
		_ = destination.Close()
		_ = os.RemoveAll(directory)
		return "", "", err
	}
	_, copyErr := io.Copy(destination, source)
	syncErr := destination.Sync()
	closeErr := destination.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.RemoveAll(directory)
		return "", "", err
	}
	return path, directory, nil
}

// Close waits for in-flight Generate calls and removes the private pinned
// executable directory. It is safe to call more than once.
func (g *CommandGenerator) Close() error {
	if g == nil {
		return nil
	}
	g.closeOnce.Do(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.closed = true
		if g.cleanupDir == "" {
			return
		}
		if err := os.RemoveAll(g.cleanupDir); err != nil {
			g.closeErr = fmt.Errorf("remove pinned external generator: %w", err)
		}
	})
	return g.closeErr
}

// Generate writes canonical request JSON to stdin and strictly decodes the
// bounded GeneratedSet JSON emitted on stdout.
func (g *CommandGenerator) Generate(ctx context.Context, request *catalog.Request) (*GeneratedSet, error) {
	if g == nil {
		return nil, NewFailureError(catalog.FailureUnavailable, "external catalog generator is unavailable", nil)
	}
	canonical, err := catalog.CanonicalRequest(request)
	if err != nil {
		return nil, NewFailureError(catalog.FailurePolicy, "catalog request is invalid", err)
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return nil, NewFailureError(catalog.FailureUnavailable, "external catalog generator is unavailable", nil)
	}
	command := exec.Command(g.execPath, g.args...)
	command.WaitDelay = 5 * time.Second
	configureCommandProcessGroup(command)
	command.Stdin = bytes.NewReader(canonical)
	stdout := &boundedCommandBuffer{limit: g.maxOutputBytes}
	stderr := &boundedCommandBuffer{limit: g.maxStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	var killOnce sync.Once
	kill := func() { killOnce.Do(func() { _ = killCommandProcessGroup(command.Process) }) }
	stdout.onLimit = kill
	stderr.onLimit = kill
	if err := command.Start(); err != nil {
		return nil, NewFailureError(catalog.FailureUnavailable, "external catalog generator is unavailable", err)
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitResult:
		// A generator may not leave descendants. Terminate any process that kept
		// the process group alive after the direct child completed.
		kill()
	case <-ctx.Done():
		kill()
		waitErr = <-waitResult
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stdout.exceeded {
		return nil, NewFailureError(catalog.FailurePolicy, "external catalog generator output exceeded its limit", errCommandOutputLimit)
	}
	if stderr.exceeded {
		return nil, NewFailureError(catalog.FailureUnavailable, "external catalog generator diagnostics exceeded their limit", errCommandOutputLimit)
	}
	if waitErr != nil {
		return nil, commandFailure(waitErr)
	}
	generated, err := DecodeGeneratedSet(stdout.Bytes())
	if err != nil {
		return nil, NewFailureError(catalog.FailurePolicy, "external catalog generator returned invalid output", err)
	}
	return generated, nil
}

type boundedCommandBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
	onLimit  func()
}

func (b *boundedCommandBuffer) Write(p []byte) (int, error) {
	if b.exceeded {
		return 0, errCommandOutputLimit
	}
	remaining := b.limit - int64(b.buffer.Len())
	if remaining < int64(len(p)) {
		if remaining > 0 {
			_, _ = b.buffer.Write(p[:remaining])
		}
		b.exceeded = true
		if b.onLimit != nil {
			b.onLimit()
		}
		return len(p), errCommandOutputLimit
	}
	return b.buffer.Write(p)
}
func (b *boundedCommandBuffer) Bytes() []byte { return bytes.Clone(b.buffer.Bytes()) }

func commandFailure(err error) error {
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return NewFailureError(catalog.FailureUnavailable, "external catalog generator is unavailable", err)
	}
	code := catalog.FailureUnavailable
	message := "external catalog generator is unavailable"
	switch exitError.ExitCode() {
	case GeneratorExitInvalidTap:
		code, message = catalog.FailureInvalidTap, "external catalog generator rejected a tap"
	case GeneratorExitMissingBottle:
		code, message = catalog.FailureMissingBottle, "external catalog generator could not find a bottle"
	case GeneratorExitPolicy:
		code, message = catalog.FailurePolicy, "external catalog generator rejected the request by policy"
	case GeneratorExitSignature:
		code, message = catalog.FailureSignature, "external catalog generator rejected authenticated input"
	case GeneratorExitTimeout:
		code, message = catalog.FailureTimeout, "external catalog generator timed out"
	case GeneratorExitUnavailable:
		code, message = catalog.FailureUnavailable, "external catalog generator is unavailable"
	}
	return NewFailureError(code, message, err)
}
