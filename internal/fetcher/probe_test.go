package fetcher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type unreadProbeBody struct {
	mu     sync.Mutex
	reads  int
	closes int
}

func (body *unreadProbeBody) Read([]byte) (int, error) {
	body.mu.Lock()
	defer body.mu.Unlock()
	body.reads++
	return 0, errors.New("probe must not read response bodies")
}

func (body *unreadProbeBody) Close() error {
	body.mu.Lock()
	defer body.mu.Unlock()
	body.closes++
	return nil
}

func (body *unreadProbeBody) counts() (int, int) {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.reads, body.closes
}

func probeResponse(status int, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        make(http.Header),
		Body:          body,
		ContentLength: -1,
	}
}

func probeRedirectResponse(location string, body io.ReadCloser) *http.Response {
	response := probeResponse(http.StatusFound, body)
	response.Header.Set("Location", location)
	return response
}

func probeSizedResponse(rawLength string, parsedLength int64, body io.ReadCloser) *http.Response {
	response := probeResponse(http.StatusOK, body)
	if rawLength != "" {
		response.Header["Content-Length"] = []string{rawLength}
	}
	response.ContentLength = parsedLength
	return response
}

func TestProbeDiscoversRedirectsWithoutDownloadingBodies(t *testing.T) {
	redirectBody := &unreadProbeBody{}
	finalBody := &unreadProbeBody{}
	finalURL := "https://cdn.example.com/releases/widget.tar.gz?signature=opaque"
	transport := &scriptedTransport{responses: []*http.Response{
		probeRedirectResponse(finalURL, redirectBody),
		probeSizedResponse("123", 123, finalBody),
	}}
	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"origin.example.com": {testPublicAddress},
		"cdn.example.com":    {netip.MustParseAddr("8.8.8.8")},
	}}
	fetcher := newTestFetcher(t, resolver, transport, Timeouts{})

	result, err := fetcher.Probe(context.Background(), "https://origin.example.com/bottles/widget.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalURL != finalURL || result.Size != 123 {
		t.Fatalf("result = %+v", result)
	}
	wantHosts := []string{"origin.example.com", "cdn.example.com"}
	if !reflect.DeepEqual(result.RedirectHostSequence, wantHosts) {
		t.Fatalf("host sequence = %v, want %v", result.RedirectHostSequence, wantHosts)
	}
	if got := resolver.callList(); !reflect.DeepEqual(got, wantHosts) {
		t.Fatalf("DNS calls = %v, want %v", got, wantHosts)
	}
	for i, request := range transport.requestList() {
		if request.Method != http.MethodHead {
			t.Fatalf("request %d method = %s", i, request.Method)
		}
		if request.Body != nil {
			t.Fatalf("request %d unexpectedly has a body", i)
		}
		if request.Header.Get("Accept") != "application/octet-stream" || request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("request %d headers = %v", i, request.Header)
		}
	}
	for name, body := range map[string]*unreadProbeBody{"redirect": redirectBody, "final": finalBody} {
		reads, closes := body.counts()
		if reads != 0 || closes != 1 {
			t.Fatalf("%s body reads=%d closes=%d", name, reads, closes)
		}
	}
}

func TestProbeRejectsUnsafeInitialURLBeforeNetwork(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "HTTP", url: "http://bottles.example.com/widget.tar.gz", want: "HTTPS"},
		{name: "nonstandard port", url: "https://bottles.example.com:8443/widget.tar.gz", want: "port 443"},
		{name: "IP literal", url: "https://8.8.8.8/widget.tar.gz", want: "IP-literal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &staticResolver{addresses: map[string][]netip.Addr{}}
			transport := &scriptedTransport{}
			fetcher := newTestFetcher(t, resolver, transport, Timeouts{})
			if _, err := fetcher.Probe(context.Background(), test.url); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
			if calls := resolver.callList(); len(calls) != 0 {
				t.Fatalf("DNS calls = %v", calls)
			}
			if requests := transport.requestList(); len(requests) != 0 {
				t.Fatalf("HTTP requests = %d", len(requests))
			}
		})
	}
}

func TestProbeRejectsPrivateRedirectAddressBeforeSecondRequest(t *testing.T) {
	redirectBody := &unreadProbeBody{}
	transport := &scriptedTransport{responses: []*http.Response{
		probeRedirectResponse("https://private.example.com/widget.tar.gz", redirectBody),
	}}
	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"origin.example.com":  {testPublicAddress},
		"private.example.com": {netip.MustParseAddr("10.0.0.1")},
	}}
	fetcher := newTestFetcher(t, resolver, transport, Timeouts{})

	_, err := fetcher.Probe(context.Background(), "https://origin.example.com/widget.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("error = %v", err)
	}
	if got := len(transport.requestList()); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
	if reads, closes := redirectBody.counts(); reads != 0 || closes != 1 {
		t.Fatalf("redirect body reads=%d closes=%d", reads, closes)
	}
}

func TestProbeRejectsHTTPSDowngradeBeforeSecondRequest(t *testing.T) {
	redirectBody := &unreadProbeBody{}
	transport := &scriptedTransport{responses: []*http.Response{
		probeRedirectResponse("http://cdn.example.com/widget.tar.gz", redirectBody),
	}}
	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"origin.example.com": {testPublicAddress},
	}}
	fetcher := newTestFetcher(t, resolver, transport, Timeouts{})

	_, err := fetcher.Probe(context.Background(), "https://origin.example.com/widget.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("error = %v", err)
	}
	if got := len(transport.requestList()); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
	if calls := resolver.callList(); !reflect.DeepEqual(calls, []string{"origin.example.com"}) {
		t.Fatalf("DNS calls = %v", calls)
	}
}

func TestProbeRequiresPositiveExactContentLength(t *testing.T) {
	tests := []struct {
		name             string
		headerValues     []string
		responseLength   int64
		transferEncoding []string
		want             string
		wantSize         int64
	}{
		{name: "missing", responseLength: -1, want: "missing Content-Length"},
		{name: "invalid", headerValues: []string{"not-a-number"}, responseLength: -1, want: "invalid Content-Length"},
		{name: "negative", headerValues: []string{"-1"}, responseLength: -1, want: "invalid Content-Length"},
		{name: "zero", headerValues: []string{"0"}, responseLength: 0, want: "outside"},
		{name: "oversized", headerValues: []string{strconv.FormatInt(MaxBottleSize+1, 10)}, responseLength: MaxBottleSize + 1, want: "outside"},
		{name: "duplicate", headerValues: []string{"10", "10"}, responseLength: 10, want: "exactly one"},
		{name: "parsed mismatch", headerValues: []string{"10"}, responseLength: 11, want: "does not match"},
		{name: "transfer encoded", headerValues: []string{"10"}, responseLength: 10, transferEncoding: []string{"chunked"}, want: "exact Content-Length"},
		{name: "maximum", headerValues: []string{strconv.FormatInt(MaxBottleSize, 10)}, responseLength: MaxBottleSize, wantSize: MaxBottleSize},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &unreadProbeBody{}
			response := probeResponse(http.StatusOK, body)
			if test.headerValues != nil {
				response.Header["Content-Length"] = append([]string(nil), test.headerValues...)
			}
			response.ContentLength = test.responseLength
			response.TransferEncoding = append([]string(nil), test.transferEncoding...)
			transport := &scriptedTransport{responses: []*http.Response{response}}
			resolver := &staticResolver{addresses: map[string][]netip.Addr{
				"origin.example.com": {testPublicAddress},
			}}
			fetcher := newTestFetcher(t, resolver, transport, Timeouts{})

			result, err := fetcher.Probe(context.Background(), "https://origin.example.com/widget.tar.gz")
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("error = %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if result.Size != test.wantSize {
					t.Fatalf("size = %d, want %d", result.Size, test.wantSize)
				}
			}
			if reads, closes := body.counts(); reads != 0 || closes != 1 {
				t.Fatalf("body reads=%d closes=%d", reads, closes)
			}
		})
	}
}

func TestProbeRedirectLoopAndLimit(t *testing.T) {
	t.Run("loop", func(t *testing.T) {
		transport := &scriptedTransport{responses: []*http.Response{
			probeRedirectResponse("/second", &unreadProbeBody{}),
			probeRedirectResponse("/first", &unreadProbeBody{}),
		}}
		resolver := &staticResolver{addresses: map[string][]netip.Addr{
			"origin.example.com": {testPublicAddress},
		}}
		fetcher := newTestFetcher(t, resolver, transport, Timeouts{})

		_, err := fetcher.Probe(context.Background(), "https://origin.example.com/first")
		if err == nil || !strings.Contains(err.Error(), "redirect loop") {
			t.Fatalf("error = %v", err)
		}
		if got := len(transport.requestList()); got != 2 {
			t.Fatalf("requests = %d, want 2", got)
		}
	})

	t.Run("five redirects", func(t *testing.T) {
		responses := make([]*http.Response, 0, MaxRedirects+1)
		for i := 1; i <= MaxRedirects; i++ {
			responses = append(responses, probeRedirectResponse("/hop-"+strconv.Itoa(i), &unreadProbeBody{}))
		}
		responses = append(responses, probeSizedResponse("1", 1, &unreadProbeBody{}))
		transport := &scriptedTransport{responses: responses}
		resolver := &staticResolver{addresses: map[string][]netip.Addr{
			"origin.example.com": {testPublicAddress},
		}}
		fetcher := newTestFetcher(t, resolver, transport, Timeouts{})

		result, err := fetcher.Probe(context.Background(), "https://origin.example.com/start")
		if err != nil {
			t.Fatal(err)
		}
		if result.FinalURL != "https://origin.example.com/hop-5" || len(result.RedirectHostSequence) != MaxRedirects+1 {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("six redirects", func(t *testing.T) {
		responses := make([]*http.Response, 0, MaxRedirects+1)
		for i := 1; i <= MaxRedirects+1; i++ {
			responses = append(responses, probeRedirectResponse("/hop-"+strconv.Itoa(i), &unreadProbeBody{}))
		}
		transport := &scriptedTransport{responses: responses}
		resolver := &staticResolver{addresses: map[string][]netip.Addr{
			"origin.example.com": {testPublicAddress},
		}}
		fetcher := newTestFetcher(t, resolver, transport, Timeouts{})

		_, err := fetcher.Probe(context.Background(), "https://origin.example.com/start")
		if err == nil || !strings.Contains(err.Error(), "redirect limit") {
			t.Fatalf("error = %v", err)
		}
		if got := len(transport.requestList()); got != MaxRedirects+1 {
			t.Fatalf("requests = %d, want %d", got, MaxRedirects+1)
		}
	})
}

func TestProbeHonorsOverallDeadline(t *testing.T) {
	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"origin.example.com": {testPublicAddress},
	}}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	fetcher := newTestFetcher(t, resolver, transport, Timeouts{Overall: 10 * time.Millisecond})

	_, err := fetcher.Probe(context.Background(), "https://origin.example.com/widget.tar.gz")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestProbeOptionalTreatsOnlyDefinitiveAbsenceAsMissing(t *testing.T) {
	resolver := &staticResolver{addresses: map[string][]netip.Addr{"origin.example.com": {testPublicAddress}}}
	missing := response(http.StatusNotFound, "")
	missing.ContentLength = 0
	transport := &scriptedTransport{responses: []*http.Response{missing}}
	fetcher := newTestFetcher(t, resolver, transport, Timeouts{})
	if _, found, err := fetcher.ProbeOptional(t.Context(), "https://origin.example.com/bundle.sigstore.json"); err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}

	failure := response(http.StatusServiceUnavailable, "")
	failure.ContentLength = 0
	transport = &scriptedTransport{responses: []*http.Response{failure}}
	fetcher = newTestFetcher(t, resolver, transport, Timeouts{})
	if _, _, err := fetcher.ProbeOptional(t.Context(), "https://origin.example.com/bundle.sigstore.json"); err == nil {
		t.Fatal("service outage was treated as absent provenance")
	}
}
