package testrunner

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
)

func checkFiles(files map[string]FileCheckOutput) error { return checkFilesLimited(files, 16<<20) }

func checkFilesLimited(files map[string]FileCheckOutput, maxBytes int64) error {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	for _, path := range paths {
		if err := checkFileLimited(path, files[path], maxBytes); err != nil {
			return err
		}
	}
	return nil
}

func checkFile(path string, check FileCheckOutput) error {
	return checkFileLimited(path, check, 16<<20)
}

func checkFileLimited(path string, check FileCheckOutput, maxBytes int64) error {
	info, err := fileInfo(path, check.NoFollow)
	if check.NotExist {
		switch {
		case os.IsNotExist(err):
			return nil
		case err != nil:
			return fmt.Errorf("file %q: check non-existence: %w", path, err)
		default:
			return fmt.Errorf("file %q: expected path not to exist", path)
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file %q: expected path to exist", path)
		}
		return fmt.Errorf("file %q: inspect metadata: %w", path, err)
	}

	if info.IsDir() != check.IsDir {
		return fmt.Errorf("file %q: expected is_dir=%t, got is_dir=%t", path, check.IsDir, info.IsDir())
	}
	if check.Permissions != 0 {
		expected := check.Permissions.Perm()
		actual := info.Mode().Perm()
		if actual != expected {
			return fmt.Errorf("file %q: expected permissions %04o, got %04o", path, expected, actual)
		}
	}
	if check.LinkTarget != "" {
		actual, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("file %q: read link target: %w", path, err)
		}
		if actual != check.LinkTarget {
			return fmt.Errorf("file %q: expected link target %q, got %q", path, check.LinkTarget, actual)
		}
	}
	if check.CheckOutput.configured() {
		contentInfo, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("file %q: inspect content target: %w", path, err)
		}
		if !contentInfo.Mode().IsRegular() {
			return fmt.Errorf("file %q: content checks require a regular file", path)
		}
		if contentInfo.Size() > maxBytes {
			return fmt.Errorf("file %q: content size %d exceeds %d bytes", path, contentInfo.Size(), maxBytes)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("file %q: read content: %w", path, err)
		}
		data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("file %q: read content: %w", path, err)
		}
		if closeErr != nil {
			return closeErr
		}
		if int64(len(data)) > maxBytes {
			return fmt.Errorf("file %q: content exceeds %d bytes", path, maxBytes)
		}
		if err := checkOutput("file "+fmt.Sprintf("%q content", path), data, check.CheckOutput); err != nil {
			return err
		}
	}
	return nil
}

func fileInfo(path string, noFollow bool) (fs.FileInfo, error) {
	if noFollow {
		return os.Lstat(path)
	}
	return os.Stat(path)
}
