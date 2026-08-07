// Package catalogclient implements the fail-closed frontend side of the
// asynchronous catalog-service protocol.
package catalogclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogauth"
	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

const (
	DefaultPollingDeadline = 30 * time.Minute
	MaxPollingDeadline     = 24 * time.Hour
	defaultRetryAfter      = 2 * time.Second
	maxRetryAfter          = 60 * time.Second
)

type Config struct {
	Origin           string
	HTTPClient       *http.Client
	Keys             metadata.KeySet
	RequiredKeyID    string
	AllowKeyOverlap  bool
	KeyPolicy        *catalogkeys.Policy
	KeyPolicyDigest  string
	PollingDeadline  time.Duration
	MinimumSequences map[catalog.TapID]uint64
	Now              func() time.Time
	Sleep            func(context.Context, time.Duration) error
}

type Client struct {
	origin          *url.URL
	http            *http.Client
	keys            metadata.KeySet
	acceptedKeyIDs  []string
	allowKeyOverlap bool
	keyPolicy       *catalogkeys.Policy
	pollingDeadline time.Duration
	minimumSequence map[catalog.TapID]uint64
	now             func() time.Time
	sleep           func(context.Context, time.Duration) error
}

type Result struct {
	Payload           *catalog.CatalogSetPayload
	Catalogs          map[catalog.TapID]*catalog.TapCatalog
	JWS               []byte
	Signatures        []metadata.SignatureInfo
	MinimumSequences  map[catalog.TapID]uint64
	SetPayloadDigest  string
	SetEnvelopeDigest string
}

type OperationError struct {
	Code catalog.FailureCode
	Msg  string
}

func (e *OperationError) Error() string {
	if e.Msg == "" {
		return "catalog operation failed: " + string(e.Code)
	}
	return fmt.Sprintf("catalog operation failed (%s): %s", e.Code, e.Msg)
}

func New(cfg Config) (*Client, error) {
	origin, err := parseOrigin(cfg.Origin)
	if err != nil {
		return nil, err
	}
	keys := cfg.Keys
	requiredKeyID := cfg.RequiredKeyID
	acceptedKeyIDs := []string{requiredKeyID}
	allowKeyOverlap := cfg.AllowKeyOverlap
	floorInput := cfg.MinimumSequences
	if cfg.KeyPolicy != nil {
		if err := catalogkeys.Validate(cfg.KeyPolicy); err != nil {
			return nil, fmt.Errorf("catalog key policy: %w", err)
		}
		digest, err := catalogkeys.Digest(cfg.KeyPolicy)
		if err != nil {
			return nil, err
		}
		if cfg.KeyPolicyDigest != "" && cfg.KeyPolicyDigest != digest.String() {
			return nil, fmt.Errorf("catalog key policy digest %s does not match %s", digest, cfg.KeyPolicyDigest)
		}
		canonicalPolicy, err := catalogkeys.Canonical(cfg.KeyPolicy)
		if err != nil {
			return nil, err
		}
		policySnapshot, err := catalogkeys.Decode(canonicalPolicy)
		if err != nil {
			return nil, err
		}
		cfg.KeyPolicy = policySnapshot
		keys, err = cfg.KeyPolicy.KeySet()
		if err != nil {
			return nil, err
		}
		requiredKeyID = cfg.KeyPolicy.RequiredKeyID
		acceptedKeyIDs = cfg.KeyPolicy.AcceptedKeyIDs()
		allowKeyOverlap = cfg.KeyPolicy.AllowUnknownOverlapSigners
		policyFloors := cfg.KeyPolicy.MinimumSequences()
		if len(floorInput) == 0 {
			floorInput = policyFloors
		} else {
			for tap, minimum := range policyFloors {
				if supplied, ok := floorInput[tap]; !ok || supplied < minimum {
					return nil, fmt.Errorf("catalog sequence floor for %s is below key-policy floor %d", tap, minimum)
				}
			}
		}
	} else if cfg.KeyPolicyDigest != "" {
		return nil, errors.New("catalog key policy digest was supplied without a policy")
	}
	if err := catalogkeys.ValidateKeyID(requiredKeyID); err != nil {
		return nil, fmt.Errorf("catalog required key ID: %w", err)
	}
	for _, keyID := range acceptedKeyIDs {
		if err := catalogkeys.ValidateKeyID(keyID); err != nil {
			return nil, fmt.Errorf("catalog accepted key ID: %w", err)
		}
	}
	deadline := cfg.PollingDeadline
	if deadline == 0 {
		deadline = DefaultPollingDeadline
	}
	if deadline <= 0 || deadline > MaxPollingDeadline {
		return nil, fmt.Errorf("catalog polling deadline %s is outside 0..%s", deadline, MaxPollingDeadline)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	// Catalog operations and documents must never redirect away from the
	// release-bound service origin. Preserve only transport and timeout seams.
	httpClient = &http.Client{Transport: httpClient.Transport, Timeout: httpClient.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	floor := make(map[catalog.TapID]uint64, len(floorInput))
	for tap, sequence := range floorInput {
		if err := tap.Validate(); err != nil || tap.IsCore() || sequence == 0 {
			return nil, fmt.Errorf("invalid catalog sequence floor %q=%d", tap, sequence)
		}
		floor[tap] = sequence
	}
	return &Client{origin: origin, http: httpClient, keys: keys, acceptedKeyIDs: slices.Clone(acceptedKeyIDs), allowKeyOverlap: allowKeyOverlap, keyPolicy: cfg.KeyPolicy, pollingDeadline: deadline, minimumSequence: floor, now: now, sleep: sleep}, nil
}

func (c *Client) Resolve(ctx context.Context, request *catalog.Request) (*Result, error) {
	requestBytes, err := catalog.CanonicalRequest(request)
	if err != nil {
		return nil, err
	}
	requestDigest, err := catalog.RequestDigest(request)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.pollingDeadline)
	defer cancel()

	endpoint := c.resolvePath("/v1/catalog-sets")
	response, err := c.do(ctx, http.MethodPost, endpoint, requestBytes)
	if err != nil {
		return nil, err
	}
	var result *catalog.CatalogSetResult
	switch response.status {
	case http.StatusOK:
		result, err = catalog.DecodeCatalogSetResult(response.body)
	case http.StatusAccepted:
		var operation *catalog.Operation
		operation, err = catalog.DecodeOperation(response.body)
		if err == nil {
			result, err = c.poll(ctx, operation, response.retryAfter)
		}
	default:
		err = fmt.Errorf("catalog service POST returned HTTP %d", response.status)
	}
	if err != nil {
		return nil, err
	}
	if result.RequestDigest != requestDigest.String() {
		return nil, fmt.Errorf("catalog result request digest %s does not match %s", result.RequestDigest, requestDigest)
	}
	verified, err := catalogauth.VerifyAccepted(result.JWS, c.keys, c.acceptedKeyIDs, requestDigest.String(), request.CoreSnapshotDigest, c.now(), c.allowKeyOverlap)
	if err != nil {
		return nil, err
	}
	if err := catalog.ValidateCatalogSetBinding(verified.Payload, request); err != nil {
		return nil, fmt.Errorf("catalog-set request binding: %w", err)
	}
	payloadDigest, err := catalog.CatalogSetPayloadDigest(verified.Payload)
	if err != nil {
		return nil, err
	}
	if result.PayloadDigest != payloadDigest.String() {
		return nil, fmt.Errorf("catalog result payload digest %s does not match authenticated payload %s", result.PayloadDigest, payloadDigest)
	}
	if c.keyPolicy != nil {
		if err := c.keyPolicy.AuthorizePayload(verified.Payload); err != nil {
			return nil, fmt.Errorf("authorize catalog-set components: %w", err)
		}
	}
	catalogs, err := c.fetchCatalogs(ctx, verified.Payload.Catalogs)
	if err != nil {
		return nil, err
	}
	envelopeSum := sha256.Sum256(result.JWS)
	return &Result{Payload: verified.Payload, Catalogs: catalogs, JWS: bytes.Clone(result.JWS), Signatures: verified.Signatures, MinimumSequences: cloneSequenceFloors(c.minimumSequence), SetPayloadDigest: result.PayloadDigest, SetEnvelopeDigest: "sha256:" + hex.EncodeToString(envelopeSum[:])}, nil
}

type response struct {
	status     int
	body       []byte
	retryAfter time.Duration
}

func (c *Client) do(ctx context.Context, method, endpoint string, body []byte) (*response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog service %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, catalog.MaxOperationBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > catalog.MaxOperationBytes {
		return nil, errors.New("catalog service response exceeds limit")
	}
	return &response{status: resp.StatusCode, body: data, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}, nil
}

func (c *Client) poll(ctx context.Context, operation *catalog.Operation, headerRetry time.Duration) (*catalog.CatalogSetResult, error) {
	for {
		switch operation.Status {
		case catalog.OperationCompleted:
			return operation.Result, nil
		case catalog.OperationFailed:
			return nil, &OperationError{Code: operation.Failure.Code, Msg: operation.Failure.Message}
		case catalog.OperationPending:
		default:
			return nil, fmt.Errorf("unsupported catalog operation status %q", operation.Status)
		}
		delay := time.Duration(operation.RetryAfterSeconds) * time.Second
		if delay == 0 {
			delay = headerRetry
		}
		if delay <= 0 {
			delay = defaultRetryAfter
		}
		if delay > maxRetryAfter {
			delay = maxRetryAfter
		}
		if err := c.sleep(ctx, delay); err != nil {
			return nil, fmt.Errorf("catalog operation %q polling deadline: %w", operation.ID, err)
		}
		response, err := c.do(ctx, http.MethodGet, c.resolvePath("/v1/operations/"+url.PathEscape(operation.ID)), nil)
		if err != nil {
			return nil, err
		}
		if response.status != http.StatusOK {
			return nil, fmt.Errorf("catalog operation %q returned HTTP %d", operation.ID, response.status)
		}
		headerRetry = response.retryAfter
		nextOperation, err := catalog.DecodeOperation(response.body)
		if err != nil {
			return nil, err
		}
		if nextOperation.ID != operation.ID {
			return nil, fmt.Errorf("catalog operation ID changed from %q to %q", operation.ID, nextOperation.ID)
		}
		operation = nextOperation
	}
}

func (c *Client) fetchCatalogs(ctx context.Context, references []catalog.CatalogReference) (map[catalog.TapID]*catalog.TapCatalog, error) {
	result := make(map[catalog.TapID]*catalog.TapCatalog, len(references))
	var aggregate int64
	for _, reference := range references {
		if err := catalog.ValidateCatalogReferenceOrigin(reference, c.origin.String()); err != nil {
			return nil, err
		}
		if floor := c.minimumSequence[reference.Tap.ID]; floor > 0 && reference.Sequence < floor {
			return nil, fmt.Errorf("catalog sequence rollback for %s: %d is below %d", reference.Tap.ID, reference.Sequence, floor)
		}
		if aggregate > catalog.MaxAggregateCatalogBytes-reference.Size {
			return nil, errors.New("aggregate catalog bytes exceed limit")
		}
		aggregate += reference.Size
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reference.URL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch catalog %s: %w", reference.Tap.ID, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch catalog %s returned HTTP %d", reference.Tap.ID, resp.StatusCode)
		}
		if resp.ContentLength >= 0 && resp.ContentLength != reference.Size {
			resp.Body.Close()
			return nil, fmt.Errorf("catalog %s Content-Length %d does not match signed size %d", reference.Tap.ID, resp.ContentLength, reference.Size)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, reference.Size+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if int64(len(data)) != reference.Size {
			return nil, fmt.Errorf("catalog %s size %d does not match signed size %d", reference.Tap.ID, len(data), reference.Size)
		}
		sum := sha256.Sum256(data)
		actual := "sha256:" + hex.EncodeToString(sum[:])
		if actual != reference.SHA256 {
			return nil, fmt.Errorf("catalog %s digest %s does not match signed digest %s", reference.Tap.ID, actual, reference.SHA256)
		}
		document, err := catalog.DecodeTapCatalog(data)
		if err != nil {
			return nil, fmt.Errorf("decode catalog %s: %w", reference.Tap.ID, err)
		}
		if err := catalog.VerifyCatalogReference(reference, document); err != nil {
			return nil, err
		}
		if _, duplicate := result[reference.Tap.ID]; duplicate {
			return nil, fmt.Errorf("duplicate catalog %s", reference.Tap.ID)
		}
		result[reference.Tap.ID] = document
	}
	return result, nil
}

func cloneSequenceFloors(input map[catalog.TapID]uint64) map[catalog.TapID]uint64 {
	result := make(map[catalog.TapID]uint64, len(input))
	for tap, minimum := range input {
		result[tap] = minimum
	}
	return result
}

func parseOrigin(raw string) (*url.URL, error) {
	if err := catalog.ValidateServiceOrigin(raw); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed, nil
}

func (c *Client) resolvePath(value string) string {
	resolved := *c.origin
	resolved.Path = path.Clean("/" + strings.TrimPrefix(value, "/"))
	return resolved.String()
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func encode(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
