package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const DefaultHTTPTimeout = 2 * time.Minute

// Limits bounds every registry response that this package reads into memory
// and every blob streamed through the generic blob API.
type Limits struct {
	IndexBytes    int64
	ManifestBytes int64
	ConfigBytes   int64
	BlobBytes     int64
	TokenBytes    int64
	ErrorBytes    int64
}

// DefaultLimits are deliberately small for JSON metadata while permitting
// large bottle blobs through the streaming API.
func DefaultLimits() Limits {
	return Limits{
		IndexBytes:    4 << 20,
		ManifestBytes: 4 << 20,
		ConfigBytes:   1 << 20,
		BlobBytes:     8 << 30,
		TokenBytes:    1 << 20,
		ErrorBytes:    64 << 10,
	}
}

// Credentials are used for a registry's initial Basic request and for bearer
// token endpoints that require authentication.
type Credentials struct {
	Username string
	Value    string
}

// ClientOption configures a Distribution client.
type ClientOption func(*clientConfig) error

type clientConfig struct {
	httpClient  *http.Client
	limits      Limits
	credentials Credentials
	userAgent   string
}

// WithHTTPClient supplies the HTTP client used for registry and token calls.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(config *clientConfig) error {
		if client == nil {
			return errors.New("nil HTTP client")
		}
		config.httpClient = client
		return nil
	}
}

// WithCredentials configures optional registry/token credentials.
func WithCredentials(username, value string) ClientOption {
	return func(config *clientConfig) error {
		if strings.ContainsAny(username, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return errors.New("credentials contain a newline")
		}
		config.credentials = Credentials{Username: username, Value: value}
		return nil
	}
}

// WithLimits replaces the default response limits.
func WithLimits(limits Limits) ClientOption {
	return func(config *clientConfig) error {
		if err := validateLimits(limits); err != nil {
			return err
		}
		config.limits = limits
		return nil
	}
}

// WithUserAgent sets the registry and token request User-Agent.
func WithUserAgent(userAgent string) ClientOption {
	return func(config *clientConfig) error {
		if strings.ContainsAny(userAgent, "\r\n") {
			return errors.New("user agent contains a newline")
		}
		config.userAgent = userAgent
		return nil
	}
}

// Content is verified OCI content together with its exact descriptor.
type Content struct {
	Descriptor ocispec.Descriptor
	Bytes      []byte
}

// Client is a bounded, digest-verifying OCI Distribution HTTP client with
// Bearer challenge and token handling.
type Client struct {
	base        *url.URL
	httpClient  *http.Client
	limits      Limits
	credentials Credentials
	userAgent   string

	authMu     sync.Mutex
	authByRepo map[string]bearerChallenge
	tokens     map[tokenKey]cachedToken
}

type bearerChallenge struct {
	Realm   string
	Service string
	Scope   string
}

type tokenKey struct {
	Realm    string
	Service  string
	Scope    string
	Username string
}

type cachedToken struct {
	Value   string
	Expires time.Time
}

// NewClient constructs a client for a registry origin such as
// "https://ghcr.io". The /v2 API prefix is added internally.
func NewClient(registry string, options ...ClientOption) (*Client, error) {
	base, err := url.Parse(registry)
	if err != nil {
		return nil, fmt.Errorf("parse registry URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("registry URL must use http or https, got %q", base.Scheme)
	}
	if base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("registry URL must be an origin with an optional path: %q", registry)
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	base.Path = strings.TrimSuffix(base.Path, "/v2")

	config := clientConfig{
		httpClient: &http.Client{Timeout: DefaultHTTPTimeout},
		limits:     DefaultLimits(),
		userAgent:  "dalec-homebrew/oci",
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil client option")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if err := validateLimits(config.limits); err != nil {
		return nil, err
	}

	return &Client{
		base:        base,
		httpClient:  config.httpClient,
		limits:      config.limits,
		credentials: config.credentials,
		userAgent:   config.userAgent,
		authByRepo:  make(map[string]bearerChallenge),
		tokens:      make(map[tokenKey]cachedToken),
	}, nil
}

// NewGHCRClient constructs a client for the canonical Homebrew registry.
func NewGHCRClient(options ...ClientOption) (*Client, error) {
	return NewClient("https://"+GHCRRegistry, options...)
}

// Limits returns a copy of the client's configured bounds.
func (client *Client) Limits() Limits {
	return client.limits
}

// FetchIndex fetches an OCI index by tag, verifies any registry digest header,
// computes the exact digest independently, and bounds the response body.
func (client *Client) FetchIndex(ctx context.Context, repository, tag string) (Content, error) {
	if !ociTagRE.MatchString(tag) {
		return Content{}, fmt.Errorf("invalid OCI tag %q", tag)
	}
	return client.fetchManifest(ctx, repository, tag, nil, ocispec.MediaTypeImageIndex, client.limits.IndexBytes)
}

// FetchManifest fetches an OCI manifest by its selected index descriptor and
// verifies media type, exact size, and exact digest.
func (client *Client) FetchManifest(ctx context.Context, repository string, descriptor ocispec.Descriptor) (Content, error) {
	if err := validateDescriptor(descriptor, ocispec.MediaTypeImageManifest, client.limits.ManifestBytes); err != nil {
		return Content{}, fmt.Errorf("manifest descriptor: %w", err)
	}
	return client.fetchManifest(ctx, repository, descriptor.Digest.String(), &descriptor, ocispec.MediaTypeImageManifest, client.limits.ManifestBytes)
}

// FetchConfig fetches and exactly verifies an OCI image config blob.
func (client *Client) FetchConfig(ctx context.Context, repository string, descriptor ocispec.Descriptor) (Content, error) {
	if err := validateDescriptor(descriptor, ocispec.MediaTypeImageConfig, client.limits.ConfigBytes); err != nil {
		return Content{}, fmt.Errorf("config descriptor: %w", err)
	}
	var buffer bytes.Buffer
	if err := client.fetchBlobTo(ctx, repository, descriptor, client.limits.ConfigBytes, &buffer); err != nil {
		return Content{}, err
	}
	return Content{Descriptor: cloneDescriptor(descriptor), Bytes: buffer.Bytes()}, nil
}

// FetchBlob fetches a descriptor into memory using the configured blob bound.
func (client *Client) FetchBlob(ctx context.Context, repository string, descriptor ocispec.Descriptor) (Content, error) {
	var buffer bytes.Buffer
	if err := client.FetchBlobTo(ctx, repository, descriptor, &buffer); err != nil {
		return Content{}, err
	}
	return Content{Descriptor: cloneDescriptor(descriptor), Bytes: buffer.Bytes()}, nil
}

// FetchBlobTo streams a blob through exact size and digest verification. The
// caller must discard bytes already written to dst if an error is returned.
func (client *Client) FetchBlobTo(ctx context.Context, repository string, descriptor ocispec.Descriptor, dst io.Writer) error {
	if dst == nil {
		return errors.New("nil blob destination")
	}
	if err := validateDescriptor(descriptor, "", client.limits.BlobBytes); err != nil {
		return fmt.Errorf("blob descriptor: %w", err)
	}
	return client.fetchBlobTo(ctx, repository, descriptor, client.limits.BlobBytes, dst)
}

func (client *Client) fetchManifest(ctx context.Context, repository, reference string, expected *ocispec.Descriptor, mediaType string, limit int64) (Content, error) {
	requestURL, err := client.registryURL(repository, "manifests", reference)
	if err != nil {
		return Content{}, err
	}
	response, err := client.doRegistryGET(ctx, repository, requestURL, mediaType)
	if err != nil {
		return Content{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Content{}, client.statusError(response)
	}
	if err := validateIdentityEncoding(response); err != nil {
		return Content{}, err
	}
	actualMediaType, err := responseMediaType(response)
	if err != nil {
		return Content{}, err
	}
	if actualMediaType != mediaType {
		return Content{}, fmt.Errorf("unexpected manifest media type %q, expected %q", actualMediaType, mediaType)
	}
	if err := validateContentLength(response.ContentLength, expected, limit); err != nil {
		return Content{}, err
	}

	body, err := readBounded(response.Body, limit)
	if err != nil {
		return Content{}, fmt.Errorf("read %s: %w", mediaType, err)
	}
	computed := digest.FromBytes(body)
	if computed.Algorithm() != digest.SHA256 {
		return Content{}, fmt.Errorf("computed unsupported digest %s", computed)
	}
	if header := strings.TrimSpace(response.Header.Get("Docker-Content-Digest")); header != "" {
		headerDigest, err := parseSHA256Digest(header)
		if err != nil {
			return Content{}, fmt.Errorf("Docker-Content-Digest: %w", err)
		}
		if headerDigest != computed {
			return Content{}, fmt.Errorf("Docker-Content-Digest %s does not match response bytes %s", headerDigest, computed)
		}
	}
	if expected != nil {
		if int64(len(body)) != expected.Size {
			return Content{}, fmt.Errorf("content size %d does not match descriptor size %d", len(body), expected.Size)
		}
		if computed != expected.Digest {
			return Content{}, fmt.Errorf("content digest %s does not match descriptor digest %s", computed, expected.Digest)
		}
	}

	descriptor := ocispec.Descriptor{MediaType: mediaType, Digest: computed, Size: int64(len(body))}
	if expected != nil {
		descriptor = cloneDescriptor(*expected)
	}
	return Content{Descriptor: descriptor, Bytes: body}, nil
}

func (client *Client) fetchBlobTo(ctx context.Context, repository string, descriptor ocispec.Descriptor, limit int64, dst io.Writer) error {
	if err := validateDescriptor(descriptor, "", limit); err != nil {
		return fmt.Errorf("blob descriptor: %w", err)
	}
	requestURL, err := client.registryURL(repository, "blobs", descriptor.Digest.String())
	if err != nil {
		return err
	}
	response, err := client.doRegistryGET(ctx, repository, requestURL, "application/octet-stream")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return client.statusError(response)
	}
	if err := validateIdentityEncoding(response); err != nil {
		return err
	}
	if err := validateContentLength(response.ContentLength, &descriptor, limit); err != nil {
		return err
	}

	hasher := digest.SHA256.Digester()
	limited := &io.LimitedReader{R: response.Body, N: limit + 1}
	written, err := io.Copy(io.MultiWriter(dst, hasher.Hash()), limited)
	if err != nil {
		return fmt.Errorf("read blob %s: %w", descriptor.Digest, err)
	}
	if written > limit {
		return fmt.Errorf("blob exceeds %d-byte limit", limit)
	}
	if written != descriptor.Size {
		return fmt.Errorf("blob size %d does not match descriptor size %d", written, descriptor.Size)
	}
	computed := hasher.Digest()
	if computed != descriptor.Digest {
		return fmt.Errorf("blob digest %s does not match descriptor digest %s", computed, descriptor.Digest)
	}
	return nil
}

func (client *Client) doRegistryGET(ctx context.Context, repository string, requestURL *url.URL, accept string) (*http.Response, error) {
	challenge, remembered := client.rememberedChallenge(repository)
	var token string
	if remembered {
		var err error
		token, err = client.bearerToken(ctx, challenge, false)
		if err != nil {
			return nil, err
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		response, err := client.doGET(ctx, requestURL, accept, token, !remembered && token == "")
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusUnauthorized {
			return response, nil
		}

		newChallenge, err := parseBearerChallenge(response.Header.Values("WWW-Authenticate"))
		client.discardAndClose(response.Body)
		if err != nil {
			return nil, fmt.Errorf("registry authentication challenge: %w", err)
		}
		if token != "" {
			client.invalidateToken(challenge)
		}
		challenge = newChallenge
		client.rememberChallenge(repository, challenge)
		token, err = client.bearerToken(ctx, challenge, true)
		if err != nil {
			return nil, err
		}
		remembered = true
	}
	return nil, errors.New("registry rejected bearer authentication")
}

func (client *Client) doGET(ctx context.Context, requestURL *url.URL, accept, bearer string, useBasic bool) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Accept-Encoding", "identity")
	if client.userAgent != "" {
		request.Header.Set("User-Agent", client.userAgent)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	} else if useBasic && client.credentials.Username != "" {
		request.SetBasicAuth(client.credentials.Username, client.credentials.Value)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", requestURL.Redacted(), err)
	}
	return response, nil
}

func (client *Client) bearerToken(ctx context.Context, challenge bearerChallenge, force bool) (string, error) {
	key := tokenKey{
		Realm: challenge.Realm, Service: challenge.Service, Scope: challenge.Scope,
		Username: client.credentials.Username,
	}
	if !force {
		client.authMu.Lock()
		cached, ok := client.tokens[key]
		client.authMu.Unlock()
		if ok && time.Now().Before(cached.Expires) {
			return cached.Value, nil
		}
	}

	realm, err := url.Parse(challenge.Realm)
	if err != nil || realm.Host == "" || realm.User != nil || realm.Fragment != "" || (realm.Scheme != "http" && realm.Scheme != "https") {
		return "", fmt.Errorf("invalid bearer token realm %q", challenge.Realm)
	}
	if client.base.Scheme == "https" && realm.Scheme != "https" {
		return "", fmt.Errorf("bearer token realm %q downgrades HTTPS registry authentication", challenge.Realm)
	}
	query := realm.Query()
	if challenge.Service != "" {
		query.Set("service", challenge.Service)
	}
	if challenge.Scope != "" {
		query.Set("scope", challenge.Scope)
	}
	realm.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	if client.userAgent != "" {
		request.Header.Set("User-Agent", client.userAgent)
	}
	if client.credentials.Username != "" {
		request.SetBasicAuth(client.credentials.Username, client.credentials.Value)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("request registry authorization: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", client.statusError(response)
	}
	if err := validateIdentityEncoding(response); err != nil {
		return "", err
	}
	body, err := readBounded(response.Body, client.limits.TokenBytes)
	if err != nil {
		return "", fmt.Errorf("read bearer token response: %w", err)
	}
	var wire struct {
		Token       string          `json:"token"`
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
		IssuedAt    string          `json:"issued_at"`
	}
	if err := decodeJSON(body, &wire); err != nil {
		return "", fmt.Errorf("decode bearer token response: %w", err)
	}
	authValue := wire.Token
	if authValue == "" {
		authValue = wire.AccessToken
	}
	if err := validateBearerToken(authValue); err != nil {
		return "", err
	}
	expiresIn, err := parseExpiresIn(wire.ExpiresIn)
	if err != nil {
		return "", err
	}
	issuedAt := time.Now()
	if wire.IssuedAt != "" {
		parsed, err := time.Parse(time.RFC3339, wire.IssuedAt)
		if err != nil {
			return "", fmt.Errorf("invalid bearer token issued_at: %w", err)
		}
		issuedAt = parsed
	}
	if expiresIn <= 0 {
		expiresIn = 5 * time.Minute
	}
	expires := issuedAt.Add(expiresIn)
	if expiresIn > time.Minute {
		expires = expires.Add(-30 * time.Second)
	}
	client.authMu.Lock()
	client.tokens[key] = cachedToken{Value: authValue, Expires: expires}
	client.authMu.Unlock()
	return authValue, nil
}

func (client *Client) rememberedChallenge(repository string) (bearerChallenge, bool) {
	client.authMu.Lock()
	defer client.authMu.Unlock()
	challenge, ok := client.authByRepo[repository]
	return challenge, ok
}

func (client *Client) rememberChallenge(repository string, challenge bearerChallenge) {
	client.authMu.Lock()
	client.authByRepo[repository] = challenge
	client.authMu.Unlock()
}

func (client *Client) invalidateToken(challenge bearerChallenge) {
	key := tokenKey{
		Realm: challenge.Realm, Service: challenge.Service, Scope: challenge.Scope,
		Username: client.credentials.Username,
	}
	client.authMu.Lock()
	delete(client.tokens, key)
	client.authMu.Unlock()
}

func (client *Client) registryURL(repository, kind, reference string) (*url.URL, error) {
	if err := validateRepository(repository); err != nil {
		return nil, err
	}
	if kind != "manifests" && kind != "blobs" && kind != "referrers" {
		return nil, fmt.Errorf("unsupported registry object kind %q", kind)
	}
	if strings.ContainsAny(reference, "/\\?#\x00\r\n") || reference == "" {
		return nil, fmt.Errorf("invalid registry reference %q", reference)
	}
	result := *client.base
	result.Path = strings.TrimSuffix(client.base.Path, "/") + "/v2/" + repository + "/" + kind + "/" + reference
	result.RawPath = ""
	return &result, nil
}

func (client *Client) statusError(response *http.Response) error {
	body, err := readBounded(response.Body, client.limits.ErrorBytes)
	if err != nil {
		return fmt.Errorf("registry returned %s (error body: %v)", response.Status, err)
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("registry returned %s", response.Status)
	}
	return fmt.Errorf("registry returned %s: %s", response.Status, message)
}

func (client *Client) discardAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, client.limits.ErrorBytes))
	_ = body.Close()
}

func parseBearerChallenge(values []string) (bearerChallenge, error) {
	for _, value := range values {
		lower := strings.ToLower(value)
		start := -1
		if strings.HasPrefix(strings.TrimSpace(lower), "bearer ") {
			start = strings.Index(lower, "bearer ")
		} else if index := strings.Index(lower, ", bearer "); index >= 0 {
			start = index + 2
		}
		if start < 0 {
			continue
		}
		params, err := parseAuthParams(value[start+len("Bearer "):])
		if err != nil {
			return bearerChallenge{}, err
		}
		realm := params["realm"]
		if realm == "" {
			return bearerChallenge{}, errors.New("Bearer challenge has no realm")
		}
		return bearerChallenge{Realm: realm, Service: params["service"], Scope: params["scope"]}, nil
	}
	return bearerChallenge{}, errors.New("401 response has no Bearer challenge")
}

func parseAuthParams(value string) (map[string]string, error) {
	params := make(map[string]string)
	for index := 0; index < len(value); {
		for index < len(value) && (value[index] == ',' || value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index >= len(value) {
			break
		}
		keyStart := index
		for index < len(value) && isAuthTokenChar(value[index]) {
			index++
		}
		if keyStart == index {
			return nil, fmt.Errorf("invalid Bearer challenge near %q", value[index:])
		}
		key := strings.ToLower(value[keyStart:index])
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index >= len(value) || value[index] != '=' {
			break // A subsequent authentication scheme starts here.
		}
		index++
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index >= len(value) {
			return nil, fmt.Errorf("Bearer challenge parameter %q has no value", key)
		}
		var parsed string
		if value[index] == '"' {
			index++
			var builder strings.Builder
			closed := false
			for index < len(value) {
				character := value[index]
				index++
				if character == '\\' {
					if index >= len(value) {
						return nil, errors.New("unterminated Bearer challenge escape")
					}
					builder.WriteByte(value[index])
					index++
					continue
				}
				if character == '"' {
					closed = true
					break
				}
				builder.WriteByte(character)
			}
			if !closed {
				return nil, fmt.Errorf("unterminated Bearer challenge value for %q", key)
			}
			parsed = builder.String()
		} else {
			valueStart := index
			for index < len(value) && value[index] != ',' && value[index] != ' ' && value[index] != '\t' {
				index++
			}
			parsed = value[valueStart:index]
		}
		if _, duplicate := params[key]; duplicate {
			return nil, fmt.Errorf("duplicate Bearer challenge parameter %q", key)
		}
		params[key] = parsed
	}
	return params, nil
}

func isAuthTokenChar(character byte) bool {
	return character > 0x20 && character < 0x7f && !strings.ContainsRune("()<>@,;:\\\"/[]?={}\t", rune(character))
}

func parseExpiresIn(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var seconds int64
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, fmt.Errorf("invalid bearer token expires_in: %w", err)
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid bearer token expires_in %q", value)
		}
		seconds = parsed
	} else {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var number json.Number
		if err := decoder.Decode(&number); err != nil {
			return 0, fmt.Errorf("invalid bearer token expires_in: %w", err)
		}
		parsed, err := number.Int64()
		if err != nil {
			return 0, fmt.Errorf("invalid bearer token expires_in %q", number)
		}
		seconds = parsed
	}
	if seconds < 0 {
		return 0, fmt.Errorf("negative bearer token expires_in %d", seconds)
	}
	if seconds > int64((1<<63-1)/time.Second) {
		return 0, fmt.Errorf("bearer token expires_in %d overflows a duration", seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

func validateBearerToken(token string) error {
	if token == "" {
		return errors.New("bearer token response contains no token")
	}
	for _, character := range token {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return errors.New("bearer token contains whitespace or control characters")
		}
	}
	return nil
}

func validateDescriptor(descriptor ocispec.Descriptor, mediaType string, limit int64) error {
	if mediaType != "" && descriptor.MediaType != mediaType {
		return fmt.Errorf("media type %q, expected %q", descriptor.MediaType, mediaType)
	}
	if descriptor.MediaType == "" {
		return errors.New("empty media type")
	}
	if _, err := parseSHA256Digest(descriptor.Digest.String()); err != nil {
		return err
	}
	if descriptor.Size <= 0 {
		return fmt.Errorf("invalid size %d", descriptor.Size)
	}
	if descriptor.Size > limit {
		return fmt.Errorf("size %d exceeds %d-byte limit", descriptor.Size, limit)
	}
	if len(descriptor.Data) != 0 {
		return errors.New("embedded descriptor data is not allowed")
	}
	if len(descriptor.URLs) != 0 {
		return errors.New("descriptor URLs are not allowed")
	}
	if descriptor.ArtifactType != "" {
		return errors.New("descriptor artifactType is not allowed")
	}
	return nil
}

func parseSHA256Digest(value string) (digest.Digest, error) {
	parsed, err := digest.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("only sha256 descriptors are allowed, got %s", parsed.Algorithm())
	}
	if err := parsed.Validate(); err != nil {
		return "", err
	}
	return parsed, nil
}

func validateRepository(repository string) error {
	if repository == "" || len(repository) > 255 || strings.ToLower(repository) != repository {
		return fmt.Errorf("invalid lowercase OCI repository %q", repository)
	}
	for _, segment := range strings.Split(repository, "/") {
		if !repositorySegmentRE.MatchString(segment) {
			return fmt.Errorf("invalid OCI repository segment %q", segment)
		}
	}
	return nil
}

func validateLimits(limits Limits) error {
	for name, value := range map[string]int64{
		"index": limits.IndexBytes, "manifest": limits.ManifestBytes,
		"config": limits.ConfigBytes, "blob": limits.BlobBytes,
		"auth": limits.TokenBytes, "error": limits.ErrorBytes,
	} {
		if value <= 0 || value == int64(^uint64(0)>>1) {
			return fmt.Errorf("%s response limit must be positive", name)
		}
	}
	return nil
}

func validateContentLength(contentLength int64, expected *ocispec.Descriptor, limit int64) error {
	if contentLength > limit {
		return fmt.Errorf("Content-Length %d exceeds %d-byte limit", contentLength, limit)
	}
	if expected != nil && contentLength >= 0 && contentLength != expected.Size {
		return fmt.Errorf("Content-Length %d does not match descriptor size %d", contentLength, expected.Size)
	}
	return nil
}

func validateIdentityEncoding(response *http.Response) error {
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fmt.Errorf("encoded registry response %q cannot be digest-verified", encoding)
	}
	return nil
}

func responseMediaType(response *http.Response) (string, error) {
	value := response.Header.Get("Content-Type")
	if value == "" {
		return "", errors.New("registry response has no Content-Type")
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "", fmt.Errorf("invalid registry Content-Type %q: %w", value, err)
	}
	return mediaType, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", limit)
	}
	return body, nil
}

func decodeJSON(data []byte, destination any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}

func cloneDescriptor(descriptor ocispec.Descriptor) ocispec.Descriptor {
	clone := descriptor
	clone.URLs = append([]string(nil), descriptor.URLs...)
	clone.Data = append([]byte(nil), descriptor.Data...)
	if descriptor.Annotations != nil {
		clone.Annotations = make(map[string]string, len(descriptor.Annotations))
		for key, value := range descriptor.Annotations {
			clone.Annotations[key] = value
		}
	}
	if descriptor.Platform != nil {
		platform := *descriptor.Platform
		platform.OSFeatures = append([]string(nil), descriptor.Platform.OSFeatures...)
		clone.Platform = &platform
	}
	return clone
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := walkJSONValue(decoder, first); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := walkJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := walkJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
