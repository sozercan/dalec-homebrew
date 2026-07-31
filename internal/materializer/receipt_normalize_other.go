//go:build !linux && !darwin

package materializer

import (
	"os"
	"path/filepath"
	"time"
)

func openReceiptKegDirectoryNoFollow(prefix, formula, pkgVersion string) (*os.File, error) {
	name := filepath.Join(prefix, "Cellar", formula, pkgVersion)
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrInvalid
	}
	return os.Open(name)
}

func createReceiptTemporaryNoFollow(directory *os.File, name string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(filepath.Join(directory.Name(), name), os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
}

func setReceiptFileTimes(file *os.File, epoch time.Time) error {
	return os.Chtimes(file.Name(), epoch, epoch)
}

func replaceReceiptAtomic(directory *os.File, oldName, newName string) error {
	return os.Rename(filepath.Join(directory.Name(), oldName), filepath.Join(directory.Name(), newName))
}

func removeReceiptTemporary(directory *os.File, name string) error {
	return os.Remove(filepath.Join(directory.Name(), name))
}

func syncReceiptDirectory(directory *os.File) error { return directory.Sync() }
