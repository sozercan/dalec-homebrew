//go:build linux

package materializer

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOSRunnerQuiescesDetachedDescendants(t *testing.T) {
	runner := OSRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runner.Run(ctx, Command{Path: "/bin/sh", Args: []string{"-c", "setsid /bin/sh -c 'sleep 60' >/dev/null 2>&1 &"}, Env: []string{"PATH=/usr/bin:/bin"}, Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	pids, err := descendantPIDs(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 0 {
		t.Fatalf("installer descendants remain: %v", pids)
	}
}
