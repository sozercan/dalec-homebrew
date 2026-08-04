//go:build !windows

package catalogservice

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func openPinnedCommand(path string) (*os.File, string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, "", errors.New("open pinned generator")
	}
	return file, "/dev/fd/3", nil
}

func validateCommandFileOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("external generator must be owned by the catalog-service user")
	}
	return nil
}

func configureCommandProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killCommandProcessGroup(process *os.Process) error {
	if process == nil {
		return nil
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
