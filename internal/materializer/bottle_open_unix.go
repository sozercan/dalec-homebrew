//go:build linux || darwin

package materializer

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func openBottleNoFollow(directory *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	runtime.KeepAlive(directory)
	if err != nil {
		return nil, fmt.Errorf("open bottle %q without following links: %w", name, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}
