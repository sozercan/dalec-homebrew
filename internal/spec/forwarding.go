package spec

import (
	"errors"
	"fmt"
	"slices"

	"github.com/project-dalec/dalec"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

const (
	ForwardingExtensionKey           = "x-dalec-homebrew"
	ForwardingExtensionSchemaVersion = "dalec-homebrew-forwarding/v1"
)

// ForwardingMetadata preserves invocation data that upstream Dalec cannot
// represent in its typed map-based dependency model. Dalec retains top-level
// x-* extension nodes when it reserializes a forwarded spec.
type ForwardingMetadata struct {
	SchemaVersion          string   `yaml:"schema_version" json:"schema_version"`
	Target                 string   `yaml:"target" json:"target"`
	RuntimeDependencyOrder []string `yaml:"runtime_dependency_order" json:"runtime_dependency_order"`
}

// ForwardingOrder returns the exact requested-root order authenticated by the
// forwarded spec extension. The order must be a complete permutation of the
// selected target's runtime dependency keys; missing or additional entries fail
// instead of falling back to map iteration or serializer order.
func ForwardingOrder(s *dalec.Spec, targetKey string) ([]string, error) {
	if s == nil {
		return nil, errors.New("nil Dalec spec")
	}
	var metadata ForwardingMetadata
	if err := s.Ext(ForwardingExtensionKey, &metadata, func(*dalec.ExtDecodeConfig) {}); err != nil {
		if errors.Is(err, dalec.ErrNodeNotFound) {
			return nil, fmt.Errorf("required extension %q is missing", ForwardingExtensionKey)
		}
		return nil, fmt.Errorf("decode extension %q: %w", ForwardingExtensionKey, err)
	}
	var errs []error
	if metadata.SchemaVersion != ForwardingExtensionSchemaVersion {
		errs = append(errs, fmt.Errorf("extension schema_version must be exactly %q", ForwardingExtensionSchemaVersion))
	}
	if metadata.Target != targetKey {
		errs = append(errs, fmt.Errorf("extension target %q does not match selected target %q", metadata.Target, targetKey))
	}
	deps := s.GetPackageDeps(targetKey).GetRuntime()
	if len(metadata.RuntimeDependencyOrder) != len(deps) {
		errs = append(errs, fmt.Errorf("extension runtime_dependency_order has %d entries; selected runtime dependencies have %d", len(metadata.RuntimeDependencyOrder), len(deps)))
	}
	ids, err := formulaid.ParseRoots(metadata.RuntimeDependencyOrder)
	if err != nil {
		errs = append(errs, fmt.Errorf("extension runtime_dependency_order: %w", err))
	} else if err := validateRuntimeRootLimits(ids); err != nil {
		errs = append(errs, fmt.Errorf("extension runtime_dependency_order: %w", err))
	}
	seen := make(map[string]struct{}, len(metadata.RuntimeDependencyOrder))
	for _, name := range metadata.RuntimeDependencyOrder {
		if _, duplicate := seen[name]; duplicate {
			errs = append(errs, fmt.Errorf("extension runtime_dependency_order repeats %q", name))
			continue
		}
		seen[name] = struct{}{}
		if _, ok := deps[name]; !ok {
			errs = append(errs, fmt.Errorf("extension runtime_dependency_order contains undeclared dependency %q", name))
		}
	}
	var missing []string
	for name := range deps {
		if _, ok := seen[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		errs = append(errs, fmt.Errorf("extension runtime_dependency_order omits dependencies %v", missing))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return slices.Clone(metadata.RuntimeDependencyOrder), nil
}
