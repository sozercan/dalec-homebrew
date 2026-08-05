package policyv2

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

const TapPolicySchemaVersion = "dalec-homebrew-tap-policy/v1"

//go:embed tap-policy.json
var embeddedTapPolicy []byte

//go:embed sigstore-trusted-root.json
var embeddedSigstoreTrustedRoot []byte

type TapPolicy struct {
	SchemaVersion             string                  `json:"schema_version"`
	RepositoryTemplate        string                  `json:"repository_template"`
	DefaultBranchOnly         bool                    `json:"default_branch_only"`
	PublicOnly                bool                    `json:"public_only"`
	AuthenticatedEndpoints    bool                    `json:"authenticated_endpoints"`
	UserSuppliedRemotes       bool                    `json:"user_supplied_remotes"`
	MaxNonCoreTaps            int                     `json:"max_non_core_taps"`
	SigstoreTrustedRootDigest string                  `json:"sigstore_trusted_root_digest"`
	SigstoreIssuer            string                  `json:"sigstore_issuer"`
	SigstoreIdentityTemplate  string                  `json:"sigstore_identity_template"`
	PrebuiltArchives          []PrebuiltArchivePolicy `json:"prebuilt_archives"`
}

func LoadTapPolicy() (*TapPolicy, error) {
	if err := validateUniqueJSON(embeddedTapPolicy); err != nil {
		return nil, fmt.Errorf("embedded tap policy: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(embeddedTapPolicy))
	dec.DisallowUnknownFields()
	var policy TapPolicy
	if err := dec.Decode(&policy); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple tap policy JSON values")
		}
		return nil, err
	}
	if err := ValidateTapPolicy(&policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

func ValidateTapPolicy(policy *TapPolicy) error {
	if policy == nil {
		return errors.New("nil tap policy")
	}
	var errs []error
	digest := sha256.Sum256(embeddedSigstoreTrustedRoot)
	wantRootDigest := "sha256:" + hex.EncodeToString(digest[:])
	if policy.SchemaVersion != TapPolicySchemaVersion || policy.RepositoryTemplate != "https://github.com/<owner>/homebrew-<tap>" || !policy.DefaultBranchOnly || !policy.PublicOnly || policy.AuthenticatedEndpoints || policy.UserSuppliedRemotes || policy.MaxNonCoreTaps != DefaultMaxNonCoreTaps || policy.SigstoreTrustedRootDigest != wantRootDigest || policy.SigstoreIssuer != "https://token.actions.githubusercontent.com" || policy.SigstoreIdentityTemplate != "^https://github.com/<owner>/homebrew-<tap>/" {
		errs = append(errs, errors.New("tap policy differs from the V2 public default-GitHub-tap contract"))
	}
	if err := validatePrebuiltArchivePolicies(policy.PrebuiltArchives); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func SigstoreTrustedRoot() ([]byte, error) {
	policy, err := LoadTapPolicy()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(embeddedSigstoreTrustedRoot)
	if policy.SigstoreTrustedRootDigest != "sha256:"+hex.EncodeToString(digest[:]) {
		return nil, errors.New("embedded Sigstore trusted root digest mismatch")
	}
	return slices.Clone(embeddedSigstoreTrustedRoot), nil
}

func CanonicalTapPolicy() ([]byte, error) {
	policy, err := LoadTapPolicy()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(policy); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func TapPolicyDigest() (string, error) {
	data, err := CanonicalTapPolicy()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
