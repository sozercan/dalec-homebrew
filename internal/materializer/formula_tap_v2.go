package materializer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimeidentity"
)

const V2TapTrustFileName = "trust.json"

type StagedFormulaEvidenceV2 struct {
	ID     resolution.FormulaID `json:"id"`
	Tap    resolution.TapID     `json:"tap"`
	Name   string               `json:"name"`
	Path   string               `json:"path"`
	SHA256 string               `json:"sha256"`
	Size   int64                `json:"size"`
}

// FormulaTapPathV2 returns the protected synthetic tap path for a V2 node.
// Non-core taps deliberately use only the default GitHub repository layout.
func FormulaTapPathV2(node resolution.NodeV2) (string, error) {
	id, err := formulaid.Parse(node.ID.String())
	if err != nil || id.String() != node.ID.String() {
		return "", fmt.Errorf("invalid canonical Formula ID %q", node.ID)
	}
	if node.Tap.String() != id.Tap().String() || node.Name != id.Name() {
		return "", fmt.Errorf("Formula %q tap/rack identity is inconsistent", node.ID)
	}
	if id.Tap() == formulaid.CoreTap() {
		shard, filename, err := coreTapFormulaLocation(node.Name)
		if err != nil {
			return "", err
		}
		return filepath.ToSlash(filepath.Join("Homebrew", "Library", "Taps", "homebrew", "homebrew-core", "Formula", shard, filename)), nil
	}
	return filepath.ToSlash(filepath.Join("Homebrew", "Library", "Taps", id.Tap().Owner(), "homebrew-"+id.Tap().Name(), "Formula", node.Name+".rb")), nil
}

// V2TapTrustFile returns Homebrew's canonical invocation-local trust store containing exactly the selected non-core Formula IDs.
func V2TapTrustFile(record *resolution.RecordV2) ([]byte, error) {
	if record == nil {
		return nil, errors.New("nil V2 resolution")
	}
	if _, err := runtimeidentity.New(record.Nodes); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(record.Nodes))
	for _, node := range record.Nodes {
		id, err := formulaid.Parse(node.ID.String())
		if err != nil || id.String() != node.ID.String() {
			return nil, fmt.Errorf("invalid Formula ID %q", node.ID)
		}
		if id.Tap() != formulaid.CoreTap() {
			ids = append(ids, id.String())
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	store := struct {
		TrustedFormulae []string `json:"trustedformulae"`
	}{TrustedFormulae: ids}
	data, err := json.Marshal(store)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// StageFormulaeV2 publishes verified bottle-embedded Formula sources into
// sealed synthetic tap trees. current tap source digests are intentionally not
// compared: the catalog source and bottle-embedded source represent distinct
// lifecycle points.
func StageFormulaeV2(prefix string, record *resolution.RecordV2, verified map[resolution.FormulaID]bottle.Result) ([]StagedFormulaEvidenceV2, error) {
	if record == nil {
		return nil, errors.New("nil V2 resolution")
	}
	if err := resolution.ValidateV2(record); err != nil {
		return nil, err
	}
	if _, err := runtimeidentity.New(record.Nodes); err != nil {
		return nil, err
	}
	prefix, err := filepath.Abs(prefix)
	if err != nil {
		return nil, err
	}
	if err := requireRealDirectory(prefix); err != nil {
		return nil, err
	}
	evidence := make([]StagedFormulaEvidenceV2, 0, len(record.Nodes))
	for _, node := range record.Nodes {
		result, ok := verified[node.ID]
		if !ok {
			return nil, fmt.Errorf("Formula %q has no verified bottle result", node.ID)
		}
		expectedBottlePath := path.Join(result.KegPrefix, ".brew", node.Name+".rb")
		if result.Name != node.Name || result.PkgVersion != node.PkgVersion || result.Formula.Path != expectedBottlePath {
			return nil, fmt.Errorf("Formula %q verified source identity is inconsistent", node.ID)
		}
		if int64(len(result.FormulaSource)) != result.Formula.Size {
			return nil, fmt.Errorf("Formula %q source size does not match verified evidence", node.ID)
		}
		sum := sha256.Sum256(result.FormulaSource)
		actual := "sha256:" + hex.EncodeToString(sum[:])
		if actual != result.Formula.SHA256 {
			return nil, fmt.Errorf("Formula %q source digest %s does not match %s", node.ID, actual, result.Formula.SHA256)
		}
		relative, err := FormulaTapPathV2(node)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(prefix, filepath.FromSlash(relative))
		if err := mkdirAllNoSymlink(prefix, filepath.Dir(target), 0o555); err != nil {
			return nil, err
		}
		if err := writeExclusiveSealed(target, result.FormulaSource); err != nil {
			return nil, fmt.Errorf("stage Formula %q: %w", node.ID, err)
		}
		evidence = append(evidence, StagedFormulaEvidenceV2{ID: node.ID, Tap: node.Tap, Name: node.Name, Path: relative, SHA256: actual, Size: int64(len(result.FormulaSource))})
	}
	slices.SortFunc(evidence, func(a, b StagedFormulaEvidenceV2) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return evidence, nil
}

func requireRealDirectory(value string) error {
	info, err := os.Lstat(value)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", value)
	}
	return nil
}

func mkdirAllNoSymlink(root, target string, mode os.FileMode) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("staging path escapes prefix")
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("staging ancestor %s is not a real directory", current)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("staging ancestor %s is group/world writable", current)
		}
	}
	return nil
}

func writeExclusiveSealed(filename string, data []byte) error {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(filename)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}
