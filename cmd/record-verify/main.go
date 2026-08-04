package main

import (
	"fmt"
	"os"

	"github.com/sozercan/dalec-homebrew/internal/policy"
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
	schemaVersion, err := resolution.SchemaVersionOf(data)
	if err != nil {
		fatal(err)
	}
	switch schemaVersion {
	case resolution.SchemaVersionV1:
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
	case resolution.SchemaVersionV2:
		record, err := resolution.DecodeV2(data)
		if err != nil {
			fatal(err)
		}
		if _, err := policy.VerifyRuntimePolicyV2(record); err != nil {
			fatal(err)
		}
		d, err := resolution.DigestV2(record)
		if err != nil {
			fatal(err)
		}
		fmt.Println(d)
	default:
		fatal(fmt.Errorf("unsupported schema_version %q", schemaVersion))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "resolution verification failed:", err)
	os.Exit(1)
}
