#!/usr/bin/env bash
set -euo pipefail

for script in scripts/*.sh; do
  bash -n "$script"
done
./scripts/live-test-test.sh
go test -trimpath ./...
go vet ./...
go test -race \
  ./internal/homebrew/formulaid \
  ./internal/homebrew/metadata \
  ./internal/homebrew/oci \
  ./internal/catalog \
  ./internal/catalogauth \
  ./internal/catalogclient \
  ./internal/catalogextractor \
  ./internal/cataloggenerator \
  ./internal/catalogkeys \
  ./internal/catalogresolver \
  ./internal/catalogservice \
  ./internal/fetcher \
  ./internal/config \
  ./internal/frontend \
  ./internal/release \
  ./internal/policy \
  ./internal/bottle \
  ./internal/materializer \
  ./internal/resolution \
  ./internal/runtimefs \
  ./internal/runtimecheck \
  ./internal/runtimebase \
  ./internal/testrunner
for arch in amd64 arm64; do
  for cmd in frontend live-input-verify materializer record-verify release-manifest release-verify test-runner resolve runtime-base-evidence snapshot-proxy bottle-fetcher catalog-service catalog-extractor v2-bindings; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "/tmp/dalec-homebrew-${cmd}-${arch}" "./cmd/${cmd}"
  done
done
