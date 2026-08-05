// Package catalogartifactstore persists immutable generated artifacts for the
// catalog service.
package catalogartifactstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

const (
	// HTTPPathPrefix is the digest-addressed catalog-service artifact route.
	HTTPPathPrefix = "/v1/artifacts/sha256/"

	artifactDirectoryName = "artifacts"
	sha256DirectoryName   = "sha256"
	privateDirectoryMode  = 0o700
	artifactFileMode      = 0o400
)

// Store is a private content-addressed store rooted below a catalog-service
// store directory. Store values are safe for concurrent Put and Open calls.
type Store struct {
	root      string
	directory string
}

// Artifact is an opened, digest-verified immutable artifact.
type Artifact struct {
	file   *os.File
	digest digest.Digest
	size   int64
}

// New opens or creates the private artifacts/sha256 store below root.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("catalog artifact store root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog artifact store root: %w", err)
	}
	if err := os.MkdirAll(absolute, privateDirectoryMode); err != nil {
		return nil, fmt.Errorf("create catalog artifact store root: %w", err)
	}
	if err := secureDirectory(absolute); err != nil {
		return nil, err
	}

	artifacts := filepath.Join(absolute, artifactDirectoryName)
	if err := createPrivateDirectory(artifacts); err != nil {
		return nil, err
	}
	directory := filepath.Join(artifacts, sha256DirectoryName)
	if err := createPrivateDirectory(directory); err != nil {
		return nil, err
	}
	return &Store{root: absolute, directory: directory}, nil
}

// Root returns the absolute catalog-service store root.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Directory returns the absolute private sha256 artifact directory.
func (s *Store) Directory() string {
	if s == nil {
		return ""
	}
	return s.directory
}

// Put persists exactly size bytes from source under expected. Repeating Put
// with the same digest, size, and bytes is idempotent; an existing corrupt or
// conflicting entry is never replaced.
func (s *Store) Put(expected digest.Digest, size int64, source io.Reader) error {
	if s == nil {
		return errors.New("catalog artifact store is nil")
	}
	if err := validateDigest(expected); err != nil {
		return err
	}
	if size < 0 {
		return errors.New("catalog artifact size is negative")
	}
	if source == nil {
		return errors.New("catalog artifact source is nil")
	}

	existing, err := s.Open(expected)
	if err == nil {
		defer existing.Close()
		if existing.Size() != size {
			return fmt.Errorf("stored catalog artifact size %d does not match expected size %d", existing.Size(), size)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	temporary, err := os.CreateTemp(s.directory, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary catalog artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if temporaryPath != "" {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary catalog artifact: %w", err)
	}

	hasher := sha256.New()
	written, err := io.CopyN(io.MultiWriter(temporary, hasher), source, size)
	if err != nil {
		return fmt.Errorf("read catalog artifact: got %d of %d bytes: %w", written, size, err)
	}
	var trailing [1]byte
	trailingCount, trailingErr := io.ReadFull(source, trailing[:])
	if trailingCount != 0 {
		return fmt.Errorf("catalog artifact exceeds expected size %d", size)
	}
	if trailingErr != nil && !errors.Is(trailingErr, io.EOF) {
		return fmt.Errorf("finish reading catalog artifact: %w", trailingErr)
	}
	actual := digest.Digest("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	if actual != expected {
		return fmt.Errorf("catalog artifact digest %s does not match expected %s", actual, expected)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary catalog artifact: %w", err)
	}
	if err := temporary.Chmod(artifactFileMode); err != nil {
		return fmt.Errorf("make catalog artifact immutable: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync catalog artifact permissions: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary catalog artifact: %w", err)
	}

	path := s.path(expected)
	if err := os.Link(temporaryPath, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("publish catalog artifact: %w", err)
		}
		existing, openErr := s.Open(expected)
		if openErr != nil {
			return openErr
		}
		defer existing.Close()
		if existing.Size() != size {
			return fmt.Errorf("stored catalog artifact size %d does not match expected size %d", existing.Size(), size)
		}
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("remove temporary catalog artifact: %w", err)
	}
	temporaryPath = ""
	if err := syncDirectory(s.directory); err != nil {
		return fmt.Errorf("sync catalog artifact directory: %w", err)
	}
	return nil
}

// Open opens expected, verifies its type, permissions, size, and SHA-256, and
// returns it positioned at byte zero. Corrupt entries fail closed.
func (s *Store) Open(expected digest.Digest) (*Artifact, error) {
	if s == nil {
		return nil, errors.New("catalog artifact store is nil")
	}
	if err := validateDigest(expected); err != nil {
		return nil, err
	}
	path := s.path(expected)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open catalog artifact %s: %w", expected, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open catalog artifact file")
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect catalog artifact %s: %w", expected, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("catalog artifact %s is not a regular file", expected)
	}
	if info.Mode().Perm() != artifactFileMode {
		return nil, fmt.Errorf("catalog artifact %s has unsafe permissions %04o", expected, info.Mode().Perm())
	}
	if info.Size() < 0 {
		return nil, fmt.Errorf("catalog artifact %s has a negative size", expected)
	}

	hasher := sha256.New()
	read, err := io.Copy(hasher, file)
	if err != nil {
		return nil, fmt.Errorf("verify catalog artifact %s: %w", expected, err)
	}
	if read != info.Size() {
		return nil, fmt.Errorf("catalog artifact %s changed size while being verified", expected)
	}
	actual := digest.Digest("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	if actual != expected {
		return nil, fmt.Errorf("stored catalog artifact digest %s does not match path %s", actual, expected)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("reinspect catalog artifact %s: %w", expected, err)
	}
	if after.Size() != info.Size() || after.Mode() != info.Mode() {
		return nil, fmt.Errorf("catalog artifact %s changed while being verified", expected)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind catalog artifact %s: %w", expected, err)
	}
	closeOnError = false
	return &Artifact{file: file, digest: expected, size: info.Size()}, nil
}

// Read reads artifact bytes.
func (a *Artifact) Read(data []byte) (int, error) {
	if a == nil || a.file == nil {
		return 0, os.ErrInvalid
	}
	return a.file.Read(data)
}

// Close closes the opened artifact.
func (a *Artifact) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.file = nil
	return err
}

// Digest returns the verified canonical SHA-256 digest.
func (a *Artifact) Digest() digest.Digest {
	if a == nil {
		return ""
	}
	return a.digest
}

// Size returns the exact verified byte size.
func (a *Artifact) Size() int64 {
	if a == nil {
		return 0
	}
	return a.size
}

func (s *Store) path(value digest.Digest) string {
	return filepath.Join(s.directory, value.Encoded())
}

func validateDigest(value digest.Digest) error {
	encoded := value.Encoded()
	if value.Algorithm() != digest.SHA256 || value.String() != "sha256:"+encoded || len(encoded) != 64 || strings.ToLower(encoded) != encoded {
		return errors.New("catalog artifact digest must be a canonical sha256 digest")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("catalog artifact digest must be a canonical sha256 digest")
	}
	return nil
}

func createPrivateDirectory(path string) error {
	if err := os.Mkdir(path, privateDirectoryMode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create catalog artifact store directory %s: %w", path, err)
	}
	return secureDirectory(path)
}

func secureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect catalog artifact store directory %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("catalog artifact store path %s is not a real directory", path)
	}
	if info.Mode().Perm() == privateDirectoryMode {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read catalog artifact store directory %s: %w", path, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("existing catalog artifact store path %s has unsafe permissions %04o", path, info.Mode().Perm())
	}
	if err := os.Chmod(path, privateDirectoryMode); err != nil {
		return fmt.Errorf("secure catalog artifact store directory %s: %w", path, err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	return nil
}
