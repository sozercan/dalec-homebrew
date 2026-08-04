package release

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

const V2BindingsSchemaVersion = "dalec-homebrew-v2-bindings/v1"

// V2BindingsInput contains the release identities needed to construct the
// catalog ingestion key policy and the remaining embedded V2 policy bindings.
type V2BindingsInput struct {
	KeyID                  string
	PublicKeyPEM           []byte
	CatalogServiceDigest   string
	CatalogExtractorDigest string
}

// V2Bindings contains values that can be passed directly to the corresponding
// V2 component build arguments. Policy-version sets are canonical, sorted,
// comma-separated strings because the compiled bindings use that encoding.
type V2Bindings struct {
	SchemaVersion                     string `json:"schema_version"`
	IngestionJWSKeyPolicyDigest       string `json:"ingestion_jws_key_policy_digest"`
	IngestionJWSKeyPolicyBase64       string `json:"ingestion_jws_key_policy_base64"`
	TapPolicyDigest                   string `json:"tap_policy_digest"`
	ExecutableRuntimePolicyDigest     string `json:"executable_runtime_policy_digest"`
	SupportedCatalogPolicyVersions    string `json:"supported_catalog_policy_versions"`
	SupportedFetchPolicyVersions      string `json:"supported_fetch_policy_versions"`
	SupportedProvenancePolicyVersions string `json:"supported_provenance_policy_versions"`
}

// GenerateV2Bindings creates the canonical catalog key policy and the complete
// set of release-bound V2 policy values derived from it. The returned policy
// bytes are independent of the input and safe for callers to retain or write.
func GenerateV2Bindings(input V2BindingsInput) (*V2Bindings, []byte, error) {
	if len(input.PublicKeyPEM) == 0 || len(input.PublicKeyPEM) > catalogkeys.MaxPolicyBytes {
		return nil, nil, fmt.Errorf("RSA public-key PEM size %d is outside 1..%d bytes", len(input.PublicKeyPEM), catalogkeys.MaxPolicyBytes)
	}
	keyPolicy := &catalogkeys.Policy{
		SchemaVersion:           catalogkeys.SchemaVersion,
		RequiredKeyID:           input.KeyID,
		Keys:                    []catalogkeys.Key{{ID: input.KeyID, Algorithm: catalog.CatalogSetJWSAlgorithm, PublicPEM: string(input.PublicKeyPEM)}},
		CatalogServiceDigests:   []string{input.CatalogServiceDigest},
		CatalogExtractorDigests: []string{input.CatalogExtractorDigest},
	}
	canonicalKeyPolicy, err := catalogkeys.Canonical(keyPolicy)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize ingestion JWS key policy: %w", err)
	}
	if len(canonicalKeyPolicy) > catalogkeys.MaxPolicyBytes {
		return nil, nil, fmt.Errorf("canonical ingestion JWS key policy exceeds %d bytes", catalogkeys.MaxPolicyBytes)
	}
	keyPolicyDigest, err := catalogkeys.Digest(keyPolicy)
	if err != nil {
		return nil, nil, fmt.Errorf("digest ingestion JWS key policy: %w", err)
	}
	tapPolicyDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		return nil, nil, fmt.Errorf("digest embedded tap policy: %w", err)
	}
	runtimePolicyDigest, err := policyv2.Digest()
	if err != nil {
		return nil, nil, fmt.Errorf("digest embedded executable runtime policy: %w", err)
	}

	bindings := &V2Bindings{
		SchemaVersion:                     V2BindingsSchemaVersion,
		IngestionJWSKeyPolicyDigest:       keyPolicyDigest.String(),
		IngestionJWSKeyPolicyBase64:       base64.StdEncoding.EncodeToString(canonicalKeyPolicy),
		TapPolicyDigest:                   tapPolicyDigest,
		ExecutableRuntimePolicyDigest:     runtimePolicyDigest,
		SupportedCatalogPolicyVersions:    joinPolicyVersions(v2CatalogPolicyVersions()),
		SupportedFetchPolicyVersions:      joinPolicyVersions(v2FetchPolicyVersions()),
		SupportedProvenancePolicyVersions: joinPolicyVersions(v2ProvenancePolicyVersions()),
	}
	if err := ValidateV2Bindings(bindings); err != nil {
		return nil, nil, fmt.Errorf("validate generated V2 bindings: %w", err)
	}
	return bindings, bytes.Clone(canonicalKeyPolicy), nil
}

// ValidateV2Bindings verifies that a binding document contains the exact
// policies implemented by this release and that its embedded key policy is
// canonical and digest-bound.
func ValidateV2Bindings(bindings *V2Bindings) error {
	if bindings == nil {
		return errors.New("nil V2 bindings")
	}
	var errs []error
	if bindings.SchemaVersion != V2BindingsSchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported V2 bindings schema %q", bindings.SchemaVersion))
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "ingestion JWS key policy", value: bindings.IngestionJWSKeyPolicyDigest},
		{name: "tap policy", value: bindings.TapPolicyDigest},
		{name: "executable runtime policy", value: bindings.ExecutableRuntimePolicyDigest},
	} {
		if err := validateDigest(field.value); err != nil {
			errs = append(errs, fmt.Errorf("%s digest: %w", field.name, err))
		}
	}

	if len(bindings.IngestionJWSKeyPolicyBase64) == 0 || len(bindings.IngestionJWSKeyPolicyBase64) > base64.StdEncoding.EncodedLen(catalogkeys.MaxPolicyBytes) {
		errs = append(errs, fmt.Errorf("ingestion JWS key policy base64 size %d is outside the supported range", len(bindings.IngestionJWSKeyPolicyBase64)))
	} else {
		keyPolicyData, err := base64.StdEncoding.Strict().DecodeString(bindings.IngestionJWSKeyPolicyBase64)
		if err != nil {
			errs = append(errs, fmt.Errorf("decode ingestion JWS key policy base64: %w", err))
		} else if base64.StdEncoding.EncodeToString(keyPolicyData) != bindings.IngestionJWSKeyPolicyBase64 {
			errs = append(errs, errors.New("ingestion JWS key policy base64 is not canonical padded standard base64"))
		} else {
			keyPolicy, err := catalogkeys.Decode(keyPolicyData)
			if err != nil {
				errs = append(errs, fmt.Errorf("decode ingestion JWS key policy: %w", err))
			} else {
				digest, err := catalogkeys.Digest(keyPolicy)
				if err != nil {
					errs = append(errs, fmt.Errorf("digest ingestion JWS key policy: %w", err))
				} else if digest.String() != bindings.IngestionJWSKeyPolicyDigest {
					errs = append(errs, fmt.Errorf("ingestion JWS key policy digest %q does not match embedded policy %q", bindings.IngestionJWSKeyPolicyDigest, digest))
				}
			}
		}
	}

	tapPolicyDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		errs = append(errs, fmt.Errorf("digest embedded tap policy: %w", err))
	} else if bindings.TapPolicyDigest != tapPolicyDigest {
		errs = append(errs, fmt.Errorf("tap policy digest %q does not match embedded policy %q", bindings.TapPolicyDigest, tapPolicyDigest))
	}
	runtimePolicyDigest, err := policyv2.Digest()
	if err != nil {
		errs = append(errs, fmt.Errorf("digest embedded executable runtime policy: %w", err))
	} else if bindings.ExecutableRuntimePolicyDigest != runtimePolicyDigest {
		errs = append(errs, fmt.Errorf("executable runtime policy digest %q does not match embedded policy %q", bindings.ExecutableRuntimePolicyDigest, runtimePolicyDigest))
	}

	for _, field := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "catalog", value: bindings.SupportedCatalogPolicyVersions, want: joinPolicyVersions(v2CatalogPolicyVersions())},
		{name: "fetch", value: bindings.SupportedFetchPolicyVersions, want: joinPolicyVersions(v2FetchPolicyVersions())},
		{name: "provenance", value: bindings.SupportedProvenancePolicyVersions, want: joinPolicyVersions(v2ProvenancePolicyVersions())},
	} {
		if field.value != field.want {
			errs = append(errs, fmt.Errorf("supported %s policy versions %q must be exactly %q", field.name, field.value, field.want))
		}
	}
	return errors.Join(errs...)
}

// CanonicalV2Bindings returns deterministic JSON without a trailing newline.
func CanonicalV2Bindings(bindings *V2Bindings) ([]byte, error) {
	if err := ValidateV2Bindings(bindings); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(bindings); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func v2CatalogPolicyVersions() []string {
	return []string{CatalogPolicyVersionV1}
}

func v2FetchPolicyVersions() []string {
	return []string{BottleFetchPolicyVersionV1}
}

func v2ProvenancePolicyVersions() []string {
	return []string{SigstoreProvenancePolicyVersionV1, ChecksumWaiverPolicyVersionV1, HTTPSSourceWaiverPolicyVersionV1, CoreWaiverPolicyVersionV1}
}

func joinPolicyVersions(values []string) string {
	values = slices.Clone(values)
	slices.Sort(values)
	return strings.Join(values, ",")
}
