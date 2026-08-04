package cataloggenerator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogextractor"
)

const (
	cacheSchemaVersion  = "dalec-homebrew-ingestion-cache/v1"
	maxCacheRecordBytes = catalog.MaxCatalogDocumentBytes + 1<<20
)

type cachedTapExtractor struct {
	inner  TapExtractor
	root   string
	maxAge time.Duration
	now    func() time.Time
	mu     sync.Mutex
}

type tapCacheIndex struct {
	SchemaVersion string    `json:"schema_version"`
	CachedAt      time.Time `json:"cached_at"`
	RepositoryKey string    `json:"repository_key"`
}

func newCachedTapExtractor(root string, maxAge time.Duration, inner TapExtractor) (*cachedTapExtractor, error) {
	if root == "" || inner == nil || maxAge <= 0 {
		return nil, errors.New("cache root, extractor, and positive max age are required")
	}
	for _, directory := range []string{filepath.Join(root, "tap-index"), filepath.Join(root, "repositories")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}
	return &cachedTapExtractor{inner: inner, root: root, maxAge: maxAge, now: time.Now}, nil
}

func (c *cachedTapExtractor) Extract(ctx context.Context, tap catalog.TapID) (*catalogextractor.ExtractedTap, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	indexPath := filepath.Join(c.root, "tap-index", cacheKey([]byte(tap))+".json")
	if indexData, err := readCacheFile(indexPath, 1<<20); err == nil {
		var index tapCacheIndex
		if decodeStrict(indexData, &index) == nil && index.SchemaVersion == cacheSchemaVersion && c.now().UTC().Before(index.CachedAt.Add(c.maxAge)) && validCacheKey(index.RepositoryKey) {
			data, err := readCacheFile(filepath.Join(c.root, "repositories", index.RepositoryKey+".json"), maxCacheRecordBytes)
			if err == nil {
				extracted, err := catalogextractor.DecodeExtractedTap(data)
				if err == nil && extracted.Tap.ID == tap {
					return extracted, nil
				}
			}
		}
	}
	extracted, err := c.inner.Extract(ctx, tap)
	if err != nil {
		return nil, err
	}
	data, err := catalogextractor.MarshalExtractedTap(extracted)
	if err != nil {
		return nil, err
	}
	repositoryKey := cacheKey([]byte(extracted.Tap.Repository + "\x00" + extracted.Tap.Commit))
	if err := writeCacheFile(filepath.Join(c.root, "repositories", repositoryKey+".json"), data); err != nil {
		return nil, err
	}
	indexData, err := json.Marshal(tapCacheIndex{SchemaVersion: cacheSchemaVersion, CachedAt: c.now().UTC().Round(0), RepositoryKey: repositoryKey})
	if err != nil {
		return nil, err
	}
	if err := writeMutableCacheFile(indexPath, indexData); err != nil {
		return nil, err
	}
	return extracted, nil
}

type cachedArtifactBuilder struct {
	inner ArtifactBuilder
	root  string
	mu    sync.Mutex
}

func newCachedArtifactBuilder(root string, inner ArtifactBuilder) (*cachedArtifactBuilder, error) {
	if root == "" || inner == nil {
		return nil, errors.New("artifact cache root and builder are required")
	}
	root = filepath.Join(root, "artifact-verification")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &cachedArtifactBuilder{inner: inner, root: root}, nil
}

func (c *cachedArtifactBuilder) Build(ctx context.Context, request *catalog.Request, core CoreSnapshot, catalogs map[catalog.TapID]*catalog.TapCatalog, node catalog.Node, platform catalog.Platform) (catalog.BottleArtifact, error) {
	keyData, err := artifactCacheIdentity(request, core, catalogs, node, platform)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	key := cacheKey(keyData)
	c.mu.Lock()
	defer c.mu.Unlock()
	path := filepath.Join(c.root, key+".json")
	if data, err := readCacheFile(path, maxCacheRecordBytes); err == nil {
		var artifact catalog.BottleArtifact
		if decodeStrict(data, &artifact) == nil && catalog.ValidateBottleArtifact(artifact) == nil && artifact.ID == node.ID && artifact.Platform == platform {
			return artifact, nil
		}
	}
	artifact, err := c.inner.Build(ctx, request, core, catalogs, node, platform)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	if err := catalog.ValidateBottleArtifact(artifact); err != nil {
		return catalog.BottleArtifact{}, err
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	if err := writeCacheFile(path, data); err != nil {
		return catalog.BottleArtifact{}, err
	}
	return artifact, nil
}

func artifactCacheIdentity(request *catalog.Request, core CoreSnapshot, catalogs map[catalog.TapID]*catalog.TapCatalog, node catalog.Node, platform catalog.Platform) ([]byte, error) {
	identity := struct {
		SchemaVersion  string             `json:"schema_version"`
		HomebrewCommit string             `json:"homebrew_commit"`
		CoreDigest     string             `json:"core_digest"`
		Node           catalog.Node       `json:"node"`
		Platform       catalog.Platform   `json:"platform"`
		Tap            *catalog.TapSource `json:"tap,omitempty"`
		Formula        *catalog.Formula   `json:"formula,omitempty"`
	}{SchemaVersion: cacheSchemaVersion, Node: node, Platform: platform}
	if request != nil {
		identity.HomebrewCommit = request.HomebrewCommit
	}
	if core != nil {
		identity.CoreDigest = core.Info().Digest
	}
	if !node.ID.IsCore() {
		document := catalogs[node.ID.Tap()]
		if document == nil {
			return nil, fmt.Errorf("tap catalog %s is missing", node.ID.Tap())
		}
		copyTap := document.Tap
		identity.Tap = &copyTap
		for i := range document.Formulae {
			if document.Formulae[i].ID == node.ID {
				copyFormula := document.Formulae[i]
				identity.Formula = &copyFormula
				break
			}
		}
		if identity.Formula == nil {
			return nil, fmt.Errorf("Formula %s is missing from tap cache identity", node.ID)
		}
	}
	return json.Marshal(identity)
}

func cacheKey(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validCacheKey(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func readCacheFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("cache record is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("cache record exceeds limit")
	}
	return data, nil
}

func writeCacheFile(path string, data []byte) error {
	if len(data) == 0 || int64(len(data)) > maxCacheRecordBytes {
		return errors.New("cache record size is invalid")
	}
	if _, err := os.Lstat(path); err == nil {
		existing, err := readCacheFile(path, maxCacheRecordBytes)
		if err != nil {
			return err
		}
		if bytes.Equal(existing, data) {
			return nil
		}
		return errors.New("immutable cache key collision")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeCacheTemp(path, data)
}

func writeMutableCacheFile(path string, data []byte) error {
	if _, err := os.Lstat(path); err == nil {
		if _, err := readCacheFile(path, maxCacheRecordBytes); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeCacheTemp(path, data)
}

func writeCacheTemp(path string, data []byte) error {
	if len(data) == 0 || int64(len(data)) > maxCacheRecordBytes {
		return errors.New("cache record size is invalid")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(0o444); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func decodeStrict(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
