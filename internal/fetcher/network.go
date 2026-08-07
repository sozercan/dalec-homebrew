package fetcher

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

var nonPublicPrefixes = []netip.Prefix{
	// IPv4 special-purpose, private, documentation, multicast, and reserved
	// ranges. IsGlobalUnicast alone still reports several of these as global.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),

	// IPv6 local, translation, discard, protocol-assignment,
	// documentation, transition, multicast, and reserved ranges.
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func isPublicIP(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

type publicResolver struct {
	resolver Resolver
	timeout  time.Duration
}

func (resolver publicResolver) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, resolver.timeout)
	defer cancel()
	addresses, err := resolver.resolver.LookupNetIP(lookupCtx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve bottle host %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolve bottle host %q: no addresses", host)
	}
	result := make([]netip.Addr, 0, len(addresses))
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if !isPublicIP(address) {
			return nil, fmt.Errorf("resolve bottle host %q: address %s is not public", host, address)
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("resolve bottle host %q: no unique public addresses", host)
	}
	return result, nil
}

type publicDialer struct {
	resolver publicResolver
	dialer   ContextDialer
}

func (dialer publicDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid dial address: %w", err)
	}
	if port != "443" {
		return nil, fmt.Errorf("refusing non-HTTPS dial port %q", port)
	}
	if parsed, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && parsed.IsValid() {
		return nil, errors.New("refusing an IP-literal dial target")
	}
	normalized, err := normalizeHostname(host)
	if err != nil {
		return nil, fmt.Errorf("invalid dial hostname: %w", err)
	}
	addresses, err := dialer.resolver.lookup(ctx, normalized)
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, candidate := range addresses {
		if network == "tcp4" && !candidate.Is4() || network == "tcp6" && !candidate.Is6() {
			continue
		}
		connection, err := dialer.dialer.DialContext(ctx, network, net.JoinHostPort(candidate.String(), port))
		if err == nil {
			return connection, nil
		}
		failures = append(failures, err)
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no public address for network %q", network)
	}
	return nil, fmt.Errorf("connect to public bottle host %q: %w", normalized, errors.Join(failures...))
}

func newDefaultTransport(resolver publicResolver, dialer ContextDialer, timeouts Timeouts) *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            (publicDialer{resolver: resolver, dialer: dialer}).DialContext,
		ForceAttemptHTTP2:      false,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    timeouts.TLSHandshake,
		ResponseHeaderTimeout:  timeouts.ResponseHeader,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 1 << 20,
		MaxConnsPerHost:        1,
		MaxIdleConns:           0,
		MaxIdleConnsPerHost:    0,
		IdleConnTimeout:        time.Second,
		WriteBufferSize:        32 << 10,
		ReadBufferSize:         32 << 10,
	}
}
