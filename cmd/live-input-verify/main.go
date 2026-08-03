package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type namedPinnedRef struct {
	name  string
	value string
}

type namedPinnedRefs []namedPinnedRef

func (refs *namedPinnedRefs) String() string {
	values := make([]string, 0, len(*refs))
	for _, ref := range *refs {
		values = append(values, ref.name+"="+ref.value)
	}
	return strings.Join(values, ",")
}

func (refs *namedPinnedRefs) Set(value string) error {
	name, ref, ok := strings.Cut(value, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return errors.New("pinned reference must use NAME=REFERENCE")
	}
	*refs = append(*refs, namedPinnedRef{name: name, value: ref})
	return nil
}

type options struct {
	runtimeBaseRef    string
	materializerRef   string
	frontendRef       string
	metadataNotBefore string
	pinnedRefs        namedPinnedRefs
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "dalec-homebrew-live-input-verify:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("dalec-homebrew-live-input-verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.runtimeBaseRef, "runtime-base-ref", "", "digest-pinned runtime-base reference")
	flags.StringVar(&opts.materializerRef, "materializer-ref", "", "digest-pinned materializer reference")
	flags.StringVar(&opts.frontendRef, "frontend-ref", "", "digest-pinned frontend reference")
	flags.StringVar(&opts.metadataNotBefore, "metadata-not-before", "", "RFC3339 metadata rollback floor")
	flags.Var(&opts.pinnedRefs, "pinned-ref", "additional digest-pinned NAME=REFERENCE to validate (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}

	refs := []namedPinnedRef{
		{name: "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF", value: opts.runtimeBaseRef},
		{name: "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF", value: opts.materializerRef},
		{name: "DALEC_HOMEBREW_LIVE_FRONTEND_REF", value: opts.frontendRef},
	}
	set := 0
	for _, ref := range refs {
		if ref.value != "" {
			set++
		}
	}
	if set != 0 && set != len(refs) {
		return errors.New("DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF, DALEC_HOMEBREW_LIVE_MATERIALIZER_REF, and DALEC_HOMEBREW_LIVE_FRONTEND_REF must be set together")
	}
	if set == len(refs) {
		for _, ref := range refs {
			if err := validatePinnedReference(ref); err != nil {
				return err
			}
		}
	}
	for _, ref := range opts.pinnedRefs {
		if err := validatePinnedReference(ref); err != nil {
			return err
		}
	}
	if opts.metadataNotBefore != "" {
		if err := validateRFC3339(opts.metadataNotBefore); err != nil {
			return fmt.Errorf("DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE must be a valid RFC3339 timestamp: %w", err)
		}
	}
	return nil
}

func validatePinnedReference(ref namedPinnedRef) error {
	if err := resolution.ValidatePinnedReference(ref.value); err != nil {
		return fmt.Errorf("%s must be a digest-pinned OCI reference using sha256: %w", ref.name, err)
	}
	return nil
}

var rfc3339Pattern = regexp.MustCompile(
	`^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]+))?(Z|([+-])([0-9]{2}):([0-9]{2}))$`,
)

func validateRFC3339(value string) error {
	match := rfc3339Pattern.FindStringSubmatch(value)
	if match == nil {
		return errors.New("invalid RFC3339 syntax")
	}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"hour", match[4], 23},
		{"minute", match[5], 59},
		{"second", match[6], 59},
	} {
		n, err := strconv.Atoi(field.value)
		if err != nil || n > field.max {
			return fmt.Errorf("invalid %s", field.name)
		}
	}
	if match[8] != "Z" {
		offsetHour, err := strconv.Atoi(match[10])
		if err != nil || offsetHour > 23 {
			return errors.New("invalid offset hour")
		}
		offsetMinute, err := strconv.Atoi(match[11])
		if err != nil || offsetMinute > 59 {
			return errors.New("invalid offset minute")
		}
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return err
	}
	return nil
}
