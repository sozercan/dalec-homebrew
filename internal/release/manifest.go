package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

const SchemaVersion = "dalec-homebrew-components/v1"

type Manifest struct {
	SchemaVersion          string    `json:"schema_version"`
	PolicyVersion          string    `json:"policy_version"`
	Frontend               Component `json:"frontend"`
	RuntimeBase            Component `json:"runtime_base"`
	Materializer           Component `json:"materializer"`
	HomebrewCommit         string    `json:"homebrew_commit"`
	PortableRubyVersion    string    `json:"portable_ruby_version"`
	VerificationKeysDigest string    `json:"verification_keys_digest"`
	DalecModule            string    `json:"dalec_module"`
	BuildKitModule         string    `json:"buildkit_module"`
}

type Component struct {
	Index     string        `json:"index"`
	Platforms []PlatformRef `json:"platforms"`
}
type PlatformRef struct {
	Platform resolution.Platform `json:"platform"`
	Ref      string              `json:"ref"`
}

func Decode(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, 4<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("component manifest exceeds 4 MiB")
	}
	if err := validateUniqueJSON(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	canonicalize(&m)
	if err := Validate(&m); err != nil {
		return nil, err
	}
	return &m, nil
}
func Canonical(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, errors.New("nil component manifest")
	}
	c := *m
	c.Frontend = cloneComponent(m.Frontend)
	c.RuntimeBase = cloneComponent(m.RuntimeBase)
	c.Materializer = cloneComponent(m.Materializer)
	canonicalize(&c)
	if err := Validate(&c); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
func Digest(m *Manifest) (digest.Digest, error) {
	b, err := Canonical(m)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(b), nil
}
func Validate(m *Manifest) error {
	var errs []error
	if m.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema %q", m.SchemaVersion))
	}
	if m.PolicyVersion != resolution.PolicyVersion {
		errs = append(errs, fmt.Errorf("unsupported policy %q", m.PolicyVersion))
	}
	for name, c := range map[string]Component{"frontend": m.Frontend, "runtime_base": m.RuntimeBase, "materializer": m.Materializer} {
		if err := validateComponent(name, c); err != nil {
			errs = append(errs, err)
		}
	}
	if len(m.HomebrewCommit) != 40 || !lowerHex(m.HomebrewCommit) {
		errs = append(errs, errors.New("invalid Homebrew commit"))
	}
	if m.PortableRubyVersion == "" {
		errs = append(errs, errors.New("portable Ruby version is required"))
	}
	if err := validateDigest(m.VerificationKeysDigest); err != nil {
		errs = append(errs, fmt.Errorf("verification keys: %w", err))
	}
	if m.DalecModule == "" || m.BuildKitModule == "" {
		errs = append(errs, errors.New("Dalec and BuildKit module versions are required"))
	}
	return errors.Join(errs...)
}
func (m *Manifest) ComponentsFor(platform resolution.Platform) (resolution.Components, error) {
	find := func(c Component) (string, error) {
		for _, p := range c.Platforms {
			if p.Platform.OS == platform.OS && p.Platform.Architecture == platform.Architecture {
				return p.Ref, nil
			}
		}
		return "", fmt.Errorf("component has no %s/%s child", platform.OS, platform.Architecture)
	}
	f, err := find(m.Frontend)
	if err != nil {
		return resolution.Components{}, err
	}
	b, err := find(m.RuntimeBase)
	if err != nil {
		return resolution.Components{}, err
	}
	mat, err := find(m.Materializer)
	if err != nil {
		return resolution.Components{}, err
	}
	return resolution.Components{FrontendRef: f, RuntimeBaseRef: b, MaterializerRef: mat, HomebrewCommit: m.HomebrewCommit, RubyRuntime: m.PortableRubyVersion, VerificationKeys: m.VerificationKeysDigest, DalecModule: m.DalecModule, BuildKitModule: m.BuildKitModule}, nil
}

func validateComponent(name string, c Component) error {
	if err := validateRef(c.Index); err != nil {
		return fmt.Errorf("%s index: %w", name, err)
	}
	repo := strings.Split(c.Index, "@")[0]
	seen := map[string]struct{}{}
	for _, p := range c.Platforms {
		key := p.Platform.OS + "/" + p.Platform.Architecture
		if p.Platform.OS != "linux" || (p.Platform.Architecture != "amd64" && p.Platform.Architecture != "arm64") || p.Platform.Variant != "" {
			return fmt.Errorf("%s unsupported platform %s", name, key)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%s duplicate platform %s", name, key)
		}
		seen[key] = struct{}{}
		if err := validateRef(p.Ref); err != nil {
			return fmt.Errorf("%s %s: %w", name, key, err)
		}
		if strings.Split(p.Ref, "@")[0] != repo {
			return fmt.Errorf("%s %s child uses a different repository", name, key)
		}
	}
	for _, key := range []string{"linux/amd64", "linux/arm64"} {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("%s misses %s", name, key)
		}
	}
	return nil
}
func validateRef(ref string) error { return resolution.ValidatePinnedReference(ref) }
func validateDigest(v string) error {
	d, err := digest.Parse(v)
	if err != nil {
		return err
	}
	if d.Algorithm() != digest.SHA256 {
		return errors.New("only sha256 is accepted")
	}
	return d.Validate()
}
func canonicalize(m *Manifest) {
	for _, c := range []*Component{&m.Frontend, &m.RuntimeBase, &m.Materializer} {
		slices.SortFunc(c.Platforms, func(a, b PlatformRef) int {
			if x := strings.Compare(a.Platform.OS, b.Platform.OS); x != 0 {
				return x
			}
			return strings.Compare(a.Platform.Architecture, b.Platform.Architecture)
		})
	}
}
func cloneComponent(c Component) Component {
	c.Platforms = append([]PlatformRef(nil), c.Platforms...)
	return c
}
func lowerHex(v string) bool {
	for _, r := range v {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
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
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("unterminated object")
		}
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
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}
