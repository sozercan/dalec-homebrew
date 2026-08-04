package catalogservice

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"golang.org/x/sys/unix"
)

const (
	sequenceSchemaVersion = "dalec-homebrew-catalog-sequence/v2"
	maxSequenceStateBytes = 4 << 10
)

type store struct {
	root       string
	catalogs   string
	operations string
	requests   string
	sequences  string
	lock       *os.File
	closeOnce  sync.Once
	closeErr   error
}

type sequenceState struct {
	SchemaVersion  string        `json:"schema_version"`
	Tap            catalog.TapID `json:"tap"`
	Sequence       uint64        `json:"sequence"`
	SourceIdentity string        `json:"source_identity"`
}

func openStore(root string) (_ *store, retErr error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("catalog store directory is empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create catalog store: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect catalog store: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("catalog store must be a directory, not a symlink")
	}
	if err := secureStoreDirectory(root, info); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog store: %w", err)
	}

	s := &store{
		root:       absolute,
		catalogs:   filepath.Join(absolute, "catalogs", "sha256"),
		operations: filepath.Join(absolute, "operations"),
		requests:   filepath.Join(absolute, "requests"),
		sequences:  filepath.Join(absolute, "sequences"),
	}
	for _, directory := range []string{s.catalogs, s.operations, s.requests, s.sequences} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create catalog store directory: %w", err)
		}
		entry, err := os.Lstat(directory)
		if err != nil || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("catalog store path %s is not a directory", directory)
		}
		if err := secureStoreDirectory(directory, entry); err != nil {
			return nil, err
		}
	}

	lockPath := filepath.Join(absolute, ".writer.lock")
	if entry, err := os.Lstat(lockPath); err == nil && (!entry.Mode().IsRegular() || entry.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("catalog writer lock must be a private regular non-symlink file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect catalog writer lock: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open catalog writer lock: %w", err)
	}
	if err := lock.Chmod(0o600); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("secure catalog writer lock: %w", err)
	}
	s.lock = lock
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("acquire catalog writer lock: %w", err)
	}
	return s, nil
}

func secureStoreDirectory(path string, info os.FileInfo) error {
	if info.Mode().Perm() == 0o700 {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("existing catalog store path %s has unsafe permissions %04o", path, info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure empty catalog store path %s: %w", path, err)
	}
	return nil
}

func (s *store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.lock == nil {
			return
		}
		if err := unix.Flock(int(s.lock.Fd()), unix.LOCK_UN); err != nil {
			s.closeErr = fmt.Errorf("release catalog writer lock: %w", err)
		}
		if err := s.lock.Close(); err != nil && s.closeErr == nil {
			s.closeErr = fmt.Errorf("close catalog writer lock: %w", err)
		}
	})
	return s.closeErr
}

func (s *store) putRequest(id string, data []byte) error {
	if !validOperationID(id) {
		return fmt.Errorf("invalid operation ID %q", id)
	}
	if _, err := catalog.DecodeRequest(data); err != nil {
		return err
	}
	return writeImmutable(filepath.Join(s.requests, id+".json"), data, 0o600)
}

func (s *store) loadRequest(id string) (*catalog.Request, []byte, error) {
	if !validOperationID(id) {
		return nil, nil, fmt.Errorf("invalid operation ID %q", id)
	}
	data, err := readBoundedFile(filepath.Join(s.requests, id+".json"), catalog.MaxRequestBytes)
	if err != nil {
		return nil, nil, err
	}
	request, err := catalog.DecodeRequest(data)
	if err != nil {
		return nil, nil, err
	}
	canonical, err := catalog.CanonicalRequest(request)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, nil, errors.New("persisted catalog request is not canonical")
	}
	return request, data, nil
}

func (s *store) saveOperation(operation *catalog.Operation) error {
	if err := catalog.ValidateOperation(operation); err != nil {
		return err
	}
	data, err := json.Marshal(operation)
	if err != nil {
		return fmt.Errorf("encode catalog operation: %w", err)
	}
	if int64(len(data)) > catalog.MaxOperationBytes {
		return errors.New("catalog operation exceeds persistence limit")
	}
	return atomicWrite(filepath.Join(s.operations, operation.ID+".json"), data, 0o600)
}

func (s *store) loadOperation(id string) (*catalog.Operation, error) {
	if !validOperationID(id) {
		return nil, fmt.Errorf("invalid operation ID %q", id)
	}
	data, err := readBoundedFile(filepath.Join(s.operations, id+".json"), catalog.MaxOperationBytes)
	if err != nil {
		return nil, err
	}
	operation, err := catalog.DecodeOperation(data)
	if err != nil {
		return nil, err
	}
	if operation.ID != id {
		return nil, fmt.Errorf("persisted operation ID %q does not match %q", operation.ID, id)
	}
	return operation, nil
}

func (s *store) listOperations() ([]string, error) {
	entries, err := os.ReadDir(s.operations)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validOperationID(id) {
			return nil, fmt.Errorf("catalog store contains invalid operation file %q", entry.Name())
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *store) putCatalog(data []byte) (string, error) {
	decoded, err := catalog.DecodeTapCatalog(data)
	if err != nil {
		return "", err
	}
	canonical, err := catalog.CanonicalTapCatalog(decoded)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(data, canonical) {
		return "", errors.New("catalog document is not canonical")
	}
	d := digest.FromBytes(data)
	if err := writeImmutable(filepath.Join(s.catalogs, d.Encoded()), data, 0o444); err != nil {
		return "", err
	}
	return d.String(), nil
}

func (s *store) loadCatalog(encoded string) ([]byte, error) {
	if !validSHA256Hex(encoded) {
		return nil, os.ErrNotExist
	}
	data, err := readBoundedFile(filepath.Join(s.catalogs, encoded), catalog.MaxCatalogDocumentBytes)
	if err != nil {
		return nil, err
	}
	actual := digest.FromBytes(data)
	if actual.Encoded() != encoded {
		return nil, fmt.Errorf("stored catalog digest %s does not match path sha256:%s", actual, encoded)
	}
	decoded, err := catalog.DecodeTapCatalog(data)
	if err != nil {
		return nil, fmt.Errorf("decode stored catalog: %w", err)
	}
	canonical, err := catalog.CanonicalTapCatalog(decoded)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("stored catalog is not canonical")
	}
	return data, nil
}

func (s *store) nextSequence(tap catalog.TapID, sourceIdentity string) (uint64, error) {
	if err := tap.Validate(); err != nil {
		return 0, err
	}
	if tap.IsCore() {
		return 0, errors.New("core tap cannot have a catalog-service sequence")
	}
	if d, err := digest.Parse(sourceIdentity); err != nil || d.Algorithm() != digest.SHA256 || d.Validate() != nil {
		return 0, errors.New("invalid catalog source identity")
	}
	ownerDirectory := filepath.Join(s.sequences, tap.Owner())
	entry, err := os.Lstat(ownerDirectory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(ownerDirectory, 0o700); err != nil {
			return 0, fmt.Errorf("create sequence directory: %w", err)
		}
		entry, err = os.Lstat(ownerDirectory)
	}
	if err != nil || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("sequence owner path %s is not a real directory", ownerDirectory)
	}
	if entry.Mode().Perm() != 0o700 {
		return 0, fmt.Errorf("sequence owner path %s has unsafe permissions %04o", ownerDirectory, entry.Mode().Perm())
	}
	path := filepath.Join(ownerDirectory, tap.Name()+".json")
	var current uint64
	data, err := readBoundedFile(path, maxSequenceStateBytes)
	if err == nil {
		var state sequenceState
		if err := decodeStrictJSON(data, maxSequenceStateBytes, "tap sequence", &state); err != nil {
			return 0, err
		}
		if state.SchemaVersion != sequenceSchemaVersion || state.Tap != tap || state.Sequence == 0 || state.SourceIdentity == "" {
			return 0, errors.New("persisted tap sequence is invalid")
		}
		if state.SourceIdentity == sourceIdentity {
			return state.Sequence, nil
		}
		current = state.Sequence
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if current == ^uint64(0) {
		return 0, errors.New("tap sequence exhausted")
	}
	next := current + 1
	state := sequenceState{SchemaVersion: sequenceSchemaVersion, Tap: tap, Sequence: next, SourceIdentity: sourceIdentity}
	encoded, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	if err := atomicWrite(path, encoded, 0o600); err != nil {
		return 0, err
	}
	return next, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open catalog store entry")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("catalog store entry is not a regular file")
	}
	if info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("catalog store entry size %d exceeds %d", info.Size(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("catalog store entry exceeds %d bytes", limit)
	}
	return data, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) (retErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func writeImmutable(path string, data []byte, mode os.FileMode) (retErr error) {
	if existing, err := readExactFile(path, int64(len(data))); err == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("immutable catalog store entry already exists with different bytes")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := readExactFile(path, int64(len(data)))
			if readErr != nil {
				return readErr
			}
			if !bytes.Equal(existing, data) {
				return errors.New("immutable catalog store entry already exists with different bytes")
			}
			return nil
		}
		return err
	}
	return syncDirectory(directory)
}

func readExactFile(path string, size int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("catalog store entry is not a regular non-symlink file")
	}
	if info.Size() != size {
		return nil, errors.New("immutable catalog store entry already exists with a different size")
	}
	return readBoundedFile(path, size)
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

func validSHA256Hex(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func (s *store) removeCatalogsExcept(referenced map[string]struct{}, now time.Time, retention time.Duration) error {
	entries, err := os.ReadDir(s.catalogs)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !validSHA256Hex(entry.Name()) {
			continue
		}
		if _, keep := referenced[entry.Name()]; keep {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if retention > 0 && now.Sub(info.ModTime()) < retention {
			continue
		}
		if err := os.Remove(filepath.Join(s.catalogs, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncDirectory(s.catalogs)
}

type storedOperationInfo struct {
	ID      string
	ModTime time.Time
}

func (s *store) operationInfos() ([]storedOperationInfo, error) {
	entries, err := os.ReadDir(s.operations)
	if err != nil {
		return nil, err
	}
	infos := make([]storedOperationInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validOperationID(id) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, storedOperationInfo{ID: id, ModTime: info.ModTime()})
	}
	return infos, nil
}

func (s *store) removeOperation(id string) error {
	if !validOperationID(id) {
		return fmt.Errorf("invalid operation ID %q", id)
	}
	for _, path := range []string{filepath.Join(s.operations, id+".json"), filepath.Join(s.requests, id+".json")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := syncDirectory(s.operations); err != nil {
		return err
	}
	return syncDirectory(s.requests)
}
