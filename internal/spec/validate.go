// Package spec validates the deliberately narrow Dalec contract accepted by
// the Homebrew runtime frontend.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/project-dalec/dalec"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

var supportedArches = map[string]struct{}{"amd64": {}, "arm64": {}}

const (
	maxRuntimeRoots    = 256
	maxNonCoreRootTaps = 16
)

type Root struct {
	// Name is the legacy core lookup name or, for non-core roots, the canonical
	// Formula ID. New code should use ID for identity decisions.
	Name      string
	Requested string
	ID        formulaid.FormulaID
}

// Capabilities are release-bound frontend features. They are passed as a
// variadic option so existing V1 callers remain source compatible and default
// to the fail-closed core-only contract.
type Capabilities struct {
	NonCoreTaps bool
}

// Forwarding describes the gateway routing metadata that must match the
// selected target when the spec was forwarded by an upstream Dalec frontend.
// Source is the executing dalec-homebrew gateway image, not the parent
// frontend identity.
type Forwarding struct {
	Source  string
	CmdLine string
}

type Selection struct {
	Roots []Root
	Tests []*dalec.TestSpec
	Image *dalec.ImageConfig
}

// Validate validates the Homebrew runtime contract without gateway-routing
// checks. Frontend builds use ValidateForwarded so routing metadata is always
// authenticated against the executing child gateway source.
func Validate(s *dalec.Spec, targetKey, arch string, capability ...Capabilities) (*Selection, error) {
	return validate(s, targetKey, arch, nil, capability...)
}

// ValidateForwarded validates the supported contract for a spec target routed
// through an upstream Dalec frontend. The target's frontend block is routing
// metadata and must identify this exact gateway invocation.
func ValidateForwarded(s *dalec.Spec, targetKey, arch string, forwarding Forwarding, capability ...Capabilities) (*Selection, error) {
	return validate(s, targetKey, arch, &forwarding, capability...)
}

func validate(s *dalec.Spec, targetKey, arch string, forwarding *Forwarding, capability ...Capabilities) (*Selection, error) {
	caps := effectiveCapabilities(capability)
	if s == nil {
		return nil, errors.New("nil Dalec spec")
	}
	if err := rejectObsoleteForwardingExtension(s); err != nil {
		return nil, err
	}
	if _, ok := supportedArches[arch]; !ok {
		return nil, fmt.Errorf("unsupported target architecture %q; V1 supports amd64 and arm64", arch)
	}
	// This frontend materializes runtime dependencies directly and does not build
	// a package from the Dalec spec, so package metadata is optional.
	var errs []error
	if len(s.Sources) > 0 {
		errs = append(errs, errors.New("sources are not supported"))
	}
	if len(s.Patches) > 0 {
		errs = append(errs, errors.New("patches are not supported"))
	}
	if len(s.Build.Steps) > 0 || len(s.Build.Env) > 0 || len(s.Build.Caches) > 0 || s.Build.NetworkMode != "" {
		errs = append(errs, errors.New("build steps, environment, caches, and network configuration are not supported"))
	}
	if !jsonEmpty(s.Artifacts) {
		errs = append(errs, errors.New("package artifacts are not supported"))
	}
	if s.PackageConfig != nil {
		errs = append(errs, errors.New("package configuration is not supported"))
	}
	if len(s.Provides) > 0 {
		errs = append(errs, errors.New("provides metadata is not supported"))
	}
	if len(s.Replaces) > 0 {
		errs = append(errs, errors.New("replaces metadata is not supported"))
	}
	if len(s.Conflicts) > 0 {
		errs = append(errs, errors.New("conflicts metadata is not supported"))
	}

	validateDeps := func(scope string, deps *dalec.PackageDependencies) {
		if deps == nil {
			return
		}
		if len(deps.Build) > 0 {
			errs = append(errs, fmt.Errorf("%s build dependencies are not supported", scope))
		}
		if len(deps.Recommends) > 0 {
			errs = append(errs, fmt.Errorf("%s recommended dependencies are not supported", scope))
		}
		if len(deps.Test) > 0 {
			errs = append(errs, fmt.Errorf("%s test dependencies are not supported", scope))
		}
		if len(deps.Sysext) > 0 {
			errs = append(errs, fmt.Errorf("%s sysext dependencies are not supported", scope))
		}
		if len(deps.ExtraRepos) > 0 {
			errs = append(errs, fmt.Errorf("%s extra package repositories are not supported", scope))
		}
		for _, name := range sortedDependencyNames(deps.Runtime) {
			constraint := deps.Runtime[name]
			id, err := formulaid.Parse(name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s runtime dependency: %w", scope, err))
			} else if !caps.NonCoreTaps && !isCoreFormula(id) {
				errs = append(errs, fmt.Errorf("%s runtime dependency %q requires release-bound non-core capability bindings", scope, name))
			}
			if len(constraint.Version) > 0 {
				errs = append(errs, fmt.Errorf("%s runtime dependency %q has version constraints; historical versions and version ranges are not supported", scope, name))
			}
			seenArch := map[string]struct{}{}
			for _, a := range constraint.Arch {
				if _, duplicate := seenArch[a]; duplicate {
					errs = append(errs, fmt.Errorf("%s runtime dependency %q repeats architecture %q", scope, name, a))
				}
				seenArch[a] = struct{}{}
				if _, ok := supportedArches[a]; !ok {
					errs = append(errs, fmt.Errorf("%s runtime dependency %q uses unsupported architecture %q", scope, name, a))
				}
			}
		}
	}
	validateDeps("global", s.Dependencies)

	selectedTarget, hasTarget := s.Targets[targetKey]
	if forwarding != nil {
		if targetKey == "" {
			errs = append(errs, errors.New("forwarded invocation is missing the selected Dalec target"))
		} else if !hasTarget {
			errs = append(errs, fmt.Errorf("forwarded target %q is not defined in the Dalec spec", targetKey))
		}
	}
	if hasTarget {
		validateDeps("target "+targetKey, selectedTarget.Dependencies)
		if forwarding != nil {
			if selectedTarget.Frontend == nil {
				errs = append(errs, fmt.Errorf("forwarded target %q does not declare frontend routing metadata", targetKey))
			} else {
				if forwarding.Source == "" {
					errs = append(errs, errors.New("forwarded invocation is missing the gateway source"))
				} else if selectedTarget.Frontend.Image != forwarding.Source {
					errs = append(errs, fmt.Errorf("forwarded target %q frontend image %q does not match invoking gateway source %q", targetKey, selectedTarget.Frontend.Image, forwarding.Source))
				}
				if selectedTarget.Frontend.CmdLine != "" {
					errs = append(errs, fmt.Errorf("forwarded target %q frontend cmdline must be empty", targetKey))
				}
				if forwarding.CmdLine != "" {
					errs = append(errs, fmt.Errorf("forwarded invocation cmdline must be empty, got %q", forwarding.CmdLine))
				}
			}
		}
		if selectedTarget.PackageConfig != nil {
			errs = append(errs, fmt.Errorf("target %q package configuration is not supported", targetKey))
		}
		if selectedTarget.Artifacts != nil && !jsonEmpty(*selectedTarget.Artifacts) {
			errs = append(errs, fmt.Errorf("target %q artifacts are not supported", targetKey))
		}
		if len(selectedTarget.Provides) > 0 || len(selectedTarget.Replaces) > 0 || len(selectedTarget.Conflicts) > 0 {
			errs = append(errs, fmt.Errorf("target %q provides, replaces, and conflicts metadata are not supported", targetKey))
		}
	}
	if err := ValidateImage("global", s.Image); err != nil {
		errs = append(errs, err)
	}
	if hasTarget {
		if err := ValidateImage("target "+targetKey, selectedTarget.Image); err != nil {
			errs = append(errs, err)
		}
	}

	tests := append([]*dalec.TestSpec(nil), s.Tests...)
	if hasTarget {
		tests = append(tests, selectedTarget.Tests...)
	}
	for i, test := range tests {
		if test == nil {
			errs = append(errs, fmt.Errorf("test %d is null", i))
			continue
		}
		if strings.TrimSpace(test.Name) == "" {
			errs = append(errs, fmt.Errorf("test %d has an empty name", i))
		}
		if len(test.Mounts) > 0 {
			errs = append(errs, fmt.Errorf("test %q uses mounts; test mounts and source fetching are not supported", test.Name))
		}
	}

	deps := s.GetPackageDeps(targetKey).GetRuntime()
	names := sortedDependencyNames(deps)
	ids, err := formulaid.ParseRoots(names)
	if err != nil {
		errs = append(errs, fmt.Errorf("runtime roots: %w", err))
	} else if err := validateRuntimeRootLimits(ids); err != nil {
		errs = append(errs, fmt.Errorf("runtime roots: %w", err))
	}
	roots := make([]Root, 0, len(names))
	if err == nil {
		for i, name := range names {
			constraint, ok := deps[name]
			if !ok {
				continue
			}
			id := ids[i]
			if !caps.NonCoreTaps && !isCoreFormula(id) {
				continue
			}
			if appliesTo(constraint.Arch, arch) {
				lookup := id.String()
				if isCoreFormula(id) {
					lookup = id.Name()
				}
				roots = append(roots, Root{Name: lookup, Requested: name, ID: id})
			}
		}
		slices.SortFunc(roots, func(a, b Root) int {
			return strings.Compare(a.ID.String(), b.ID.String())
		})
	}
	if len(roots) == 0 {
		errs = append(errs, fmt.Errorf("target linux/%s has no applicable runtime roots", arch))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Selection{Roots: roots, Tests: tests, Image: dalec.MergeSpecImage(s, targetKey)}, nil
}

func ValidateFormulaName(name string) error {
	_, err := formulaid.Parse(name)
	return err
}

func isCoreFormula(id formulaid.FormulaID) bool {
	return id.Tap() == formulaid.CoreTap()
}

func validateRuntimeRootLimits(ids []formulaid.FormulaID) error {
	var errs []error
	if len(ids) > maxRuntimeRoots {
		errs = append(errs, fmt.Errorf("%d canonical runtime roots exceed maximum %d", len(ids), maxRuntimeRoots))
	}
	nonCoreTaps := make(map[formulaid.Tap]struct{})
	for _, id := range ids {
		if !isCoreFormula(id) {
			nonCoreTaps[id.Tap()] = struct{}{}
		}
	}
	if len(nonCoreTaps) > maxNonCoreRootTaps {
		errs = append(errs, fmt.Errorf("%d distinct non-core root taps exceed maximum %d", len(nonCoreTaps), maxNonCoreRootTaps))
	}
	return errors.Join(errs...)
}

func effectiveCapabilities(values []Capabilities) Capabilities {
	if len(values) == 0 {
		return Capabilities{}
	}
	return values[0]
}

func ValidateImage(scope string, img *dalec.ImageConfig) error {
	if img == nil {
		return nil
	}
	var errs []error
	if img.Base != "" || len(img.Bases) > 0 {
		errs = append(errs, fmt.Errorf("%s image.base and image.bases are not supported", scope))
	}
	if img.Post != nil {
		errs = append(errs, fmt.Errorf("%s image.post is not supported", scope))
	}
	for volume := range img.Volumes {
		clean := path.Clean(volume)
		if !strings.HasPrefix(clean, "/") || clean != volume {
			errs = append(errs, fmt.Errorf("%s volume %q must be an absolute clean path", scope, volume))
			continue
		}
		for _, reserved := range []string{"/home/linuxbrew/.linuxbrew", "/usr/share/dalec-homebrew", "/etc/passwd", "/etc/group"} {
			if overlaps(clean, reserved) {
				errs = append(errs, fmt.Errorf("%s volume %q overlaps required runtime path %q", scope, volume, reserved))
			}
		}
	}
	return errors.Join(errs...)
}

func sortedDependencyNames(deps dalec.PackageDependencyList) []string {
	out := make([]string, 0, len(deps))
	for name := range deps {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

const obsoleteForwardingExtensionKey = "x-dalec-homebrew"

func rejectObsoleteForwardingExtension(s *dalec.Spec) error {
	var value any
	err := s.Ext(obsoleteForwardingExtensionKey, &value, func(*dalec.ExtDecodeConfig) {})
	if errors.Is(err, dalec.ErrNodeNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode unsupported extension %q: %w", obsoleteForwardingExtensionKey, err)
	}
	return fmt.Errorf("top-level extension %q is unsupported", obsoleteForwardingExtensionKey)
}

func appliesTo(arches []string, arch string) bool {
	if len(arches) == 0 {
		return true
	}
	return slices.Contains(arches, arch)
}

func overlaps(a, b string) bool {
	if a == "/" || b == "/" {
		return true
	}
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func jsonEmpty(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var x any
	if json.Unmarshal(b, &x) != nil {
		return false
	}
	return emptyJSONValue(x)
}

func emptyJSONValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case float64:
		return x == 0
	case []any:
		if len(x) == 0 {
			return true
		}
		for _, e := range x {
			if !emptyJSONValue(e) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, e := range x {
			if !emptyJSONValue(e) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
