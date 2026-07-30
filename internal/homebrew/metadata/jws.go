package metadata

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// UnknownSignaturePolicy controls signatures whose kid is absent from the
// configured key set.
type UnknownSignaturePolicy uint8

const (
	// RejectUnknownSignatures is the fail-closed default.
	RejectUnknownSignatures UnknownSignaturePolicy = iota
	// IgnoreUnknownSignatures permits a rollover envelope to carry additional
	// signatures, while the required configured signer must still verify.
	IgnoreUnknownSignatures
)

// VerificationPolicy selects the required signer. An empty RequiredKeyID uses
// homebrew-1.
type VerificationPolicy struct {
	RequiredKeyID     string
	UnknownSignatures UnknownSignaturePolicy
}

func (p VerificationPolicy) normalized() (VerificationPolicy, error) {
	if p.RequiredKeyID == "" {
		p.RequiredKeyID = DefaultRequiredKeyID
	}
	if strings.TrimSpace(p.RequiredKeyID) == "" {
		return VerificationPolicy{}, fmt.Errorf("required JWS kid is empty")
	}
	if p.UnknownSignatures != RejectUnknownSignatures && p.UnknownSignatures != IgnoreUnknownSignatures {
		return VerificationPolicy{}, fmt.Errorf("invalid unknown-signature policy %d", p.UnknownSignatures)
	}
	return p, nil
}

// VerifiedJWS contains caller-owned accessors for one authenticated JWS.
type VerifiedJWS struct {
	payload        []byte
	envelopeDigest string
	payloadDigest  string
	signatures     []SignatureInfo
}

func (v VerifiedJWS) Payload() []byte             { return slices.Clone(v.payload) }
func (v VerifiedJWS) EnvelopeDigest() string      { return v.envelopeDigest }
func (v VerifiedJWS) PayloadDigest() string       { return v.payloadDigest }
func (v VerifiedJWS) Signatures() []SignatureInfo { return cloneSignatureInfo(v.signatures) }

// VerifyJWS verifies flattened or general JSON JWS serialization using an
// unencoded payload (RFC 7797) and RSA-PSS SHA-512 (PS512).
func VerifyJWS(data []byte, keys KeySet, policy VerificationPolicy) (VerifiedJWS, error) {
	if keys.empty() {
		var err error
		keys, err = DefaultKeySet()
		if err != nil {
			return VerifiedJWS{}, err
		}
	}
	policy, err := policy.normalized()
	if err != nil {
		return VerifiedJWS{}, err
	}

	envelope, err := decodeJSONObject(data)
	if err != nil {
		return VerifiedJWS{}, fmt.Errorf("%w: envelope: %v", ErrInvalidJWS, err)
	}
	allowed := map[string]bool{
		"payload": true, "signatures": true,
		"protected": true, "header": true, "signature": true,
	}
	for field := range envelope {
		if !allowed[field] {
			return VerifiedJWS{}, fmt.Errorf("%w: unsupported envelope member %q", ErrInvalidJWS, field)
		}
	}

	payloadString, err := decodeString(envelope["payload"], "payload")
	if err != nil {
		return VerifiedJWS{}, fmt.Errorf("%w: %v", ErrInvalidJWS, err)
	}
	payload := []byte(payloadString)

	hasGeneral := len(envelope["signatures"]) != 0
	hasFlattened := len(envelope["protected"]) != 0 || len(envelope["header"]) != 0 || len(envelope["signature"]) != 0
	if hasGeneral == hasFlattened {
		return VerifiedJWS{}, fmt.Errorf("%w: envelope must use exactly one of flattened or general serialization", ErrInvalidJWS)
	}

	var signatures []json.RawMessage
	if hasGeneral {
		if err := json.Unmarshal(envelope["signatures"], &signatures); err != nil {
			return VerifiedJWS{}, fmt.Errorf("%w: signatures must be an array: %v", ErrInvalidJWS, err)
		}
		if len(signatures) == 0 {
			return VerifiedJWS{}, fmt.Errorf("%w: signatures array is empty", ErrInvalidJWS)
		}
	} else {
		flattened := map[string]json.RawMessage{}
		for _, field := range []string{"protected", "header", "signature"} {
			if raw := envelope[field]; len(raw) != 0 {
				flattened[field] = raw
			}
		}
		raw, marshalErr := json.Marshal(flattened)
		if marshalErr != nil {
			return VerifiedJWS{}, marshalErr
		}
		signatures = []json.RawMessage{raw}
	}

	seenKids := map[string]struct{}{}
	verified := make([]SignatureInfo, 0, len(signatures))
	requiredVerified := false
	for i, raw := range signatures {
		signature, err := parseJWSSignature(raw)
		if err != nil {
			return VerifiedJWS{}, fmt.Errorf("%w: signature %d: %v", ErrInvalidJWS, i, err)
		}
		if _, duplicate := seenKids[signature.kid]; duplicate {
			return VerifiedJWS{}, fmt.Errorf("%w: duplicate signature kid %q", ErrInvalidJWS, signature.kid)
		}
		seenKids[signature.kid] = struct{}{}

		key, known := keys.get(signature.kid)
		if !known {
			if policy.UnknownSignatures == IgnoreUnknownSignatures {
				continue
			}
			return VerifiedJWS{}, fmt.Errorf("%w %q", ErrUnknownSigner, signature.kid)
		}
		if err := verifyPS512(key, signature.protected, payload, signature.signature); err != nil {
			return VerifiedJWS{}, fmt.Errorf("signature kid %q: %w", signature.kid, err)
		}
		info := SignatureInfo{
			KeyID:           signature.kid,
			Algorithm:       JWSAlgorithmPS512,
			Verified:        true,
			ProtectedDigest: digestBytes(signature.protectedJSON),
			SignatureDigest: digestBytes(signature.signature),
		}
		verified = append(verified, info)
		if signature.kid == policy.RequiredKeyID {
			requiredVerified = true
		}
	}
	if !requiredVerified {
		return VerifiedJWS{}, fmt.Errorf("%w for kid %q", ErrRequiredSignatureMissing, policy.RequiredKeyID)
	}
	slices.SortFunc(verified, func(a, b SignatureInfo) int {
		if c := strings.Compare(a.KeyID, b.KeyID); c != 0 {
			return c
		}
		return strings.Compare(a.SignatureDigest, b.SignatureDigest)
	})

	return VerifiedJWS{
		payload:        slices.Clone(payload),
		envelopeDigest: digestBytes(data),
		payloadDigest:  digestBytes(payload),
		signatures:     verified,
	}, nil
}

type parsedJWSSignature struct {
	kid           string
	protected     string
	protectedJSON []byte
	signature     []byte
}

func parseJWSSignature(raw []byte) (parsedJWSSignature, error) {
	object, err := decodeJSONObject(raw)
	if err != nil {
		return parsedJWSSignature{}, err
	}
	for field := range object {
		if field != "protected" && field != "header" && field != "signature" {
			return parsedJWSSignature{}, fmt.Errorf("unsupported signature member %q", field)
		}
	}
	protectedEncoded, err := decodeString(object["protected"], "protected")
	if err != nil || protectedEncoded == "" {
		if err == nil {
			err = fmt.Errorf("protected header is empty")
		}
		return parsedJWSSignature{}, err
	}
	protectedJSON, err := decodeBase64URL(protectedEncoded)
	if err != nil {
		return parsedJWSSignature{}, fmt.Errorf("decode protected header: %w", err)
	}
	protected, err := decodeJSONObject(protectedJSON)
	if err != nil {
		return parsedJWSSignature{}, fmt.Errorf("protected header: %w", err)
	}

	var unprotected map[string]json.RawMessage
	if rawHeader := object["header"]; len(rawHeader) != 0 {
		unprotected, err = decodeJSONObject(rawHeader)
		if err != nil {
			return parsedJWSSignature{}, fmt.Errorf("unprotected header: %w", err)
		}
	} else {
		unprotected = map[string]json.RawMessage{}
	}
	for name := range protected {
		if _, duplicate := unprotected[name]; duplicate {
			return parsedJWSSignature{}, fmt.Errorf("header parameter %q appears in protected and unprotected headers", name)
		}
	}

	algorithm, err := decodeString(protected["alg"], "protected alg")
	if err != nil || algorithm != JWSAlgorithmPS512 {
		if err != nil {
			return parsedJWSSignature{}, err
		}
		return parsedJWSSignature{}, fmt.Errorf("protected alg must be %q, got %q", JWSAlgorithmPS512, algorithm)
	}
	var encodedPayload *bool
	if rawB64 := protected["b64"]; len(rawB64) == 0 {
		return parsedJWSSignature{}, fmt.Errorf("protected b64 is required")
	} else if err := json.Unmarshal(rawB64, &encodedPayload); err != nil || encodedPayload == nil {
		if err == nil {
			err = fmt.Errorf("value is null")
		}
		return parsedJWSSignature{}, fmt.Errorf("protected b64 must be a boolean: %w", err)
	}
	if *encodedPayload {
		return parsedJWSSignature{}, fmt.Errorf("protected b64 must be false")
	}

	var critical []string
	if rawCritical := protected["crit"]; len(rawCritical) == 0 {
		return parsedJWSSignature{}, fmt.Errorf("protected crit is required")
	} else if err := json.Unmarshal(rawCritical, &critical); err != nil {
		return parsedJWSSignature{}, fmt.Errorf("protected crit must be a string array: %w", err)
	}
	if len(critical) == 0 {
		return parsedJWSSignature{}, fmt.Errorf("protected crit is empty")
	}
	seenCritical := map[string]struct{}{}
	containsB64 := false
	for _, name := range critical {
		if name == "" {
			return parsedJWSSignature{}, fmt.Errorf("protected crit contains an empty name")
		}
		if _, duplicate := seenCritical[name]; duplicate {
			return parsedJWSSignature{}, fmt.Errorf("protected crit contains duplicate %q", name)
		}
		seenCritical[name] = struct{}{}
		if _, present := protected[name]; !present {
			return parsedJWSSignature{}, fmt.Errorf("critical header %q is not protected", name)
		}
		if name != "b64" {
			return parsedJWSSignature{}, fmt.Errorf("unsupported critical header %q", name)
		}
		containsB64 = true
	}
	if !containsB64 {
		return parsedJWSSignature{}, fmt.Errorf("protected crit must contain b64")
	}

	kidRaw, inProtected := protected["kid"]
	if !inProtected {
		kidRaw = unprotected["kid"]
	}
	kid, err := decodeString(kidRaw, "kid")
	if err != nil || strings.TrimSpace(kid) == "" {
		if err == nil {
			err = fmt.Errorf("kid is empty")
		}
		return parsedJWSSignature{}, err
	}

	signatureEncoded, err := decodeString(object["signature"], "signature")
	if err != nil || signatureEncoded == "" {
		if err == nil {
			err = fmt.Errorf("signature is empty")
		}
		return parsedJWSSignature{}, err
	}
	signature, err := decodeBase64URL(signatureEncoded)
	if err != nil {
		return parsedJWSSignature{}, fmt.Errorf("decode signature: %w", err)
	}
	return parsedJWSSignature{
		kid:           kid,
		protected:     protectedEncoded,
		protectedJSON: slices.Clone(protectedJSON),
		signature:     slices.Clone(signature),
	}, nil
}

func verifyPS512(key *rsa.PublicKey, protected string, payload, signature []byte) error {
	hash := sha512.New()
	_, _ = hash.Write([]byte(protected))
	_, _ = hash.Write([]byte{'.'})
	_, _ = hash.Write(payload)
	digest := hash.Sum(nil)
	if err := rsa.VerifyPSS(key, crypto.SHA512, digest, signature, &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA512,
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrSignatureMismatch, err)
	}
	return nil
}

func decodeBase64URL(value string) ([]byte, error) {
	if strings.ContainsAny(value, "+/\r\n\t ") {
		return nil, fmt.Errorf("not strict base64url")
	}
	if strings.Contains(value, "=") {
		return base64.URLEncoding.Strict().DecodeString(value)
	}
	return base64.RawURLEncoding.Strict().DecodeString(value)
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func snapshotDigest(formulaDigest, migrationsDigest string) string {
	hash := sha256.New()
	writeDigestFrame(hash, SnapshotSchema)
	writeDigestFrame(hash, FormulaEndpoint)
	writeDigestFrame(hash, formulaDigest)
	writeDigestFrame(hash, MigrationsEndpoint)
	writeDigestFrame(hash, migrationsDigest)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func writeDigestFrame(hash interface{ Write([]byte) (int, error) }, value string) {
	length := fmt.Sprintf("%d:", len(value))
	_, _ = hash.Write([]byte(length))
	_, _ = hash.Write([]byte(value))
	_, _ = hash.Write([]byte{0})
}
