#!/usr/bin/env bash
set -euo pipefail

bash -n scripts/check.sh scripts/live-test.sh scripts/image-size-report.sh
go test ./...
go vet ./...
go test -race \
  ./internal/homebrew/metadata \
  ./internal/homebrew/oci \
  ./internal/bottle \
  ./internal/resolution \
  ./internal/runtimefs \
  ./internal/testrunner
for arch in amd64 arm64; do
  for cmd in frontend materializer record-verify release-verify test-runner resolve; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "/tmp/dalec-homebrew-${cmd}-${arch}" "./cmd/${cmd}"
  done
done
