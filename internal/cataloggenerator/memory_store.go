package cataloggenerator

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	digest "github.com/opencontainers/go-digest"
)

// MemoryArtifactStore retains verified generated artifacts for the lifetime of
// one gateway invocation. Values are content-addressed and defensively copied.
const maxBuildLocalArtifactBytes = int64(64 << 20)

type MemoryArtifactStore struct {
	mu     sync.RWMutex
	values map[digest.Digest][]byte
}

func NewMemoryArtifactStore() *MemoryArtifactStore {
	return &MemoryArtifactStore{values: make(map[digest.Digest][]byte)}
}

func (s *MemoryArtifactStore) Put(expected digest.Digest, size int64, source io.Reader) error {
	if s == nil || source == nil {
		return errors.New("build-local artifact store and source are required")
	}
	if err := expected.Validate(); err != nil || expected.Algorithm() != digest.SHA256 {
		return fmt.Errorf("invalid build-local artifact digest %q", expected)
	}
	if size <= 0 || size > maxBuildLocalArtifactBytes {
		return fmt.Errorf("build-local artifact size %d is outside 1..%d", size, maxBuildLocalArtifactBytes)
	}
	data, err := io.ReadAll(io.LimitReader(source, size+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("build-local artifact size %d does not match expected %d", len(data), size)
	}
	if digest.FromBytes(data) != expected {
		return fmt.Errorf("build-local artifact digest does not match %s", expected)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.values[expected]; ok {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("conflicting build-local artifact for %s", expected)
		}
		return nil
	}
	s.values[expected] = bytes.Clone(data)
	return nil
}

func (s *MemoryArtifactStore) Verify(expected digest.Digest, size int64) error {
	_, err := s.Bytes(expected, size)
	return err
}

func (s *MemoryArtifactStore) Bytes(expected digest.Digest, size int64) ([]byte, error) {
	if s == nil {
		return nil, errors.New("build-local artifact store is nil")
	}
	s.mu.RLock()
	data, ok := s.values[expected]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("build-local artifact %s is unavailable", expected)
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("build-local artifact %s size %d does not match %d", expected, len(data), size)
	}
	if digest.FromBytes(data) != expected {
		return nil, fmt.Errorf("build-local artifact %s failed digest verification", expected)
	}
	return bytes.Clone(data), nil
}
