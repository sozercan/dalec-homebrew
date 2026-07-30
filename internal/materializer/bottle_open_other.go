//go:build !linux && !darwin

package materializer

import (
	"os"
)

func openBottleNoFollow(directory *os.File, name string) (*os.File, error) {
	root, err := os.OpenRoot(directory.Name())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrInvalid
	}
	return root.Open(name)
}
