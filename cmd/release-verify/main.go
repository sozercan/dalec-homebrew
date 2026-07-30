package main

import (
	"fmt"
	"github.com/sozercan/dalec-homebrew/internal/release"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: dalec-homebrew-release-verify components.json")
		os.Exit(64)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	m, err := release.Decode(f)
	if err != nil {
		fatal(err)
	}
	d, err := release.Digest(m)
	if err != nil {
		fatal(err)
	}
	fmt.Println(d)
}
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "component manifest verification failed:", err)
	os.Exit(1)
}
