//go:build windows

package testrunner

import (
	"os/exec"
	"time"
)

func configureCommand(cmd *exec.Cmd, waitDelay time.Duration) { cmd.WaitDelay = waitDelay }

func terminateCommandGroup(cmd *exec.Cmd) {}
