package cataloggenerator

import (
	"bytes"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

func TestMemoryArtifactStoreRetainsExactDefensiveBytes(t *testing.T) {
	store := NewMemoryArtifactStore()
	data := []byte("derived bottle")
	d := digest.FromBytes(data)
	if err := store.Put(d, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Bytes(d, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got[0] ^= 0xff
	again, err := store.Bytes(d, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, data) {
		t.Fatal("returned bytes mutated stored artifact")
	}
	if err := store.Put(d, int64(len(data)-1), bytes.NewReader(data)); err == nil {
		t.Fatal("size mismatch accepted")
	}
}
