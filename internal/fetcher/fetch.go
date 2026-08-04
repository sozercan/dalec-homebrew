package fetcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Fetcher performs one bounded bottle fetch at a time.
type Fetcher struct {
	resolver publicResolver
	client   *http.Client
	timeouts Timeouts
}

// New validates the fetcher's bounded timeout policy and constructs a client
// that never uses environment proxies, cookies, HTTP-layer compression, or
// automatic redirects.
func New(config Config) (*Fetcher, error) {
	timeouts, err := normalizeTimeouts(config.Timeouts)
	if err != nil {
		return nil, err
	}
	resolver := config.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	guardedResolver := publicResolver{resolver: resolver, timeout: timeouts.DNS}
	transport := config.Transport
	if transport == nil {
		dialer := config.Dialer
		if dialer == nil {
			dialer = &net.Dialer{Timeout: timeouts.Connect, KeepAlive: -1}
		}
		transport = newDefaultTransport(guardedResolver, dialer, timeouts)
	} else if config.Dialer != nil {
		return nil, errors.New("custom dialer requires the default HTTP transport")
	}
	return &Fetcher{
		resolver: guardedResolver,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeouts.Overall,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeouts: timeouts,
	}, nil
}

// Fetch streams one verified bottle to destination. Callers that need
// fail-atomic publication and read-only output modes should use FetchToFiles.
func (fetcher *Fetcher) Fetch(ctx context.Context, request Request, destination io.Writer) (Evidence, error) {
	if fetcher == nil {
		return Evidence{}, errors.New("nil fetcher")
	}
	if ctx == nil {
		return Evidence{}, errors.New("nil fetch context")
	}
	if destination == nil {
		return Evidence{}, errors.New("nil bottle destination")
	}
	validated, err := validateRequest(request)
	if err != nil {
		return Evidence{}, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetcher.timeouts.Overall)
	defer cancel()
	return fetcher.fetch(fetchCtx, validated, destination, true)
}

// FetchObserved performs the same public-network, redirect, exact-size, and
// timeout checks as Fetch when catalog ingestion must first observe content at
// an immutable derived URL. The returned evidence contains the computed digest;
// callers must bind or compare it before trusting the bytes.
func (fetcher *Fetcher) FetchObserved(ctx context.Context, rawURL string, expectedSize int64, filename string, allowedRedirectHosts []string, destination io.Writer) (Evidence, error) {
	if fetcher == nil {
		return Evidence{}, errors.New("nil fetcher")
	}
	request := Request{SchemaVersion: RequestSchemaVersion, FetchPolicyVersion: FetchPolicyVersion, ArtifactID: "catalog-source", URL: rawURL, ExpectedSize: expectedSize, SHA256: strings.Repeat("0", 64), Filename: filename, AllowedRedirectHosts: allowedRedirectHosts}
	validated, err := validateRequest(request)
	if err != nil {
		return Evidence{}, err
	}
	if ctx == nil || destination == nil {
		return Evidence{}, errors.New("nil observed fetch context or destination")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetcher.timeouts.Overall)
	defer cancel()
	return fetcher.fetch(fetchCtx, validated, destination, false)
}

func (fetcher *Fetcher) fetch(ctx context.Context, validated validatedRequest, destination io.Writer, verifyDigest bool) (Evidence, error) {
	current := cloneURL(validated.url)
	visited := make(map[string]struct{}, MaxRedirects+1)
	hostSequence := make([]string, 0, MaxRedirects+1)
	redirects := 0

	for {
		if err := ctx.Err(); err != nil {
			return Evidence{}, fmt.Errorf("fetch bottle: %w", err)
		}
		key := current.String()
		if _, exists := visited[key]; exists {
			return Evidence{}, errors.New("bottle redirect loop detected")
		}
		visited[key] = struct{}{}

		host, err := validateEndpointURL(current)
		if err != nil {
			return Evidence{}, fmt.Errorf("validate bottle redirect target: %w", err)
		}
		if redirects > 0 {
			if _, allowed := validated.allowedRedirectHosts[host]; !allowed {
				return Evidence{}, fmt.Errorf("redirect host %q is not in the signed allowlist", host)
			}
		}
		if _, err := fetcher.resolver.lookup(ctx, host); err != nil {
			return Evidence{}, err
		}
		hostSequence = append(hostSequence, host)

		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return Evidence{}, errors.New("construct bottle request")
		}
		httpRequest.Header.Set("Accept", "application/octet-stream")
		httpRequest.Header.Set("Accept-Encoding", "identity")
		httpRequest.Header.Set("User-Agent", "dalec-homebrew-bottle-fetcher/1")

		response, err := fetcher.client.Do(httpRequest)
		if err != nil {
			return Evidence{}, redactedNetworkError{operation: "request", host: host, err: err}
		}
		if response == nil {
			return Evidence{}, fmt.Errorf("request bottle host %q: empty HTTP response", host)
		}
		if response.Body == nil {
			return Evidence{}, fmt.Errorf("request bottle host %q: empty HTTP response body", host)
		}

		if isRedirectStatus(response.StatusCode) {
			_ = response.Body.Close()
			if redirects >= MaxRedirects {
				return Evidence{}, fmt.Errorf("bottle redirect limit of %d exceeded", MaxRedirects)
			}
			location := response.Header.Get("Location")
			if location == "" {
				return Evidence{}, errors.New("bottle redirect is missing a Location header")
			}
			if len(location) > MaxURLBytes || strings.ContainsAny(location, "#\x00\r\n") {
				return Evidence{}, errors.New("bottle redirect Location is invalid")
			}
			reference, err := url.Parse(location)
			if err != nil {
				return Evidence{}, errors.New("bottle redirect Location is invalid")
			}
			next := current.ResolveReference(reference)
			nextHost, err := validateEndpointURL(next)
			if err != nil {
				return Evidence{}, fmt.Errorf("validate bottle redirect target: %w", err)
			}
			if _, allowed := validated.allowedRedirectHosts[nextHost]; !allowed {
				return Evidence{}, fmt.Errorf("redirect host %q is not in the signed allowlist", nextHost)
			}
			current = next
			redirects++
			continue
		}

		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return Evidence{}, fmt.Errorf("bottle host %q returned HTTP status %d", host, response.StatusCode)
		}
		if !hasIdentityContentEncoding(response.Header) {
			_ = response.Body.Close()
			return Evidence{}, fmt.Errorf("bottle host %q returned unsupported Content-Encoding", host)
		}
		if response.ContentLength < -1 {
			_ = response.Body.Close()
			return Evidence{}, fmt.Errorf("bottle host %q returned invalid Content-Length", host)
		}
		if response.ContentLength >= 0 && response.ContentLength != validated.request.ExpectedSize {
			_ = response.Body.Close()
			return Evidence{}, fmt.Errorf("bottle Content-Length %d does not match expected size %d", response.ContentLength, validated.request.ExpectedSize)
		}

		digest := sha256.New()
		limited := &io.LimitedReader{R: response.Body, N: validated.request.ExpectedSize + 1}
		written, copyErr := io.Copy(io.MultiWriter(destination, digest), limited)
		closeErr := response.Body.Close()
		if copyErr != nil {
			return Evidence{}, redactedNetworkError{operation: "read", host: host, err: copyErr}
		}
		if closeErr != nil {
			return Evidence{}, redactedNetworkError{operation: "close", host: host, err: closeErr}
		}
		if written > validated.request.ExpectedSize {
			return Evidence{}, fmt.Errorf("bottle body exceeds expected size %d", validated.request.ExpectedSize)
		}
		if written != validated.request.ExpectedSize {
			return Evidence{}, fmt.Errorf("bottle body size %d does not match expected size %d", written, validated.request.ExpectedSize)
		}
		actualDigest := hex.EncodeToString(digest.Sum(nil))
		if verifyDigest && actualDigest != validated.request.SHA256 {
			return Evidence{}, errors.New("bottle SHA-256 does not match the signed digest")
		}
		return Evidence{
			SchemaVersion:        EvidenceSchemaVersion,
			FetchPolicyVersion:   FetchPolicyVersion,
			ArtifactID:           validated.request.ArtifactID,
			Filename:             validated.request.Filename,
			Size:                 written,
			SHA256:               actualDigest,
			RedactedHostSequence: hostSequence,
		}, nil
	}
}

func hasIdentityContentEncoding(header http.Header) bool {
	values := header.Values("Content-Encoding")
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if !strings.EqualFold(strings.TrimSpace(value), "identity") {
			return false
		}
	}
	return true
}

func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func cloneURL(source *url.URL) *url.URL {
	result := *source
	if source.User != nil {
		copyUser := *source.User
		result.User = &copyUser
	}
	return &result
}

type redactedNetworkError struct {
	operation string
	host      string
	err       error
}

func (err redactedNetworkError) Error() string {
	return fmt.Sprintf("%s bottle host %q: network operation failed", err.operation, err.host)
}

func (err redactedNetworkError) Unwrap() error {
	return err.err
}
