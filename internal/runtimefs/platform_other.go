//go:build !linux

package runtimefs

import (
	"fmt"
	"os"
	"reflect"
	"time"
)

func openRegularNoFollow(filename string) (*os.File, error) {
	before, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("path changed while opening")
	}
	return f, nil
}

func checkSecurityXattrs(string, bool) error { return nil }

func inodeKey(info os.FileInfo) string {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return ""
	}
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return ""
	}
	dev := value.FieldByName("Dev")
	ino := value.FieldByName("Ino")
	nlink := value.FieldByName("Nlink")
	if !dev.IsValid() || !ino.IsValid() || !nlink.IsValid() || numericValue(nlink) <= 1 {
		return ""
	}
	return fmt.Sprintf("%d:%d", numericValue(dev), numericValue(ino))
}

func fileOwner(info os.FileInfo) (uid, gid int, ok bool) {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return 0, 0, false
	}
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return 0, 0, false
	}
	u := value.FieldByName("Uid")
	g := value.FieldByName("Gid")
	if !u.IsValid() || !g.IsValid() {
		return 0, 0, false
	}
	return int(numericValue(u)), int(numericValue(g)), true
}

func numericValue(value reflect.Value) uint64 {
	switch value.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(value.Int())
	default:
		return 0
	}
}

func setPathMtime(filename string, epoch time.Time, symlink bool) error {
	if symlink {
		// os.Chtimes follows symlinks. Non-Linux development hosts skip link
		// timestamps; production assembly is Linux and uses AT_SYMLINK_NOFOLLOW.
		return nil
	}
	return os.Chtimes(filename, epoch, epoch)
}
