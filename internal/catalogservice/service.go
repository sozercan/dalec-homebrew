package catalogservice

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogartifactstore"
	"github.com/sozercan/dalec-homebrew/internal/catalogauth"
	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

const (
	DefaultRetryAfter               = 2 * time.Second
	DefaultGenerationTimeout        = 30 * time.Minute
	DefaultCatalogRefresh           = time.Hour
	DefaultMaxConcurrentGenerations = 4
	DefaultMaxPendingGenerations    = 64
	DefaultMaxStoredOperations      = 4096
	maxPendingGenerationsLimit      = 4096

	generationAdmissionFailureMessage = "catalog generation capacity is exhausted"
)

// Config contains release-bound service inputs and process-local controls.
type Config struct {
	StoreDir                 string
	ArtifactStore            *catalogartifactstore.Store
	Origin                   string
	Generator                Generator
	SigningKeyPath           string
	SigningKeyID             string
	CatalogService           catalog.ComponentIdentity
	Extractor                catalog.ComponentIdentity
	CatalogSetLifetime       time.Duration
	GenerationTimeout        time.Duration
	RetryAfter               time.Duration
	CatalogRefresh           time.Duration
	MaxConcurrentGenerations int
	MaxPendingGenerations    int
	MaxStoredOperations      int
	Now                      func() time.Time
}

// Service is a single-writer persistent catalog-service HTTP handler.
type Service struct {
	store               *store
	origin              string
	generator           Generator
	signingKey          *rsa.PrivateKey
	signingKeyID        string
	verificationKeys    metadata.KeySet
	catalogService      catalog.ComponentIdentity
	extractor           catalog.ComponentIdentity
	catalogSetLifetime  time.Duration
	generationTimeout   time.Duration
	retryAfterSeconds   int
	catalogRefresh      time.Duration
	generationSlots     chan struct{}
	maxPending          int
	maxStoredOperations int
	now                 func() time.Time

	ctx    context.Context
	cancel context.CancelFunc

	mu             sync.Mutex
	active         map[string]struct{}
	queued         map[string]*catalog.Request
	queue          []string
	terminalErrors map[string]error
	publishMu      sync.Mutex
	handlers       sync.WaitGroup
	workers        sync.WaitGroup
	closeOnce      sync.Once
	closeErr       error
	closed         bool
}

// New validates all release-bound inputs, loads the mounted signing key,
// acquires the persistent single-writer lock, and resumes pending operations.
func New(config Config) (_ *Service, retErr error) {
	if config.Generator == nil {
		return nil, errors.New("catalog generator is required")
	}
	origin, err := validateOrigin(config.Origin)
	if err != nil {
		return nil, err
	}
	if err := validateKeyID(config.SigningKeyID); err != nil {
		return nil, err
	}
	if err := validateComponentIdentity(config.CatalogService); err != nil {
		return nil, fmt.Errorf("catalog service identity: %w", err)
	}
	if err := validateComponentIdentity(config.Extractor); err != nil {
		return nil, fmt.Errorf("catalog extractor identity: %w", err)
	}
	lifetime := config.CatalogSetLifetime
	if lifetime == 0 {
		lifetime = catalog.MaxCatalogSetLifetime
	}
	if lifetime <= 0 || lifetime > catalog.MaxCatalogSetLifetime {
		return nil, fmt.Errorf("catalog-set lifetime %s is outside 0..%s", lifetime, catalog.MaxCatalogSetLifetime)
	}
	generationTimeout := config.GenerationTimeout
	if generationTimeout == 0 {
		generationTimeout = DefaultGenerationTimeout
	}
	if generationTimeout <= 0 || generationTimeout > DefaultGenerationTimeout {
		return nil, fmt.Errorf("generation timeout %s is outside 0..%s", generationTimeout, DefaultGenerationTimeout)
	}
	retryAfter := config.RetryAfter
	if retryAfter == 0 {
		retryAfter = DefaultRetryAfter
	}
	if retryAfter < time.Second || retryAfter > time.Hour || retryAfter%time.Second != 0 {
		return nil, errors.New("retry-after must be a whole number of seconds in 1..3600")
	}
	catalogRefresh := config.CatalogRefresh
	if catalogRefresh == 0 {
		catalogRefresh = DefaultCatalogRefresh
	}
	if catalogRefresh <= 0 || catalogRefresh > catalog.MaxCatalogSetLifetime {
		return nil, fmt.Errorf("catalog refresh %s is outside 0..%s", catalogRefresh, catalog.MaxCatalogSetLifetime)
	}
	maxConcurrent := config.MaxConcurrentGenerations
	if maxConcurrent == 0 {
		maxConcurrent = DefaultMaxConcurrentGenerations
	}
	if maxConcurrent < 1 || maxConcurrent > 64 {
		return nil, errors.New("max concurrent generations must be in 1..64")
	}
	maxPending := config.MaxPendingGenerations
	if maxPending == 0 {
		maxPending = DefaultMaxPendingGenerations
	}
	if maxPending < 1 || maxPending > maxPendingGenerationsLimit {
		return nil, fmt.Errorf("max pending generations must be in 1..%d", maxPendingGenerationsLimit)
	}
	maxStoredOperations := config.MaxStoredOperations
	if maxStoredOperations == 0 {
		maxStoredOperations = DefaultMaxStoredOperations
	}
	if maxStoredOperations < 1 || maxStoredOperations > 100000 {
		return nil, errors.New("max stored operations must be in 1..100000")
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	key, err := catalogauth.LoadSigningKey(config.SigningKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load catalog signing key: %w", err)
	}
	keys, err := metadata.NewKeySet(map[string]*rsa.PublicKey{config.SigningKeyID: &key.PublicKey})
	if err != nil {
		return nil, fmt.Errorf("construct catalog verification key set: %w", err)
	}
	persistentStore, err := openStoreWithArtifacts(config.StoreDir, config.ArtifactStore)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = persistentStore.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		store:               persistentStore,
		origin:              origin,
		generator:           config.Generator,
		signingKey:          key,
		signingKeyID:        config.SigningKeyID,
		verificationKeys:    keys,
		catalogService:      config.CatalogService,
		extractor:           config.Extractor,
		catalogSetLifetime:  lifetime,
		generationTimeout:   generationTimeout,
		retryAfterSeconds:   int(retryAfter / time.Second),
		catalogRefresh:      catalogRefresh,
		generationSlots:     make(chan struct{}, maxConcurrent),
		maxPending:          maxPending,
		maxStoredOperations: maxStoredOperations,
		now:                 now,
		ctx:                 ctx,
		cancel:              cancel,
		active:              make(map[string]struct{}),
		queued:              make(map[string]*catalog.Request),
		terminalErrors:      make(map[string]error),
	}
	if err := service.resumePending(); err != nil {
		cancel()
		service.workers.Wait()
		return nil, fmt.Errorf("resume pending catalog operations: %w", err)
	}
	return service, nil
}

// Close cancels active generation, waits for workers to stop, and releases the
// single-writer lock. A generator must honor context cancellation.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		s.handlers.Wait()
		s.workers.Wait()
		generatorErr := closeGenerator(s.generator)
		storeErr := s.store.Close()
		s.closeErr = errors.Join(generatorErr, storeErr)
	})
	return s.closeErr
}

// OperationID returns the deterministic operation ID for a canonical request.
func OperationID(request *catalog.Request) (string, error) {
	requestDigest, err := catalog.RequestDigest(request)
	if err != nil {
		return "", err
	}
	return operationIDFromDigest(requestDigest), nil
}

func operationIDFromDigest(requestDigest digest.Digest) string {
	return "op-" + requestDigest.Encoded()
}

func validOperationID(value string) bool {
	return strings.HasPrefix(value, "op-") && validSHA256Hex(strings.TrimPrefix(value, "op-"))
}

func (s *Service) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("X-Content-Type-Options", "nosniff")
	s.mu.Lock()
	if !s.closed {
		s.handlers.Add(1)
	}
	closed := s.closed
	s.mu.Unlock()
	if closed {
		s.writeHTTPFailure(response, http.StatusServiceUnavailable, catalog.FailureUnavailable, "catalog service is shutting down")
		return
	}
	defer s.handlers.Done()
	switch {
	case request.URL.Path == "/v1/catalog-sets":
		if request.Method != http.MethodPost {
			response.Header().Set("Allow", http.MethodPost)
			s.writeHTTPFailure(response, http.StatusMethodNotAllowed, catalog.FailurePolicy, "method not allowed")
			return
		}
		s.handleCatalogSet(response, request)
	case strings.HasPrefix(request.URL.Path, "/v1/operations/"):
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			s.writeHTTPFailure(response, http.StatusMethodNotAllowed, catalog.FailurePolicy, "method not allowed")
			return
		}
		s.handleOperation(response, request)
	case strings.HasPrefix(request.URL.Path, catalogartifactstore.HTTPPathPrefix):
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
			s.writeHTTPFailure(response, http.StatusMethodNotAllowed, catalog.FailurePolicy, "method not allowed")
			return
		}
		s.handleArtifact(response, request)
	case strings.HasPrefix(request.URL.Path, catalog.CatalogDocumentPathPrefix):
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			s.writeHTTPFailure(response, http.StatusMethodNotAllowed, catalog.FailurePolicy, "method not allowed")
			return
		}
		s.handleCatalog(response, request)
	default:
		s.writeHTTPFailure(response, http.StatusNotFound, catalog.FailureUnavailable, "resource not found")
	}
}

func (s *Service) handleCatalogSet(response http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		s.writeHTTPFailure(response, http.StatusBadRequest, catalog.FailurePolicy, "queries are not supported")
		return
	}
	if encoding := request.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		s.writeHTTPFailure(response, http.StatusUnsupportedMediaType, catalog.FailurePolicy, "content encoding is not supported")
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	invalidParameters := len(parameters) > 1
	if len(parameters) == 1 {
		_, invalidParameters = parameters["charset"]
		invalidParameters = !invalidParameters
	}
	if err != nil || mediaType != "application/json" || invalidParameters || (parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8")) {
		s.writeHTTPFailure(response, http.StatusUnsupportedMediaType, catalog.FailurePolicy, "content type must be application/json")
		return
	}
	if request.ContentLength > catalog.MaxRequestBytes {
		s.writeHTTPFailure(response, http.StatusRequestEntityTooLarge, catalog.FailurePolicy, "catalog request exceeds size limit")
		return
	}
	body := http.MaxBytesReader(response, request.Body, catalog.MaxRequestBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		s.writeHTTPFailure(response, http.StatusRequestEntityTooLarge, catalog.FailurePolicy, "catalog request exceeds size limit")
		return
	}
	decoded, err := catalog.DecodeRequest(data)
	if err != nil {
		s.writeHTTPFailure(response, http.StatusBadRequest, catalog.FailurePolicy, "invalid catalog request")
		return
	}
	canonical, err := catalog.CanonicalRequest(decoded)
	if err != nil {
		s.writeHTTPFailure(response, http.StatusBadRequest, catalog.FailurePolicy, "invalid catalog request")
		return
	}
	requestDigest, err := catalog.RequestDigest(decoded)
	if err != nil {
		s.writeHTTPFailure(response, http.StatusBadRequest, catalog.FailurePolicy, "invalid catalog request")
		return
	}
	id := operationIDFromDigest(requestDigest)

	s.mu.Lock()
	if terminalErr := s.terminalErrors[id]; terminalErr != nil {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog operation could not be persisted")
		return
	}
	operation, err := s.store.loadOperation(id)
	if errors.Is(err, os.ErrNotExist) {
		if !s.generationCapacityAvailableLocked(id) {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusServiceUnavailable, catalog.FailureUnavailable, generationAdmissionFailureMessage)
			return
		}
		if evictErr := s.evictOperationsLocked(s.maxStoredOperations - 1); evictErr != nil {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog operation storage is unavailable")
			return
		}
		ids, listErr := s.store.listOperations()
		if listErr != nil {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog operation storage is unavailable")
			return
		}
		if len(ids) >= s.maxStoredOperations {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusServiceUnavailable, catalog.FailureUnavailable, "catalog operation quota is exhausted")
			return
		}
	}
	if err := s.store.putRequest(id, canonical); err != nil {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog service storage is unavailable")
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		operation = s.pendingOperation(id)
		if err := s.store.saveOperation(operation); err != nil {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog service storage is unavailable")
			return
		}
		s.startLocked(id, decoded)
		s.mu.Unlock()
		s.writePending(response, http.StatusAccepted, operation)
		return
	}
	if err != nil {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog operation state is unavailable")
		return
	}
	if operation.Status == catalog.OperationCompleted && s.cachedResultFresh(decoded, operation.Result) {
		result := operation.Result
		s.mu.Unlock()
		s.writeJSON(response, http.StatusOK, result, true)
		return
	}
	if !s.generationCapacityAvailableLocked(id) {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusServiceUnavailable, catalog.FailureUnavailable, generationAdmissionFailureMessage)
		return
	}
	if operation.Status != catalog.OperationPending {
		operation = s.pendingOperation(id)
		if err := s.store.saveOperation(operation); err != nil {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog service storage is unavailable")
			return
		}
	}
	s.startLocked(id, decoded)
	s.mu.Unlock()
	s.writePending(response, http.StatusAccepted, operation)
}

func (s *Service) handleOperation(response http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		s.writeHTTPFailure(response, http.StatusBadRequest, catalog.FailurePolicy, "queries are not supported")
		return
	}
	id := strings.TrimPrefix(request.URL.Path, "/v1/operations/")
	if !validOperationID(id) || strings.Contains(id, "/") {
		s.writeHTTPFailure(response, http.StatusNotFound, catalog.FailureUnavailable, "operation not found")
		return
	}

	s.mu.Lock()
	if terminalErr := s.terminalErrors[id]; terminalErr != nil {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog operation could not be persisted")
		return
	}
	operation, err := s.store.loadOperation(id)
	if errors.Is(err, os.ErrNotExist) {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusNotFound, catalog.FailureUnavailable, "operation not found")
		return
	}
	if err != nil {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog operation state is unavailable")
		return
	}
	decoded, _, err := s.store.loadRequest(id)
	if err != nil {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog operation request is unavailable")
		return
	}
	expectedID, err := OperationID(decoded)
	if err != nil || expectedID != id {
		s.mu.Unlock()
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog operation request is invalid")
		return
	}
	if operation.Status == catalog.OperationCompleted && !s.cachedResultFresh(decoded, operation.Result) {
		if !s.generationCapacityAvailableLocked(id) {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusServiceUnavailable, catalog.FailureUnavailable, generationAdmissionFailureMessage)
			return
		}
		operation = s.pendingOperation(id)
		if err := s.store.saveOperation(operation); err != nil {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog service storage is unavailable")
			return
		}
	}
	if operation.Status == catalog.OperationPending {
		if !s.generationCapacityAvailableLocked(id) {
			s.mu.Unlock()
			s.writeHTTPFailure(response, http.StatusServiceUnavailable, catalog.FailureUnavailable, generationAdmissionFailureMessage)
			return
		}
		s.startLocked(id, decoded)
	}
	s.mu.Unlock()
	if operation.Status == catalog.OperationPending {
		response.Header().Set("Retry-After", strconv.Itoa(operation.RetryAfterSeconds))
	}
	s.writeJSON(response, http.StatusOK, operation, true)
}

func (s *Service) handleArtifact(response http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" || request.URL.ForceQuery {
		s.writeHTTPFailure(response, http.StatusBadRequest, catalog.FailurePolicy, "queries are not supported")
		return
	}
	if len(request.Header.Values("Range")) != 0 {
		s.writeHTTPFailure(response, http.StatusBadRequest, catalog.FailurePolicy, "range requests are not supported")
		return
	}
	encoded := strings.TrimPrefix(request.URL.Path, catalogartifactstore.HTTPPathPrefix)
	if request.URL.RawPath != "" || !validSHA256Hex(encoded) || strings.Contains(encoded, "/") {
		s.writeHTTPFailure(response, http.StatusNotFound, catalog.FailureUnavailable, "artifact not found")
		return
	}
	expected := digest.Digest("sha256:" + encoded)
	artifact, err := s.store.artifacts.Open(expected)
	if errors.Is(err, os.ErrNotExist) {
		s.writeHTTPFailure(response, http.StatusNotFound, catalog.FailureUnavailable, "artifact not found")
		return
	}
	if err != nil {
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "artifact storage is unavailable")
		return
	}
	defer artifact.Close()

	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	response.Header().Set("Content-Type", "application/gzip")
	response.Header().Set("Content-Length", strconv.FormatInt(artifact.Size(), 10))
	response.Header().Set("ETag", `"`+expected.String()+`"`)
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = io.Copy(response, artifact)
	}
}

func (s *Service) handleCatalog(response http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		s.writeHTTPFailure(response, http.StatusBadRequest, catalog.FailurePolicy, "queries are not supported")
		return
	}
	encoded := strings.TrimPrefix(request.URL.Path, catalog.CatalogDocumentPathPrefix)
	if !validSHA256Hex(encoded) || strings.Contains(encoded, "/") {
		s.writeHTTPFailure(response, http.StatusNotFound, catalog.FailureUnavailable, "catalog not found")
		return
	}
	data, err := s.store.loadCatalog(encoded)
	if errors.Is(err, os.ErrNotExist) {
		s.writeHTTPFailure(response, http.StatusNotFound, catalog.FailureUnavailable, "catalog not found")
		return
	}
	if err != nil {
		s.writeHTTPFailure(response, http.StatusInternalServerError, catalog.FailureUnavailable, "catalog storage is unavailable")
		return
	}
	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Content-Length", strconv.Itoa(len(data)))
	response.Header().Set("ETag", `"sha256:`+encoded+`"`)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(data)
}

func (s *Service) pendingOperation(id string) *catalog.Operation {
	return &catalog.Operation{
		SchemaVersion:     catalog.OperationSchemaVersion,
		ID:                id,
		Status:            catalog.OperationPending,
		RetryAfterSeconds: s.retryAfterSeconds,
	}
}

func (s *Service) startLocked(id string, request *catalog.Request) {
	if s.closed || s.terminalErrors[id] != nil {
		return
	}
	if _, running := s.active[id]; running {
		return
	}
	if _, queued := s.queued[id]; queued {
		return
	}
	if !s.generationCapacityAvailableLocked(id) {
		return
	}
	cloned, err := cloneRequest(request)
	if err != nil {
		s.terminalErrors[id] = err
		return
	}
	select {
	case s.generationSlots <- struct{}{}:
		s.launchGenerationLocked(id, cloned)
	default:
		s.queued[id] = cloned
		s.queue = append(s.queue, id)
	}
}

func (s *Service) generationCapacityAvailableLocked(id string) bool {
	if _, active := s.active[id]; active {
		return true
	}
	if _, queued := s.queued[id]; queued {
		return true
	}
	return len(s.active)+len(s.queued) < s.maxPending
}

func (s *Service) launchGenerationLocked(id string, request *catalog.Request) {
	s.active[id] = struct{}{}
	s.workers.Add(1)
	go s.runGeneration(id, request)
}

func (s *Service) generationDone(id string) {
	s.mu.Lock()
	delete(s.active, id)
	<-s.generationSlots
	if !s.closed {
		for len(s.queue) > 0 {
			nextID := s.queue[0]
			s.queue = s.queue[1:]
			request := s.queued[nextID]
			delete(s.queued, nextID)
			if request == nil || s.terminalErrors[nextID] != nil {
				continue
			}
			s.generationSlots <- struct{}{}
			s.launchGenerationLocked(nextID, request)
			break
		}
	}
	s.mu.Unlock()
	s.workers.Done()
}

func (s *Service) runGeneration(id string, request *catalog.Request) {
	defer s.generationDone(id)

	ctx, cancel := context.WithTimeout(s.ctx, s.generationTimeout)
	defer cancel()
	generatorRequest, err := cloneRequest(request)
	if err != nil {
		s.finishFailure(id, catalog.Failure{Code: catalog.FailurePolicy, Message: "persisted catalog request is invalid"})
		return
	}
	generated, err := callGenerator(ctx, s.generator, generatorRequest)
	if s.ctx.Err() != nil {
		return
	}
	if ctx.Err() != nil {
		s.finishFailure(id, catalog.Failure{Code: catalog.FailureTimeout, Message: "catalog generation timed out"})
		return
	}
	if err != nil {
		s.finishFailure(id, failureForError(ctx, err))
		return
	}
	result, err := s.publishGenerated(request, generated)
	if err != nil {
		var invalid *invalidGenerationError
		if errors.As(err, &invalid) {
			s.finishFailure(id, catalog.Failure{Code: catalog.FailurePolicy, Message: "generator returned invalid catalog data"})
			return
		}
		s.finishFailure(id, catalog.Failure{Code: catalog.FailureUnavailable, Message: "catalog service could not publish generated data"})
		return
	}
	operation := &catalog.Operation{SchemaVersion: catalog.OperationSchemaVersion, ID: id, Status: catalog.OperationCompleted, Result: result}
	s.finishOperation(operation)
}

func cloneRequest(request *catalog.Request) (*catalog.Request, error) {
	canonical, err := catalog.CanonicalRequest(request)
	if err != nil {
		return nil, err
	}
	return catalog.DecodeRequest(canonical)
}

func callGenerator(ctx context.Context, generator Generator, request *catalog.Request) (generated *GeneratedSet, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("catalog generator panicked")
		}
	}()
	return generator.Generate(ctx, request)
}

func (s *Service) finishFailure(id string, failure catalog.Failure) {
	if err := catalog.ValidateFailure(failure); err != nil {
		failure = catalog.Failure{Code: catalog.FailureUnavailable, Message: "catalog generation failed"}
	}
	operation := &catalog.Operation{SchemaVersion: catalog.OperationSchemaVersion, ID: id, Status: catalog.OperationFailed, Failure: &failure}
	s.finishOperation(operation)
}

func (s *Service) finishOperation(operation *catalog.Operation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.saveOperation(operation); err != nil {
		s.terminalErrors[operation.ID] = err
	} else {
		delete(s.terminalErrors, operation.ID)
		if operation.Status == catalog.OperationCompleted {
			_ = s.gcCatalogsLocked()
		}
	}
}

func (s *Service) evictOperationsLocked(max int) error {
	infos, err := s.store.operationInfos()
	if err != nil {
		return err
	}
	remaining := len(infos)
	if remaining <= max {
		return nil
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ModTime.Before(infos[j].ModTime) })
	for _, info := range infos {
		if remaining <= max {
			break
		}
		if _, active := s.active[info.ID]; active {
			continue
		}
		operation, err := s.store.loadOperation(info.ID)
		if err != nil {
			continue
		}
		if operation.Status == catalog.OperationPending && s.terminalErrors[info.ID] == nil {
			continue
		}
		if err := s.store.removeOperation(info.ID); err != nil {
			return err
		}
		delete(s.terminalErrors, info.ID)
		remaining--
	}
	return nil
}

func (s *Service) gcCatalogsLocked() error {
	referenced := map[string]struct{}{}
	ids, err := s.store.listOperations()
	if err != nil {
		return err
	}
	for _, id := range ids {
		operation, err := s.store.loadOperation(id)
		if err != nil || operation.Status != catalog.OperationCompleted || operation.Result == nil {
			continue
		}
		request, _, err := s.store.loadRequest(id)
		if err != nil {
			continue
		}
		requestDigest, err := catalog.RequestDigest(request)
		if err != nil {
			continue
		}
		verified, err := catalogauth.Verify(operation.Result.JWS, s.verificationKeys, s.signingKeyID, requestDigest.String(), request.CoreSnapshotDigest, s.now().UTC(), false)
		if err != nil {
			continue
		}
		for _, reference := range verified.Payload.Catalogs {
			referenced[strings.TrimPrefix(reference.SHA256, "sha256:")] = struct{}{}
		}
	}
	return s.store.removeCatalogsExcept(referenced, s.now().UTC(), s.catalogSetLifetime)
}

func failureForError(ctx context.Context, err error) catalog.Failure {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return catalog.Failure{Code: catalog.FailureTimeout, Message: "catalog generation timed out"}
	}
	var failureError *FailureError
	if errors.As(err, &failureError) && failureError != nil {
		if catalog.ValidateFailure(failureError.Failure) == nil {
			return failureError.Failure
		}
	}
	return catalog.Failure{Code: catalog.FailureUnavailable, Message: "catalog generation is unavailable"}
}

type invalidGenerationError struct{ err error }

func (e *invalidGenerationError) Error() string { return e.err.Error() }
func (e *invalidGenerationError) Unwrap() error { return e.err }

func invalidGeneration(format string, args ...any) error {
	return &invalidGenerationError{err: fmt.Errorf(format, args...)}
}

func (s *Service) publishGenerated(request *catalog.Request, generated *GeneratedSet) (*catalog.CatalogSetResult, error) {
	if generated == nil {
		return nil, invalidGeneration("generator returned nil result")
	}
	if err := validateGeneratedSet(request, generated); err != nil {
		return nil, err
	}
	requestDigest, err := catalog.RequestDigest(request)
	if err != nil {
		return nil, invalidGeneration("request digest: %w", err)
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	publishedAt := s.now().UTC().Round(0)
	if publishedAt.IsZero() {
		return nil, errors.New("catalog service clock returned zero time")
	}

	catalogs := slices.Clone(generated.Catalogs)
	sort.Slice(catalogs, func(i, j int) bool { return catalogs[i].Tap.ID < catalogs[j].Tap.ID })
	type stagedCatalog struct {
		tap    catalog.TapID
		data   []byte
		digest string
	}
	staged := make([]stagedCatalog, 0, len(catalogs))
	references := make([]catalog.CatalogReference, 0, len(catalogs))
	for i := range catalogs {
		catalogDocument := catalogs[i]
		catalogDocument.PublishedAt = publishedAt
		sourceBytes, err := json.Marshal(catalogDocument.Tap)
		if err != nil {
			return nil, err
		}
		sourceIdentity := digest.FromBytes(sourceBytes).String()
		sequence, err := s.store.nextSequence(catalogDocument.Tap.ID, sourceIdentity)
		if err != nil {
			return nil, fmt.Errorf("allocate catalog sequence for %s: %w", catalogDocument.Tap.ID, err)
		}
		catalogDocument.Sequence = sequence
		canonical, err := catalog.CanonicalTapCatalog(&catalogDocument)
		if err != nil {
			return nil, invalidGeneration("canonicalize tap catalog %s: %w", catalogDocument.Tap.ID, err)
		}
		documentDigest := digest.FromBytes(canonical).String()
		staged = append(staged, stagedCatalog{tap: catalogDocument.Tap.ID, data: canonical, digest: documentDigest})
		references = append(references, catalog.CatalogReference{Tap: catalogDocument.Tap, PublishedAt: publishedAt, Sequence: sequence, URL: s.origin + catalog.CatalogDocumentPathPrefix + strings.TrimPrefix(documentDigest, "sha256:"), Size: int64(len(canonical)), SHA256: documentDigest})
	}
	payload := &catalog.CatalogSetPayload{
		SchemaVersion:      catalog.CatalogSetSchemaVersion,
		RequestDigest:      requestDigest.String(),
		CoreSnapshotDigest: request.CoreSnapshotDigest,
		GeneratedAt:        publishedAt,
		ExpiresAt:          publishedAt.Add(s.catalogSetLifetime),
		CatalogService:     s.catalogService,
		Extractor:          s.extractor,
		Catalogs:           references,
		Results:            slices.Clone(generated.Results),
	}
	if err := catalog.ValidateCatalogSetPayload(payload); err != nil {
		return nil, invalidGeneration("validate catalog-set payload: %w", err)
	}
	for _, item := range staged {
		persisted, err := s.store.putCatalog(item.data)
		if err != nil {
			return nil, fmt.Errorf("persist tap catalog %s: %w", item.tap, err)
		}
		if persisted != item.digest {
			return nil, errors.New("persisted catalog digest changed")
		}
	}
	envelope, err := catalogauth.Sign(payload, s.signingKeyID, s.signingKey)
	if err != nil {
		return nil, fmt.Errorf("sign catalog-set payload: %w", err)
	}
	payloadDigest, err := catalog.CatalogSetPayloadDigest(payload)
	if err != nil {
		return nil, fmt.Errorf("digest catalog-set payload: %w", err)
	}
	result := &catalog.CatalogSetResult{
		SchemaVersion: catalog.ResultSchemaVersion,
		RequestDigest: requestDigest.String(),
		PayloadDigest: payloadDigest.String(),
		JWS:           json.RawMessage(bytes.Clone(envelope)),
	}
	if err := catalog.ValidateCatalogSetResult(result); err != nil {
		return nil, fmt.Errorf("validate catalog-set result: %w", err)
	}
	return result, nil
}

func validateGeneratedSet(request *catalog.Request, generated *GeneratedSet) error {
	if generated.Catalogs == nil || len(generated.Catalogs) == 0 {
		return invalidGeneration("catalogs must be non-empty")
	}
	if len(generated.Catalogs) > catalog.MaxTaps {
		return invalidGeneration("catalog count exceeds limit")
	}
	targets, err := request.NormalizedTargets()
	if err != nil {
		return invalidGeneration("normalize request targets: %w", err)
	}
	if generated.Results == nil || len(generated.Results) != len(targets) {
		return invalidGeneration("platform result count %d does not match request %d", len(generated.Results), len(targets))
	}
	targetRoots := make(map[string][]catalog.FormulaID, len(targets))
	for _, target := range targets {
		key := target.Platform.OS + "/" + target.Platform.Architecture
		roots := append(slices.Clone(target.CoreRoots), target.ExternalRoots...)
		slices.Sort(roots)
		targetRoots[key] = roots
	}
	seenPlatforms := make(map[string]struct{}, len(generated.Results))
	for i, result := range generated.Results {
		if _, err := catalog.CanonicalPlatformResult(result); err != nil {
			return invalidGeneration("results[%d]: %w", i, err)
		}
		key := result.Platform.OS + "/" + result.Platform.Architecture
		expectedRoots, expected := targetRoots[key]
		if !expected {
			return invalidGeneration("unexpected result platform %s", key)
		}
		if _, duplicate := seenPlatforms[key]; duplicate {
			return invalidGeneration("duplicate result platform %s", key)
		}
		seenPlatforms[key] = struct{}{}
		roots := make([]catalog.FormulaID, 0, len(result.Closure.RequestedMappings))
		if len(result.Closure.RequestedMappings) == 0 {
			roots = slices.Clone(result.Closure.Requested)
		} else {
			for _, mapping := range result.Closure.RequestedMappings {
				roots = append(roots, mapping.Requested)
			}
		}
		slices.Sort(roots)
		if !slices.Equal(roots, expectedRoots) {
			return invalidGeneration("result platform %s requested roots do not match request", key)
		}
	}
	seenTaps := make(map[catalog.TapID]struct{}, len(generated.Catalogs))
	validationTime := time.Unix(1, 0).UTC()
	for i, value := range generated.Catalogs {
		if value.Tap.ID.IsCore() {
			return invalidGeneration("catalogs[%d] is homebrew/core", i)
		}
		if _, duplicate := seenTaps[value.Tap.ID]; duplicate {
			return invalidGeneration("duplicate tap catalog %s", value.Tap.ID)
		}
		seenTaps[value.Tap.ID] = struct{}{}
		value.PublishedAt = validationTime
		value.Sequence = 1
		if _, err := catalog.CanonicalTapCatalog(&value); err != nil {
			return invalidGeneration("catalogs[%d]: %w", i, err)
		}
	}
	return nil
}

func (s *Service) cachedResultFresh(request *catalog.Request, result *catalog.CatalogSetResult) bool {
	if result == nil || catalog.ValidateCatalogSetResult(result) != nil {
		return false
	}
	requestDigest, err := catalog.RequestDigest(request)
	if err != nil || result.RequestDigest != requestDigest.String() {
		return false
	}
	verified, err := catalogauth.Verify(result.JWS, s.verificationKeys, s.signingKeyID, requestDigest.String(), request.CoreSnapshotDigest, s.now().UTC(), false)
	if err != nil {
		return false
	}
	if verified.Payload.CatalogService != s.catalogService || verified.Payload.Extractor != s.extractor {
		return false
	}
	if !s.now().UTC().Before(verified.Payload.GeneratedAt.Add(s.catalogRefresh)) {
		return false
	}
	payloadDigest, err := catalog.CatalogSetPayloadDigest(verified.Payload)
	if err != nil || result.PayloadDigest != payloadDigest.String() {
		return false
	}
	catalogs := make([]catalog.TapCatalog, 0, len(verified.Payload.Catalogs))
	for _, reference := range verified.Payload.Catalogs {
		if catalog.ValidateCatalogReferenceOrigin(reference, s.origin) != nil {
			return false
		}
		data, err := s.store.loadCatalog(strings.TrimPrefix(reference.SHA256, "sha256:"))
		if err != nil {
			return false
		}
		decoded, err := catalog.DecodeReferencedTapCatalog(reference, data)
		if err != nil {
			return false
		}
		catalogs = append(catalogs, *decoded)
	}
	return validateGeneratedSet(request, &GeneratedSet{Catalogs: catalogs, Results: verified.Payload.Results}) == nil
}

func (s *Service) resumePending() error {
	ids, err := s.store.listOperations()
	if err != nil {
		return err
	}
	type pendingRequest struct {
		id      string
		request *catalog.Request
	}
	pending := make([]pendingRequest, 0, min(len(ids), s.maxPending))
	for _, id := range ids {
		operation, err := s.store.loadOperation(id)
		if err != nil {
			return err
		}
		request, _, err := s.store.loadRequest(id)
		if err != nil {
			return err
		}
		expectedID, err := OperationID(request)
		if err != nil || expectedID != id {
			return fmt.Errorf("operation %s has mismatched persisted request", id)
		}
		if operation.Status == catalog.OperationPending {
			pending = append(pending, pendingRequest{id: id, request: request})
		}
	}
	if len(pending) > s.maxPending {
		return fmt.Errorf("persisted pending operation count %d exceeds admission limit %d", len(pending), s.maxPending)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range pending {
		s.startLocked(item.id, item.request)
	}
	return nil
}

func (s *Service) writePending(response http.ResponseWriter, status int, operation *catalog.Operation) {
	response.Header().Set("Retry-After", strconv.Itoa(operation.RetryAfterSeconds))
	s.writeJSON(response, status, operation, true)
}

func (s *Service) writeHTTPFailure(response http.ResponseWriter, status int, code catalog.FailureCode, message string) {
	failure := catalog.Failure{Code: code, Message: message}
	if catalog.ValidateFailure(failure) != nil {
		failure = catalog.Failure{Code: catalog.FailureUnavailable, Message: "catalog service failure"}
	}
	s.writeJSON(response, status, failure, true)
}

func (s *Service) writeJSON(response http.ResponseWriter, status int, value any, noStore bool) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(response, "catalog service failure", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if noStore {
		response.Header().Set("Cache-Control", "no-store")
	}
	response.WriteHeader(status)
	_, _ = response.Write(data)
}

func validateOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse catalog service origin: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("catalog service origin must contain only an HTTPS scheme and authority")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", errors.New("catalog service origin may use only port 443")
	}
	canonical := parsed.Scheme + "://" + parsed.Host
	sha := "sha256:" + strings.Repeat("0", 64)
	reference := catalog.CatalogReference{
		Tap: catalog.TapSource{
			ID:            "fixture/validation",
			Repository:    "https://github.com/fixture/homebrew-validation",
			Commit:        strings.Repeat("0", 40),
			TreeDigest:    sha,
			ArchiveDigest: sha,
		},
		PublishedAt: time.Unix(1, 0).UTC(),
		Sequence:    1,
		URL:         canonical + catalog.CatalogDocumentPathPrefix + strings.Repeat("0", 64),
		Size:        1,
		SHA256:      sha,
	}
	if err := catalog.ValidateCatalogReferenceOrigin(reference, canonical); err != nil {
		return "", fmt.Errorf("catalog service origin: %w", err)
	}
	return canonical, nil
}

func validateKeyID(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("catalog signing key ID is empty or unsafe")
	}
	return catalogkeys.ValidateKeyID(value)
}

func validateComponentIdentity(identity catalog.ComponentIdentity) error {
	if err := validateBoundedText(identity.Name, 128); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if err := validateBoundedText(identity.Version, 256); err != nil {
		return fmt.Errorf("version: %w", err)
	}
	parsed, err := digest.Parse(identity.Digest)
	if err != nil || parsed.Algorithm() != digest.SHA256 || parsed.String() != identity.Digest {
		return errors.New("digest must be a canonical sha256 digest")
	}
	return nil
}

func validateBoundedText(value string, maximum int) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > maximum {
		return errors.New("value is empty, padded, or overlong")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return errors.New("value contains a control character")
		}
	}
	return nil
}
