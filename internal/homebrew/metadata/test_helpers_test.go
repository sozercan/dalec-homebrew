package metadata

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

var (
	testKeysOnce sync.Once
	testKeyOne   *rsa.PrivateKey
	testKeyTwo   *rsa.PrivateKey
	testKeysErr  error
)

func generatedTestKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	testKeysOnce.Do(func() {
		testKeyOne, testKeysErr = rsa.GenerateKey(rand.Reader, 2048)
		if testKeysErr == nil {
			testKeyTwo, testKeysErr = rsa.GenerateKey(rand.Reader, 2048)
		}
	})
	if testKeysErr != nil {
		t.Fatalf("generate RSA test keys: %v", testKeysErr)
	}
	return testKeyOne, testKeyTwo
}

func testKeySet(t *testing.T) KeySet {
	t.Helper()
	one, two := generatedTestKeys(t)
	keys, err := NewKeySet(map[string]*rsa.PublicKey{
		DefaultRequiredKeyID: &one.PublicKey,
		"homebrew-2":         &two.PublicKey,
	})
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	return keys
}

type testJWSSignature struct {
	kid         string
	key         *rsa.PrivateKey
	protected   map[string]any
	unprotected map[string]any
}

func validTestSignature(t *testing.T, kid string, key *rsa.PrivateKey) testJWSSignature {
	t.Helper()
	return testJWSSignature{
		kid: kid,
		key: key,
		protected: map[string]any{
			"alg":  JWSAlgorithmPS512,
			"b64":  false,
			"crit": []string{"b64"},
		},
	}
}

func makeGeneralJWS(t *testing.T, payload string, signatures ...testJWSSignature) []byte {
	t.Helper()
	encoded := make([]map[string]any, 0, len(signatures))
	for _, signature := range signatures {
		encoded = append(encoded, makeSignature(t, payload, signature))
	}
	return mustJSON(t, map[string]any{"payload": payload, "signatures": encoded})
}

func makeFlattenedJWS(t *testing.T, payload string, signature testJWSSignature) []byte {
	t.Helper()
	flattened := makeSignature(t, payload, signature)
	flattened["payload"] = payload
	return mustJSON(t, flattened)
}

func makeSignature(t *testing.T, payload string, signature testJWSSignature) map[string]any {
	t.Helper()
	return makeSignatureWithEncoding(t, payload, signature, base64.RawURLEncoding)
}

func makeSignatureWithEncoding(t *testing.T, payload string, signature testJWSSignature, encoding *base64.Encoding) map[string]any {
	t.Helper()
	protectedJSON := mustJSON(t, signature.protected)
	protected := encoding.EncodeToString(protectedJSON)
	hash := sha512.Sum512([]byte(protected + "." + payload))
	signed, err := rsa.SignPSS(rand.Reader, signature.key, crypto.SHA512, hash[:], &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA512,
	})
	if err != nil {
		t.Fatalf("sign JWS: %v", err)
	}
	header := map[string]any{"kid": signature.kid}
	for key, value := range signature.unprotected {
		header[key] = value
	}
	return map[string]any{
		"protected": protected,
		"header":    header,
		"signature": encoding.EncodeToString(signed),
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return data
}

func validFormulaPayload(t *testing.T) []byte {
	t.Helper()
	tool := testFormula("tool", "2.0")
	tool["aliases"] = []string{"tool-current", "tool@2"}
	tool["oldnames"] = []string{"oldtool"}
	tool["versioned_formulae"] = []string{"tool@1"}
	tool["revision"] = 1
	tool["version_scheme"] = 2
	tool["dependencies"] = []string{"tool@1"}
	tool["variations"] = map[string]any{
		"x86_64_linux": map[string]any{
			"dependencies": []string{"python@3.14", "tool@1"},
			"keg_only":     true,
		},
	}

	versioned := testFormula("tool@1", "1.9")
	python := testFormula("python@3.14", "3.14.1")
	python["aliases"] = []string{"python", "python3", "python@3"}
	sourceOnly := testFormula("source-only", "1.0")
	sourceOnly["versions"] = map[string]any{"stable": "1.0", "bottle": false}
	sourceOnly["bottle"] = map[string]any{}

	return mustJSON(t, []any{tool, versioned, python, sourceOnly})
}

func validMigrationPayload(t *testing.T) []byte {
	t.Helper()
	return mustJSON(t, map[string]string{
		"legacy":    "homebrew/core/tool",
		"legacy2":   "legacy",
		"caskish":   "homebrew/cask",
		"caskchain": "caskish",
	})
}

func testFormula(name, version string) map[string]any {
	checksum := fmt.Sprintf("%064x", len(name)+len(version))
	return map[string]any{
		"name":               name,
		"full_name":          name,
		"tap":                "homebrew/core",
		"oldnames":           []string{},
		"aliases":            []string{},
		"versioned_formulae": []string{},
		"desc":               "test " + name,
		"license":            "MIT",
		"homepage":           "https://example.test/" + name,
		"versions":           map[string]any{"stable": version, "bottle": true},
		"revision":           0,
		"version_scheme":     0,
		"bottle": map[string]any{
			"stable": map[string]any{
				"rebuild":  0,
				"root_url": "https://ghcr.io/v2/homebrew/core",
				"files": map[string]any{
					"all": map[string]any{
						"cellar": ":any_skip_relocation",
						"url":    "https://ghcr.io/v2/homebrew/core/" + name + "/blobs/sha256:" + checksum,
						"sha256": checksum,
					},
				},
			},
		},
		"keg_only":     false,
		"dependencies": []string{},
		"variations":   map[string]any{},
	}
}
