//go:build linux || darwin

package materializer

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

func openReceiptKegDirectoryNoFollow(prefix, formula, pkgVersion string) (*os.File, error) {
	fd, err := unix.Open(prefix, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), prefix)
	for _, component := range []string{"Cellar", formula, pkgVersion} {
		nextFD, openErr := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		runtime.KeepAlive(current)
		if openErr != nil {
			_ = current.Close()
			return nil, fmt.Errorf("open path component %q without following links: %w", component, openErr)
		}
		next := os.NewFile(uintptr(nextFD), component)
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, err
		}
		current = next
	}
	return current, nil
}

func createReceiptTemporaryNoFollow(directory *os.File, name string, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, uint32(mode.Perm()))
	runtime.KeepAlive(directory)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func setReceiptFileTimes(file *os.File, epoch time.Time) error {
	tv := unix.NsecToTimeval(epoch.UnixNano())
	err := unix.Futimes(int(file.Fd()), []unix.Timeval{tv, tv})
	runtime.KeepAlive(file)
	return err
}

func replaceReceiptAtomic(directory *os.File, oldName, newName string) error {
	err := unix.Renameat(int(directory.Fd()), oldName, int(directory.Fd()), newName)
	runtime.KeepAlive(directory)
	return err
}

func removeReceiptTemporary(directory *os.File, name string) error {
	err := unix.Unlinkat(int(directory.Fd()), name, 0)
	runtime.KeepAlive(directory)
	return err
}

func syncReceiptDirectory(directory *os.File) error {
	err := unix.Fsync(int(directory.Fd()))
	runtime.KeepAlive(directory)
	return err
}
