package main

import (
	"fmt"
	"os"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: dalec-homebrew-record-verify RESOLUTION.json")
		os.Exit(64)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatal(err)
	}
	record, err := resolution.Decode(data)
	if err != nil {
		fatal(err)
	}
	if err := resolution.ValidateForMaterialization(record); err != nil {
		fatal(err)
	}
	d, err := resolution.Digest(record)
	if err != nil {
		fatal(err)
	}
	fmt.Println(d)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "resolution verification failed:", err)
	os.Exit(1)
}
