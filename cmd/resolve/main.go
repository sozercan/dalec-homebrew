package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/oci"
	"github.com/sozercan/dalec-homebrew/internal/policy"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/resolver"
	"github.com/sozercan/dalec-homebrew/internal/runtime"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dalec-homebrew-resolve:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dalec-homebrew-resolve", flag.ContinueOnError)
	rootsValue := fs.String("roots", "", "comma-separated Formula roots")
	arch := fs.String("arch", "amd64", "amd64 or arm64")
	output := fs.String("output", "-", "resolution output path or -")
	baseURL := fs.String("metadata-base", metadata.DefaultBaseURL, "signed Homebrew metadata base URL")
	maxAge := fs.Duration("max-age", 7*24*time.Hour, "maximum metadata age")
	notBefore := fs.String("not-before", "", "RFC3339 metadata rollback floor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var roots []string
	for _, name := range strings.Split(*rootsValue, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			roots = append(roots, name)
		}
	}
	if len(roots) == 0 {
		return fmt.Errorf("--roots is required")
	}
	if *arch != "amd64" && *arch != "arm64" {
		return fmt.Errorf("unsupported arch %q", *arch)
	}
	var floor time.Time
	if *notBefore != "" {
		var err error
		floor, err = time.Parse(time.RFC3339, *notBefore)
		if err != nil {
			return err
		}
	}
	snapshot, err := metadata.Fetch(ctx, metadata.Config{BaseURL: *baseURL, Freshness: metadata.FreshnessPolicy{MaxAge: *maxAge, RollbackFloor: floor}})
	if err != nil {
		return err
	}
	registry, err := oci.NewClient("https://ghcr.io")
	if err != nil {
		return err
	}
	seed := sha256.Sum256([]byte(strings.Join(roots, "\n")))
	record, err := resolver.Resolve(ctx, snapshot, registry, roots, ocispec.Platform{OS: "linux", Architecture: *arch}, resolver.Options{SpecDigest: "sha256:" + hex.EncodeToString(seed[:]), Metadata: snapshot.Info(), Runtime: resolution.RuntimePolicy{User: runtime.DefaultUser, UID: runtime.DefaultUID, GID: runtime.DefaultGID, CPUBaseline: map[bool]string{true: "armv8", false: "core2"}[*arch == "arm64"]}, Attestation: resolution.AttestationPolicy{Waiver: "homebrew-jws-and-verified-oci-chain-v1"}})
	if err != nil {
		return err
	}
	if _, err := policy.BindRuntimePolicy(record); err != nil {
		return err
	}
	data, err := resolution.Canonical(record)
	if err != nil {
		return err
	}
	if *output == "-" {
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}
	return os.WriteFile(*output, append(data, '\n'), 0o644)
}
