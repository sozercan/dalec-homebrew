package policyv2

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

const (
	// PrebuiltDerivedBottlePolicyVersion identifies the only release-bound
	// prebuilt archive derivation policy accepted by this V2 tap policy.
	PrebuiltDerivedBottlePolicyVersion = "prebuilt-derived-bottle-v1"

	prebuiltA365FormulaID           = "sozercan/repo/a365"
	prebuiltA365Version             = "0.3.3"
	prebuiltArchiveFormat           = "tar+gzip"
	prebuiltArchiveMemberType       = "regular"
	prebuiltBinaryFormat            = "elf64"
	prebuiltBinaryLinkage           = "static"
	prebuiltBinaryForbidden         = "forbidden"
	prebuiltMaxURLBytes             = 2048
	prebuiltMaxCompressedBytes      = int64(32 << 20)
	prebuiltMaxExpandedBytes        = int64(64 << 20)
	prebuiltMaxExpansionRatio       = 8
	prebuiltMaxEntries              = 3
	prebuiltMaxFileBytes            = int64(16 << 20)
	prebuiltMaxDepth                = 1
	prebuiltMaxPathBytes            = 255
	prebuiltMaxInstallPathBytes     = 255
	prebuiltMaxPolicyFormulaEntries = DefaultMaxNonCoreTaps
)

// PrebuiltArchivePolicy is an exact, release-bound authorization to transform
// one authenticated upstream prebuilt archive into a deterministic derived
// bottle. It is not a general source-build or Formula-execution capability.
type PrebuiltArchivePolicy struct {
	FormulaID           string                          `json:"formula_id"`
	PolicyVersion       string                          `json:"policy_version"`
	Version             string                          `json:"version"`
	FormulaSourceDigest string                          `json:"formula_source_digest"`
	License             string                          `json:"license"`
	RootOnly            bool                            `json:"root_only"`
	RequireNoBottle     bool                            `json:"require_no_bottle"`
	Dependencies        []string                        `json:"dependencies"`
	Platforms           []PrebuiltArchivePlatformPolicy `json:"platforms"`
	Archive             PrebuiltArchiveConstraints      `json:"archive"`
	Install             PrebuiltArchiveInstallPolicy    `json:"install"`
	Binary              PrebuiltBinaryPolicy            `json:"binary"`
}

// PrebuiltArchivePlatformPolicy binds one target platform to exact upstream
// release bytes.
type PrebuiltArchivePlatformPolicy struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
}

// PrebuiltArchiveConstraints bounds hostile tar+gzip input and enumerates its
// complete permitted inventory.
type PrebuiltArchiveConstraints struct {
	Format             string                        `json:"format"`
	SingleGzipMember   bool                          `json:"single_gzip_member"`
	MaxCompressedBytes int64                         `json:"max_compressed_bytes"`
	MaxExpandedBytes   int64                         `json:"max_expanded_bytes"`
	MaxExpansionRatio  int                           `json:"max_expansion_ratio"`
	MaxEntries         int                           `json:"max_entries"`
	MaxFileBytes       int64                         `json:"max_file_bytes"`
	MaxDepth           int                           `json:"max_depth"`
	MaxPathBytes       int                           `json:"max_path_bytes"`
	Members            []PrebuiltArchiveMemberPolicy `json:"members"`
}

// PrebuiltArchiveMemberPolicy describes one and only one permitted archive
// member. Initial support accepts regular files only.
type PrebuiltArchiveMemberPolicy struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode"`
}

// PrebuiltArchiveInstallPolicy is the complete copy transformation applied to
// the verified payload. Formula install and post-install methods remain out of
// scope.
type PrebuiltArchiveInstallPolicy struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        string `json:"mode"`
}

// PrebuiltBinaryPolicy fixes the static executable properties required before
// a payload may be placed in a derived bottle.
type PrebuiltBinaryPolicy struct {
	Format                     string                        `json:"format"`
	Machines                   []PrebuiltBinaryMachinePolicy `json:"machines"`
	Linkage                    string                        `json:"linkage"`
	Interpreter                string                        `json:"interpreter"`
	NeededLibraries            []string                      `json:"needed_libraries"`
	RPath                      string                        `json:"rpath"`
	WritableExecutableSegments string                        `json:"writable_executable_segments"`
	GoModule                   string                        `json:"go_module"`
	CGOEnabled                 *bool                         `json:"cgo_enabled"`
}

// PrebuiltBinaryMachinePolicy binds each supported platform to its required
// ELF machine identity.
type PrebuiltBinaryMachinePolicy struct {
	Platform string `json:"platform"`
	Machine  string `json:"machine"`
}

// PrebuiltArchiveForFormula returns a defensive copy of the exact prebuilt
// archive authorization for id. It deliberately performs no short-name,
// alias, migration, or core fallback lookup.
func (p *TapPolicy) PrebuiltArchiveForFormula(id string) (PrebuiltArchivePolicy, bool) {
	if p == nil {
		return PrebuiltArchivePolicy{}, false
	}
	for i := range p.PrebuiltArchives {
		if p.PrebuiltArchives[i].FormulaID == id {
			return clonePrebuiltArchivePolicy(p.PrebuiltArchives[i]), true
		}
	}
	return PrebuiltArchivePolicy{}, false
}

func validatePrebuiltArchivePolicies(policies []PrebuiltArchivePolicy) error {
	var errs []error
	if len(policies) == 0 {
		errs = append(errs, errors.New("tap policy must contain the release-bound prebuilt archive authorization"))
	}
	if len(policies) > prebuiltMaxPolicyFormulaEntries {
		errs = append(errs, fmt.Errorf("prebuilt archive policy contains %d Formulae; maximum is %d", len(policies), prebuiltMaxPolicyFormulaEntries))
	}
	if len(policies) != 1 {
		errs = append(errs, fmt.Errorf("prebuilt archive policy must authorize exactly %q", prebuiltA365FormulaID))
	}

	previous := ""
	for i := range policies {
		policy := &policies[i]
		if err := validateExactNonCoreFormulaID(policy.FormulaID); err != nil {
			errs = append(errs, fmt.Errorf("prebuilt archive policy %d Formula ID: %w", i, err))
		}
		if i > 0 && policy.FormulaID <= previous {
			errs = append(errs, errors.New("prebuilt archive Formula IDs must be strictly sorted and unique"))
		}
		previous = policy.FormulaID
		if err := validatePrebuiltArchivePolicy(policy); err != nil {
			errs = append(errs, fmt.Errorf("prebuilt archive policy %q: %w", policy.FormulaID, err))
		}
	}

	if len(policies) == 1 && !reflect.DeepEqual(policies[0], expectedA365PrebuiltArchivePolicy()) {
		errs = append(errs, errors.New("prebuilt archive policy differs from the exact release-bound sozercan/repo/a365 v0.3.3 authorization"))
	}
	return errors.Join(errs...)
}

func validatePrebuiltArchivePolicy(policy *PrebuiltArchivePolicy) error {
	if policy == nil {
		return errors.New("nil prebuilt archive policy")
	}
	var errs []error
	if policy.PolicyVersion != PrebuiltDerivedBottlePolicyVersion {
		errs = append(errs, fmt.Errorf("unsupported derivation policy %q", policy.PolicyVersion))
	}
	if policy.Version != prebuiltA365Version {
		errs = append(errs, fmt.Errorf("version must be exactly %q", prebuiltA365Version))
	}
	if !canonicalSHA256(policy.FormulaSourceDigest) {
		errs = append(errs, errors.New("Formula source digest must be canonical lowercase sha256"))
	}
	if policy.License != "MIT" {
		errs = append(errs, errors.New("prebuilt archive license must be exactly MIT"))
	}
	if !policy.RootOnly {
		errs = append(errs, errors.New("prebuilt archive authorization must be root-only"))
	}
	if !policy.RequireNoBottle {
		errs = append(errs, errors.New("prebuilt archive authorization must require the Formula to have no bottle"))
	}
	if policy.Dependencies == nil {
		errs = append(errs, errors.New("prebuilt archive dependencies must be an explicit empty list"))
	} else if len(policy.Dependencies) != 0 {
		errs = append(errs, errors.New("prebuilt archive Formula must have no dependencies"))
	}
	if err := validatePrebuiltPlatforms(policy.Platforms); err != nil {
		errs = append(errs, err)
	}
	if err := validatePrebuiltArchiveConstraints(&policy.Archive); err != nil {
		errs = append(errs, err)
	}
	if err := validatePrebuiltInstall(&policy.Install, policy.Archive.Members); err != nil {
		errs = append(errs, err)
	}
	if err := validatePrebuiltBinary(&policy.Binary, policy.Platforms); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func validatePrebuiltPlatforms(platforms []PrebuiltArchivePlatformPolicy) error {
	if platforms == nil {
		return errors.New("prebuilt archive platforms must be present")
	}
	var errs []error
	if len(platforms) != 2 {
		errs = append(errs, errors.New("prebuilt archive policy must contain exactly linux/amd64 and linux/arm64"))
	}
	previous := ""
	for i, platform := range platforms {
		if i > 0 && platform.Platform <= previous {
			errs = append(errs, errors.New("prebuilt archive platforms must be strictly sorted and unique"))
		}
		previous = platform.Platform
		if platform.Platform != "linux/amd64" && platform.Platform != "linux/arm64" {
			errs = append(errs, fmt.Errorf("unsupported prebuilt archive platform %q", platform.Platform))
		}
		if err := validatePrebuiltURL(platform.URL); err != nil {
			errs = append(errs, fmt.Errorf("platform %q URL: %w", platform.Platform, err))
		}
		if !canonicalSHA256(platform.SHA256) {
			errs = append(errs, fmt.Errorf("platform %q digest must be canonical lowercase sha256", platform.Platform))
		}
	}
	return errors.Join(errs...)
}

func validatePrebuiltArchiveConstraints(archive *PrebuiltArchiveConstraints) error {
	if archive == nil {
		return errors.New("nil prebuilt archive constraints")
	}
	var errs []error
	if archive.Format != prebuiltArchiveFormat {
		errs = append(errs, fmt.Errorf("archive format must be %q", prebuiltArchiveFormat))
	}
	if !archive.SingleGzipMember {
		errs = append(errs, errors.New("archive must require exactly one gzip member"))
	}
	if archive.MaxCompressedBytes <= 0 || archive.MaxCompressedBytes > prebuiltMaxCompressedBytes {
		errs = append(errs, fmt.Errorf("max compressed bytes must be in [1,%d]", prebuiltMaxCompressedBytes))
	}
	if archive.MaxExpandedBytes <= 0 || archive.MaxExpandedBytes > prebuiltMaxExpandedBytes {
		errs = append(errs, fmt.Errorf("max expanded bytes must be in [1,%d]", prebuiltMaxExpandedBytes))
	}
	if archive.MaxExpansionRatio <= 0 || archive.MaxExpansionRatio > prebuiltMaxExpansionRatio {
		errs = append(errs, fmt.Errorf("max expansion ratio must be in [1,%d]", prebuiltMaxExpansionRatio))
	}
	if archive.MaxEntries <= 0 || archive.MaxEntries > prebuiltMaxEntries {
		errs = append(errs, fmt.Errorf("max entries must be in [1,%d]", prebuiltMaxEntries))
	}
	if archive.MaxFileBytes <= 0 || archive.MaxFileBytes > prebuiltMaxFileBytes || archive.MaxFileBytes > archive.MaxExpandedBytes {
		errs = append(errs, fmt.Errorf("max file bytes must be positive and at most %d and max expanded bytes", prebuiltMaxFileBytes))
	}
	if archive.MaxDepth <= 0 || archive.MaxDepth > prebuiltMaxDepth {
		errs = append(errs, fmt.Errorf("max archive depth must be in [1,%d]", prebuiltMaxDepth))
	}
	if archive.MaxPathBytes <= 0 || archive.MaxPathBytes > prebuiltMaxPathBytes {
		errs = append(errs, fmt.Errorf("max archive path bytes must be in [1,%d]", prebuiltMaxPathBytes))
	}
	if archive.MaxCompressedBytes > 0 && archive.MaxCompressedBytes <= prebuiltMaxCompressedBytes && archive.MaxExpansionRatio > 0 && archive.MaxExpansionRatio <= prebuiltMaxExpansionRatio && archive.MaxExpandedBytes > archive.MaxCompressedBytes*int64(archive.MaxExpansionRatio) {
		errs = append(errs, errors.New("max expanded bytes exceed the bounded compression ratio"))
	}
	if archive.Members == nil {
		errs = append(errs, errors.New("archive members must be present"))
	}
	if len(archive.Members) != archive.MaxEntries {
		errs = append(errs, errors.New("archive member allowlist must exactly fill the entry bound"))
	}

	previous := ""
	seenFolded := make(map[string]struct{}, len(archive.Members))
	for i, member := range archive.Members {
		if i > 0 && member.Path <= previous {
			errs = append(errs, errors.New("archive members must be strictly sorted and unique"))
		}
		previous = member.Path
		if err := validateRelativePolicyPath(member.Path, archive.MaxPathBytes); err != nil {
			errs = append(errs, fmt.Errorf("archive member %q: %w", member.Path, err))
		} else if strings.Count(member.Path, "/")+1 > archive.MaxDepth {
			errs = append(errs, fmt.Errorf("archive member %q exceeds depth %d", member.Path, archive.MaxDepth))
		}
		folded := strings.ToLower(member.Path)
		if _, ok := seenFolded[folded]; ok {
			errs = append(errs, fmt.Errorf("archive member %q has a case-folding collision", member.Path))
		}
		seenFolded[folded] = struct{}{}
		if member.Type != prebuiltArchiveMemberType {
			errs = append(errs, fmt.Errorf("archive member %q must be a regular file", member.Path))
		}
		if _, err := parsePolicyMode(member.Mode); err != nil {
			errs = append(errs, fmt.Errorf("archive member %q mode: %w", member.Path, err))
		}
	}
	return errors.Join(errs...)
}

func validatePrebuiltInstall(install *PrebuiltArchiveInstallPolicy, members []PrebuiltArchiveMemberPolicy) error {
	if install == nil {
		return errors.New("nil prebuilt archive install policy")
	}
	var errs []error
	if err := validateRelativePolicyPath(install.Source, prebuiltMaxPathBytes); err != nil {
		errs = append(errs, fmt.Errorf("install source: %w", err))
	}
	if err := validateRelativePolicyPath(install.Destination, prebuiltMaxInstallPathBytes); err != nil {
		errs = append(errs, fmt.Errorf("install destination: %w", err))
	}
	mode, err := parsePolicyMode(install.Mode)
	if err != nil {
		errs = append(errs, fmt.Errorf("install mode: %w", err))
	} else {
		if mode&0o222 != 0 {
			errs = append(errs, errors.New("installed payload must be non-writable"))
		}
		if mode&0o111 != 0o111 {
			errs = append(errs, errors.New("installed payload must be executable by owner, group, and other"))
		}
	}

	found := false
	for _, member := range members {
		if member.Path == install.Source {
			found = member.Type == prebuiltArchiveMemberType
			break
		}
	}
	if !found {
		errs = append(errs, errors.New("install source must identify one permitted regular archive member"))
	}
	return errors.Join(errs...)
}

func validatePrebuiltBinary(binary *PrebuiltBinaryPolicy, platforms []PrebuiltArchivePlatformPolicy) error {
	if binary == nil {
		return errors.New("nil prebuilt binary policy")
	}
	var errs []error
	if binary.Format != prebuiltBinaryFormat {
		errs = append(errs, fmt.Errorf("binary format must be %q", prebuiltBinaryFormat))
	}
	if binary.Machines == nil {
		errs = append(errs, errors.New("binary machine constraints must be present"))
	}
	if len(binary.Machines) != len(platforms) || len(binary.Machines) != 2 {
		errs = append(errs, errors.New("binary machine constraints must cover exactly both archive platforms"))
	}
	previous := ""
	for i, machine := range binary.Machines {
		if i > 0 && machine.Platform <= previous {
			errs = append(errs, errors.New("binary machine constraints must be strictly sorted and unique"))
		}
		previous = machine.Platform
		if i < len(platforms) && machine.Platform != platforms[i].Platform {
			errs = append(errs, errors.New("binary machine platform order must match archive platform order"))
		}
		switch machine.Platform {
		case "linux/amd64":
			if machine.Machine != "x86_64" {
				errs = append(errs, errors.New("linux/amd64 binary machine must be x86_64"))
			}
		case "linux/arm64":
			if machine.Machine != "aarch64" {
				errs = append(errs, errors.New("linux/arm64 binary machine must be aarch64"))
			}
		default:
			errs = append(errs, fmt.Errorf("unsupported binary machine platform %q", machine.Platform))
		}
	}
	if binary.Linkage != prebuiltBinaryLinkage {
		errs = append(errs, errors.New("prebuilt executable must be statically linked"))
	}
	if binary.Interpreter != prebuiltBinaryForbidden {
		errs = append(errs, errors.New("ELF interpreter must be forbidden"))
	}
	if binary.NeededLibraries == nil {
		errs = append(errs, errors.New("needed libraries must be an explicit empty list"))
	} else if len(binary.NeededLibraries) != 0 {
		errs = append(errs, errors.New("prebuilt executable must have no needed libraries"))
	}
	if binary.RPath != prebuiltBinaryForbidden {
		errs = append(errs, errors.New("ELF RPATH and RUNPATH must be forbidden"))
	}
	if binary.WritableExecutableSegments != prebuiltBinaryForbidden {
		errs = append(errs, errors.New("writable executable segments must be forbidden"))
	}
	if binary.GoModule != "github.com/sozercan/a365cli" {
		errs = append(errs, errors.New("Go module must be exactly github.com/sozercan/a365cli"))
	}
	if binary.CGOEnabled == nil {
		errs = append(errs, errors.New("CGO constraint must be explicit"))
	} else if *binary.CGOEnabled {
		errs = append(errs, errors.New("CGO must be disabled"))
	}
	return errors.Join(errs...)
}

func validateExactNonCoreFormulaID(value string) error {
	id, err := formulaid.Parse(value)
	if err != nil {
		return err
	}
	if id.String() != value {
		return errors.New("Formula ID must be fully qualified and canonical")
	}
	if id.Tap() == formulaid.CoreTap() {
		return errors.New("prebuilt archive policy cannot authorize homebrew/core")
	}
	return nil
}

func validatePrebuiltURL(raw string) error {
	if raw == "" || len(raw) > prebuiltMaxURLBytes {
		return fmt.Errorf("URL length must be in [1,%d]", prebuiltMaxURLBytes)
	}
	if strings.Contains(raw, "%") || strings.Contains(raw, "\\") {
		return errors.New("URL must not contain escaping or backslashes")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.Hostname() != "github.com" || u.Host != u.Hostname() {
		return errors.New("URL must use HTTPS on github.com without a port")
	}
	if u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("URL must not contain userinfo, opaque data, escaping, a query, or a fragment")
	}
	if u.Path == "" || path.Clean(u.Path) != u.Path || strings.Contains(u.Path, "//") {
		return errors.New("URL path must be non-empty and canonical")
	}
	return nil
}

func validateRelativePolicyPath(value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes {
		return fmt.Errorf("path length must be in [1,%d]", maxBytes)
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") || path.Clean(value) != value {
		return errors.New("path must be canonical and relative")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return errors.New("path contains an invalid component")
		}
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("._+@-/", rune(c))) {
			return errors.New("path contains a non-canonical character")
		}
	}
	return nil
}

func parsePolicyMode(value string) (uint64, error) {
	if len(value) != 4 || value[0] != '0' {
		return 0, errors.New("mode must be four canonical octal digits")
	}
	mode, err := strconv.ParseUint(value, 8, 12)
	if err != nil || mode > 0o777 {
		return 0, errors.New("mode must contain only permission bits")
	}
	return mode, nil
}

func canonicalSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for i := len("sha256:"); i < len(value); i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func clonePrebuiltArchivePolicy(policy PrebuiltArchivePolicy) PrebuiltArchivePolicy {
	policy.Dependencies = slices.Clone(policy.Dependencies)
	policy.Platforms = slices.Clone(policy.Platforms)
	policy.Archive.Members = slices.Clone(policy.Archive.Members)
	policy.Binary.Machines = slices.Clone(policy.Binary.Machines)
	policy.Binary.NeededLibraries = slices.Clone(policy.Binary.NeededLibraries)
	if policy.Binary.CGOEnabled != nil {
		value := *policy.Binary.CGOEnabled
		policy.Binary.CGOEnabled = &value
	}
	return policy
}

func expectedA365PrebuiltArchivePolicy() PrebuiltArchivePolicy {
	cgoEnabled := false
	return PrebuiltArchivePolicy{
		FormulaID:           prebuiltA365FormulaID,
		PolicyVersion:       PrebuiltDerivedBottlePolicyVersion,
		Version:             prebuiltA365Version,
		FormulaSourceDigest: "sha256:d6c00086e77905de6f2c93c59ff2b560101925dc77a1f3094468fd154a89e997",
		License:             "MIT",
		RootOnly:            true,
		RequireNoBottle:     true,
		Dependencies:        []string{},
		Platforms: []PrebuiltArchivePlatformPolicy{
			{Platform: "linux/amd64", URL: "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz", SHA256: "sha256:71461c31e350cabf4e718a5e1331b39a395a6dc9183bb3ea5922f0fac67404ce"},
			{Platform: "linux/arm64", URL: "https://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_arm64.tar.gz", SHA256: "sha256:fe7e6b2efa8bab9b804e401e3664dcb6adbb4e2cdcf7d2049b05e645f3eccc83"},
		},
		Archive: PrebuiltArchiveConstraints{
			Format:             prebuiltArchiveFormat,
			SingleGzipMember:   true,
			MaxCompressedBytes: prebuiltMaxCompressedBytes,
			MaxExpandedBytes:   prebuiltMaxExpandedBytes,
			MaxExpansionRatio:  prebuiltMaxExpansionRatio,
			MaxEntries:         prebuiltMaxEntries,
			MaxFileBytes:       prebuiltMaxFileBytes,
			MaxDepth:           prebuiltMaxDepth,
			MaxPathBytes:       prebuiltMaxPathBytes,
			Members: []PrebuiltArchiveMemberPolicy{
				{Path: "LICENSE", Type: prebuiltArchiveMemberType, Mode: "0644"},
				{Path: "README.md", Type: prebuiltArchiveMemberType, Mode: "0644"},
				{Path: "a365", Type: prebuiltArchiveMemberType, Mode: "0755"},
			},
		},
		Install: PrebuiltArchiveInstallPolicy{Source: "a365", Destination: "bin/a365", Mode: "0555"},
		Binary: PrebuiltBinaryPolicy{
			Format: prebuiltBinaryFormat,
			Machines: []PrebuiltBinaryMachinePolicy{
				{Platform: "linux/amd64", Machine: "x86_64"},
				{Platform: "linux/arm64", Machine: "aarch64"},
			},
			Linkage:                    prebuiltBinaryLinkage,
			Interpreter:                prebuiltBinaryForbidden,
			NeededLibraries:            []string{},
			RPath:                      prebuiltBinaryForbidden,
			WritableExecutableSegments: prebuiltBinaryForbidden,
			GoModule:                   "github.com/sozercan/a365cli",
			CGOEnabled:                 &cgoEnabled,
		},
	}
}
