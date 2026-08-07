// Package policyv2 exposes the release-bound executable runtime policy used by
// V2 dalec-homebrew component tuples.
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
	"strings"
)

const (
	SchemaVersion             = "dalec-homebrew-policy/v2"
	ResolverPolicyVersion     = "homebrew-runtime-v2"
	FetchPolicyVersion        = "homebrew-bottle-fetch-v1"
	ProvenancePolicyVersion   = "homebrew-bottle-provenance-v1"
	NonCoreProvenanceWaiver   = "tap-catalog-buildkit-and-verified-checksum-v1"
	DefaultMaxNonCoreTaps     = 16
	DefaultMaxClosureNodes    = 256
	DefaultMaxCatalogBytes    = int64(64 << 20)
	DefaultMaxMetadataBytes   = int64(256 << 20)
	DefaultMaxBottleBytes     = int64(1 << 30)
	DefaultMaxRedirects       = 5
	DefaultFetchTimeoutSecond = 15 * 60
)

//go:embed policy.json
var embedded []byte

type Policy struct {
	SchemaVersion         string                  `json:"schema_version"`
	ResolverPolicyVersion string                  `json:"resolver_policy_version"`
	Platforms             map[string]Platform     `json:"platforms"`
	Linuxbrew             Linuxbrew               `json:"linuxbrew"`
	CatalogLimits         CatalogLimits           `json:"catalog_limits"`
	FetchPolicy           FetchPolicy             `json:"fetch_policy"`
	ProvenancePolicy      ProvenancePolicy        `json:"provenance_policy"`
	WritablePathTemplate  string                  `json:"writable_path_template"`
	PackageCapabilities   map[string]Capabilities `json:"package_capabilities"`
}

type Platform struct {
	BottleTag   string `json:"bottle_tag"`
	CPUBaseline string `json:"cpu_baseline"`
}

type Linuxbrew struct {
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Prefix string `json:"prefix"`
}

type CatalogLimits struct {
	NonCoreTaps           int   `json:"non_core_taps"`
	ClosureNodes          int   `json:"closure_nodes"`
	CatalogBytes          int64 `json:"catalog_bytes"`
	AggregateNonCoreBytes int64 `json:"aggregate_non_core_bytes"`
}

type FetchPolicy struct {
	Version               string `json:"version"`
	MaxBytes              int64  `json:"max_bytes"`
	MaxRedirects          int    `json:"max_redirects"`
	OverallTimeoutSeconds int    `json:"overall_timeout_seconds"`
}

type ProvenancePolicy struct {
	Version string `json:"version"`
	Waiver  string `json:"waiver"`
}

type Capabilities struct {
	SharedEtc            []string `json:"shared_etc,omitempty"`
	GeneratedGlobalPaths []string `json:"generated_global_paths,omitempty"`
	GeneratedKegPaths    []string `json:"generated_keg_paths,omitempty"`
	Rules                []string `json:"rules,omitempty"`
}

func Load() (*Policy, error) {
	if err := validateUniqueJSON(embedded); err != nil {
		return nil, fmt.Errorf("embedded V2 policy: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(embedded))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode embedded V2 policy: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	canonicalize(&p)
	if err := Validate(&p); err != nil {
		return nil, fmt.Errorf("validate embedded V2 policy: %w", err)
	}
	return &p, nil
}

func Validate(p *Policy) error {
	if p == nil {
		return errors.New("nil V2 policy")
	}
	var errs []error
	if p.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema %q", p.SchemaVersion))
	}
	if p.ResolverPolicyVersion != ResolverPolicyVersion {
		errs = append(errs, fmt.Errorf("unsupported resolver policy %q", p.ResolverPolicyVersion))
	}
	for key, want := range map[string]Platform{
		"linux/amd64": {BottleTag: "x86_64_linux", CPUBaseline: "core2"},
		"linux/arm64": {BottleTag: "arm64_linux", CPUBaseline: "armv8"},
	} {
		if got, ok := p.Platforms[key]; !ok || got != want {
			errs = append(errs, fmt.Errorf("platform %s must equal %+v", key, want))
		}
	}
	if len(p.Platforms) != 2 {
		errs = append(errs, errors.New("V2 policy must contain exactly the two supported Linux platforms"))
	}
	if p.Linuxbrew.UID != 1000 || p.Linuxbrew.GID != 1000 || p.Linuxbrew.Prefix != "/home/linuxbrew/.linuxbrew" {
		errs = append(errs, errors.New("invalid linuxbrew identity or prefix"))
	}
	if p.CatalogLimits.NonCoreTaps != DefaultMaxNonCoreTaps || p.CatalogLimits.ClosureNodes != DefaultMaxClosureNodes || p.CatalogLimits.CatalogBytes != DefaultMaxCatalogBytes || p.CatalogLimits.AggregateNonCoreBytes != DefaultMaxMetadataBytes {
		errs = append(errs, errors.New("catalog limits differ from the V2 contract"))
	}
	if p.FetchPolicy.Version != FetchPolicyVersion || p.FetchPolicy.MaxBytes != DefaultMaxBottleBytes || p.FetchPolicy.MaxRedirects != DefaultMaxRedirects || p.FetchPolicy.OverallTimeoutSeconds != DefaultFetchTimeoutSecond {
		errs = append(errs, errors.New("fetch policy differs from the V2 contract"))
	}
	if p.ProvenancePolicy.Version != ProvenancePolicyVersion || p.ProvenancePolicy.Waiver != NonCoreProvenanceWaiver {
		errs = append(errs, errors.New("provenance policy differs from the V2 contract"))
	}
	if p.WritablePathTemplate != "/home/linuxbrew/.linuxbrew/var/<rack-name>" {
		errs = append(errs, errors.New("invalid writable path template"))
	}
	for id, caps := range p.PackageCapabilities {
		if !validFormulaID(id) {
			errs = append(errs, fmt.Errorf("invalid package capability Formula ID %q", id))
		}
		for _, values := range [][]string{caps.SharedEtc, caps.GeneratedGlobalPaths, caps.GeneratedKegPaths} {
			if !sortedUniqueSafePaths(values) {
				errs = append(errs, fmt.Errorf("package capability %q contains unsafe or duplicate paths", id))
			}
		}
		if !sortedUniqueRules(caps.Rules) {
			errs = append(errs, fmt.Errorf("package capability %q contains unsafe or duplicate rules", id))
		}
	}
	return errors.Join(errs...)
}

func Canonical() ([]byte, error) {
	p, err := Load()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func Digest() (string, error) {
	data, err := Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ForFormula returns a defensive copy of the capabilities for an exact Formula
// ID. It deliberately does not fall back to the rack/short name.
func (p *Policy) ForFormula(id string) (Capabilities, bool) {
	if p == nil {
		return Capabilities{}, false
	}
	caps, ok := p.PackageCapabilities[id]
	if !ok {
		return Capabilities{}, false
	}
	caps.SharedEtc = append([]string(nil), caps.SharedEtc...)
	caps.GeneratedGlobalPaths = append([]string(nil), caps.GeneratedGlobalPaths...)
	caps.GeneratedKegPaths = append([]string(nil), caps.GeneratedKegPaths...)
	caps.Rules = append([]string(nil), caps.Rules...)
	return caps, true
}

func (p *Policy) HasRule(id, rule string) bool {
	caps, ok := p.ForFormula(id)
	return ok && slices.Contains(caps.Rules, rule)
}

func canonicalize(p *Policy) {
	for id, caps := range p.PackageCapabilities {
		slices.Sort(caps.SharedEtc)
		slices.Sort(caps.GeneratedGlobalPaths)
		slices.Sort(caps.GeneratedKegPaths)
		slices.Sort(caps.Rules)
		p.PackageCapabilities[id] = caps
	}
}

func validFormulaID(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 128 {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.ContainsRune("+_.@-", rune(c))) {
				return false
			}
		}
	}
	return true
}

func sortedUniqueSafePaths(values []string) bool {
	previous := ""
	for i, value := range values {
		if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.Contains(value, "//") {
			return false
		}
		for _, part := range strings.Split(value, "/") {
			if part == "" || part == "." || part == ".." {
				return false
			}
		}
		if i > 0 && value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func sortedUniqueRules(values []string) bool {
	previous := ""
	for i, value := range values {
		if value == "" || len(value) > 128 {
			return false
		}
		for _, r := range value {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
		if i > 0 && value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func ensureEOF(dec *json.Decoder) error {
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
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
	if err := walkUniqueJSON(dec, token); err != nil {
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

func walkUniqueJSON(dec *json.Decoder, token json.Token) error {
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
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, value); err != nil {
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
			if err := walkUniqueJSON(dec, value); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

// HasEmbeddedRule checks one exact Formula-ID rule in the authoritative
// embedded policy and fails closed if the embedded document cannot load.
func HasEmbeddedRule(id, rule string) bool {
	policy, err := Load()
	return err == nil && policy.HasRule(id, rule)
}
