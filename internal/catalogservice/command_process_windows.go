//go:build windows

package catalogservice

import (
	"os"
	"os/exec"
)

func openPinnedCommand(path string) (*os.File, string, error) {
	file, err := os.Open(path)
	return file, path, err
}

func validateCommandFileOwner(os.FileInfo) error { return nil }

func configureCommandProcessGroup(*exec.Cmd) {}
func killCommandProcessGroup(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}
