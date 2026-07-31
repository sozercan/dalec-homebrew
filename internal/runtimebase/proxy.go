package runtimebase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

var snapshotPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)

// SnapshotProxy redirects Chisel's hard-coded Ubuntu archive hosts to an
// immutable snapshot. Chisel still verifies signed Release metadata and every
// package digest after this transport-only redirect.
type SnapshotProxy struct {
	Snapshot string
	Client   *http.Client
}

func NewSnapshotProxy(snapshot string) (*SnapshotProxy, error) {
	if !snapshotPattern.MatchString(snapshot) {
		return nil, fmt.Errorf("invalid Ubuntu snapshot %q", snapshot)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &SnapshotProxy{Snapshot: snapshot, Client: &http.Client{
		Transport: transport,
		Timeout:   5 * time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("Ubuntu snapshot redirects are not allowed")
		},
	}}, nil
}

func (p *SnapshotProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	upstream, err := p.Rewrite(r.URL, r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeaders(req.Header, r.Header)
	resp, err := p.Client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch Ubuntu snapshot: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *SnapshotProxy) Rewrite(requestURL *url.URL, requestHost string) (*url.URL, error) {
	if requestURL == nil {
		return nil, errors.New("missing proxy request URL")
	}
	u := *requestURL
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	if u.Host == "" {
		u.Host = requestHost
	}
	if u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("unsupported Ubuntu archive URL %q", u.String())
	}
	var archivePath string
	switch strings.ToLower(u.Hostname()) {
	case "archive.ubuntu.com", "security.ubuntu.com":
		archivePath = strings.TrimPrefix(u.Path, "/ubuntu/")
	case "ports.ubuntu.com":
		archivePath = strings.TrimPrefix(u.Path, "/ubuntu-ports/")
	default:
		return nil, fmt.Errorf("proxy target %q is not an Ubuntu archive", u.Host)
	}
	if archivePath == u.Path || archivePath == "" || strings.HasPrefix(archivePath, "../") || strings.Contains(archivePath, "/../") {
		return nil, fmt.Errorf("invalid Ubuntu archive path %q", u.Path)
	}
	u.Scheme = "https"
	u.Host = "snapshot.ubuntu.com"
	u.Path = path.Join("/ubuntu", p.Snapshot, archivePath)
	if strings.HasSuffix(requestURL.Path, "/") {
		u.Path += "/"
	}
	return &u, nil
}

var hopByHopHeaders = map[string]struct{}{
	"Connection": {}, "Keep-Alive": {}, "Proxy-Authenticate": {}, "Proxy-Authorization": {},
	"Proxy-Connection": {}, "Te": {}, "Trailer": {}, "Transfer-Encoding": {}, "Upgrade": {},
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func ServeSnapshotProxy(ctx context.Context, listen, readyFile, snapshot string) error {
	proxy, err := NewSnapshotProxy(snapshot)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	if readyFile != "" {
		if err := os.WriteFile(readyFile, []byte(listener.Addr().String()+"\n"), 0o600); err != nil {
			return err
		}
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-serveErr
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
