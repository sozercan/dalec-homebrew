// Package resolution defines immutable, canonical records that bind a Dalec
// input to the exact Homebrew metadata and bottle artifacts used to build a
// runtime image.
package resolution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
)

const (
	SchemaVersionV1 = "dalec-homebrew-resolution/v1"
	SchemaVersionV2 = "dalec-homebrew-resolution/v2"
	PolicyVersionV1 = "homebrew-runtime-v1"
	PolicyVersionV2 = "homebrew-runtime-v2"

	// SchemaVersion and PolicyVersion retain their V1 values for source
	// compatibility with the existing resolver and materializer. V2 callers use
	// the explicitly versioned constants and RecordV2 APIs.
	SchemaVersion = SchemaVersionV1
	PolicyVersion = PolicyVersionV1
)

// Platform is the normalized OCI target platform. V1 intentionally excludes
// variants and non-Linux operating systems from policy decisions.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// Record is a replayable materialization input. It intentionally contains no
// registry tags without their resolved descriptor chain.
type Record struct {
	SchemaVersion       string            `json:"schema_version"`
	PolicyVersion       string            `json:"policy_version"`
	Input               Input             `json:"input"`
	Metadata            MetadataSnapshot  `json:"metadata"`
	ResolvedAt          time.Time         `json:"resolved_at"`
	SourceDateEpoch     int64             `json:"source_date_epoch"`
	Requested           []RequestedRoot   `json:"requested"`
	Nodes               []Node            `json:"nodes"`
	InstallOrder        []string          `json:"install_order"`
	Components          Components        `json:"components"`
	Runtime             RuntimePolicy     `json:"runtime"`
	AttestationPolicy   AttestationPolicy `json:"attestation_policy"`
	PruningPolicyDigest string            `json:"pruning_policy_digest,omitempty"`
}

type Input struct {
	DalecSpecDigest string   `json:"dalec_spec_digest"`
	TargetKey       string   `json:"target_key,omitempty"`
	Platform        Platform `json:"platform"`
}

type MetadataSnapshot struct {
	Digest                   string      `json:"digest"`
	FormulaDigest            string      `json:"formula_digest"`
	MigrationDigest          string      `json:"migration_digest"`
	FormulaEnvelopeDigest    string      `json:"formula_envelope_digest,omitempty"`
	MigrationEnvelopeDigest  string      `json:"migration_envelope_digest,omitempty"`
	FormulaFreshnessSource   string      `json:"formula_freshness_source,omitempty"`
	MigrationFreshnessSource string      `json:"migration_freshness_source,omitempty"`
	GeneratedAt              time.Time   `json:"generated_at"`
	FetchedAt                time.Time   `json:"fetched_at"`
	Signatures               []Signature `json:"signatures,omitempty"`
	FormulaSignatures        []Signature `json:"formula_signatures,omitempty"`
	MigrationSignatures      []Signature `json:"migration_signatures,omitempty"`
	FormulaURL               string      `json:"formula_url"`
	MigrationURL             string      `json:"migration_url"`
}

type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Verified  bool   `json:"verified"`
}

type RequestedRoot struct {
	Requested string `json:"requested"`
	Canonical string `json:"canonical"`
	KegOnly   bool   `json:"keg_only,omitempty"`
}

type Node struct {
	Name              string        `json:"name"`
	FullName          string        `json:"full_name"`
	FormulaVersion    string        `json:"formula_version"`
	FormulaRevision   int           `json:"formula_revision"`
	PkgVersion        string        `json:"pkg_version"`
	VersionScheme     int           `json:"version_scheme"`
	BottleRebuild     int           `json:"bottle_rebuild"`
	License           string        `json:"license,omitempty"`
	KegOnly           bool          `json:"keg_only,omitempty"`
	Dependencies      []Requirement `json:"dependencies,omitempty"`
	Bottle            Bottle        `json:"bottle"`
	ExecutablePaths   []string      `json:"executable_paths,omitempty"`
	UpstreamFormulaID string        `json:"upstream_formula_identity,omitempty"`
	// PolicyFormulaID is an in-memory V2 projection marker. It is never
	// serialized in immutable V1 records.
	PolicyFormulaID string `json:"-"`
}

type Requirement struct {
	Name          string `json:"name"`
	Minimum       string `json:"minimum_pkg_version"`
	Revision      int    `json:"minimum_revision"`
	BottleRebuild int    `json:"minimum_bottle_rebuild"`
	Direct        bool   `json:"declared_directly,omitempty"`
}

type Descriptor struct {
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	MediaType string            `json:"media_type"`
	Platform  *Platform         `json:"platform,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type Bottle struct {
	Tag                 string     `json:"tag"`
	Filename            string     `json:"filename"`
	Repository          string     `json:"repository"`
	Index               Descriptor `json:"index"`
	Manifest            Descriptor `json:"manifest"`
	Config              Descriptor `json:"config"`
	Layer               Descriptor `json:"layer"`
	HomebrewSHA256      string     `json:"homebrew_sha256"`
	Cellar              string     `json:"cellar"`
	Tab                 BottleTab  `json:"tab"`
	SelectedAnnotations []KV       `json:"selected_annotations,omitempty"`
}

type BottleTab struct {
	HomebrewVersion string              `json:"homebrew_version"`
	Arch            string              `json:"arch,omitempty"`
	Compiler        string              `json:"compiler,omitempty"`
	ChangedFiles    []string            `json:"changed_files,omitempty"`
	BuiltOn         BuiltOn             `json:"built_on,omitempty"`
	Dependencies    []RuntimeDependency `json:"runtime_dependencies,omitempty"`
}

type BuiltOn struct {
	OS              string `json:"os,omitempty"`
	OSVersion       string `json:"os_version,omitempty"`
	CPUFamily       string `json:"cpu_family,omitempty"`
	OldestCPUFamily string `json:"oldest_cpu_family,omitempty"`
	GlibcVersion    string `json:"glibc_version,omitempty"`
}

type RuntimeDependency struct {
	FullName         string `json:"full_name"`
	Version          string `json:"version"`
	Revision         int    `json:"revision"`
	BottleRebuild    int    `json:"bottle_rebuild"`
	PkgVersion       string `json:"pkg_version"`
	DeclaredDirectly bool   `json:"declared_directly,omitempty"`
}

type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Components struct {
	FrontendRef      string `json:"frontend_ref"`
	RuntimeBaseRef   string `json:"runtime_base_ref"`
	MaterializerRef  string `json:"materializer_ref"`
	HomebrewCommit   string `json:"homebrew_commit"`
	RubyRuntime      string `json:"ruby_runtime"`
	VerificationKeys string `json:"verification_keys_digest"`
	DalecModule      string `json:"dalec_module"`
	BuildKitModule   string `json:"buildkit_module"`
}

type RuntimePolicy struct {
	User          string   `json:"user"`
	UID           int      `json:"uid"`
	GID           int      `json:"gid"`
	GeneratedPATH []string `json:"generated_path"`
	WritablePaths []string `json:"writable_paths,omitempty"`
	CPUBaseline   string   `json:"cpu_baseline"`
}

type AttestationPolicy struct {
	Identity string `json:"identity,omitempty"`
	Waiver   string `json:"waiver,omitempty"`
}

// Canonical returns stable JSON for hashing and transport. Slices whose order
// does not carry policy meaning are sorted; Requested and InstallOrder remain
// ordered because they affect PATH exposure and materialization.
func Canonical(r *Record) ([]byte, error) {
	if r == nil {
		return nil, errors.New("nil resolution record")
	}
	c := cloneRecord(*r)
	canonicalize(&c)
	if err := Validate(&c); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("encode canonical resolution: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func Digest(r *Record) (digest.Digest, error) {
	b, err := Canonical(r)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(b), nil
}

func Decode(data []byte) (*Record, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("decode resolution: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return nil, errors.New("resolution contains trailing JSON values")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("resolution contains invalid trailing data: %w", err)
	}
	canonicalize(&r)
	if err := Validate(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func Validate(r *Record) error {
	var errs []error
	if r.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q", r.SchemaVersion))
	}
	if r.PolicyVersion != PolicyVersion {
		errs = append(errs, fmt.Errorf("unsupported policy_version %q", r.PolicyVersion))
	}
	if r.Input.Platform.OS != "linux" || (r.Input.Platform.Architecture != "amd64" && r.Input.Platform.Architecture != "arm64") || r.Input.Platform.Variant != "" {
		errs = append(errs, fmt.Errorf("unsupported platform %s/%s", r.Input.Platform.OS, r.Input.Platform.Architecture))
	}
	for field, value := range map[string]string{
		"input.dalec_spec_digest":   r.Input.DalecSpecDigest,
		"metadata.digest":           r.Metadata.Digest,
		"metadata.formula_digest":   r.Metadata.FormulaDigest,
		"metadata.migration_digest": r.Metadata.MigrationDigest,
	} {
		if err := validateDigest(value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field, err))
		}
	}
	if r.SourceDateEpoch <= 0 {
		errs = append(errs, errors.New("source_date_epoch must be positive"))
	}
	if r.ResolvedAt.IsZero() || r.Metadata.GeneratedAt.IsZero() || r.Metadata.FetchedAt.IsZero() {
		errs = append(errs, errors.New("resolution and metadata timestamps must be set"))
	}
	if len(r.Requested) == 0 {
		errs = append(errs, errors.New("resolution has no requested roots"))
	}
	if len(r.Nodes) == 0 {
		errs = append(errs, errors.New("resolution has no closure nodes"))
	}

	nodes := make(map[string]Node, len(r.Nodes))
	for _, n := range r.Nodes {
		if n.Name == "" || n.FullName != "homebrew/core/"+n.Name {
			errs = append(errs, fmt.Errorf("node %q has invalid homebrew/core identity %q", n.Name, n.FullName))
		}
		if _, ok := nodes[n.Name]; ok {
			errs = append(errs, fmt.Errorf("duplicate closure node %q", n.Name))
		}
		nodes[n.Name] = n
		if n.FormulaVersion == "" || n.PkgVersion == "" {
			errs = append(errs, fmt.Errorf("node %q has empty version", n.Name))
		}
		if n.Bottle.Filename != "" && (path.Base(n.Bottle.Filename) != n.Bottle.Filename || strings.ContainsAny(n.Bottle.Filename, "/\\")) {
			errs = append(errs, fmt.Errorf("node %q has unsafe bottle filename %q", n.Name, n.Bottle.Filename))
		}
		if n.Bottle.Repository == "" || n.Bottle.Filename == "" || n.Bottle.Tag == "" {
			errs = append(errs, fmt.Errorf("node %q has incomplete bottle identity", n.Name))
		}
		expectedTag, expectedTabArch := "x86_64_linux", "x86_64"
		if r.Input.Platform.Architecture == "arm64" {
			expectedTag, expectedTabArch = "arm64_linux", "arm64"
		}
		switch n.Bottle.Tag {
		case "all":
			if n.Bottle.Tab.Arch != "" {
				errs = append(errs, fmt.Errorf("node %q all bottle has architecture %q", n.Name, n.Bottle.Tab.Arch))
			}
			if n.Bottle.Manifest.Platform != nil {
				errs = append(errs, fmt.Errorf("node %q all bottle manifest unexpectedly has a platform", n.Name))
			}
		case expectedTag:
			if n.Bottle.Tab.Arch != expectedTabArch {
				errs = append(errs, fmt.Errorf("node %q bottle tab architecture %q, expected %q", n.Name, n.Bottle.Tab.Arch, expectedTabArch))
			}
			if p := n.Bottle.Manifest.Platform; p == nil || p.OS != "linux" || p.Architecture != r.Input.Platform.Architecture || p.Variant != "" {
				errs = append(errs, fmt.Errorf("node %q bottle manifest platform does not match target", n.Name))
			}
		default:
			errs = append(errs, fmt.Errorf("node %q bottle tag %q does not match target", n.Name, n.Bottle.Tag))
		}
		for label, d := range map[string]Descriptor{"index": n.Bottle.Index, "manifest": n.Bottle.Manifest, "config": n.Bottle.Config, "layer": n.Bottle.Layer} {
			if err := validateDescriptor(d); err != nil {
				errs = append(errs, fmt.Errorf("node %q %s descriptor: %w", n.Name, label, err))
			}
		}
		if err := validateSHA256Hex(n.Bottle.HomebrewSHA256); err != nil {
			errs = append(errs, fmt.Errorf("node %q homebrew checksum: %w", n.Name, err))
		}
		if n.Bottle.Layer.Digest != "sha256:"+strings.ToLower(n.Bottle.HomebrewSHA256) {
			errs = append(errs, fmt.Errorf("node %q layer digest does not match authenticated Homebrew checksum", n.Name))
		}
		seenDeps := map[string]struct{}{}
		for _, dep := range n.Dependencies {
			if dep.Name == "" {
				errs = append(errs, fmt.Errorf("node %q has empty dependency", n.Name))
			}
			if _, ok := seenDeps[dep.Name]; ok {
				errs = append(errs, fmt.Errorf("node %q has duplicate dependency %q", n.Name, dep.Name))
			}
			seenDeps[dep.Name] = struct{}{}
		}
	}
	for _, root := range r.Requested {
		if _, ok := nodes[root.Canonical]; !ok {
			errs = append(errs, fmt.Errorf("requested root %q resolves to missing node %q", root.Requested, root.Canonical))
		}
	}
	for _, n := range r.Nodes {
		for _, dep := range n.Dependencies {
			if _, ok := nodes[dep.Name]; !ok {
				errs = append(errs, fmt.Errorf("node %q references missing dependency %q", n.Name, dep.Name))
			}
		}
	}
	if len(r.InstallOrder) != len(nodes) {
		errs = append(errs, fmt.Errorf("install_order has %d entries for %d nodes", len(r.InstallOrder), len(nodes)))
	} else {
		position := make(map[string]int, len(r.InstallOrder))
		for i, name := range r.InstallOrder {
			if _, ok := nodes[name]; !ok {
				errs = append(errs, fmt.Errorf("install_order references unknown node %q", name))
			}
			if _, ok := position[name]; ok {
				errs = append(errs, fmt.Errorf("duplicate install_order entry %q", name))
			}
			position[name] = i
		}
		for _, n := range r.Nodes {
			if _, ok := position[n.Name]; !ok {
				errs = append(errs, fmt.Errorf("install_order omits node %q", n.Name))
				continue
			}
			for _, dep := range n.Dependencies {
				depPos, depOK := position[dep.Name]
				if !depOK || depPos >= position[n.Name] {
					errs = append(errs, fmt.Errorf("install_order places %q before dependency %q", n.Name, dep.Name))
				}
			}
		}
	}
	reachable := map[string]struct{}{}
	var walk func(string)
	walk = func(name string) {
		if _, ok := reachable[name]; ok {
			return
		}
		reachable[name] = struct{}{}
		n, ok := nodes[name]
		if !ok {
			return
		}
		for _, dep := range n.Dependencies {
			walk(dep.Name)
		}
	}
	for _, root := range r.Requested {
		walk(root.Canonical)
	}
	for name := range nodes {
		if _, ok := reachable[name]; !ok {
			errs = append(errs, fmt.Errorf("closure node %q is unreachable from requested roots", name))
		}
	}
	if err := validateRuntimeIdentity(r.Runtime); err != nil {
		errs = append(errs, err)
	}
	if (r.AttestationPolicy.Identity == "") == (r.AttestationPolicy.Waiver == "") {
		errs = append(errs, errors.New("attestation policy must set exactly one of identity or waiver"))
	}
	return errors.Join(errs...)
}

func validateRuntimeIdentity(runtime RuntimePolicy) error {
	if runtime.UID <= 0 || runtime.GID <= 0 {
		return errors.New("runtime identity must have positive non-root uid/gid")
	}
	user := strings.TrimSpace(runtime.User)
	if user == "linuxbrew" || user == "linuxbrew:linuxbrew" {
		if runtime.UID != 1000 || runtime.GID != 1000 {
			return errors.New("linuxbrew runtime identity must be uid/gid 1000")
		}
		return nil
	}
	parts := strings.Split(user, ":")
	if len(parts) != 2 {
		return fmt.Errorf("numeric runtime user %q must be uid:gid", user)
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("runtime user %q is unsupported", user)
	}
	gid, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("runtime user %q is unsupported", user)
	}
	if uid <= 0 || gid <= 0 || uid != runtime.UID || gid != runtime.GID {
		return fmt.Errorf("runtime user %q does not match recorded uid/gid %d:%d", user, runtime.UID, runtime.GID)
	}
	return nil
}

func validateDescriptor(d Descriptor) error {
	if err := validateDigest(d.Digest); err != nil {
		return err
	}
	if d.Size <= 0 {
		return fmt.Errorf("invalid size %d", d.Size)
	}
	if strings.TrimSpace(d.MediaType) == "" {
		return errors.New("empty media type")
	}
	return nil
}

func validateDigest(s string) error {
	d, err := digest.Parse(s)
	if err != nil {
		return err
	}
	if d.Algorithm() != digest.SHA256 {
		return fmt.Errorf("only sha256 is allowed, got %s", d.Algorithm())
	}
	return d.Validate()
}

func validateSHA256Hex(s string) error {
	if len(s) != sha256.Size*2 {
		return fmt.Errorf("expected %d hex characters", sha256.Size*2)
	}
	_, err := hex.DecodeString(s)
	return err
}

func cloneRecord(r Record) Record {
	b, _ := json.Marshal(r)
	var c Record
	_ = json.Unmarshal(b, &c)
	return c
}

func canonicalize(r *Record) {
	r.ResolvedAt = r.ResolvedAt.UTC().Round(0)
	r.Metadata.GeneratedAt = r.Metadata.GeneratedAt.UTC().Round(0)
	r.Metadata.FetchedAt = r.Metadata.FetchedAt.UTC().Round(0)
	sortSignatures := func(values []Signature) {
		slices.SortFunc(values, func(a, b Signature) int {
			if c := strings.Compare(a.KeyID, b.KeyID); c != 0 {
				return c
			}
			if c := strings.Compare(a.Algorithm, b.Algorithm); c != 0 {
				return c
			}
			if a.Verified == b.Verified {
				return 0
			}
			if !a.Verified {
				return -1
			}
			return 1
		})
	}
	sortSignatures(r.Metadata.Signatures)
	sortSignatures(r.Metadata.FormulaSignatures)
	sortSignatures(r.Metadata.MigrationSignatures)
	slices.SortFunc(r.Nodes, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(r.Runtime.WritablePaths)
	for i := range r.Nodes {
		n := &r.Nodes[i]
		slices.SortFunc(n.Dependencies, func(a, b Requirement) int { return strings.Compare(a.Name, b.Name) })
		slices.Sort(n.ExecutablePaths)
		slices.Sort(n.Bottle.Tab.ChangedFiles)
		slices.SortFunc(n.Bottle.SelectedAnnotations, func(a, b KV) int {
			if c := strings.Compare(a.Key, b.Key); c != 0 {
				return c
			}
			return strings.Compare(a.Value, b.Value)
		})
		for _, d := range []*Descriptor{&n.Bottle.Index, &n.Bottle.Manifest, &n.Bottle.Config, &n.Bottle.Layer} {
			if len(d.Metadata) == 0 {
				d.Metadata = nil
			}
		}
	}
}

// ValidateForMaterialization applies the stronger release-component checks
// required before any bottle is exposed to Homebrew. Validate remains useful
// for schema fixtures and resolver construction before platform component
// references have been attached.
func ValidateForMaterialization(r *Record) error {
	var errs []error
	if err := Validate(r); err != nil {
		errs = append(errs, err)
	}
	for label, value := range map[string]string{"Formula envelope": r.Metadata.FormulaEnvelopeDigest, "migration envelope": r.Metadata.MigrationEnvelopeDigest} {
		if err := validateDigest(value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
		}
	}
	for label, value := range map[string]string{"Formula freshness source": r.Metadata.FormulaFreshnessSource, "migration freshness source": r.Metadata.MigrationFreshnessSource} {
		if value != "signed-payload" && value != "http-last-modified" {
			errs = append(errs, fmt.Errorf("%s %q is unsupported", label, value))
		}
	}
	verified := func(values []Signature) bool {
		for _, sig := range values {
			if sig.Verified {
				return true
			}
		}
		return false
	}
	if !verified(r.Metadata.FormulaSignatures) {
		errs = append(errs, errors.New("Formula metadata document has no verified signature"))
	}
	if !verified(r.Metadata.MigrationSignatures) {
		errs = append(errs, errors.New("migration metadata document has no verified signature"))
	}
	for name, ref := range map[string]string{"frontend": r.Components.FrontendRef, "runtime base": r.Components.RuntimeBaseRef, "materializer": r.Components.MaterializerRef} {
		if err := validatePinnedReference(ref); err != nil {
			errs = append(errs, fmt.Errorf("%s component: %w", name, err))
		}
	}
	if strings.TrimSpace(r.Components.RubyRuntime) == "" {
		errs = append(errs, errors.New("component tuple is missing the portable Ruby identity"))
	}
	if len(r.Components.HomebrewCommit) != 40 || !isLowerHex(r.Components.HomebrewCommit) {
		errs = append(errs, errors.New("component tuple has an invalid pinned Homebrew commit"))
	}
	if err := validateDigest(r.Components.VerificationKeys); err != nil {
		errs = append(errs, fmt.Errorf("component verification key set: %w", err))
	}
	if r.Components.DalecModule == "" || r.Components.DalecModule == "unknown" || r.Components.BuildKitModule == "" || r.Components.BuildKitModule == "unknown" {
		errs = append(errs, errors.New("component tuple is missing Dalec or BuildKit module versions"))
	}
	if err := validateDigest(r.PruningPolicyDigest); err != nil {
		errs = append(errs, fmt.Errorf("pruning policy: %w", err))
	}
	if r.AttestationPolicy.Waiver != "" && r.AttestationPolicy.Waiver != "homebrew-jws-and-verified-oci-chain-v1" {
		errs = append(errs, fmt.Errorf("unsupported attestation waiver %q", r.AttestationPolicy.Waiver))
	}
	return errors.Join(errs...)
}

func ValidatePinnedReference(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("digest-pinned reference is required")
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return fmt.Errorf("invalid OCI reference %q: %w", ref, err)
	}
	digested, ok := named.(reference.Digested)
	if !ok {
		return fmt.Errorf("reference %q is not digest-pinned", ref)
	}
	return validateDigest(digested.Digest().String())
}

// SameReferenceRepository reports whether two OCI references resolve to the
// same normalized repository identity. Tags and digests do not affect the
// comparison.
func SameReferenceRepository(a, b string) bool {
	aNamed, aErr := reference.ParseNormalizedNamed(a)
	bNamed, bErr := reference.ParseNormalizedNamed(b)
	return aErr == nil && bErr == nil && reference.TrimNamed(aNamed).String() == reference.TrimNamed(bNamed).String()
}

func validatePinnedReference(ref string) error { return ValidatePinnedReference(ref) }

func isLowerHex(value string) bool {
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
