package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sozercan/dalec-homebrew/internal/runtimebase"
)

func main() {
	manifest := flag.String("manifest", "", "Chisel manifest.wall path")
	root := flag.String("root", "", "Chisel rootfs")
	inventory := flag.String("inventory", "", "runtime-base-packages.tsv output")
	flag.Parse()
	if *manifest == "" || *root == "" || *inventory == "" {
		fmt.Fprintln(os.Stderr, "manifest, root, and inventory are required")
		os.Exit(2)
	}
	packages, err := runtimebase.ReadChiselManifest(*manifest, *root)
	if err == nil {
		err = runtimebase.WritePackageEvidence(packages, *inventory)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
