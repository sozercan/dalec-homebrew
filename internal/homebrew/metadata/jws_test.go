package metadata

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
)

func TestVerifyJWSGeneralAndFlattened(t *testing.T) {
	one, _ := generatedTestKeys(t)
	keys := testKeySet(t)
	payload := `[{"name":"hello"}]`

	for _, tc := range []struct {
		name string
		jws  func() []byte
	}{
		{name: "general", jws: func() []byte {
			return makeGeneralJWS(t, payload, validTestSignature(t, DefaultRequiredKeyID, one))
		}},
		{name: "flattened", jws: func() []byte {
			return makeFlattenedJWS(t, payload, validTestSignature(t, DefaultRequiredKeyID, one))
		}},
		{name: "general with padded base64url", jws: func() []byte {
			signature := makeSignatureWithEncoding(t, payload, validTestSignature(t, DefaultRequiredKeyID, one), base64.URLEncoding)
			return mustJSON(t, map[string]any{"payload": payload, "signatures": []any{signature}})
		}},
		{name: "protected kid without unprotected header", jws: func() []byte {
			config := validTestSignature(t, DefaultRequiredKeyID, one)
			config.protected["kid"] = DefaultRequiredKeyID
			signature := makeSignature(t, payload, config)
			delete(signature, "header")
			return mustJSON(t, map[string]any{"payload": payload, "signatures": []any{signature}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.jws()
			verified, err := VerifyJWS(data, keys, VerificationPolicy{})
			if err != nil {
				t.Fatalf("VerifyJWS: %v", err)
			}
			if got := string(verified.Payload()); got != payload {
				t.Fatalf("payload = %q, want %q", got, payload)
			}
			if verified.PayloadDigest() != digestBytes([]byte(payload)) {
				t.Fatalf("payload digest = %q", verified.PayloadDigest())
			}
			if verified.EnvelopeDigest() != digestBytes(data) {
				t.Fatalf("envelope digest = %q", verified.EnvelopeDigest())
			}
			signatures := verified.Signatures()
			if len(signatures) != 1 || signatures[0].KeyID != DefaultRequiredKeyID || signatures[0].Algorithm != JWSAlgorithmPS512 || !signatures[0].Verified {
				t.Fatalf("unexpected signatures: %#v", signatures)
			}

			// Accessors must not expose mutable backing storage.
			payloadCopy := verified.Payload()
			payloadCopy[0] = 'x'
			if string(verified.Payload()) != payload {
				t.Fatal("payload accessor mutated verified document")
			}
			signatures[0].KeyID = "mutated"
			if verified.Signatures()[0].KeyID != DefaultRequiredKeyID {
				t.Fatal("signature accessor mutated verified document")
			}
		})
	}
}

func TestVerifyJWSHeaderPolicy(t *testing.T) {
	one, _ := generatedTestKeys(t)
	keys := testKeySet(t)
	payload := `{}`

	tests := []struct {
		name      string
		configure func(*testJWSSignature)
	}{
		{name: "wrong algorithm", configure: func(s *testJWSSignature) { s.protected["alg"] = "RS512" }},
		{name: "missing algorithm", configure: func(s *testJWSSignature) { delete(s.protected, "alg") }},
		{name: "b64 true", configure: func(s *testJWSSignature) { s.protected["b64"] = true }},
		{name: "b64 null", configure: func(s *testJWSSignature) { s.protected["b64"] = nil }},
		{name: "missing b64", configure: func(s *testJWSSignature) { delete(s.protected, "b64") }},
		{name: "missing crit", configure: func(s *testJWSSignature) { delete(s.protected, "crit") }},
		{name: "empty crit", configure: func(s *testJWSSignature) { s.protected["crit"] = []string{} }},
		{name: "crit omits b64", configure: func(s *testJWSSignature) { s.protected["crit"] = []string{"other"}; s.protected["other"] = true }},
		{name: "unknown critical header", configure: func(s *testJWSSignature) { s.protected["crit"] = []string{"b64", "other"}; s.protected["other"] = true }},
		{name: "duplicate critical header", configure: func(s *testJWSSignature) { s.protected["crit"] = []string{"b64", "b64"} }},
		{name: "header collision", configure: func(s *testJWSSignature) { s.unprotected = map[string]any{"alg": JWSAlgorithmPS512} }},
		{name: "empty kid", configure: func(s *testJWSSignature) { s.kid = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			signature := validTestSignature(t, DefaultRequiredKeyID, one)
			tc.configure(&signature)
			_, err := VerifyJWS(makeGeneralJWS(t, payload, signature), keys, VerificationPolicy{})
			if !errors.Is(err, ErrInvalidJWS) {
				t.Fatalf("error = %v, want ErrInvalidJWS", err)
			}
		})
	}
}

func TestVerifyJWSRejectsTamperingAndMalformedSerialization(t *testing.T) {
	one, _ := generatedTestKeys(t)
	keys := testKeySet(t)
	good := makeGeneralJWS(t, `{"safe":true}`, validTestSignature(t, DefaultRequiredKeyID, one))

	t.Run("tampered payload", func(t *testing.T) {
		var envelope map[string]any
		if err := json.Unmarshal(good, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["payload"] = `{"safe":false}`
		_, err := VerifyJWS(mustJSON(t, envelope), keys, VerificationPolicy{})
		if !errors.Is(err, ErrSignatureMismatch) {
			t.Fatalf("error = %v, want ErrSignatureMismatch", err)
		}
	})

	t.Run("duplicate envelope member", func(t *testing.T) {
		duplicate := append([]byte(`{"payload":"duplicate",`), good[1:]...)
		_, err := VerifyJWS(duplicate, keys, VerificationPolicy{})
		if !errors.Is(err, ErrInvalidJWS) || !strings.Contains(err.Error(), "duplicate JSON member") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("mixed flattened and general", func(t *testing.T) {
		var envelope map[string]any
		if err := json.Unmarshal(good, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["signature"] = "AA"
		_, err := VerifyJWS(mustJSON(t, envelope), keys, VerificationPolicy{})
		if !errors.Is(err, ErrInvalidJWS) {
			t.Fatalf("error = %v, want ErrInvalidJWS", err)
		}
	})

	t.Run("invalid protected base64url", func(t *testing.T) {
		var envelope map[string]any
		if err := json.Unmarshal(good, &envelope); err != nil {
			t.Fatal(err)
		}
		signatures := envelope["signatures"].([]any)
		signatures[0].(map[string]any)["protected"] = "not+base64"
		_, err := VerifyJWS(mustJSON(t, envelope), keys, VerificationPolicy{})
		if !errors.Is(err, ErrInvalidJWS) {
			t.Fatalf("error = %v, want ErrInvalidJWS", err)
		}
	})

	t.Run("unsupported member", func(t *testing.T) {
		var envelope map[string]any
		if err := json.Unmarshal(good, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["unexpected"] = true
		_, err := VerifyJWS(mustJSON(t, envelope), keys, VerificationPolicy{})
		if !errors.Is(err, ErrInvalidJWS) {
			t.Fatalf("error = %v, want ErrInvalidJWS", err)
		}
	})

	t.Run("duplicate protected header member", func(t *testing.T) {
		protectedJSON := []byte(`{"alg":"PS512","alg":"PS512","b64":false,"crit":["b64"]}`)
		protected := base64.RawURLEncoding.EncodeToString(protectedJSON)
		hash := sha512.Sum512([]byte(protected + ".{}"))
		signed, signErr := rsa.SignPSS(rand.Reader, one, crypto.SHA512, hash[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA512})
		if signErr != nil {
			t.Fatal(signErr)
		}
		envelope := mustJSON(t, map[string]any{
			"payload": `{}`,
			"signatures": []any{map[string]any{
				"protected": protected,
				"header":    map[string]any{"kid": DefaultRequiredKeyID},
				"signature": base64.RawURLEncoding.EncodeToString(signed),
			}},
		})
		_, err := VerifyJWS(envelope, keys, VerificationPolicy{})
		if !errors.Is(err, ErrInvalidJWS) || !strings.Contains(err.Error(), "duplicate JSON member") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestVerifyJWSKeySelectionAndRollover(t *testing.T) {
	one, two := generatedTestKeys(t)
	keys := testKeySet(t)
	payload := `{}`

	t.Run("required key defaults to homebrew-1", func(t *testing.T) {
		_, err := VerifyJWS(makeGeneralJWS(t, payload, validTestSignature(t, "homebrew-2", two)), keys, VerificationPolicy{})
		if !errors.Is(err, ErrRequiredSignatureMissing) {
			t.Fatalf("error = %v, want ErrRequiredSignatureMissing", err)
		}
	})

	t.Run("configurable required key", func(t *testing.T) {
		_, err := VerifyJWS(makeGeneralJWS(t, payload, validTestSignature(t, "homebrew-2", two)), keys, VerificationPolicy{RequiredKeyID: "homebrew-2"})
		if err != nil {
			t.Fatalf("VerifyJWS: %v", err)
		}
	})

	t.Run("unknown signer rejected by default", func(t *testing.T) {
		unknown := validTestSignature(t, "future-key", two)
		_, err := VerifyJWS(makeGeneralJWS(t, payload,
			validTestSignature(t, DefaultRequiredKeyID, one), unknown), keys, VerificationPolicy{})
		if !errors.Is(err, ErrUnknownSigner) {
			t.Fatalf("error = %v, want ErrUnknownSigner", err)
		}
	})

	t.Run("unknown rollover signature can be ignored explicitly", func(t *testing.T) {
		unknown := validTestSignature(t, "future-key", two)
		verified, err := VerifyJWS(makeGeneralJWS(t, payload,
			unknown, validTestSignature(t, DefaultRequiredKeyID, one)), keys, VerificationPolicy{UnknownSignatures: IgnoreUnknownSignatures})
		if err != nil {
			t.Fatalf("VerifyJWS: %v", err)
		}
		if got := verified.Signatures(); len(got) != 1 || got[0].KeyID != DefaultRequiredKeyID {
			t.Fatalf("verified signatures = %#v", got)
		}
	})

	t.Run("duplicate kid rejected", func(t *testing.T) {
		_, err := VerifyJWS(makeGeneralJWS(t, payload,
			validTestSignature(t, DefaultRequiredKeyID, one),
			validTestSignature(t, DefaultRequiredKeyID, one)), keys, VerificationPolicy{})
		if !errors.Is(err, ErrInvalidJWS) {
			t.Fatalf("error = %v, want ErrInvalidJWS", err)
		}
	})
}

func TestKeySetClonesKeysAndDefaultKeyParses(t *testing.T) {
	one, _ := generatedTestKeys(t)
	originalN := &rsa.PublicKey{N: new(big.Int).Set(one.N), E: one.E}
	keys, err := NewKeySet(map[string]*rsa.PublicKey{"test": originalN})
	if err != nil {
		t.Fatal(err)
	}
	originalN.N.SetInt64(3)
	key, ok := keys.get("test")
	if !ok || key.N.BitLen() < 1000 {
		t.Fatal("KeySet did not clone RSA modulus")
	}

	defaults, err := DefaultKeySet()
	if err != nil {
		t.Fatalf("DefaultKeySet: %v", err)
	}
	ids := defaults.IDs()
	if len(ids) != 1 || ids[0] != DefaultRequiredKeyID {
		t.Fatalf("default key IDs = %v", ids)
	}
}
