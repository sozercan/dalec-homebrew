package spec

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
	"gopkg.in/yaml.v3"
)

// RuntimeDependencyNames validates the raw dependency shape and returns the
// selected runtime dependency keys in deterministic lexical order. YAML map
// declaration order is not part of the supported contract.
func RuntimeDependencyNames(data []byte, targetKey string) ([]string, error) {
	names, err := runtimeDependencyNames(data, targetKey)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

func runtimeDependencyNames(data []byte, targetKey string) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse runtime dependencies: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	global, err := runtimeDependencyMap("global", mappingPath(root, "dependencies"))
	if err != nil {
		return nil, err
	}
	var selected *yaml.Node
	if targetKey != "" {
		selected, err = runtimeDependencyMap("target "+targetKey, mappingPath(root, "targets", targetKey, "dependencies"))
		if err != nil {
			return nil, err
		}
	}
	if selected != nil {
		return namesFromNode(selected)
	}
	return namesFromNode(global)
}

func PreflightFormulaNames(data []byte, targetKey string, capability ...Capabilities) error {
	names, err := RuntimeDependencyNames(data, targetKey)
	if err != nil {
		return err
	}
	ids, err := formulaid.ParseRoots(names)
	if err != nil {
		return fmt.Errorf("runtime roots: %w", err)
	}
	if err := validateRuntimeRootLimits(ids); err != nil {
		return fmt.Errorf("runtime roots: %w", err)
	}
	caps := effectiveCapabilities(capability)
	for i, id := range ids {
		if !caps.NonCoreTaps && !isCoreFormula(id) {
			return fmt.Errorf("runtime dependency %q requires release-bound non-core capability bindings", names[i])
		}
	}
	return nil
}

func mappingPath(n *yaml.Node, keys ...string) *yaml.Node {
	for _, key := range keys {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				next = n.Content[i+1]
				break
			}
		}
		n = next
	}
	return n
}

func runtimeDependencyMap(scope string, dependencies *yaml.Node) (*yaml.Node, error) {
	if dependencies == nil {
		return nil, nil
	}
	if dependencies.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s dependencies must use map form", scope)
	}
	runtime := mappingPath(dependencies, "runtime")
	if runtime == nil || runtime.Kind != yaml.MappingNode || len(runtime.Content) == 0 {
		return nil, fmt.Errorf("%s dependencies.runtime must use map form and contain at least one entry", scope)
	}
	return runtime, nil
}

func namesFromNode(n *yaml.Node) ([]string, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, errors.New("dependencies.runtime must use map form")
	}
	out := make([]string, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Kind == yaml.ScalarNode && n.Content[i].Value != "<<" {
			out = append(out, n.Content[i].Value)
		}
	}
	return out, nil
}
