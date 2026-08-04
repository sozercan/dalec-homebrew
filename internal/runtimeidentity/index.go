// Package runtimeidentity builds collision-checked indexes that separate V2
// Formula graph identity from Homebrew filesystem rack identity.
package runtimeidentity

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type Index struct {
	byID   map[resolution.FormulaID]resolution.NodeV2
	byRack map[string]resolution.FormulaID
	ids    []resolution.FormulaID
}

func New(nodes []resolution.NodeV2) (*Index, error) {
	if len(nodes) == 0 {
		return nil, errors.New("runtime identity index requires at least one node")
	}
	index := &Index{byID: make(map[resolution.FormulaID]resolution.NodeV2, len(nodes)), byRack: make(map[string]resolution.FormulaID, len(nodes)), ids: make([]resolution.FormulaID, 0, len(nodes))}
	for _, node := range nodes {
		parsed, err := formulaid.Parse(node.ID.String())
		if err != nil || parsed.String() != node.ID.String() {
			return nil, fmt.Errorf("invalid canonical Formula ID %q", node.ID)
		}
		// A migration may retain a distinct exact receipt identity, but the rack
		// must always be a non-empty safe Formula component. Reuse the shared
		// parser by attaching it to the node's tap.
		if _, err := formulaid.New(parsed.Tap(), node.Name); err != nil {
			return nil, fmt.Errorf("Formula %s rack %q: %w", node.ID, node.Name, err)
		}
		if _, duplicate := index.byID[node.ID]; duplicate {
			return nil, fmt.Errorf("duplicate Formula ID %s", node.ID)
		}
		if previous, collision := index.byRack[node.Name]; collision {
			return nil, fmt.Errorf("rack name %q is shared by %s and %s", node.Name, previous, node.ID)
		}
		index.byID[node.ID] = cloneNode(node)
		index.byRack[node.Name] = node.ID
		index.ids = append(index.ids, node.ID)
	}
	slices.Sort(index.ids)
	return index, nil
}

func (index *Index) Node(id resolution.FormulaID) (resolution.NodeV2, bool) {
	if index == nil {
		return resolution.NodeV2{}, false
	}
	node, ok := index.byID[id]
	return cloneNode(node), ok
}

func (index *Index) FormulaIDForRack(rack string) (resolution.FormulaID, bool) {
	if index == nil {
		return "", false
	}
	id, ok := index.byRack[rack]
	return id, ok
}

func (index *Index) IDs() []resolution.FormulaID {
	if index == nil {
		return nil
	}
	return slices.Clone(index.ids)
}

func cloneNode(node resolution.NodeV2) resolution.NodeV2 {
	node.Dependencies = slices.Clone(node.Dependencies)
	node.ExecutablePaths = slices.Clone(node.ExecutablePaths)
	node.Bottle.Tab.ChangedFiles = slices.Clone(node.Bottle.Tab.ChangedFiles)
	node.Bottle.Tab.Dependencies = slices.Clone(node.Bottle.Tab.Dependencies)
	node.Bottle.SelectedAnnotations = slices.Clone(node.Bottle.SelectedAnnotations)
	if node.Bottle.Transport.HTTPS != nil {
		transport := *node.Bottle.Transport.HTTPS
		transport.AllowedRedirectHosts = slices.Clone(transport.AllowedRedirectHosts)
		node.Bottle.Transport.HTTPS = &transport
	}
	if node.Bottle.Transport.OCI != nil {
		transport := *node.Bottle.Transport.OCI
		node.Bottle.Transport.OCI = &transport
	}
	return node
}
