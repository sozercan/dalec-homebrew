//go:build linux || darwin

package materializer

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

func openFormulaTapDirectoryNoFollow(name string) (*os.File, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open core tap directory without following links: %w", err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openFormulaTapDirectoryAtNoFollow(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func createFormulaTapDirectoryExclusive(parent *os.File, name string, uid, gid int) (*os.File, error) {
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil {
		runtime.KeepAlive(parent)
		return nil, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	runtime.KeepAlive(parent)
	if err != nil {
		cleanupErr := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
		runtime.KeepAlive(parent)
		return nil, errors.Join(err, cleanupErr)
	}
	dir := os.NewFile(uintptr(fd), name)
	info, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		cleanupErr := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
		runtime.KeepAlive(parent)
		return nil, errors.Join(err, cleanupErr)
	}
	actualUID, actualGID, known := snapshotOwnership(info)
	if !known {
		_ = dir.Close()
		cleanupErr := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
		runtime.KeepAlive(parent)
		return nil, errors.Join(fmt.Errorf("Formula staging directory ownership is unavailable"), cleanupErr)
	}
	if int(actualUID) != uid || int(actualGID) != gid {
		if err := dir.Chown(uid, gid); err != nil {
			_ = dir.Close()
			cleanupErr := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
			runtime.KeepAlive(parent)
			return nil, errors.Join(err, cleanupErr)
		}
	}
	return dir, nil
}

func createFormulaTapRegularNoFollow(parent *os.File, name string, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, uint32(mode.Perm()))
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func publishFormulaTapDirectoryNoReplace(parent *os.File, oldName, newName string) error {
	if err := requireFormulaTapEntryAbsent(parent, newName); err != nil {
		return err
	}
	err := unix.Renameat(int(parent.Fd()), oldName, int(parent.Fd()), newName)
	runtime.KeepAlive(parent)
	return err
}

func removeFormulaTapTree(parent *os.File, name string) error {
	dir, err := openFormulaTapDirectoryAtNoFollow(parent, name)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			err = unix.Unlinkat(int(parent.Fd()), name, 0)
			runtime.KeepAlive(parent)
			return err
		}
		return err
	}
	if err := dir.Chmod(0o700); err != nil {
		_ = dir.Close()
		return err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		_ = dir.Close()
		return err
	}
	for _, entry := range entries {
		if err := removeFormulaTapTree(dir, entry.Name()); err != nil {
			_ = dir.Close()
			return err
		}
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	err = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
	runtime.KeepAlive(parent)
	return err
}

func syncFormulaTapDirectory(directory *os.File) error {
	return directory.Sync()
}

func setFormulaTapFileTimes(f *os.File, epoch time.Time) error {
	tv := unix.NsecToTimeval(epoch.UnixNano())
	err := unix.Futimes(int(f.Fd()), []unix.Timeval{tv, tv})
	runtime.KeepAlive(f)
	return err
}
