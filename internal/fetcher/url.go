package fetcher

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

func parseBottleURL(raw string) (*url.URL, string, error) {
	if raw == "" || len(raw) > MaxURLBytes {
		return nil, "", fmt.Errorf("bottle URL must be between 1 and %d bytes", MaxURLBytes)
	}
	if strings.ContainsAny(raw, "\x00\r\n") {
		return nil, "", errors.New("bottle URL contains a control character")
	}
	// Reject even an empty fragment marker so the signed URL has one
	// unambiguous fetch representation.
	if strings.Contains(raw, "#") {
		return nil, "", errors.New("bottle URL fragments are not allowed")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, "", errors.New("bottle URL is invalid")
	}
	host, err := validateEndpointURL(parsed)
	if err != nil {
		return nil, "", err
	}
	return parsed, host, nil
}

func validateEndpointURL(parsed *url.URL) (string, error) {
	if parsed == nil {
		return "", errors.New("missing bottle URL")
	}
	if parsed.Scheme != "https" {
		return "", errors.New("bottle URL must use HTTPS")
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return "", errors.New("bottle URL must be absolute")
	}
	if parsed.User != nil {
		return "", errors.New("bottle URL userinfo is not allowed")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", errors.New("bottle URL fragments are not allowed")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return "", errors.New("bottle URL must use port 443")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return "", errors.New("bottle URL has an invalid empty port")
	}
	host, err := normalizeHostname(parsed.Hostname())
	if err != nil {
		return "", fmt.Errorf("invalid bottle URL host: %w", err)
	}
	return host, nil
}

func normalizeAllowedHost(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, ":/@?#[]\\") {
		return "", errors.New("redirect hosts must be DNS hostnames without a scheme, port, or path")
	}
	return normalizeHostname(raw)
}

func normalizeHostname(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("hostname is empty")
	}
	if raw != strings.TrimSpace(raw) || strings.HasSuffix(raw, ".") {
		return "", errors.New("hostname must not contain whitespace or a trailing dot")
	}
	host := strings.ToLower(raw)
	if len(host) > 253 {
		return "", errors.New("hostname is too long")
	}
	if address, err := netip.ParseAddr(host); err == nil && address.IsValid() {
		return "", errors.New("IP-literal destinations are not allowed")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("hostname must be a fully qualified DNS name")
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return "", errors.New("hostname contains an invalid DNS label length")
		}
		if !isASCIIAlphanumeric(label[0]) || !isASCIIAlphanumeric(label[len(label)-1]) {
			return "", errors.New("hostname labels must start and end with an ASCII letter or digit")
		}
		for i := 1; i < len(label)-1; i++ {
			if !isASCIIAlphanumeric(label[i]) && label[i] != '-' {
				return "", errors.New("hostname contains a non-DNS character")
			}
		}
	}
	last := labels[len(labels)-1]
	if allASCIIDigits(last) {
		return "", errors.New("hostname has an all-numeric final label")
	}
	return host, nil
}

func isASCIIAlphanumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func allASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
