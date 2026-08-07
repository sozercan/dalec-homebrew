package catalogartifactstore

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

func testArtifact(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	writer.Name = "generated.bottle.tar"
	writer.ModTime = writer.ModTime.UTC()
	if _, err := writer.Write([]byte("generated artifact bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestPutOpenIsPrivateImmutableAndRestartSafe(t *testing.T) {
	root := t.TempDir()
	data := testArtifact(t)
	expected := digest.FromBytes(data)

	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(expected, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(expected, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}

	for _, directory := range []string{root, filepath.Join(root, "artifacts"), store.Directory()} {
		info, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s mode=%v", directory, info.Mode())
		}
	}
	path := filepath.Join(store.Directory(), expected.Encoded())
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Size() != int64(len(data)) {
		t.Fatalf("artifact mode=%v size=%d", info.Mode(), info.Size())
	}

	opened, err := store.Open(expected)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := io.ReadAll(opened)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if opened.Digest() != expected || opened.Size() != int64(len(data)) || !bytes.Equal(loaded, data) {
		t.Fatalf("opened digest=%s size=%d bytes_match=%t", opened.Digest(), opened.Size(), bytes.Equal(loaded, data))
	}

	restarted, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := restarted.Open(expected)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded, err := io.ReadAll(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reloaded, data) {
		t.Fatal("artifact changed across store restart")
	}
}

func TestOpenRejectsCorruptionUnsafePermissionsAndSymlink(t *testing.T) {
	root := t.TempDir()
	data := testArtifact(t)
	expected := digest.FromBytes(data)
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(expected, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Directory(), expected.Encoded())

	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-1] ^= 0xff
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(expected); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("corrupt artifact error=%v", err)
	}
	if err := store.Put(expected, int64(len(data)), bytes.NewReader(data)); err == nil {
		t.Fatal("Put replaced a corrupt immutable artifact")
	}

	other := digest.FromBytes([]byte("other artifact"))
	otherPath := filepath.Join(store.Directory(), other.Encoded())
	if err := os.Symlink(path, otherPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(other); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink artifact error=%v", err)
	}

	unsafe := digest.FromBytes([]byte("unsafe mode"))
	unsafePath := filepath.Join(store.Directory(), unsafe.Encoded())
	if err := os.WriteFile(unsafePath, []byte("unsafe mode"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(unsafe); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("unsafe artifact error=%v", err)
	}
}

func TestPutRequiresCanonicalDigestAndExactSize(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := testArtifact(t)
	expected := digest.FromBytes(data)

	for name, candidate := range map[string]digest.Digest{
		"algorithm": digest.Digest("sha512:" + strings.Repeat("a", 128)),
		"uppercase": digest.Digest("sha256:" + strings.Repeat("A", 64)),
		"short":     digest.Digest("sha256:abcd"),
		"padded":    digest.Digest(" sha256:" + strings.Repeat("a", 64)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.Put(candidate, int64(len(data)), bytes.NewReader(data)); err == nil {
				t.Fatal("non-canonical digest accepted")
			}
			if _, err := store.Open(candidate); err == nil {
				t.Fatal("non-canonical digest opened")
			}
		})
	}
	if err := store.Put(expected, -1, bytes.NewReader(data)); err == nil {
		t.Fatal("negative size accepted")
	}
	if err := store.Put(expected, int64(len(data)-1), bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized source error=%v", err)
	}
	if err := store.Put(expected, int64(len(data)+1), bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "got") {
		t.Fatalf("short source error=%v", err)
	}
	wrong := digest.FromBytes([]byte("different"))
	if err := store.Put(wrong, int64(len(data)), bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("digest mismatch error=%v", err)
	}

	entries, err := os.ReadDir(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary artifact remains after failed Put: %s", entry.Name())
		}
	}
}

func TestConcurrentPutPublishesOneVerifiedArtifact(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := testArtifact(t)
	expected := digest.FromBytes(data)

	const writers = 16
	var wait sync.WaitGroup
	errorsChannel := make(chan error, writers)
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsChannel <- store.Put(expected, int64(len(data)), bytes.NewReader(data))
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Error(err)
		}
	}
	opened, err := store.Open(expected)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	loaded, err := io.ReadAll(opened)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, data) {
		t.Fatal("concurrently stored artifact differs")
	}
	entries, err := os.ReadDir(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != expected.Encoded() {
		t.Fatalf("artifact entries=%v", entries)
	}
}

func TestNewRejectsUnsafeExistingPaths(t *testing.T) {
	t.Run("non-empty unsafe root", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "untrusted"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := New(root); err == nil {
			t.Fatal("unsafe non-empty root accepted")
		}
	})
	t.Run("symlinked artifacts directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "artifacts")); err != nil {
			t.Fatal(err)
		}
		if _, err := New(root); err == nil {
			t.Fatal("symlinked artifacts directory accepted")
		}
	})
}
