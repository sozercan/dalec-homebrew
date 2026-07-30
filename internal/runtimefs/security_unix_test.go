//go:build linux || darwin

package runtimefs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRejectsSpecialFile(t *testing.T) {
	fx := newFixture(t)
	fifo := filepath.Join(fx.source, "Cellar/hello/1.0/var/run.pipe")
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	_, err := Assemble(fx.source, filepath.Join(t.TempDir(), "out"), fx.record, fx.opts)
	if errorCode(err) != CodeUnsafeType {
		t.Fatalf("error = %v, code = %q", err, errorCode(err))
	}
}
