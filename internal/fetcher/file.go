package fetcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FetchToFiles fetches into a private temporary file, verifies the exact size
// and digest, and only then publishes the bottle and evidence with mode 0444.
// Existing output paths are never replaced.
func (fetcher *Fetcher) FetchToFiles(ctx context.Context, request Request, bottlePath, evidencePath string) (Evidence, error) {
	if fetcher == nil {
		return Evidence{}, errors.New("nil fetcher")
	}
	if ctx == nil {
		return Evidence{}, errors.New("nil fetch context")
	}
	validated, err := validateRequest(request)
	if err != nil {
		return Evidence{}, err
	}
	bottlePath, evidencePath, err = validateOutputPaths(bottlePath, evidencePath)
	if err != nil {
		return Evidence{}, err
	}
	if err := requireAbsent(bottlePath); err != nil {
		return Evidence{}, err
	}
	if err := requireAbsent(evidencePath); err != nil {
		return Evidence{}, err
	}

	bottleTemp, err := os.CreateTemp(filepath.Dir(bottlePath), ".bottle-fetch-*")
	if err != nil {
		return Evidence{}, fmt.Errorf("create temporary bottle output: %w", err)
	}
	bottleTempPath := bottleTemp.Name()
	defer os.Remove(bottleTempPath)

	fetchCtx, cancel := context.WithTimeout(ctx, fetcher.timeouts.Overall)
	defer cancel()
	evidence, fetchErr := fetcher.fetch(fetchCtx, validated, bottleTemp, true)
	if fetchErr != nil {
		_ = bottleTemp.Close()
		return Evidence{}, fetchErr
	}
	if err := sealFile(bottleTemp); err != nil {
		return Evidence{}, fmt.Errorf("seal bottle output: %w", err)
	}

	evidenceBytes, err := MarshalEvidence(evidence)
	if err != nil {
		return Evidence{}, fmt.Errorf("marshal fetch evidence: %w", err)
	}
	evidenceBytes = append(evidenceBytes, '\n')
	evidenceTemp, err := os.CreateTemp(filepath.Dir(evidencePath), ".bottle-evidence-*")
	if err != nil {
		return Evidence{}, fmt.Errorf("create temporary evidence output: %w", err)
	}
	evidenceTempPath := evidenceTemp.Name()
	defer os.Remove(evidenceTempPath)
	if _, err := evidenceTemp.Write(evidenceBytes); err != nil {
		_ = evidenceTemp.Close()
		return Evidence{}, fmt.Errorf("write fetch evidence: %w", err)
	}
	if err := sealFile(evidenceTemp); err != nil {
		return Evidence{}, fmt.Errorf("seal fetch evidence: %w", err)
	}

	if err := publishNoReplace(bottleTempPath, bottlePath); err != nil {
		return Evidence{}, fmt.Errorf("publish bottle output: %w", err)
	}
	publishedBottle := true
	defer func() {
		if publishedBottle {
			_ = os.Remove(bottlePath)
		}
	}()
	if err := publishNoReplace(evidenceTempPath, evidencePath); err != nil {
		return Evidence{}, fmt.Errorf("publish fetch evidence: %w", err)
	}
	publishedBottle = false
	return evidence, nil
}

func validateOutputPaths(bottlePath, evidencePath string) (string, string, error) {
	if bottlePath == "" || evidencePath == "" {
		return "", "", errors.New("bottle and evidence output paths are required")
	}
	absoluteBottle, err := filepath.Abs(bottlePath)
	if err != nil {
		return "", "", fmt.Errorf("resolve bottle output path: %w", err)
	}
	absoluteEvidence, err := filepath.Abs(evidencePath)
	if err != nil {
		return "", "", fmt.Errorf("resolve evidence output path: %w", err)
	}
	if filepath.Clean(absoluteBottle) == filepath.Clean(absoluteEvidence) {
		return "", "", errors.New("bottle and evidence output paths must differ")
	}
	for _, path := range []string{absoluteBottle, absoluteEvidence} {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			return "", "", fmt.Errorf("inspect output directory: %w", err)
		}
		if !info.IsDir() {
			return "", "", errors.New("output parent is not a directory")
		}
	}
	return absoluteBottle, absoluteEvidence, nil
}

func requireAbsent(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("output path %q already exists", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output path %q: %w", path, err)
	}
	return nil
}

func sealFile(file *os.File) error {
	if err := file.Chmod(0o444); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func publishNoReplace(source, target string) error {
	// Hard-link publication is atomic and, unlike Rename, fails if target was
	// created after the preflight check. Source and target are intentionally in
	// the same directory.
	if filepath.Dir(source) != filepath.Dir(target) {
		return errors.New("temporary and final output paths are on different directories")
	}
	if err := os.Link(source, target); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		_ = os.Remove(target)
		return err
	}
	return nil
}
