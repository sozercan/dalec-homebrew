package runtimefs

import (
	"fmt"
	"path"
	"testing"
)

func TestDescendantPathIndexConsumesOverlappingClosuresOnce(t *testing.T) {
	const descendantCount = 4096

	root := "Cellar/dep/1/include/runtime"
	nested := path.Join(root, "nested")
	paths := []string{
		root,
		nested,
		root + "-sibling/file.h",
		root + "0/file.h",
	}
	for i := range descendantCount {
		parent := root
		if i%2 == 0 {
			parent = nested
		}
		paths = append(paths, path.Join(parent, fmt.Sprintf("header-%04d.h", i)))
	}

	index := newDescendantPathIndex(paths)
	visits := make(map[string]int, descendantCount+1)
	consume := func(rel string) {
		visits[rel]++
	}
	// Model many protected links that repeatedly resolve to the same and
	// overlapping directory targets. A full-list rescan would invoke consume
	// millions of times; the successor index emits each descendant once.
	for range 2048 {
		index.consumeDescendants(nested, consume)
		index.consumeDescendants(root, consume)
	}

	if got, want := len(visits), descendantCount+1; got != want {
		t.Fatalf("visited paths = %d, want %d", got, want)
	}
	for rel, count := range visits {
		if count != 1 {
			t.Fatalf("%s visited %d times, want once", rel, count)
		}
	}
	for _, rel := range []string{root, root + "-sibling/file.h", root + "0/file.h"} {
		if visits[rel] != 0 {
			t.Fatalf("non-descendant %s was visited", rel)
		}
	}
	if got, want := len(index.ranges), 2; got != want {
		t.Fatalf("cached ranges = %d, want %d", got, want)
	}
}
