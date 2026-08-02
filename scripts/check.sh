#!/usr/bin/env bash
set -euo pipefail

for script in scripts/*.sh; do
  bash -n "$script"
done
./scripts/live-test-test.sh
go test -trimpath ./...
go vet ./...
go test -race \
  ./internal/homebrew/metadata \
  ./internal/homebrew/oci \
  ./internal/bottle \
  ./internal/materializer \
  ./internal/resolution \
  ./internal/runtimefs \
  ./internal/runtimebase \
  ./internal/testrunner
for arch in amd64 arm64; do
  for cmd in frontend materializer record-verify release-manifest release-verify test-runner resolve runtime-base-evidence snapshot-proxy; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "/tmp/dalec-homebrew-${cmd}-${arch}" "./cmd/${cmd}"
  done
done
