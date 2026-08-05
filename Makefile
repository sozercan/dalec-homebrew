.PHONY: test vet check build live-test

test:
	go test ./...

vet:
	go vet ./...

check:
	./scripts/check.sh

live-test:
	./scripts/live-test.sh

build:
	mkdir -p bin
	go build -trimpath -o bin/dalec-homebrew-frontend ./cmd/frontend
	go build -trimpath -o bin/dalec-homebrew-materializer ./cmd/materializer
	go build -trimpath -o bin/dalec-homebrew-record-verify ./cmd/record-verify
	go build -trimpath -o bin/dalec-homebrew-release-verify ./cmd/release-verify
	go build -trimpath -o bin/dalec-homebrew-release-manifest ./cmd/release-manifest
	go build -trimpath -o bin/dalec-homebrew-test-runner ./cmd/test-runner
	go build -trimpath -o bin/dalec-homebrew-resolve ./cmd/resolve
	go build -trimpath -o bin/dalec-homebrew-bottle-fetcher ./cmd/bottle-fetcher
	go build -trimpath -o bin/dalec-homebrew-catalog-extractor ./cmd/catalog-extractor
	go build -trimpath -o bin/dalec-homebrew-v2-bindings ./cmd/v2-bindings
