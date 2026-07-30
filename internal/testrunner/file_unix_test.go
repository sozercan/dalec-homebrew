//go:build !windows

package testrunner

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestContentCheckRejectsFIFOWithoutOpening(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkFileLimited(fifo, FileCheckOutput{CheckOutput: CheckOutput{Empty: true}}, 1024)
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("err=%v", err)
	}
}
