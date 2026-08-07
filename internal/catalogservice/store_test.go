package catalogservice

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogartifactstore"
)

func TestStoreOwnsPersistentArtifactStoreUnderRoot(t *testing.T) {
	root := t.TempDir()
	configured, err := catalogartifactstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := openStoreWithArtifacts(root, configured)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("generated artifact")
	expected := digest.FromBytes(data)
	if err := persistent.artifacts.Put(expected, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if persistent.artifacts != configured || persistent.artifacts.Root() != persistent.root {
		t.Fatalf("artifact store root=%q catalog root=%q", persistent.artifacts.Root(), persistent.root)
	}
	if err := persistent.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	artifact, err := restarted.artifacts.Open(expected)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	if artifact.Size() != int64(len(data)) {
		t.Fatalf("artifact size=%d", artifact.Size())
	}
}

func TestStoreSingleWriterLockAndPersistentSequence(t *testing.T) {
	root := t.TempDir()
	first, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := openStore(root); err == nil || !strings.Contains(err.Error(), "writer lock") {
		t.Fatalf("second writer error=%v", err)
	}
	tap, err := catalog.ParseTapID("acme/tools")
	if err != nil {
		t.Fatal(err)
	}
	sourceA := testDigest('a')
	sourceB := testDigest('b')
	sourceC := testDigest('c')
	one, err := first.nextSequence(tap, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	same, err := first.nextSequence(tap, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	if same != one {
		t.Fatalf("same source sequence=%d, want %d", same, one)
	}
	two, err := first.nextSequence(tap, sourceB)
	if err != nil {
		t.Fatal(err)
	}
	if one != 1 || two != 2 {
		t.Fatalf("sequences=%d,%d", one, two)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	three, err := restarted.nextSequence(tap, sourceC)
	if err != nil {
		t.Fatal(err)
	}
	if three != 3 {
		t.Fatalf("restarted sequence=%d, want 3", three)
	}
	sequencePath := filepath.Join(root, "sequences", "acme", "tools.json")
	data, err := os.ReadFile(sequencePath)
	if err != nil {
		t.Fatal(err)
	}
	var state sequenceState
	if err := decodeStrictJSON(data, maxSequenceStateBytes, "tap sequence", &state); err != nil {
		t.Fatal(err)
	}
	if state.Tap != tap || state.Sequence != 3 {
		t.Fatalf("sequence state=%+v", state)
	}
}

func TestStoreCatalogIsCanonicalContentAddressedAndTamperFailsClosed(t *testing.T) {
	persistent, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer persistent.Close()
	generated := testGeneratedSet(t)
	document := generated.Catalogs[0]
	document.PublishedAt = time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	document.Sequence = 1
	canonical, err := catalog.CanonicalTapCatalog(&document)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := persistent.putCatalog(canonical)
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.TrimPrefix(digest, "sha256:")
	loaded, err := persistent.loadCatalog(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded) != string(canonical) {
		t.Fatal("loaded catalog differs")
	}
	path := filepath.Join(persistent.catalogs, encoded)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("catalog mode=%04o", info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte(nil), canonical[:len(canonical)-1]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.loadCatalog(encoded); err == nil {
		t.Fatal("tampered catalog was served")
	}
}

func TestNewRejectsInsecureSigningKeyPermissions(t *testing.T) {
	storeDir := t.TempDir()
	keyPath, _ := writeSigningKey(t, t.TempDir())
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	clock := &lockedClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	generator := GeneratorFunc(func(_ context.Context, _ *catalog.Request) (*GeneratedSet, error) {
		return testGeneratedSet(t), nil
	})
	_, err := New(testConfig(storeDir, keyPath, generator, clock))
	if err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("insecure key error=%v", err)
	}
}

func TestAtomicStateWritesLeaveNoTemporaryFiles(t *testing.T) {
	persistent, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer persistent.Close()
	request := testRequest(t)
	canonical, err := catalog.CanonicalRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	id, err := OperationID(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistent.putRequest(id, canonical); err != nil {
		t.Fatal(err)
	}
	operation := &catalog.Operation{SchemaVersion: catalog.OperationSchemaVersion, ID: id, Status: catalog.OperationPending, RetryAfterSeconds: 1}
	if err := persistent.saveOperation(operation); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{persistent.operations, persistent.requests, persistent.sequences} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".tmp-") {
				t.Fatalf("temporary state file remains: %s", filepath.Join(directory, entry.Name()))
			}
		}
	}
}

func TestLoadCatalogRejectsSymlink(t *testing.T) {
	persistent, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer persistent.Close()
	encoded := strings.Repeat("a", 64)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(persistent.catalogs, encoded)); err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.loadCatalog(encoded); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink error=%v", err)
	}
}

func TestStoreRepairsGroupWritableExistingRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("store mode=%04o", info.Mode().Perm())
	}
}

func TestStoreRejectsUnsafeNonEmptyExistingRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "untrusted"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(root); err == nil {
		t.Fatal("unsafe non-empty store root accepted")
	}
}

func TestSequenceOwnerDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	store, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "sequences", "acme")); err != nil {
		t.Fatal(err)
	}
	tap, _ := catalog.ParseTapID("acme/tools")
	if _, err := store.nextSequence(tap, testDigest('a')); err == nil {
		t.Fatal("symlinked sequence owner accepted")
	}
}
