package fetcher

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestIsPublicIP(t *testing.T) {
	tests := []struct {
		address string
		public  bool
	}{
		{address: "8.8.8.8", public: true},
		{address: "93.184.216.34", public: true},
		{address: "2001:4860:4860::8888", public: true},
		{address: "2606:4700:4700::1111", public: true},
		{address: "0.0.0.0", public: false},
		{address: "10.0.0.1", public: false},
		{address: "100.64.0.1", public: false},
		{address: "127.0.0.1", public: false},
		{address: "169.254.1.1", public: false},
		{address: "172.16.0.1", public: false},
		{address: "192.0.2.1", public: false},
		{address: "192.168.1.1", public: false},
		{address: "198.18.0.1", public: false},
		{address: "198.51.100.1", public: false},
		{address: "203.0.113.1", public: false},
		{address: "224.0.0.1", public: false},
		{address: "240.0.0.1", public: false},
		{address: "::", public: false},
		{address: "::1", public: false},
		{address: "::127.0.0.1", public: false},
		{address: "::10.0.0.1", public: false},
		{address: "::ffff:127.0.0.1", public: false},
		{address: "64:ff9b::7f00:1", public: false},
		{address: "100::1", public: false},
		{address: "2001:db8::1", public: false},
		{address: "2002::1", public: false},
		{address: "3fff::1", public: false},
		{address: "fc00::1", public: false},
		{address: "fe80::1", public: false},
		{address: "fec0::1", public: false},
		{address: "ff02::1", public: false},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			if got := isPublicIP(netip.MustParseAddr(test.address)); got != test.public {
				t.Fatalf("isPublicIP(%s) = %t, want %t", test.address, got, test.public)
			}
		})
	}
}

func TestPublicResolverRejectsAnyNonPublicAnswer(t *testing.T) {
	resolver := publicResolver{
		resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
		}),
		timeout: time.Second,
	}
	if _, err := resolver.lookup(context.Background(), "bottles.example.com"); err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("error = %v", err)
	}
}
