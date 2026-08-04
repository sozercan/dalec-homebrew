// Package catalog defines the immutable protocol and data-model foundation for
// release-bound non-core Homebrew catalog resolution.
//
// The package deliberately contains no networking, JWS verification, service
// persistence, BuildKit orchestration, or frontend integration. Callers are
// expected to authenticate a CatalogSetPayload before trusting any referenced
// catalog, transport, provenance, or closure result.
package catalog

import "time"

const (
	RequestSchemaVersion    = "dalec-homebrew-catalog-request/v1"
	TapCatalogSchemaVersion = "dalec-homebrew-tap-catalog/v1"
	CatalogSetSchemaVersion = "dalec-homebrew-catalog-set/v1"
	OperationSchemaVersion  = "dalec-homebrew-catalog-operation/v1"
	ResultSchemaVersion     = "dalec-homebrew-catalog-result/v1"

	HTTPSFetchPolicyVersion         = "homebrew-bottle-fetch-v1"
	BottleVerificationPolicy        = "homebrew-bottle-static-v1"
	VerifiedProvenancePolicy        = "sigstore-in-toto-v1"
	ChecksumProvenanceWaiver        = "tap-catalog-jws-and-verified-checksum-v1"
	HTTPSBottleSourceWaiver         = "https-bottle-embedded-formula-digest-only-v1"
	CatalogSetJWSAlgorithm          = "PS512"
	TapCatalogPolicyVersion         = "tap-catalog-v1"
	CatalogDocumentPathPrefix       = "/v1/catalogs/sha256/"
	MaxTaps                         = 16
	MaxClosureNodes                 = 256
	MaxCatalogDocumentBytes   int64 = 64 << 20
	MaxAggregateCatalogBytes  int64 = 256 << 20
	MaxBottleBytes            int64 = 1 << 30
	MaxRedirects                    = 5
	MaxJSONDepth                    = 128
	MaxFormulaIDBytes               = 39 + 1 + 91 + 1 + 255
	MaxSourcePathBytes              = 1024
	MaxOperationIDBytes             = 128
	MaxFailureMessageBytes          = 1024
	MaxJWSBytes               int64 = 64 << 20
	MaxRequestBytes           int64 = 1 << 20
	MaxCatalogSetBytes        int64 = 64 << 20
	MaxOperationBytes         int64 = 64 << 20
)

const MaxCatalogSetLifetime = 24 * time.Hour
