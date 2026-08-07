//go:build !linux

package catalogextractor

import "os/exec"

func runBrewAsLinuxbrew(*exec.Cmd) {}
