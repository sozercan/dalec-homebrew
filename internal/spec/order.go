package spec

import (
	"fmt"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
	"gopkg.in/yaml.v3"
)

// RuntimeDependencyOrder extracts declaration order without replacing Dalec's
// authoritative decoder. It supports both the list shorthand and map form.
func RuntimeDependencyOrder(data []byte, targetKey string) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse dependency order: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	global := mappingPath(root, "dependencies", "runtime")
	selected := mappingPath(root, "targets", targetKey, "dependencies", "runtime")
	if nodeLength(selected) > 0 {
		return namesFromNode(selected), nil
	}
	return namesFromNode(global), nil
}

func PreflightFormulaNames(data []byte, targetKey string, capability ...Capabilities) error {
	order, err := RuntimeDependencyOrder(data, targetKey)
	if err != nil {
		return err
	}
	ids, err := formulaid.ParseRoots(order)
	if err != nil {
		return fmt.Errorf("runtime roots: %w", err)
	}
	if err := validateRuntimeRootLimits(ids); err != nil {
		return fmt.Errorf("runtime roots: %w", err)
	}
	caps := effectiveCapabilities(capability)
	for i, id := range ids {
		if !caps.NonCoreTaps && !isCoreFormula(id) {
			return fmt.Errorf("runtime dependency %q requires release-bound non-core capability bindings", order[i])
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

func nodeLength(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	if n.Kind == yaml.MappingNode {
		return len(n.Content) / 2
	}
	return len(n.Content)
}

func namesFromNode(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	var out []string
	switch n.Kind {
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				out = append(out, c.Value)
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Kind == yaml.ScalarNode && n.Content[i].Value != "<<" {
				out = append(out, n.Content[i].Value)
			}
		}
	}
	return out
}
