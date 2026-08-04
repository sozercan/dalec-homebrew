// Package catalogservice implements the persistent, signed HTTP service for
// generating immutable non-core Homebrew tap catalog sets.
package catalogservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

// GeneratedSet is the generator-owned portion of a completed catalog set.
//
// The service assigns catalog publication times and monotonic per-tap
// sequences, stores the resulting canonical catalog documents, constructs
// immutable service-origin references, and signs the final payload. Generators
// therefore cannot choose persistence, URL, sequence, or signing inputs.
type GeneratedSet struct {
	Catalogs []catalog.TapCatalog
	Results  []catalog.PlatformResult
}

// Generator evaluates one canonical request. Implementations may perform
// expensive extraction work, but they must honor context cancellation. The
// service may invoke Generate concurrently for distinct request digests.
type Generator interface {
	Generate(context.Context, *catalog.Request) (*GeneratedSet, error)
}

// closeGenerator releases generator-owned process-local resources when the
// implementation exposes an idempotent Close method. Service.Close calls this
// only after all active Generate calls have returned.
func closeGenerator(generator Generator) error {
	closer, ok := generator.(interface{ Close() error })
	if !ok {
		return nil
	}
	if err := closer.Close(); err != nil {
		return fmt.Errorf("close catalog generator: %w", err)
	}
	return nil
}

// GeneratorFunc adapts a function to Generator.
type GeneratorFunc func(context.Context, *catalog.Request) (*GeneratedSet, error)

// Generate implements Generator.
func (f GeneratorFunc) Generate(ctx context.Context, request *catalog.Request) (*GeneratedSet, error) {
	return f(ctx, request)
}

// FailureError lets a generator select one of the stable protocol failure
// codes without exposing an arbitrary underlying error to clients.
type FailureError struct {
	Failure catalog.Failure
	Cause   error
}

// NewFailureError constructs a generator error with a stable, client-safe
// failure. Message must already be suitable for returning to an untrusted
// client; Cause is retained only for errors.Is/errors.As and server-side use.
func NewFailureError(code catalog.FailureCode, message string, cause error) error {
	return &FailureError{Failure: catalog.Failure{Code: code, Message: message}, Cause: cause}
}

func (e *FailureError) Error() string {
	if e == nil {
		return "catalog generation failed"
	}
	if e.Failure.Message != "" {
		return e.Failure.Message
	}
	if e.Failure.Code != "" {
		return "catalog generation failed: " + string(e.Failure.Code)
	}
	return "catalog generation failed"
}

// Unwrap exposes the private cause without adding it to the client response.
func (e *FailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// StaticGenerator is a deterministic fixture generator. It is intended for
// hermetic tests and local protocol fixtures, not dynamic tap extraction.
type StaticGenerator struct {
	requestDigest string
	generated     GeneratedSet
}

// NewStaticGenerator binds a generated set to exactly one canonical request.
func NewStaticGenerator(request *catalog.Request, generated *GeneratedSet) (*StaticGenerator, error) {
	if request == nil {
		return nil, errors.New("static generator request is nil")
	}
	if generated == nil {
		return nil, errors.New("static generator result is nil")
	}
	digest, err := catalog.RequestDigest(request)
	if err != nil {
		return nil, fmt.Errorf("validate static generator request: %w", err)
	}
	clone, err := cloneGeneratedSet(generated)
	if err != nil {
		return nil, fmt.Errorf("clone static generator result: %w", err)
	}
	return &StaticGenerator{requestDigest: digest.String(), generated: *clone}, nil
}

// Generate returns a fresh copy only when request matches the fixture binding.
func (g *StaticGenerator) Generate(ctx context.Context, request *catalog.Request) (*GeneratedSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g == nil {
		return nil, NewFailureError(catalog.FailureUnavailable, "catalog generator is unavailable", nil)
	}
	digest, err := catalog.RequestDigest(request)
	if err != nil {
		return nil, NewFailureError(catalog.FailurePolicy, "catalog request is invalid", err)
	}
	if digest.String() != g.requestDigest {
		return nil, NewFailureError(catalog.FailurePolicy, "static catalog fixture does not match request", nil)
	}
	clone, err := cloneGeneratedSet(&g.generated)
	if err != nil {
		return nil, NewFailureError(catalog.FailureUnavailable, "static catalog fixture is unavailable", err)
	}
	return clone, nil
}

func cloneGeneratedSet(generated *GeneratedSet) (*GeneratedSet, error) {
	data, err := json.Marshal(generated)
	if err != nil {
		return nil, err
	}
	var clone GeneratedSet
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}
