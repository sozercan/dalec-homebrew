package runtimebase

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSnapshotProxyRewrite(t *testing.T) {
	proxy, err := NewSnapshotProxy("20260610T000000Z")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"http://archive.ubuntu.com/ubuntu/dists/noble/InRelease":                         "https://snapshot.ubuntu.com/ubuntu/20260610T000000Z/dists/noble/InRelease",
		"http://security.ubuntu.com/ubuntu/pool/main/g/glibc/libc6.deb?x=1":              "https://snapshot.ubuntu.com/ubuntu/20260610T000000Z/pool/main/g/glibc/libc6.deb?x=1",
		"http://ports.ubuntu.com/ubuntu-ports/dists/noble/main/binary-arm64/Packages.xz": "https://snapshot.ubuntu.com/ubuntu/20260610T000000Z/dists/noble/main/binary-arm64/Packages.xz",
	}
	for raw, want := range tests {
		u, _ := url.Parse(raw)
		got, err := proxy.Rewrite(u, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

func TestSnapshotProxyRejectsUnexpectedInput(t *testing.T) {
	if _, err := NewSnapshotProxy("latest"); err == nil {
		t.Fatal("expected invalid snapshot error")
	}
	proxy, _ := NewSnapshotProxy("20260610T000000Z")
	for _, raw := range []string{"https://archive.ubuntu.com/ubuntu/dists/noble/InRelease", "http://example.com/ubuntu/dists/noble/InRelease", "http://archive.ubuntu.com/not-ubuntu/x", "http://archive.ubuntu.com/ubuntu/../etc/passwd"} {
		u, _ := url.Parse(raw)
		if _, err := proxy.Rewrite(u, ""); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

func TestCopyHeadersDropsHopByHopHeaders(t *testing.T) {
	src := http.Header{"X-Test": {"ok"}, "Connection": {"close"}, "Proxy-Authorization": {"secret"}}
	dst := make(http.Header)
	copyHeaders(dst, src)
	if dst.Get("X-Test") != "ok" || dst.Get("Connection") != "" || dst.Get("Proxy-Authorization") != "" {
		t.Fatalf("headers=%v", dst)
	}
}

func TestRootPathRejectsNonCanonicalPath(t *testing.T) {
	_, err := rootPath(t.TempDir(), "/usr/../etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "invalid chisel content path") {
		t.Fatalf("error=%v", err)
	}
}
