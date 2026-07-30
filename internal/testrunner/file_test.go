package testrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckFileRegularFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(file, []byte("prefix value=42 suffix"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := checkFile(file, FileCheckOutput{
		CheckOutput: CheckOutput{
			Equals:     "prefix value=42 suffix",
			Contains:   []string{"value=42"},
			Matches:    []string{`value=\d+`},
			StartsWith: "prefix",
			EndsWith:   "suffix",
		},
		Permissions: 0o640,
	}); err != nil {
		t.Fatal(err)
	}

	if err := checkFile(dir, FileCheckOutput{IsDir: true}); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(dir, FileCheckOutput{}); err == nil || !strings.Contains(err.Error(), "is_dir") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckFileExistenceAndPermissionsFailures(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if err := checkFile(missing, FileCheckOutput{NotExist: true}); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(missing, FileCheckOutput{}); err == nil || !strings.Contains(err.Error(), "expected path to exist") {
		t.Fatalf("unexpected error: %v", err)
	}

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(file, FileCheckOutput{NotExist: true}); err == nil || !strings.Contains(err.Error(), "not to exist") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := checkFile(file, FileCheckOutput{Permissions: 0o644}); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := checkFile(file, FileCheckOutput{CheckOutput: CheckOutput{Contains: []string{"missing"}}}); err == nil || !strings.Contains(err.Error(), "contains") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckFileSymlinkAndNoFollow(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("target content"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Skipf("symlinks are not supported: %v", err)
	}

	if err := checkFile(link, FileCheckOutput{
		CheckOutput: CheckOutput{Contains: []string{"target"}},
		LinkTarget:  "target.txt",
	}); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(link, FileCheckOutput{NoFollow: true, LinkTarget: "target.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(link, FileCheckOutput{LinkTarget: "other"}); err == nil || !strings.Contains(err.Error(), "expected link target") {
		t.Fatalf("unexpected error: %v", err)
	}

	targetDir := filepath.Join(dir, "target-dir")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(dir, "dir-link")
	if err := os.Symlink("target-dir", dirLink); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(dirLink, FileCheckOutput{IsDir: true}); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(dirLink, FileCheckOutput{NoFollow: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckFileDanglingSymlinkExistence(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink("missing", link); err != nil {
		t.Skipf("symlinks are not supported: %v", err)
	}

	// Following the link sees a missing target.
	if err := checkFile(link, FileCheckOutput{NotExist: true}); err != nil {
		t.Fatal(err)
	}
	// NoFollow sees the symlink itself, so it exists and its link target can be checked.
	if err := checkFile(link, FileCheckOutput{NotExist: true, NoFollow: true}); err == nil || !strings.Contains(err.Error(), "not to exist") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := checkFile(link, FileCheckOutput{NoFollow: true, LinkTarget: "missing"}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckFilesUsesDeterministicPathOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a")
	second := filepath.Join(dir, "b")
	err := checkFiles(map[string]FileCheckOutput{
		second: {},
		first:  {},
	})
	if err == nil || !strings.Contains(err.Error(), first) {
		t.Fatalf("expected first sorted path in error, got %v", err)
	}
}

func TestFileContentReadIsBounded(t *testing.T) {
	file := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(file, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkFileLimited(file, FileCheckOutput{CheckOutput: CheckOutput{Contains: []string{"0"}}}, 4)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err=%v", err)
	}
}
