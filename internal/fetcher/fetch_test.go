package fetcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var testPublicAddress = netip.MustParseAddr("93.184.216.34")

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (fn resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return fn(ctx, network, host)
}

type staticResolver struct {
	mu        sync.Mutex
	addresses map[string][]netip.Addr
	calls     []string
}

func (resolver *staticResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if network != "ip" {
		return nil, errors.New("unexpected DNS network")
	}
	resolver.calls = append(resolver.calls, host)
	addresses, ok := resolver.addresses[host]
	if !ok {
		return nil, errors.New("unconfigured DNS host")
	}
	return append([]netip.Addr(nil), addresses...), nil
}

func (resolver *staticResolver) callList() []string {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return append([]string(nil), resolver.calls...)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type scriptedTransport struct {
	mu        sync.Mutex
	responses []*http.Response
	requests  []*http.Request
}

func (transport *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.requests = append(transport.requests, cloneHTTPRequest(request))
	if len(transport.responses) == 0 {
		return nil, errors.New("unexpected HTTP request")
	}
	response := transport.responses[0]
	transport.responses = transport.responses[1:]
	response.Request = request
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	return response, nil
}

func (transport *scriptedTransport) requestList() []*http.Request {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]*http.Request(nil), transport.requests...)
}

func cloneHTTPRequest(request *http.Request) *http.Request {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	if request.URL != nil {
		copyURL := *request.URL
		clone.URL = &copyURL
	}
	return clone
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: -1,
	}
}

func redirectResponse(location string) *http.Response {
	result := response(http.StatusFound, "redirect body is ignored")
	result.Header.Set("Location", location)
	return result
}

func validRequest(body string) Request {
	digest := sha256.Sum256([]byte(body))
	return Request{
		SchemaVersion:        RequestSchemaVersion,
		FetchPolicyVersion:   FetchPolicyVersion,
		ArtifactID:           "acme/tools/widget:linux-amd64",
		URL:                  "https://origin.example.com/bottles/widget.tar.gz",
		ExpectedSize:         int64(len(body)),
		SHA256:               hex.EncodeToString(digest[:]),
		Filename:             "widget--1.2.3.x86_64_linux.bottle.tar.gz",
		AllowedRedirectHosts: []string{"cdn.example.com"},
	}
}

func newTestFetcher(t *testing.T, resolver Resolver, transport http.RoundTripper, timeouts Timeouts) *Fetcher {
	t.Helper()
	result, err := New(Config{Resolver: resolver, Transport: transport, Timeouts: timeouts})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFetchPolicyIdentifierIsFrozen(t *testing.T) {
	if FetchPolicyVersion != "homebrew-bottle-fetch-v1" {
		t.Fatalf("fetch policy identifier = %q", FetchPolicyVersion)
	}
}

func TestValidateRequestRejectsUnsafeInputs(t *testing.T) {
	base := validRequest("bottle")
	tooManyHosts := make([]string, MaxRedirectHostCount+1)
	for i := range tooManyHosts {
		tooManyHosts[i] = "cdn" + string(rune('a'+i%26)) + ".example.com"
	}
	tests := []struct {
		name   string
		mutate func(*Request)
		want   string
	}{
		{name: "schema", mutate: func(r *Request) { r.SchemaVersion = "v2" }, want: "schema"},
		{name: "policy", mutate: func(r *Request) { r.FetchPolicyVersion = "unbounded" }, want: "policy"},
		{name: "empty artifact", mutate: func(r *Request) { r.ArtifactID = "" }, want: "artifact ID"},
		{name: "artifact whitespace", mutate: func(r *Request) { r.ArtifactID = "bad id" }, want: "artifact ID"},
		{name: "filename traversal", mutate: func(r *Request) { r.Filename = "../bottle" }, want: "single path component"},
		{name: "filename unicode", mutate: func(r *Request) { r.Filename = "bøttle" }, want: "visible ASCII"},
		{name: "zero size", mutate: func(r *Request) { r.ExpectedSize = 0 }, want: "expected size"},
		{name: "negative size", mutate: func(r *Request) { r.ExpectedSize = -1 }, want: "expected size"},
		{name: "oversized", mutate: func(r *Request) { r.ExpectedSize = MaxBottleSize + 1 }, want: "expected size"},
		{name: "digest prefix", mutate: func(r *Request) { r.SHA256 = "sha256:" + r.SHA256 }, want: "SHA-256"},
		{name: "uppercase digest", mutate: func(r *Request) { r.SHA256 = strings.ToUpper(r.SHA256) }, want: "SHA-256"},
		{name: "nonhex digest", mutate: func(r *Request) { r.SHA256 = strings.Repeat("z", 64) }, want: "SHA-256"},
		{name: "http", mutate: func(r *Request) { r.URL = "http://origin.example.com/bottle" }, want: "HTTPS"},
		{name: "wrong port", mutate: func(r *Request) { r.URL = "https://origin.example.com:8443/bottle" }, want: "port 443"},
		{name: "empty port", mutate: func(r *Request) { r.URL = "https://origin.example.com:/bottle" }, want: "empty port"},
		{name: "userinfo", mutate: func(r *Request) { r.URL = "https://" + "user" + ":" + "pass" + "@origin.example.com/bottle" }, want: "userinfo"},
		{name: "fragment", mutate: func(r *Request) { r.URL = "https://origin.example.com/bottle#marker" }, want: "fragments"},
		{name: "empty fragment", mutate: func(r *Request) { r.URL = "https://origin.example.com/bottle#" }, want: "fragments"},
		{name: "IPv4 literal", mutate: func(r *Request) { r.URL = "https://8.8.8.8/bottle" }, want: "IP-literal"},
		{name: "IPv6 literal", mutate: func(r *Request) { r.URL = "https://[2606:4700:4700::1111]/bottle" }, want: "IP-literal"},
		{name: "single label", mutate: func(r *Request) { r.URL = "https://localhost/bottle" }, want: "fully qualified"},
		{name: "numeric hostname", mutate: func(r *Request) { r.URL = "https://127.1/bottle" }, want: "all-numeric"},
		{name: "allowlist URL", mutate: func(r *Request) { r.AllowedRedirectHosts = []string{"https://cdn.example.com"} }, want: "redirect hosts"},
		{name: "allowlist port", mutate: func(r *Request) { r.AllowedRedirectHosts = []string{"cdn.example.com:443"} }, want: "redirect hosts"},
		{name: "allowlist IP", mutate: func(r *Request) { r.AllowedRedirectHosts = []string{"1.1.1.1"} }, want: "IP-literal"},
		{name: "allowlist duplicate", mutate: func(r *Request) { r.AllowedRedirectHosts = []string{"cdn.example.com", "CDN.EXAMPLE.COM"} }, want: "duplicate"},
		{name: "allowlist count", mutate: func(r *Request) { r.AllowedRedirectHosts = tooManyHosts }, want: "exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.AllowedRedirectHosts = append([]string(nil), base.AllowedRedirectHosts...)
			test.mutate(&request)
			if _, err := validateRequest(request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateRequestAllowsHTTPS443AndQueries(t *testing.T) {
	request := validRequest("body")
	request.URL = "https://ORIGIN.EXAMPLE.COM:443/bottle?opaque=value"
	request.AllowedRedirectHosts = []string{"CDN.EXAMPLE.COM"}
	validated, err := validateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if validated.url.RawQuery != "opaque=value" {
		t.Fatalf("query = %q", validated.url.RawQuery)
	}
	if _, ok := validated.allowedRedirectHosts["cdn.example.com"]; !ok {
		t.Fatalf("allowlist = %v", validated.allowedRedirectHosts)
	}
}

func TestFetchRedirectSuccessEmitsDeterministicRedactedEvidence(t *testing.T) {
	body := "verified bottle bytes"
	request := validRequest(body)
	request.URL = "https://origin.example.com/files/bottle.tar.gz?marker=alpha-value"
	transport := &scriptedTransport{responses: []*http.Response{
		redirectResponse("https://cdn.example.com/download/object?marker=beta-value"),
		response(http.StatusOK, body),
	}}
	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"origin.example.com": {testPublicAddress},
		"cdn.example.com":    {netip.MustParseAddr("2606:4700:4700::1111")},
	}}
	client := newTestFetcher(t, resolver, transport, Timeouts{})
	var output bytes.Buffer
	evidence, err := client.Fetch(context.Background(), request, &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != body {
		t.Fatalf("output = %q", output.String())
	}
	wantHosts := []string{"origin.example.com", "cdn.example.com"}
	if !reflect.DeepEqual(evidence.RedactedHostSequence, wantHosts) {
		t.Fatalf("hosts = %v, want %v", evidence.RedactedHostSequence, wantHosts)
	}
	if evidence.FetchPolicyVersion != "homebrew-bottle-fetch-v1" {
		t.Fatalf("evidence policy = %q", evidence.FetchPolicyVersion)
	}
	if evidence.Size != int64(len(body)) || evidence.SHA256 != request.SHA256 || evidence.ArtifactID != request.ArtifactID {
		t.Fatalf("evidence = %+v", evidence)
	}
	first, err := MarshalEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("evidence is not deterministic:\n%s\n%s", first, second)
	}
	for _, marker := range []string{"alpha-value", "beta-value", "/files/", "/download/"} {
		if bytes.Contains(first, []byte(marker)) {
			t.Fatalf("evidence leaks %q: %s", marker, first)
		}
	}
	if got := resolver.callList(); !reflect.DeepEqual(got, wantHosts) {
		t.Fatalf("DNS calls = %v, want %v", got, wantHosts)
	}
	requests := transport.requestList()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	for _, got := range requests {
		if got.Method != http.MethodGet || got.Header.Get("Accept-Encoding") != "identity" || got.Header.Get("Accept") != "application/octet-stream" {
			t.Fatalf("request = %+v headers=%v", got, got.Header)
		}
		for _, forbidden := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
			if got.Header.Get(forbidden) != "" {
				t.Fatalf("request carried %s", forbidden)
			}
		}
		if got.URL.User != nil {
			t.Fatal("request carried URL credentials")
		}
	}
}

func TestFetchRedirectPolicy(t *testing.T) {
	body := "bottle"
	tests := []struct {
		name      string
		request   func() Request
		responses []*http.Response
		want      string
		requests  int
	}{
		{
			name: "unapproved host",
			request: func() Request {
				r := validRequest(body)
				r.AllowedRedirectHosts = nil
				return r
			},
			responses: []*http.Response{redirectResponse("https://cdn.example.com/bottle")},
			want:      "not in the signed allowlist",
			requests:  1,
		},
		{
			name:      "HTTPS downgrade",
			request:   func() Request { return validRequest(body) },
			responses: []*http.Response{redirectResponse("http://cdn.example.com/bottle")},
			want:      "must use HTTPS",
			requests:  1,
		},
		{
			name:      "IP literal",
			request:   func() Request { return validRequest(body) },
			responses: []*http.Response{redirectResponse("https://1.1.1.1/bottle")},
			want:      "IP-literal",
			requests:  1,
		},
		{
			name:      "userinfo",
			request:   func() Request { return validRequest(body) },
			responses: []*http.Response{redirectResponse("https://" + "left" + ":" + "right" + "@cdn.example.com/bottle")},
			want:      "userinfo",
			requests:  1,
		},
		{
			name:      "fragment",
			request:   func() Request { return validRequest(body) },
			responses: []*http.Response{redirectResponse("https://cdn.example.com/bottle#fragment")},
			want:      "Location is invalid",
			requests:  1,
		},
		{
			name:      "missing location",
			request:   func() Request { return validRequest(body) },
			responses: []*http.Response{response(http.StatusFound, "")},
			want:      "missing a Location",
			requests:  1,
		},
		{
			name: "same host not allowlisted",
			request: func() Request {
				r := validRequest(body)
				r.AllowedRedirectHosts = nil
				return r
			},
			responses: []*http.Response{redirectResponse("/next")},
			want:      "not in the signed allowlist",
			requests:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &scriptedTransport{responses: test.responses}
			resolver := &staticResolver{addresses: map[string][]netip.Addr{
				"origin.example.com": {testPublicAddress},
				"cdn.example.com":    {testPublicAddress},
			}}
			client := newTestFetcher(t, resolver, transport, Timeouts{})
			_, err := client.Fetch(context.Background(), test.request(), io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
			if got := len(transport.requestList()); got != test.requests {
				t.Fatalf("requests = %d, want %d", got, test.requests)
			}
		})
	}
}

func TestFetchRedirectLimitAndLoop(t *testing.T) {
	body := "bottle"
	t.Run("maximum five redirects", func(t *testing.T) {
		request := validRequest(body)
		request.AllowedRedirectHosts = []string{"origin.example.com"}
		responses := make([]*http.Response, MaxRedirects+1)
		for i := range responses {
			responses[i] = redirectResponse("/redirect-" + string(rune('a'+i)))
		}
		transport := &scriptedTransport{responses: responses}
		resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
		client := newTestFetcher(t, resolver, transport, Timeouts{})
		_, err := client.Fetch(context.Background(), request, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "limit of 5 exceeded") {
			t.Fatalf("error = %v", err)
		}
		if got := len(transport.requestList()); got != MaxRedirects+1 {
			t.Fatalf("requests = %d, want %d", got, MaxRedirects+1)
		}
	})

	t.Run("loop", func(t *testing.T) {
		request := validRequest(body)
		request.AllowedRedirectHosts = []string{"origin.example.com"}
		transport := &scriptedTransport{responses: []*http.Response{redirectResponse(request.URL)}}
		resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
		client := newTestFetcher(t, resolver, transport, Timeouts{})
		_, err := client.Fetch(context.Background(), request, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "redirect loop") {
			t.Fatalf("error = %v", err)
		}
		if got := len(transport.requestList()); got != 1 {
			t.Fatalf("requests = %d, want 1", got)
		}
	})
}

func TestFetchRejectsNonPublicDNSBeforeHTTP(t *testing.T) {
	body := "bottle"
	tests := []struct {
		name      string
		addresses []netip.Addr
	}{
		{name: "private", addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}},
		{name: "mixed public private", addresses: []netip.Addr{testPublicAddress, netip.MustParseAddr("127.0.0.1")}},
		{name: "link local IPv6", addresses: []netip.Addr{netip.MustParseAddr("fe80::1")}},
		{name: "documentation", addresses: []netip.Addr{netip.MustParseAddr("203.0.113.1")}},
		{name: "none", addresses: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return response(http.StatusOK, body), nil
			})
			resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": test.addresses}}
			client := newTestFetcher(t, resolver, transport, Timeouts{})
			_, err := client.Fetch(context.Background(), validRequest(body), io.Discard)
			if err == nil || !strings.Contains(err.Error(), "resolve bottle host") {
				t.Fatalf("error = %v", err)
			}
			if calls != 0 {
				t.Fatalf("HTTP calls = %d", calls)
			}
		})
	}
}

func TestFetchEnforcesSizeDigestAndEncoding(t *testing.T) {
	body := "abcd"
	tests := []struct {
		name     string
		request  func() Request
		response func() *http.Response
		want     string
		written  int
	}{
		{
			name:    "Content-Length mismatch",
			request: func() Request { return validRequest(body) },
			response: func() *http.Response {
				r := response(http.StatusOK, body)
				r.ContentLength = int64(len(body) + 1)
				return r
			},
			want:    "Content-Length",
			written: 0,
		},
		{
			name: "lying Content-Length",
			request: func() Request {
				r := validRequest("abc")
				return r
			},
			response: func() *http.Response {
				r := response(http.StatusOK, "abcd")
				r.ContentLength = 3
				return r
			},
			want:    "exceeds expected size",
			written: 4,
		},
		{
			name: "chunked oversized",
			request: func() Request {
				r := validRequest("abc")
				return r
			},
			response: func() *http.Response { return response(http.StatusOK, "abcdef") },
			want:     "exceeds expected size",
			written:  4,
		},
		{
			name:     "short body",
			request:  func() Request { return validRequest(body) },
			response: func() *http.Response { return response(http.StatusOK, "abc") },
			want:     "body size 3",
			written:  3,
		},
		{
			name: "digest mismatch",
			request: func() Request {
				r := validRequest(body)
				r.SHA256 = strings.Repeat("0", 64)
				return r
			},
			response: func() *http.Response { return response(http.StatusOK, body) },
			want:     "SHA-256",
			written:  len(body),
		},
		{
			name:    "content encoding",
			request: func() Request { return validRequest(body) },
			response: func() *http.Response {
				r := response(http.StatusOK, body)
				r.Header.Set("Content-Encoding", "gzip")
				return r
			},
			want:    "Content-Encoding",
			written: 0,
		},
		{
			name:    "multiple content encodings",
			request: func() Request { return validRequest(body) },
			response: func() *http.Response {
				r := response(http.StatusOK, body)
				r.Header["Content-Encoding"] = []string{"identity", "gzip"}
				return r
			},
			want:    "Content-Encoding",
			written: 0,
		},
		{
			name:     "HTTP status",
			request:  func() Request { return validRequest(body) },
			response: func() *http.Response { return response(http.StatusForbidden, "untrusted response") },
			want:     "status 403",
			written:  0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &scriptedTransport{responses: []*http.Response{test.response()}}
			resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
			client := newTestFetcher(t, resolver, transport, Timeouts{})
			var output bytes.Buffer
			_, err := client.Fetch(context.Background(), test.request(), &output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
			if output.Len() != test.written {
				t.Fatalf("written = %d, want %d", output.Len(), test.written)
			}
		})
	}
}

func TestFetchRedactsNetworkErrorsAndPreservesCause(t *testing.T) {
	body := "body"
	request := validRequest(body)
	request.URL += "?marker=not-for-errors"
	sentinel := errors.New("dial failed")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: sentinel}
	})
	resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
	client := newTestFetcher(t, resolver, transport, Timeouts{})
	_, err := client.Fetch(context.Background(), request, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "not-for-errors") || strings.Contains(err.Error(), request.URL) {
		t.Fatalf("error leaks URL: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not preserve cause: %v", err)
	}
}

func TestFetchTimeouts(t *testing.T) {
	body := "body"
	t.Run("DNS", func(t *testing.T) {
		resolver := resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		client := newTestFetcher(t, resolver, roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("HTTP should not be reached")
			return nil, nil
		}), Timeouts{DNS: 20 * time.Millisecond, Overall: time.Second})
		start := time.Now()
		_, err := client.Fetch(context.Background(), validRequest(body), io.Discard)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("DNS timeout took %s", elapsed)
		}
	})

	t.Run("overall", func(t *testing.T) {
		resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		client := newTestFetcher(t, resolver, transport, Timeouts{DNS: 10 * time.Millisecond, Overall: 25 * time.Millisecond})
		start := time.Now()
		_, err := client.Fetch(context.Background(), validRequest(body), io.Discard)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("overall timeout took %s", elapsed)
		}
	})
}

func TestFetchToFilesPublishesReadOnlyVerifiedOutputs(t *testing.T) {
	body := "verified bottle"
	request := validRequest(body)
	transport := &scriptedTransport{responses: []*http.Response{response(http.StatusOK, body)}}
	resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
	client := newTestFetcher(t, resolver, transport, Timeouts{})
	directory := t.TempDir()
	bottlePath := filepath.Join(directory, request.Filename)
	evidencePath := filepath.Join(directory, "fetch-evidence.json")
	evidence, err := client.FetchToFiles(context.Background(), request, bottlePath, evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(bottlePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("bottle = %q", data)
	}
	for _, path := range []string{bottlePath, evidencePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o444 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	evidenceData, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(evidenceData, []byte("\n")) {
		t.Fatalf("evidence lacks final newline: %q", evidenceData)
	}
	var decoded Evidence
	if err := json.Unmarshal(evidenceData, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, evidence) {
		t.Fatalf("decoded evidence = %+v, want %+v", decoded, evidence)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("directory entries = %v", entries)
	}
}

func TestFetchToFilesRemovesPartialNetworkOutput(t *testing.T) {
	body := "body"
	request := validRequest(body)
	readFailure := errors.New("connection reset")
	transport := &scriptedTransport{responses: []*http.Response{{
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		Body:          &failingReadCloser{reader: strings.NewReader("bo"), err: readFailure},
		ContentLength: -1,
	}}}
	resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
	client := newTestFetcher(t, resolver, transport, Timeouts{})
	directory := t.TempDir()
	bottlePath := filepath.Join(directory, request.Filename)
	evidencePath := filepath.Join(directory, "fetch-evidence.json")
	_, err := client.FetchToFiles(context.Background(), request, bottlePath, evidencePath)
	if err == nil || !errors.Is(err, readFailure) {
		t.Fatalf("error = %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial outputs remain: %v", entries)
	}
}

type failingReadCloser struct {
	reader io.Reader
	err    error
}

func (reader *failingReadCloser) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	if err == io.EOF {
		return 0, reader.err
	}
	return n, err
}

func (*failingReadCloser) Close() error { return nil }

func TestFetchToFilesLeavesNoPartialOutput(t *testing.T) {
	body := "body"
	request := validRequest(body)
	request.SHA256 = strings.Repeat("0", 64)
	transport := &scriptedTransport{responses: []*http.Response{response(http.StatusOK, body)}}
	resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
	client := newTestFetcher(t, resolver, transport, Timeouts{})
	directory := t.TempDir()
	bottlePath := filepath.Join(directory, request.Filename)
	evidencePath := filepath.Join(directory, "fetch-evidence.json")
	if _, err := client.FetchToFiles(context.Background(), request, bottlePath, evidencePath); err == nil {
		t.Fatal("expected digest error")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial outputs remain: %v", entries)
	}
}

func TestFetchToFilesRefusesExistingOutputBeforeNetwork(t *testing.T) {
	body := "body"
	request := validRequest(body)
	calls := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(http.StatusOK, body), nil
	})
	resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
	client := newTestFetcher(t, resolver, transport, Timeouts{})
	directory := t.TempDir()
	bottlePath := filepath.Join(directory, request.Filename)
	evidencePath := filepath.Join(directory, "fetch-evidence.json")
	if err := os.WriteFile(bottlePath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.FetchToFiles(context.Background(), request, bottlePath, evidencePath); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("HTTP calls = %d", calls)
	}
	data, err := os.ReadFile(bottlePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("existing output changed to %q", data)
	}
}

func TestDecodeRequestIsStrict(t *testing.T) {
	request := validRequest("body")
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRequest(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, request) {
		t.Fatalf("decoded = %+v, want %+v", decoded, request)
	}
	unknown := strings.TrimSuffix(string(data), "}") + `,"unexpected":true}`
	if _, err := DecodeRequest(strings.NewReader(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error = %v", err)
	}
	duplicate := strings.TrimSuffix(string(data), "}") + `,"url":"https://other.example.com/bottle"}`
	if _, err := DecodeRequest(strings.NewReader(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate JSON member") {
		t.Fatalf("duplicate-member error = %v", err)
	}
	if _, err := DecodeRequest(strings.NewReader(string(data) + `{}`)); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing-value error = %v", err)
	}
	if _, err := DecodeRequest(strings.NewReader(strings.Repeat(" ", MaxRequestBytes+1))); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized-request error = %v", err)
	}
	if _, err := DecodeRequest(bytes.NewReader([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid-UTF-8 error = %v", err)
	}
}

func TestNewDefaultTransportIsCredentiallessAndBounded(t *testing.T) {
	client, err := New(Config{Resolver: &staticResolver{addresses: map[string][]netip.Addr{}}})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.client.Transport)
	}
	if transport.Proxy != nil || !transport.DisableCompression || !transport.DisableKeepAlives || transport.ForceAttemptHTTP2 {
		t.Fatalf("unsafe transport settings: %+v", transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion == 0 {
		t.Fatal("TLS minimum version is not set")
	}
	if client.client.Jar != nil {
		t.Fatal("HTTP client has a cookie jar")
	}
	if client.client.Timeout != DefaultOverallTimeout {
		t.Fatalf("client timeout = %s", client.client.Timeout)
	}
}

func TestTimeoutConfigurationCannotExceedPolicyMaxima(t *testing.T) {
	tests := []Timeouts{
		{DNS: MaxDNSTimeout + time.Nanosecond},
		{Connect: MaxConnectTimeout + time.Nanosecond},
		{TLSHandshake: MaxTLSHandshakeTimeout + time.Nanosecond},
		{ResponseHeader: MaxResponseHeaderTimeout + time.Nanosecond},
		{Overall: MaxOverallTimeout + time.Nanosecond},
	}
	for _, timeouts := range tests {
		if _, err := New(Config{Timeouts: timeouts}); err == nil {
			t.Fatalf("timeouts %+v unexpectedly accepted", timeouts)
		}
	}
}

func TestPublicDialerRejectsDNSRebinding(t *testing.T) {
	sequence := [][]netip.Addr{
		{testPublicAddress},
		{netip.MustParseAddr("127.0.0.1")},
	}
	var mu sync.Mutex
	resolver := resolverFunc(func(_ context.Context, _, _ string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(sequence) == 0 {
			return nil, errors.New("too many lookups")
		}
		result := sequence[0]
		sequence = sequence[1:]
		return result, nil
	})
	underlying := &recordingDialer{}
	guard := publicResolver{resolver: resolver, timeout: time.Second}
	dialer := publicDialer{resolver: guard, dialer: underlying}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		_, err := dialer.DialContext(request.Context(), "tcp", net.JoinHostPort(request.URL.Hostname(), "443"))
		return nil, err
	})
	client := newTestFetcher(t, resolver, transport, Timeouts{})
	_, err := client.Fetch(context.Background(), validRequest("body"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "network operation failed") {
		t.Fatalf("error = %v", err)
	}
	if underlying.calls != 0 {
		t.Fatalf("underlying dial calls = %d", underlying.calls)
	}
}

type recordingDialer struct {
	calls int
}

func (dialer *recordingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	dialer.calls++
	return nil, errors.New("unexpected dial")
}

func TestFetchObservedComputesDigestWithoutPretrustedChecksum(t *testing.T) {
	body := "immutable source"
	resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
	transport := &scriptedTransport{responses: []*http.Response{response(http.StatusOK, body)}}
	fetcher := newTestFetcher(t, resolver, transport, Timeouts{})
	var output bytes.Buffer
	evidence, err := fetcher.FetchObserved(t.Context(), "https://origin.example.com/source.rb", int64(len(body)), "source.rb", []string{"origin.example.com"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	if output.String() != body || evidence.SHA256 != hex.EncodeToString(sum[:]) || evidence.Size != int64(len(body)) {
		t.Fatalf("output=%q evidence=%+v", output.String(), evidence)
	}
}
