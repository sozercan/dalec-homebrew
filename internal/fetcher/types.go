// Package fetcher downloads digest- and size-bound public HTTPS bottle
// artifacts without carrying credentials or following unverified redirects.
package fetcher

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// RequestSchemaVersion is the only request schema accepted by this
	// implementation.
	RequestSchemaVersion = "bottle-fetch-request/v1"
	// EvidenceSchemaVersion identifies the canonical fetch evidence emitted by
	// this implementation.
	EvidenceSchemaVersion = "bottle-fetch-evidence/v1"
	// FetchPolicyVersion is the frozen identifier for the exact fetch policy implemented here.
	FetchPolicyVersion = "homebrew-bottle-fetch-v1"

	MaxBottleSize        int64 = 1 << 30
	MaxRedirects               = 5
	MaxURLBytes                = 8 << 10
	MaxArtifactIDBytes         = 512
	MaxFilenameBytes           = 255
	MaxRedirectHostCount       = 32
	MaxRequestBytes            = 64 << 10

	DefaultDNSTimeout            = 10 * time.Second
	DefaultConnectTimeout        = 30 * time.Second
	DefaultTLSHandshakeTimeout   = 30 * time.Second
	DefaultResponseHeaderTimeout = 30 * time.Second
	DefaultOverallTimeout        = 15 * time.Minute

	MaxDNSTimeout            = 30 * time.Second
	MaxConnectTimeout        = time.Minute
	MaxTLSHandshakeTimeout   = time.Minute
	MaxResponseHeaderTimeout = time.Minute
	MaxOverallTimeout        = 15 * time.Minute
)

// Request is the immutable, signed-input portion of one bottle fetch. SHA256
// is a canonical lowercase hexadecimal digest without a "sha256:" prefix.
type Request struct {
	SchemaVersion        string   `json:"schema_version"`
	FetchPolicyVersion   string   `json:"fetch_policy_version"`
	ArtifactID           string   `json:"artifact_id"`
	URL                  string   `json:"url"`
	ExpectedSize         int64    `json:"expected_size"`
	SHA256               string   `json:"sha256"`
	Filename             string   `json:"filename"`
	AllowedRedirectHosts []string `json:"allowed_redirect_hosts"`
}

// Evidence contains no URL paths or query strings. RedactedHostSequence lists
// only canonical DNS hostnames in request order, starting with the original
// host and followed by every accepted redirect target.
type Evidence struct {
	SchemaVersion        string   `json:"schema_version"`
	FetchPolicyVersion   string   `json:"fetch_policy_version"`
	ArtifactID           string   `json:"artifact_id"`
	Filename             string   `json:"filename"`
	Size                 int64    `json:"size"`
	SHA256               string   `json:"sha256"`
	RedactedHostSequence []string `json:"redacted_host_sequence"`
}

// Resolver is the DNS capability used by the fetcher and its guarded dialer.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ContextDialer is the network dialing capability wrapped by the public-IP
// guard. It is configurable so rebinding behavior can be tested without a
// network.
type ContextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Timeouts bounds every network phase. Zero fields select package defaults.
type Timeouts struct {
	DNS            time.Duration
	Connect        time.Duration
	TLSHandshake   time.Duration
	ResponseHeader time.Duration
	Overall        time.Duration
}

// Config provides test seams for DNS, HTTP, and dialing. Production callers
// should normally use a zero Config. Transport, when supplied, is still wrapped
// in an http.Client with redirects disabled and no cookie jar.
type Config struct {
	Resolver  Resolver
	Transport http.RoundTripper
	Dialer    ContextDialer
	Timeouts  Timeouts
}

// DecodeRequest decodes exactly one strict JSON object. Unknown fields and
// trailing values are rejected before any network access.
func DecodeRequest(reader io.Reader) (Request, error) {
	if reader == nil {
		return Request{}, errors.New("missing fetch request reader")
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxRequestBytes+1))
	if err != nil {
		return Request{}, fmt.Errorf("read fetch request: %w", err)
	}
	if len(data) > MaxRequestBytes {
		return Request{}, fmt.Errorf("fetch request exceeds %d bytes", MaxRequestBytes)
	}
	if !utf8.Valid(data) {
		return Request{}, errors.New("fetch request is not valid UTF-8")
	}
	if err := rejectDuplicateJSONMembers(data); err != nil {
		return Request{}, fmt.Errorf("decode fetch request: %w", err)
	}
	if err := requireExactTopLevelMembers(data, map[string]struct{}{"schema_version": {}, "fetch_policy_version": {}, "artifact_id": {}, "url": {}, "expected_size": {}, "sha256": {}, "filename": {}, "allowed_redirect_hosts": {}}); err != nil {
		return Request{}, fmt.Errorf("decode fetch request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("decode fetch request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Request{}, errors.New("decode fetch request: trailing JSON value")
		}
		return Request{}, fmt.Errorf("decode fetch request trailing data: %w", err)
	}
	if _, err := validateRequest(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

// MarshalEvidence returns stable compact JSON. Evidence intentionally contains
// no timestamps, response ordering maps, URL paths, or query strings.
func MarshalEvidence(evidence Evidence) ([]byte, error) {
	return json.Marshal(evidence)
}

type validatedRequest struct {
	request              Request
	url                  *url.URL
	originHost           string
	allowedRedirectHosts map[string]struct{}
}

func validateRequest(request Request) (validatedRequest, error) {
	if request.SchemaVersion != RequestSchemaVersion {
		return validatedRequest{}, fmt.Errorf("unsupported fetch request schema %q", request.SchemaVersion)
	}
	if request.FetchPolicyVersion != FetchPolicyVersion {
		return validatedRequest{}, fmt.Errorf("unsupported fetch policy %q", request.FetchPolicyVersion)
	}
	if err := validateArtifactID(request.ArtifactID); err != nil {
		return validatedRequest{}, err
	}
	if err := validateFilename(request.Filename); err != nil {
		return validatedRequest{}, err
	}
	if request.ExpectedSize <= 0 || request.ExpectedSize > MaxBottleSize {
		return validatedRequest{}, fmt.Errorf("expected size must be between 1 and %d bytes", MaxBottleSize)
	}
	if len(request.SHA256) != 64 || request.SHA256 != strings.ToLower(request.SHA256) {
		return validatedRequest{}, errors.New("SHA-256 must be 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(request.SHA256); err != nil {
		return validatedRequest{}, errors.New("SHA-256 must be 64 lowercase hexadecimal characters")
	}
	parsed, originHost, err := parseBottleURL(request.URL)
	if err != nil {
		return validatedRequest{}, err
	}
	if len(request.AllowedRedirectHosts) > MaxRedirectHostCount {
		return validatedRequest{}, fmt.Errorf("redirect host allowlist exceeds %d entries", MaxRedirectHostCount)
	}
	allowlist := make(map[string]struct{}, len(request.AllowedRedirectHosts))
	for _, raw := range request.AllowedRedirectHosts {
		normalized, err := normalizeAllowedHost(raw)
		if err != nil {
			return validatedRequest{}, fmt.Errorf("invalid redirect host allowlist entry: %w", err)
		}
		if _, exists := allowlist[normalized]; exists {
			return validatedRequest{}, fmt.Errorf("duplicate redirect host %q", normalized)
		}
		allowlist[normalized] = struct{}{}
	}
	return validatedRequest{
		request:              request,
		url:                  parsed,
		originHost:           originHost,
		allowedRedirectHosts: allowlist,
	}, nil
}

func validateArtifactID(value string) error {
	if value == "" || len(value) > MaxArtifactIDBytes || !utf8.ValidString(value) {
		return fmt.Errorf("artifact ID must be valid UTF-8 and between 1 and %d bytes", MaxArtifactIDBytes)
	}
	if value != strings.TrimSpace(value) {
		return errors.New("artifact ID must not contain leading or trailing whitespace")
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return errors.New("artifact ID must not contain control characters or whitespace")
		}
	}
	return nil
}

func validateFilename(value string) error {
	if value == "" || len(value) > MaxFilenameBytes || !utf8.ValidString(value) {
		return fmt.Errorf("filename must be valid UTF-8 and between 1 and %d bytes", MaxFilenameBytes)
	}
	if value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return errors.New("filename must be a single path component")
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return errors.New("filename must contain only visible ASCII characters")
		}
	}
	return nil
}

func normalizeTimeouts(timeouts Timeouts) (Timeouts, error) {
	defaults := Timeouts{
		DNS:            DefaultDNSTimeout,
		Connect:        DefaultConnectTimeout,
		TLSHandshake:   DefaultTLSHandshakeTimeout,
		ResponseHeader: DefaultResponseHeaderTimeout,
		Overall:        DefaultOverallTimeout,
	}
	if timeouts.DNS == 0 {
		timeouts.DNS = defaults.DNS
	}
	if timeouts.Connect == 0 {
		timeouts.Connect = defaults.Connect
	}
	if timeouts.TLSHandshake == 0 {
		timeouts.TLSHandshake = defaults.TLSHandshake
	}
	if timeouts.ResponseHeader == 0 {
		timeouts.ResponseHeader = defaults.ResponseHeader
	}
	if timeouts.Overall == 0 {
		timeouts.Overall = defaults.Overall
	}
	checks := []struct {
		name  string
		value time.Duration
		max   time.Duration
	}{
		{name: "DNS", value: timeouts.DNS, max: MaxDNSTimeout},
		{name: "connect", value: timeouts.Connect, max: MaxConnectTimeout},
		{name: "TLS handshake", value: timeouts.TLSHandshake, max: MaxTLSHandshakeTimeout},
		{name: "response header", value: timeouts.ResponseHeader, max: MaxResponseHeaderTimeout},
		{name: "overall", value: timeouts.Overall, max: MaxOverallTimeout},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.max {
			return Timeouts{}, fmt.Errorf("%s timeout must be positive and no greater than %s", check.name, check.max)
		}
	}
	return timeouts, nil
}

// DecodeEvidence strictly decodes deterministic fetch evidence.
func DecodeEvidence(reader io.Reader) (Evidence, error) {
	if reader == nil {
		return Evidence{}, errors.New("missing fetch evidence reader")
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxRequestBytes+1))
	if err != nil {
		return Evidence{}, fmt.Errorf("read fetch evidence: %w", err)
	}
	if len(data) > MaxRequestBytes {
		return Evidence{}, fmt.Errorf("fetch evidence exceeds %d bytes", MaxRequestBytes)
	}
	if !utf8.Valid(data) {
		return Evidence{}, errors.New("fetch evidence is not valid UTF-8")
	}
	if err := rejectDuplicateJSONMembers(data); err != nil {
		return Evidence{}, fmt.Errorf("decode fetch evidence: %w", err)
	}
	if err := requireExactTopLevelMembers(data, map[string]struct{}{"schema_version": {}, "fetch_policy_version": {}, "artifact_id": {}, "filename": {}, "size": {}, "sha256": {}, "redacted_host_sequence": {}}); err != nil {
		return Evidence{}, fmt.Errorf("decode fetch evidence: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var evidence Evidence
	if err := decoder.Decode(&evidence); err != nil {
		return Evidence{}, fmt.Errorf("decode fetch evidence: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Evidence{}, errors.New("decode fetch evidence: trailing JSON value")
		}
		return Evidence{}, err
	}
	if err := ValidateEvidence(evidence); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

func ValidateEvidence(evidence Evidence) error {
	var errs []error
	if evidence.SchemaVersion != EvidenceSchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported evidence schema %q", evidence.SchemaVersion))
	}
	if evidence.FetchPolicyVersion != FetchPolicyVersion {
		errs = append(errs, fmt.Errorf("unsupported evidence fetch policy %q", evidence.FetchPolicyVersion))
	}
	if err := validateArtifactID(evidence.ArtifactID); err != nil {
		errs = append(errs, err)
	}
	if err := validateFilename(evidence.Filename); err != nil {
		errs = append(errs, err)
	}
	if evidence.Size <= 0 || evidence.Size > MaxBottleSize {
		errs = append(errs, fmt.Errorf("evidence size %d is outside 1..%d", evidence.Size, MaxBottleSize))
	}
	if len(evidence.SHA256) != 64 {
		errs = append(errs, errors.New("evidence SHA-256 has invalid length"))
	} else if _, err := hex.DecodeString(evidence.SHA256); err != nil || strings.ToLower(evidence.SHA256) != evidence.SHA256 {
		errs = append(errs, errors.New("evidence SHA-256 is not canonical lowercase hex"))
	}
	if len(evidence.RedactedHostSequence) == 0 || len(evidence.RedactedHostSequence) > MaxRedirects+1 {
		errs = append(errs, fmt.Errorf("evidence host sequence length %d is invalid", len(evidence.RedactedHostSequence)))
	}
	for _, host := range evidence.RedactedHostSequence {
		if normalized, err := normalizeHostname(host); err != nil || normalized != host {
			errs = append(errs, fmt.Errorf("evidence host %q is not canonical", host))
		}
	}
	return errors.Join(errs...)
}

// VerifyEvidence binds evidence back to the exact signed request.
func VerifyEvidence(evidence Evidence, request Request) error {
	if err := ValidateEvidence(evidence); err != nil {
		return err
	}
	validated, err := validateRequest(request)
	if err != nil {
		return err
	}
	if evidence.ArtifactID != validated.request.ArtifactID || evidence.Filename != validated.request.Filename || evidence.Size != validated.request.ExpectedSize || evidence.SHA256 != validated.request.SHA256 {
		return errors.New("fetch evidence does not match signed request")
	}
	if evidence.RedactedHostSequence[0] != validated.originHost {
		return errors.New("fetch evidence host sequence does not start at signed origin")
	}
	for _, host := range evidence.RedactedHostSequence[1:] {
		if _, ok := validated.allowedRedirectHosts[host]; !ok {
			return fmt.Errorf("fetch evidence contains unapproved redirect host %q", host)
		}
	}
	return nil
}

// ValidateRequest exposes the exact production fetch contract for frontend LLB
// construction without performing network access.
func ValidateRequest(request Request) error {
	_, err := validateRequest(request)
	return err
}
