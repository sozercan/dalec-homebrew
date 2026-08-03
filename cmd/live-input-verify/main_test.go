package main

import (
	"io"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	validRefs := []string{
		"--runtime-base-ref", "GHCR.IO/example/runtime-base@" + digest,
		"--materializer-ref", "localhost:5000/example/materializer.v2@" + digest,
		"--frontend-ref", "example/frontend_name:release-1@" + digest,
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "empty"},
		{name: "valid refs and timestamp", args: append(append([]string{}, validRefs...), "--metadata-not-before", "2026-08-02T12:34:56.1-07:00")},
		{name: "valid long fraction", args: []string{"--metadata-not-before", "2026-08-02T12:34:56.1234567890Z"}},
		{name: "valid additional refs", args: []string{"--pinned-ref", "GO_IMAGE=GHCR.IO/example/go@" + digest, "--pinned-ref", "dockerfile-frontend=docker/dockerfile:1.12@" + digest}},
		{name: "additional ref scheme", args: []string{"--pinned-ref", "GO_IMAGE=https://ghcr.io/example/go@" + digest}, want: "GO_IMAGE must be a digest-pinned OCI reference"},
		{name: "malformed additional ref", args: []string{"--pinned-ref", "ghcr.io/example/go@" + digest}, want: "NAME=REFERENCE"},
		{name: "partial refs", args: []string{"--runtime-base-ref", "ghcr.io/example/base@" + digest}, want: "must be set together"},
		{name: "scheme", args: []string{"--runtime-base-ref", "https://ghcr.io/example/base@" + digest, "--materializer-ref", "ghcr.io/example/materializer@" + digest, "--frontend-ref", "ghcr.io/example/frontend@" + digest}, want: "invalid OCI reference"},
		{name: "empty segment", args: []string{"--runtime-base-ref", "ghcr.io//base@" + digest, "--materializer-ref", "ghcr.io/example/materializer@" + digest, "--frontend-ref", "ghcr.io/example/frontend@" + digest}, want: "invalid OCI reference"},
		{name: "malformed tag", args: []string{"--runtime-base-ref", "ghcr.io/example/base@" + digest, "--materializer-ref", "ghcr.io/example/materializer:bad:tag@" + digest, "--frontend-ref", "ghcr.io/example/frontend@" + digest}, want: "invalid OCI reference"},
		{name: "invalid date", args: []string{"--metadata-not-before", "2026-02-31T00:00:00Z"}, want: "valid RFC3339"},
		{name: "invalid hour", args: []string{"--metadata-not-before", "2026-08-02T24:00:00Z"}, want: "valid RFC3339"},
		{name: "invalid offset", args: []string{"--metadata-not-before", "2026-08-02T00:00:00+01:60"}, want: "valid RFC3339"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args, io.Discard)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}
