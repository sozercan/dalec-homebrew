//go:build !linux && !darwin

package materializer

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

func openFormulaTapDirectoryNoFollow(name string) (*os.File, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrInvalid
	}
	return os.Open(name)
}

func openFormulaTapDirectoryAtNoFollow(parent *os.File, name string) (*os.File, error) {
	return openFormulaTapDirectoryNoFollow(filepath.Join(parent.Name(), name))
}

func createFormulaTapDirectoryExclusive(parent *os.File, name string, uid, gid int) (*os.File, error) {
	full := filepath.Join(parent.Name(), name)
	if err := os.Mkdir(full, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.Open(full)
	if err != nil {
		return nil, errors.Join(err, os.Remove(full))
	}
	return dir, nil
}

func createFormulaTapRegularNoFollow(parent *os.File, name string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(filepath.Join(parent.Name(), name), os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
}

func publishFormulaTapDirectoryNoReplace(parent *os.File, oldName, newName string) error {
	if err := requireFormulaTapEntryAbsent(parent, newName); err != nil {
		return err
	}
	return os.Rename(filepath.Join(parent.Name(), oldName), filepath.Join(parent.Name(), newName))
}

func removeFormulaTapTree(parent *os.File, name string) error {
	return os.RemoveAll(filepath.Join(parent.Name(), name))
}

func syncFormulaTapDirectory(directory *os.File) error {
	return directory.Sync()
}

func setFormulaTapFileTimes(f *os.File, epoch time.Time) error {
	return os.Chtimes(f.Name(), epoch, epoch)
}
