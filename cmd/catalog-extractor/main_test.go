package main

import "testing"

func TestRunRequiresPaths(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("missing paths accepted")
	}
	if err := run([]string{"extra"}); err == nil {
		t.Fatal("positional argument accepted")
	}
}
