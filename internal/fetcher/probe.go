package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ProbeResult records the exact public HTTPS endpoint and size observed by a
// bounded HEAD probe. RedirectHostSequence starts with the original request
// host and then records each redirect target in request order.
type ProbeResult struct {
	FinalURL             string
	Size                 int64
	RedirectHostSequence []string
}

type ProbeStatusError struct {
	Host   string
	Status int
}

func (e *ProbeStatusError) Error() string {
	if e == nil {
		return "bottle probe returned an HTTP error"
	}
	return fmt.Sprintf("bottle probe host %q returned HTTP status %d", e.Host, e.Status)
}

// Probe discovers the final public HTTPS URL, redirect-host sequence, and exact
// Content-Length for one bottle without downloading its body. Unlike Fetch,
// Probe intentionally takes no redirect allowlist: catalog ingestion uses the
// returned sequence to construct the later signed fetch request.
func (fetcher *Fetcher) Probe(ctx context.Context, rawURL string) (ProbeResult, error) {
	if fetcher == nil {
		return ProbeResult{}, errors.New("nil fetcher")
	}
	if ctx == nil {
		return ProbeResult{}, errors.New("nil probe context")
	}
	parsed, _, err := parseBottleURL(rawURL)
	if err != nil {
		return ProbeResult{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, fetcher.timeouts.Overall)
	defer cancel()
	return fetcher.probe(probeCtx, parsed)
}

// ProbeOptional is identical to Probe except that a definitive 404 or 410 is
// reported as absence. Other status and network failures remain errors so an
// advertised/derived evidence endpoint cannot silently downgrade on outage.
func (fetcher *Fetcher) ProbeOptional(ctx context.Context, rawURL string) (ProbeResult, bool, error) {
	result, err := fetcher.Probe(ctx, rawURL)
	if err == nil {
		return result, true, nil
	}
	var status *ProbeStatusError
	if errors.As(err, &status) && (status.Status == http.StatusNotFound || status.Status == http.StatusGone) {
		return ProbeResult{}, false, nil
	}
	return ProbeResult{}, false, err
}

func (fetcher *Fetcher) probe(ctx context.Context, initial *url.URL) (ProbeResult, error) {
	current := cloneURL(initial)
	visited := make(map[string]struct{}, MaxRedirects+1)
	hostSequence := make([]string, 0, MaxRedirects+1)
	redirects := 0

	for {
		if err := ctx.Err(); err != nil {
			return ProbeResult{}, fmt.Errorf("probe bottle: %w", err)
		}
		key := current.String()
		if _, exists := visited[key]; exists {
			return ProbeResult{}, errors.New("bottle probe redirect loop detected")
		}
		visited[key] = struct{}{}

		host, err := validateEndpointURL(current)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("validate bottle probe redirect target: %w", err)
		}
		if _, err := fetcher.resolver.lookup(ctx, host); err != nil {
			return ProbeResult{}, err
		}
		hostSequence = append(hostSequence, host)

		request, err := http.NewRequestWithContext(ctx, http.MethodHead, current.String(), nil)
		if err != nil {
			return ProbeResult{}, errors.New("construct bottle probe request")
		}
		request.Header.Set("Accept", "application/octet-stream")
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("User-Agent", "dalec-homebrew-bottle-fetcher/1")

		response, err := fetcher.client.Do(request)
		if err != nil {
			return ProbeResult{}, redactedNetworkError{operation: "probe", host: host, err: err}
		}
		if response == nil {
			return ProbeResult{}, fmt.Errorf("probe bottle host %q: empty HTTP response", host)
		}
		if response.Body == nil {
			return ProbeResult{}, fmt.Errorf("probe bottle host %q: empty HTTP response body", host)
		}
		closeErr := response.Body.Close()
		if closeErr != nil {
			return ProbeResult{}, redactedNetworkError{operation: "close probe response", host: host, err: closeErr}
		}

		if isRedirectStatus(response.StatusCode) {
			if redirects >= MaxRedirects {
				return ProbeResult{}, fmt.Errorf("bottle probe redirect limit of %d exceeded", MaxRedirects)
			}
			next, err := resolveProbeRedirect(current, response.Header.Get("Location"))
			if err != nil {
				return ProbeResult{}, err
			}
			current = next
			redirects++
			continue
		}

		if response.StatusCode != http.StatusOK {
			return ProbeResult{}, &ProbeStatusError{Host: host, Status: response.StatusCode}
		}
		if !hasIdentityContentEncoding(response.Header) {
			return ProbeResult{}, fmt.Errorf("bottle probe host %q returned unsupported Content-Encoding", host)
		}
		size, err := exactProbeContentLength(response)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("bottle probe host %q: %w", host, err)
		}
		return ProbeResult{
			FinalURL:             current.String(),
			Size:                 size,
			RedirectHostSequence: hostSequence,
		}, nil
	}
}

func resolveProbeRedirect(current *url.URL, location string) (*url.URL, error) {
	if location == "" {
		return nil, errors.New("bottle probe redirect is missing a Location header")
	}
	if len(location) > MaxURLBytes || strings.ContainsAny(location, "#\x00\r\n") {
		return nil, errors.New("bottle probe redirect Location is invalid")
	}
	reference, err := url.Parse(location)
	if err != nil {
		return nil, errors.New("bottle probe redirect Location is invalid")
	}
	next := current.ResolveReference(reference)
	if len(next.String()) > MaxURLBytes {
		return nil, fmt.Errorf("bottle probe redirect URL exceeds %d bytes", MaxURLBytes)
	}
	if _, err := validateEndpointURL(next); err != nil {
		return nil, fmt.Errorf("validate bottle probe redirect target: %w", err)
	}
	return next, nil
}

func exactProbeContentLength(response *http.Response) (int64, error) {
	if len(response.TransferEncoding) != 0 {
		return 0, errors.New("response does not have an exact Content-Length")
	}
	values := response.Header.Values("Content-Length")
	if len(values) == 0 {
		return 0, errors.New("response is missing Content-Length")
	}
	if len(values) != 1 {
		return 0, errors.New("response must contain exactly one Content-Length")
	}
	raw := strings.TrimSpace(values[0])
	if !allASCIIDigits(raw) {
		return 0, errors.New("response has an invalid Content-Length")
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New("response has an invalid Content-Length")
	}
	if size <= 0 || size > MaxBottleSize {
		return 0, fmt.Errorf("Content-Length %d is outside 1..%d", size, MaxBottleSize)
	}
	if response.ContentLength != size {
		return 0, fmt.Errorf("parsed Content-Length %d does not match response length %d", size, response.ContentLength)
	}
	return size, nil
}
