// Package catalogauth signs and verifies release-bound catalog-set envelopes.
package catalogauth

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
	"unicode/utf8"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

var rawURLEncoding = base64.RawURLEncoding

// VerifiedSet is returned only after the JWS, canonical payload, request
// binding, core-snapshot binding, and validity window have all verified.
type VerifiedSet struct {
	Payload    *catalog.CatalogSetPayload
	PayloadRaw []byte
	Signatures []metadata.SignatureInfo
}

// Sign creates a flattened RFC 7797 JSON JWS using PS512. The payload must be
// canonical JSON; Sign canonicalizes and validates it before signing.
func Sign(payload *catalog.CatalogSetPayload, kid string, key *rsa.PrivateKey) ([]byte, error) {
	if err := catalogkeys.ValidateKeyID(kid); err != nil || !utf8.ValidString(kid) || hasControl(kid) {
		return nil, errors.New("signing key ID is empty or unsafe")
	}
	if err := validateSigningKey(key); err != nil {
		return nil, err
	}
	payloadRaw, err := catalog.CanonicalCatalogSetPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("canonicalize catalog-set payload: %w", err)
	}
	protectedRaw, err := json.Marshal(struct {
		Algorithm string   `json:"alg"`
		B64       bool     `json:"b64"`
		Critical  []string `json:"crit"`
		KeyID     string   `json:"kid"`
	}{Algorithm: "PS512", B64: false, Critical: []string{"b64"}, KeyID: kid})
	if err != nil {
		return nil, err
	}
	protected := rawURLEncoding.EncodeToString(protectedRaw)
	hash := sha512.New()
	_, _ = io.WriteString(hash, protected)
	_, _ = io.WriteString(hash, ".")
	_, _ = hash.Write(payloadRaw)
	signature, err := rsa.SignPSS(rand.Reader, key, crypto.SHA512, hash.Sum(nil), &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA512})
	if err != nil {
		return nil, fmt.Errorf("sign catalog-set payload: %w", err)
	}
	envelope := struct {
		Payload   string `json:"payload"`
		Protected string `json:"protected"`
		Signature string `json:"signature"`
	}{Payload: string(payloadRaw), Protected: protected, Signature: rawURLEncoding.EncodeToString(signature)}
	return json.Marshal(envelope)
}

// Verify authenticates one catalog-set JWS with one required signer and
// enforces the request, core snapshot, and validity-window bindings.
func Verify(envelope []byte, keys metadata.KeySet, requiredKeyID, expectedRequestDigest, expectedCoreSnapshotDigest string, now time.Time, allowUnknownOverlap bool) (*VerifiedSet, error) {
	return VerifyAccepted(envelope, keys, []string{requiredKeyID}, expectedRequestDigest, expectedCoreSnapshotDigest, now, allowUnknownOverlap)
}

// VerifyAccepted authenticates one catalog-set JWS when any one explicitly
// accepted signer verifies. This supports overlap rotation without requiring
// the service to emit multiple signatures: a release first embeds both public
// keys and accepts both IDs, then the service switches to the new signer.
func VerifyAccepted(envelope []byte, keys metadata.KeySet, acceptedKeyIDs []string, expectedRequestDigest, expectedCoreSnapshotDigest string, now time.Time, allowUnknownOverlap bool) (*VerifiedSet, error) {
	for name, value := range map[string]string{"request": expectedRequestDigest, "core snapshot": expectedCoreSnapshotDigest} {
		d, err := digest.Parse(value)
		if err != nil || d.Algorithm() != digest.SHA256 || d.Validate() != nil {
			return nil, fmt.Errorf("expected %s digest is required and must be canonical sha256", name)
		}
	}
	unknown := metadata.RejectUnknownSignatures
	if allowUnknownOverlap {
		unknown = metadata.IgnoreUnknownSignatures
	}
	if len(acceptedKeyIDs) == 0 {
		return nil, errors.New("catalog-set accepted signer IDs are empty")
	}
	seenKeyIDs := make(map[string]struct{}, len(acceptedKeyIDs))
	var (
		verified   metadata.VerifiedJWS
		verifiedOK bool
		verifyErrs []error
	)
	for _, keyID := range acceptedKeyIDs {
		if err := catalogkeys.ValidateKeyID(keyID); err != nil {
			return nil, fmt.Errorf("catalog-set accepted signer: %w", err)
		}
		if _, duplicate := seenKeyIDs[keyID]; duplicate {
			return nil, fmt.Errorf("catalog-set accepted signer %q is duplicated", keyID)
		}
		seenKeyIDs[keyID] = struct{}{}
		candidate, err := metadata.VerifyJWS(envelope, keys, metadata.VerificationPolicy{RequiredKeyID: keyID, UnknownSignatures: unknown})
		if err == nil {
			verified = candidate
			verifiedOK = true
			break
		}
		verifyErrs = append(verifyErrs, fmt.Errorf("kid %q: %w", keyID, err))
	}
	if !verifiedOK {
		return nil, fmt.Errorf("verify catalog-set JWS with accepted signers: %w", errors.Join(verifyErrs...))
	}
	payloadRaw := verified.Payload()
	payload, err := catalog.DecodeCatalogSetPayload(payloadRaw)
	if err != nil {
		return nil, fmt.Errorf("decode authenticated catalog-set payload: %w", err)
	}
	canonical, err := catalog.CanonicalCatalogSetPayload(payload)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(payloadRaw, canonical) {
		return nil, errors.New("authenticated catalog-set payload is not canonical")
	}
	if expectedRequestDigest != "" && payload.RequestDigest != expectedRequestDigest {
		return nil, fmt.Errorf("catalog-set request digest %s does not match %s", payload.RequestDigest, expectedRequestDigest)
	}
	if expectedCoreSnapshotDigest != "" && payload.CoreSnapshotDigest != expectedCoreSnapshotDigest {
		return nil, fmt.Errorf("catalog-set core snapshot digest %s does not match %s", payload.CoreSnapshotDigest, expectedCoreSnapshotDigest)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	if now.Before(payload.GeneratedAt) {
		return nil, fmt.Errorf("catalog-set is not valid before %s", payload.GeneratedAt.UTC().Format(time.RFC3339))
	}
	if !now.Before(payload.ExpiresAt) {
		return nil, fmt.Errorf("catalog-set expired at %s", payload.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return &VerifiedSet{Payload: payload, PayloadRaw: slices.Clone(payloadRaw), Signatures: verified.Signatures()}, nil
}

// LoadSigningKey reads an RSA private key from a root/operator-controlled file.
// Group or world permissions are rejected before parsing.
func LoadSigningKey(filename string) (*rsa.PrivateKey, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("signing key must be a regular non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("signing key permissions %04o expose group or world access", info.Mode().Perm())
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("signing key must contain exactly one PEM block")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			var ok bool
			key, ok = parsed.(*rsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("private key is %T, not RSA", parsed)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported signing key PEM type %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	if err := validateSigningKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

func validateSigningKey(key *rsa.PrivateKey) error {
	if key == nil || key.N == nil || key.N.Sign() <= 0 || key.E < 3 {
		return errors.New("invalid RSA signing key")
	}
	if err := key.Validate(); err != nil {
		return fmt.Errorf("validate RSA private key: %w", err)
	}
	if key.N.BitLen() < 2048 || (key.N.BitLen()-1+7)/8 < 2*64+2 {
		return errors.New("RSA signing key is too small for PS512")
	}
	return nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
