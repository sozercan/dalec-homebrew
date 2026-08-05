package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogartifactstore"
	"github.com/sozercan/dalec-homebrew/internal/cataloggenerator"
	"github.com/sozercan/dalec-homebrew/internal/catalogservice"
)

type tapCommitFlags map[catalog.TapID]string

func (pins *tapCommitFlags) String() string {
	if pins == nil || len(*pins) == 0 {
		return ""
	}
	values := make([]string, 0, len(*pins))
	for tap, commit := range *pins {
		values = append(values, string(tap)+"="+commit)
	}
	slices.Sort(values)
	return strings.Join(values, ",")
}

func (pins *tapCommitFlags) Set(value string) error {
	tapValue, commit, ok := strings.Cut(value, "=")
	if !ok || tapValue == "" || commit == "" {
		return errors.New("tap commit must use TAP=COMMIT")
	}
	tap, err := catalog.ParseTapID(tapValue)
	if err != nil {
		return fmt.Errorf("tap commit tap %q: %w", tapValue, err)
	}
	if tap.IsCore() {
		return errors.New("homebrew/core cannot be pinned with --tap-commit")
	}
	if !validTapCommit(commit) {
		return errors.New("tap commit must be a lowercase 40-hex commit")
	}
	if _, exists := (*pins)[tap]; exists {
		return fmt.Errorf("duplicate tap commit pin for %s", tap)
	}
	if len(*pins) >= catalog.MaxTaps {
		return fmt.Errorf("tap commit pin count exceeds limit %d", catalog.MaxTaps)
	}
	if *pins == nil {
		*pins = make(tapCommitFlags)
	}
	(*pins)[tap] = commit
	return nil
}

func (pins tapCommitFlags) clone() map[catalog.TapID]string {
	if len(pins) == 0 {
		return nil
	}
	cloned := make(map[catalog.TapID]string, len(pins))
	for tap, commit := range pins {
		cloned[tap] = commit
	}
	return cloned
}

func validTapCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range []byte(value) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dalec-homebrew-catalog-service:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) (retErr error) {
	flags := flag.NewFlagSet("dalec-homebrew-catalog-service", flag.ContinueOnError)
	var tapCommits tapCommitFlags
	listenAddress := flags.String("listen", ":8080", "HTTP listen address")
	storeDir := flags.String("store", "", "persistent single-writer store directory")
	origin := flags.String("origin", "", "public HTTPS catalog-service origin")
	signingKey := flags.String("signing-key", "", "mounted PS512 RSA private key PEM")
	signingKeyID := flags.String("signing-key-id", "", "JWS signing key ID")
	fixturePath := flags.String("fixture", "", "strict static generator fixture (test/local only)")
	buildkitAddress := flags.String("buildkit-address", "", "dedicated BuildKit worker address for production tap ingestion")
	extractorRef := flags.String("extractor-ref", "", "digest-pinned multi-platform catalog-extractor image")
	homebrewCommit := flags.String("homebrew-commit", "", "release-pinned Homebrew commit")
	flags.Var(&tapCommits, "tap-commit", "exact non-core tap commit TAP=COMMIT (repeatable)")
	serviceVersion := flags.String("service-version", "", "catalog-service component version")
	serviceDigest := flags.String("service-digest", "", "catalog-service component sha256 digest")
	extractorVersion := flags.String("extractor-version", "", "catalog-extractor component version")
	extractorDigest := flags.String("extractor-digest", "", "catalog-extractor component sha256 digest")
	setLifetime := flags.Duration("set-lifetime", catalog.MaxCatalogSetLifetime, "signed catalog-set lifetime")
	generationTimeout := flags.Duration("generation-timeout", catalogservice.DefaultGenerationTimeout, "per-operation generation deadline")
	retryAfter := flags.Duration("retry-after", catalogservice.DefaultRetryAfter, "pending operation retry interval")
	catalogRefresh := flags.Duration("catalog-refresh", catalogservice.DefaultCatalogRefresh, "default-branch catalog refresh interval")
	maxConcurrent := flags.Int("max-concurrent-generations", catalogservice.DefaultMaxConcurrentGenerations, "maximum concurrent catalog generations")
	maxPending := flags.Int("max-pending-generations", catalogservice.DefaultMaxPendingGenerations, "maximum admitted active and queued catalog generations")
	maxStored := flags.Int("max-stored-operations", catalogservice.DefaultMaxStoredOperations, "maximum persistent request/operation records")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not supported")
	}
	for _, required := range []struct {
		name  string
		value string
	}{
		{"--store", *storeDir},
		{"--origin", *origin},
		{"--signing-key", *signingKey},
		{"--signing-key-id", *signingKeyID},
		{"--service-version", *serviceVersion},
		{"--service-digest", *serviceDigest},
		{"--extractor-version", *extractorVersion},
		{"--extractor-digest", *extractorDigest},
	} {
		if required.value == "" {
			return fmt.Errorf("%s is required", required.name)
		}
	}
	if (*fixturePath == "") == (*buildkitAddress == "") {
		return errors.New("exactly one of --buildkit-address or --fixture is required")
	}
	if *fixturePath != "" && len(tapCommits) != 0 {
		return errors.New("--tap-commit is not supported with --fixture")
	}
	artifactStore, err := catalogartifactstore.New(*storeDir)
	if err != nil {
		return fmt.Errorf("configure generated artifact store: %w", err)
	}
	var (
		generator catalogservice.Generator
	)
	if *fixturePath != "" {
		generator, err = catalogservice.LoadStaticGenerator(*fixturePath)
		if err != nil {
			return fmt.Errorf("load static catalog generator: %w", err)
		}
	} else {
		for _, required := range []struct {
			name  string
			value string
		}{{"--extractor-ref", *extractorRef}, {"--homebrew-commit", *homebrewCommit}} {
			if required.value == "" {
				return fmt.Errorf("%s is required with --buildkit-address", required.name)
			}
		}
		if !strings.HasSuffix(*extractorRef, "@"+*extractorDigest) {
			return errors.New("--extractor-ref digest does not match --extractor-digest")
		}
		generator, err = cataloggenerator.NewProduction(ctx, cataloggenerator.ProductionConfig{BuildKitAddress: *buildkitAddress, ExtractorRef: *extractorRef, HomebrewCommit: *homebrewCommit, TapCommits: tapCommits.clone(), CatalogServiceOrigin: *origin, ArtifactStore: artifactStore, CacheDir: filepath.Join(*storeDir, "ingestion-cache"), CacheMaxAge: *catalogRefresh, VerificationIdentity: *serviceDigest + "\x00" + *extractorDigest})
		if err != nil {
			return fmt.Errorf("configure BuildKit catalog generator: %w", err)
		}
	}
	service, err := catalogservice.New(catalogservice.Config{
		StoreDir:                 *storeDir,
		ArtifactStore:            artifactStore,
		Origin:                   *origin,
		Generator:                generator,
		SigningKeyPath:           *signingKey,
		SigningKeyID:             *signingKeyID,
		CatalogService:           catalog.ComponentIdentity{Name: "catalog-service", Version: *serviceVersion, Digest: *serviceDigest},
		Extractor:                catalog.ComponentIdentity{Name: "catalog-extractor", Version: *extractorVersion, Digest: *extractorDigest},
		CatalogSetLifetime:       *setLifetime,
		GenerationTimeout:        *generationTimeout,
		RetryAfter:               *retryAfter,
		CatalogRefresh:           *catalogRefresh,
		MaxConcurrentGenerations: *maxConcurrent,
		MaxPendingGenerations:    *maxPending,
		MaxStoredOperations:      *maxStored,
	})
	if err != nil {
		return errors.Join(err, closeGenerator(generator))
	}
	defer func() { retErr = errors.Join(retErr, service.Close()) }()

	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listenAddress, err)
	}
	defer listener.Close()
	httpServer := &http.Server{
		Handler:           service,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	serveError := make(chan error, 1)
	go func() {
		serveError <- httpServer.Serve(listener)
	}()
	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve catalog HTTP API: %w", err)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down catalog HTTP API: %w", err)
		}
		err := <-serveError
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve catalog HTTP API: %w", err)
		}
		return nil
	}
}

func closeGenerator(generator catalogservice.Generator) error {
	closer, ok := generator.(interface{ Close() error })
	if !ok {
		return nil
	}
	return closer.Close()
}
