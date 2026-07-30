//go:build linux

package materializer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const installerSupervisorArg = "__dalec_homebrew_install_supervisor"

func init() {
	if len(os.Args) > 1 && os.Args[1] == installerSupervisorArg {
		os.Exit(runInstallerSupervisor(os.Args[2:]))
	}
}

func installerSupervisorCommand(ctx context.Context, program string, args []string) (*exec.Cmd, func(), error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	supervisorArgs := append([]string{installerSupervisorArg, program}, args...)
	cmd := exec.CommandContext(ctx, executable, supervisorArgs...)
	cmd.ExtraFiles = []*os.File{reader}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	cleanup := func() { _ = reader.Close(); _ = writer.Close() }
	return cmd, cleanup, nil
}

func runInstallerSupervisor(args []string) int {
	if len(args) == 0 {
		return 127
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "installer supervisor:", err)
		return 125
	}
	parentPipe := os.NewFile(3, "materializer-parent")
	if parentPipe == nil {
		return 125
	}
	unix.CloseOnExec(3)
	parentGone := make(chan struct{}, 1)
	go func() { _, _ = io.Copy(io.Discard, parentPipe); parentGone <- struct{}{} }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	child := exec.Command(args[0], args[1:]...)
	child.Env = os.Environ()
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "installer supervisor:", err)
		return 127
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	cancelled := false
	var waitErr error
	select {
	case waitErr = <-done:
	case <-parentGone:
		cancelled = true
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		waitErr = <-done
	case <-signals:
		cancelled = true
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		waitErr = <-done
	}
	if err := quiesceScopedDescendants(os.Getpid(), child.Process.Pid); err != nil {
		fmt.Fprintln(os.Stderr, "installer supervisor:", err)
		return 125
	}
	if cancelled {
		return 124
	}
	if waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "installer supervisor:", waitErr)
	return 125
}

func quiesceScopedDescendants(supervisorPID, childPID int) error {
	_ = syscall.Kill(-childPID, syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if pid > 0 {
				continue
			}
			if errors.Is(err, syscall.ECHILD) {
				return nil
			}
			if err != nil {
				return err
			}
			break
		}
		pids, err := descendantPIDs(supervisorPID)
		if err != nil {
			return err
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("installer descendants did not quiesce: %v", pids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func descendantPIDs(root int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	parents := map[int]int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		text := string(data)
		end := strings.LastIndex(text, ")")
		if end < 0 {
			continue
		}
		fields := strings.Fields(text[end+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err == nil {
			parents[pid] = ppid
		}
	}
	var out []int
	known := map[int]bool{root: true}
	changed := true
	for changed {
		changed = false
		for pid, ppid := range parents {
			if known[pid] || !known[ppid] {
				continue
			}
			known[pid] = true
			out = append(out, pid)
			changed = true
		}
	}
	return out, nil
}
