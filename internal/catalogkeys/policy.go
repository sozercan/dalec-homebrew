// Package catalogkeys defines the release-pinned catalog ingestion JWS key and
// component-authorization policy.
package catalogkeys

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

const (
	SchemaVersion  = "dalec-homebrew-catalog-key-policy/v1"
	MaxPolicyBytes = 1 << 20
)

type Policy struct {
	SchemaVersion              string          `json:"schema_version"`
	RequiredKeyID              string          `json:"required_key_id"`
	OverlapKeyIDs              []string        `json:"overlap_key_ids,omitempty"`
	Keys                       []Key           `json:"keys"`
	CatalogServiceDigests      []string        `json:"catalog_service_digests"`
	CatalogExtractorDigests    []string        `json:"catalog_extractor_digests"`
	AllowUnknownOverlapSigners bool            `json:"allow_unknown_overlap_signers,omitempty"`
	SequenceFloors             []SequenceFloor `json:"sequence_floors,omitempty"`
}

type SequenceFloor struct {
	Tap     catalog.TapID `json:"tap"`
	Minimum uint64        `json:"minimum"`
}

type Key struct {
	ID        string `json:"id"`
	Algorithm string `json:"algorithm"`
	PublicPEM string `json:"public_pem"`
}

func Decode(data []byte) (*Policy, error) {
	if len(data) == 0 || len(data) > MaxPolicyBytes {
		return nil, fmt.Errorf("catalog key policy size %d is outside 1..%d", len(data), MaxPolicyBytes)
	}
	if err := validateUniqueJSON(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var policy Policy
	if err := dec.Decode(&policy); err != nil {
		return nil, err
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	canonicalize(&policy)
	if err := Validate(&policy); err != nil {
		return nil, err
	}
	canonical, err := Canonical(&policy)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(bytes.TrimSpace(data), canonical) {
		return nil, errors.New("catalog key policy JSON is not canonical")
	}
	return &policy, nil
}

func Validate(policy *Policy) error {
	if policy == nil {
		return errors.New("nil catalog key policy")
	}
	var errs []error
	if policy.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q", policy.SchemaVersion))
	}
	if !safeToken(policy.RequiredKeyID) {
		errs = append(errs, errors.New("required_key_id is empty or unsafe"))
	}
	if len(policy.Keys) == 0 {
		errs = append(errs, errors.New("keys must be non-empty"))
	}
	seen := map[string]struct{}{}
	seenKeyMaterial := map[string]string{}
	required := false
	keyPEM := map[string][]byte{}
	for i, key := range policy.Keys {
		if !safeToken(key.ID) {
			errs = append(errs, fmt.Errorf("keys[%d].id is empty or unsafe", i))
		}
		if key.Algorithm != catalog.CatalogSetJWSAlgorithm {
			errs = append(errs, fmt.Errorf("keys[%d] algorithm %q is not PS512", i, key.Algorithm))
		}
		if _, duplicate := seen[key.ID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate key ID %q", key.ID))
		}
		seen[key.ID] = struct{}{}
		if key.ID == policy.RequiredKeyID {
			required = true
		}
		publicKey, parseErr := metadata.ParseRSAPublicKeyPEM([]byte(key.PublicPEM))
		if parseErr != nil {
			errs = append(errs, fmt.Errorf("keys[%d]: %w", i, parseErr))
		} else if publicKey.N.BitLen() < 2048 || (publicKey.N.BitLen()-1+7)/8 < 2*64+2 {
			errs = append(errs, fmt.Errorf("keys[%d] RSA modulus is too small for PS512", i))
		} else if der, marshalErr := x509.MarshalPKIXPublicKey(publicKey); marshalErr != nil {
			errs = append(errs, fmt.Errorf("keys[%d]: %w", i, marshalErr))
		} else {
			sum := sha256.Sum256(der)
			fingerprint := hex.EncodeToString(sum[:])
			if prior, duplicate := seenKeyMaterial[fingerprint]; duplicate {
				errs = append(errs, fmt.Errorf("keys %q and %q contain identical public key material", prior, key.ID))
			} else {
				seenKeyMaterial[fingerprint] = key.ID
			}
		}
		keyPEM[key.ID] = []byte(key.PublicPEM)
	}
	if !required {
		errs = append(errs, fmt.Errorf("required key %q is absent", policy.RequiredKeyID))
	}
	previousOverlap := ""
	for i, keyID := range policy.OverlapKeyIDs {
		if !safeToken(keyID) {
			errs = append(errs, fmt.Errorf("overlap_key_ids[%d] is empty or unsafe", i))
		}
		if keyID == policy.RequiredKeyID {
			errs = append(errs, fmt.Errorf("overlap key %q duplicates required_key_id", keyID))
		}
		if _, ok := seen[keyID]; !ok {
			errs = append(errs, fmt.Errorf("overlap key %q is absent from keys", keyID))
		}
		if i > 0 && keyID <= previousOverlap {
			errs = append(errs, errors.New("overlap_key_ids must be sorted and unique"))
		}
		previousOverlap = keyID
	}
	if _, err := metadata.NewKeySetFromPEM(keyPEM); err != nil {
		errs = append(errs, err)
	}
	if len(policy.CatalogServiceDigests) == 0 || len(policy.CatalogExtractorDigests) == 0 {
		errs = append(errs, errors.New("catalog service and extractor authorization sets must be non-empty"))
	}
	for label, values := range map[string][]string{"catalog_service_digests": policy.CatalogServiceDigests, "catalog_extractor_digests": policy.CatalogExtractorDigests} {
		previous := ""
		for i, value := range values {
			d, err := digest.Parse(value)
			if err != nil || d.Algorithm() != digest.SHA256 || d.Validate() != nil {
				errs = append(errs, fmt.Errorf("%s contains invalid sha256 digest %q", label, value))
			}
			if i > 0 && value <= previous {
				errs = append(errs, fmt.Errorf("%s must be sorted and unique", label))
			}
			previous = value
		}
	}
	previousTap := catalog.TapID("")
	for i, floor := range policy.SequenceFloors {
		if err := floor.Tap.Validate(); err != nil || floor.Tap.IsCore() || floor.Minimum == 0 {
			errs = append(errs, fmt.Errorf("sequence_floors[%d] is invalid", i))
		}
		if i > 0 && floor.Tap <= previousTap {
			errs = append(errs, errors.New("sequence_floors must be sorted and unique by tap"))
		}
		previousTap = floor.Tap
	}
	return errors.Join(errs...)
}

func Canonical(policy *Policy) ([]byte, error) {
	if policy == nil {
		return nil, errors.New("nil catalog key policy")
	}
	clone := *policy
	clone.Keys = slices.Clone(policy.Keys)
	clone.OverlapKeyIDs = slices.Clone(policy.OverlapKeyIDs)
	clone.CatalogServiceDigests = slices.Clone(policy.CatalogServiceDigests)
	clone.CatalogExtractorDigests = slices.Clone(policy.CatalogExtractorDigests)
	clone.SequenceFloors = slices.Clone(policy.SequenceFloors)
	canonicalize(&clone)
	if err := Validate(&clone); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(clone); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func Digest(policy *Policy) (digest.Digest, error) {
	data, err := Canonical(policy)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

func (policy *Policy) KeySet() (metadata.KeySet, error) {
	if err := Validate(policy); err != nil {
		return metadata.KeySet{}, err
	}
	values := make(map[string][]byte, len(policy.Keys))
	for _, key := range policy.Keys {
		values[key.ID] = []byte(key.PublicPEM)
	}
	return metadata.NewKeySetFromPEM(values)
}

// AcceptedKeyIDs returns the primary signer plus any explicitly authorized
// overlap signers. An overlap release embeds both old and new public keys and
// lists the not-yet-primary key here before the service switches signers.
func (policy *Policy) AcceptedKeyIDs() []string {
	if policy == nil {
		return nil
	}
	result := make([]string, 0, 1+len(policy.OverlapKeyIDs))
	result = append(result, policy.RequiredKeyID)
	result = append(result, policy.OverlapKeyIDs...)
	return result
}

// AuthorizePayload verifies that the authenticated component identity claims
// are explicitly permitted by the release-pinned key policy.
func (policy *Policy) AuthorizePayload(payload *catalog.CatalogSetPayload) error {
	if err := Validate(policy); err != nil {
		return err
	}
	if payload == nil {
		return errors.New("nil catalog-set payload")
	}
	if !slices.Contains(policy.CatalogServiceDigests, payload.CatalogService.Digest) {
		return fmt.Errorf("catalog service digest %s is not authorized", payload.CatalogService.Digest)
	}
	if !slices.Contains(policy.CatalogExtractorDigests, payload.Extractor.Digest) {
		return fmt.Errorf("catalog extractor digest %s is not authorized", payload.Extractor.Digest)
	}
	return nil
}

func (policy *Policy) MinimumSequences() map[catalog.TapID]uint64 {
	if policy == nil {
		return map[catalog.TapID]uint64{}
	}
	result := make(map[catalog.TapID]uint64, len(policy.SequenceFloors))
	for _, floor := range policy.SequenceFloors {
		result[floor.Tap] = floor.Minimum
	}
	return result
}

func canonicalize(policy *Policy) {
	slices.SortFunc(policy.Keys, func(a, b Key) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(policy.OverlapKeyIDs)
	slices.Sort(policy.CatalogServiceDigests)
	slices.Sort(policy.CatalogExtractorDigests)
	slices.SortFunc(policy.SequenceFloors, func(a, b SequenceFloor) int { return strings.Compare(string(a.Tap), string(b.Tap)) })
}

func safeToken(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func ensureEOF(dec *json.Decoder) error {
	var value any
	if err := dec.Decode(&value); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateUniqueJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if err := walk(dec, token); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func walk(dec *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walk(dec, value); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	case '[':
		for dec.More() {
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walk(dec, value); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

// ValidateKeyID enforces the key-role grammar shared by policies, signers, and
// catalog-service configuration.
func ValidateKeyID(value string) error {
	if !safeToken(value) {
		return fmt.Errorf("key ID %q is empty or unsafe", value)
	}
	return nil
}
