package metadata

import (
	"crypto/rsa"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"
)

//go:embed homebrew-1.pub
var homebrew1PublicKeyPEM []byte

// KeySet is an immutable set of RSA verification keys indexed by JWS kid.
type KeySet struct {
	keys map[string]*rsa.PublicKey
}

// NewKeySet clones and validates a configurable RSA key map.
func NewKeySet(keys map[string]*rsa.PublicKey) (KeySet, error) {
	if len(keys) == 0 {
		return KeySet{}, fmt.Errorf("verification key set is empty")
	}
	out := make(map[string]*rsa.PublicKey, len(keys))
	for kid, key := range keys {
		if strings.TrimSpace(kid) == "" {
			return KeySet{}, fmt.Errorf("verification key has an empty kid")
		}
		if key == nil || key.N == nil || key.N.Sign() <= 0 || key.E < 3 {
			return KeySet{}, fmt.Errorf("verification key %q is not a valid RSA public key", kid)
		}
		out[kid] = &rsa.PublicKey{N: new(big.Int).Set(key.N), E: key.E}
	}
	return KeySet{keys: out}, nil
}

// NewKeySetFromPEM parses PKIX or PKCS#1 RSA public keys.
func NewKeySetFromPEM(keys map[string][]byte) (KeySet, error) {
	parsed := make(map[string]*rsa.PublicKey, len(keys))
	for kid, data := range keys {
		key, err := ParseRSAPublicKeyPEM(data)
		if err != nil {
			return KeySet{}, fmt.Errorf("parse verification key %q: %w", kid, err)
		}
		parsed[kid] = key
	}
	return NewKeySet(parsed)
}

// ParseRSAPublicKeyPEM accepts PUBLIC KEY and RSA PUBLIC KEY PEM blocks.
func ParseRSAPublicKeyPEM(data []byte) (*rsa.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("unexpected data after PEM block")
	}
	switch block.Type {
	case "PUBLIC KEY":
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key is %T, not RSA", key)
		}
		return rsaKey, nil
	case "RSA PUBLIC KEY":
		return x509.ParsePKCS1PublicKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
}

// DefaultKeySet returns a fresh immutable set containing Homebrew's pinned
// homebrew-1 public key.
func DefaultKeySet() (KeySet, error) {
	return NewKeySetFromPEM(map[string][]byte{DefaultRequiredKeyID: homebrew1PublicKeyPEM})
}

// DefaultKeySetDigest binds release metadata to the exact embedded Homebrew
// verification key bytes.
func DefaultKeySetDigest() string { return digest.FromBytes(homebrew1PublicKeyPEM).String() }

func (k KeySet) get(kid string) (*rsa.PublicKey, bool) {
	key, ok := k.keys[kid]
	return key, ok
}

// IDs returns sorted key IDs without exposing the internal map.
func (k KeySet) IDs() []string {
	ids := make([]string, 0, len(k.keys))
	for id := range k.keys {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (k KeySet) empty() bool { return len(k.keys) == 0 }
