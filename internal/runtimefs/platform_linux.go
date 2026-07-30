//go:build linux

package runtimefs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func openRegularNoFollow(filename string) (*os.File, error) {
	fd, err := unix.Open(filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filename), nil
}

func checkSecurityXattrs(filename string, symlink bool) error {
	var size int
	var err error
	if symlink {
		size, err = unix.Llistxattr(filename, nil)
	} else {
		size, err = unix.Listxattr(filename, nil)
	}
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENODATA) {
			return nil
		}
		return fmt.Errorf("list xattrs: %w", err)
	}
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	if symlink {
		size, err = unix.Llistxattr(filename, buf)
	} else {
		size, err = unix.Listxattr(filename, buf)
	}
	if err != nil {
		return fmt.Errorf("read xattrs: %w", err)
	}
	for start := 0; start < size; {
		end := start
		for end < size && buf[end] != 0 {
			end++
		}
		name := string(buf[start:end])
		if name == "security.capability" || len(name) >= len("security.") && name[:len("security.")] == "security." {
			return fmt.Errorf("security xattr %q is forbidden", name)
		}
		start = end + 1
	}
	return nil
}

func inodeKey(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

func fileOwner(info os.FileInfo) (uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

func setPathMtime(filename string, epoch time.Time, symlink bool) error {
	times := []unix.Timespec{unix.NsecToTimespec(epoch.UnixNano()), unix.NsecToTimespec(epoch.UnixNano())}
	flags := 0
	if symlink {
		flags = unix.AT_SYMLINK_NOFOLLOW
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, filename, times, flags)
}
