//go:build linux

package catalogextractor

import (
	"os/exec"
	"syscall"
)

func runBrewAsLinuxbrew(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
}
