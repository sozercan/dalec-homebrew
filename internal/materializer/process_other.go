//go:build !linux

package materializer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func installerSupervisorCommand(ctx context.Context, program string, args []string) (*exec.Cmd, func(), error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return cmd, func() {}, nil
}
