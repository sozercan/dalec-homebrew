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
)

var supportedArches = map[string]struct{}{"amd64": {}, "arm64": {}}

type Root struct {
	Name string
}

type Selection struct {
	Roots []Root
	Tests []*dalec.TestSpec
	Image *dalec.ImageConfig
}

func Validate(s *dalec.Spec, targetKey, arch string, declarationOrder []string) (*Selection, error) {
	if s == nil {
		return nil, errors.New("nil Dalec spec")
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
		for name, constraint := range deps.Runtime {
			if err := ValidateFormulaName(name); err != nil {
				errs = append(errs, fmt.Errorf("%s runtime dependency: %w", scope, err))
			}
			if len(constraint.Version) > 0 {
				errs = append(errs, fmt.Errorf("%s runtime dependency %q has version constraints; historical and ranged resolution is a V2 feature", scope, name))
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
	if hasTarget {
		validateDeps("target "+targetKey, selectedTarget.Dependencies)
		if selectedTarget.Frontend != nil {
			errs = append(errs, fmt.Errorf("target %q frontend forwarding is not supported by this frontend", targetKey))
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
	order := orderedNames(deps, declarationOrder)
	roots := make([]Root, 0, len(order))
	for _, name := range order {
		constraint, ok := deps[name]
		if !ok {
			continue
		}
		if appliesTo(constraint.Arch, arch) {
			roots = append(roots, Root{Name: name})
		}
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
	if strings.TrimSpace(name) != name || name == "" {
		return fmt.Errorf("invalid Formula name %q", name)
	}
	if strings.ContainsAny(name, `/\\:`) {
		return fmt.Errorf("Formula name %q contains tap or path syntax", name)
	}
	if strings.Count(name, "@") > 1 || strings.HasPrefix(name, "@") || strings.HasSuffix(name, "@") {
		return fmt.Errorf("Formula name %q has invalid versioned-Formula syntax", name)
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("+_.@-", r)) {
			return fmt.Errorf("Formula name %q contains unsupported character %q", name, r)
		}
	}
	if !((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= '0' && name[0] <= '9')) {
		return fmt.Errorf("Formula name %q has invalid first character", name)
	}
	return nil
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

func orderedNames(deps dalec.PackageDependencyList, preferred []string) []string {
	seen := make(map[string]struct{}, len(deps))
	out := make([]string, 0, len(deps))
	for _, name := range preferred {
		if _, ok := deps[name]; !ok {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	var rest []string
	for name := range deps {
		if _, ok := seen[name]; !ok {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
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
