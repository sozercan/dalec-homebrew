package runtimefs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type normalizedRule struct {
	Path     string `json:"path"`
	Package  string `json:"package"`
	Writable bool   `json:"writable,omitempty"`
	Required bool   `json:"required,omitempty"`
}

type normalizedAllowlist struct {
	Cellar bool             `json:"cellar"`
	Opt    bool             `json:"opt"`
	Bin    bool             `json:"bin,omitempty"`
	Sbin   bool             `json:"sbin,omitempty"`
	Lib    bool             `json:"lib,omitempty"`
	Share  bool             `json:"share,omitempty"`
	Etc    []normalizedRule `json:"etc,omitempty"`
	Var    []normalizedRule `json:"var,omitempty"`
	Owners []normalizedRule `json:"owners,omitempty"`
}

type normalizedPolicy struct {
	installPrefix string
	architecture  string
	allowlist     normalizedAllowlist
	digest        string
	nodes         map[string]resolution.Node
	writable      map[string]struct{}
}

func normalizeOptions(record *resolution.Record, opts Options) (*normalizedPolicy, error) {
	if record == nil {
		return nil, runtimeError(CodeInvalidRecord, "", "nil resolution record")
	}
	if err := resolution.ValidateForMaterialization(record); err != nil {
		return nil, runtimeError(CodeInvalidRecord, "", "%v", err)
	}
	installPrefix, err := normalizeInstallPrefix(opts.InstallPrefix)
	if err != nil {
		return nil, runtimeError(CodeInvalidOptions, "", "%v", err)
	}
	if !opts.Allowlist.Cellar || !opts.Allowlist.Opt {
		return nil, runtimeError(CodeInvalidOptions, "", "Cellar and opt must be explicitly allowlisted")
	}

	nodes := make(map[string]resolution.Node, len(record.Nodes))
	for _, node := range record.Nodes {
		if err := validatePathComponent(node.Name); err != nil {
			return nil, runtimeError(CodeInvalidRecord, node.Name, "unsafe Formula name: %v", err)
		}
		if err := validatePathComponent(node.PkgVersion); err != nil {
			return nil, runtimeError(CodeInvalidRecord, node.PkgVersion, "unsafe PkgVersion for %q: %v", node.Name, err)
		}
		nodes[node.Name] = node
	}

	allowlist := normalizedAllowlist{
		Cellar: opts.Allowlist.Cellar,
		Opt:    opts.Allowlist.Opt,
		Bin:    opts.Allowlist.Bin,
		Sbin:   opts.Allowlist.Sbin,
		Lib:    opts.Allowlist.Lib,
		Share:  opts.Allowlist.Share,
	}
	allowlist.Etc, err = normalizeRules(opts.Allowlist.Etc, "etc", installPrefix, nodes, false)
	if err != nil {
		return nil, err
	}
	allowlist.Var, err = normalizeRules(opts.Allowlist.Var, "var", installPrefix, nodes, true)
	if err != nil {
		return nil, err
	}
	allowlist.Owners, err = normalizeOwnerRules(opts.Allowlist.Owners, installPrefix, nodes, allowlist)
	if err != nil {
		return nil, err
	}

	writable := make(map[string]struct{})
	for _, raw := range record.Runtime.WritablePaths {
		n, err := normalizeRulePath(raw, "var", installPrefix)
		if err != nil {
			return nil, runtimeError(CodeInvalidRecord, raw, "invalid runtime writable path: %v", err)
		}
		if n != "var" && !strings.HasPrefix(n, "var/") {
			return nil, runtimeError(CodeInvalidRecord, raw, "runtime writable path must be below var")
		}
		if _, duplicate := writable[n]; duplicate {
			return nil, runtimeError(CodeInvalidRecord, raw, "duplicate runtime writable path")
		}
		writable[n] = struct{}{}
	}
	approvedWritable := make(map[string]struct{})
	for _, rule := range allowlist.Var {
		if rule.Writable {
			approvedWritable[rule.Path] = struct{}{}
		}
	}
	if !sameStringSet(writable, approvedWritable) {
		return nil, runtimeError(CodeInvalidOptions, "", "writable var rules must exactly match resolution runtime.writable_paths")
	}

	policyDigest, err := digestNormalizedPolicy(installPrefix, allowlist)
	if err != nil {
		return nil, runtimeError(CodeInvalidOptions, "", "canonicalize allowlist: %v", err)
	}
	if record.PruningPolicyDigest != "" && record.PruningPolicyDigest != policyDigest {
		return nil, runtimeError(CodeInvalidRecord, "", "pruning policy digest %s does not match record %s", policyDigest, record.PruningPolicyDigest)
	}

	return &normalizedPolicy{
		installPrefix: installPrefix,
		architecture:  record.Input.Platform.Architecture,
		allowlist:     allowlist,
		digest:        policyDigest,
		nodes:         nodes,
		writable:      writable,
	}, nil
}

// PolicyDigest returns the digest that can be placed in
// resolution.Record.PruningPolicyDigest. Runtime writable paths are not part of
// this digest; they are already bound by the resolution record and are checked
// for exact agreement during Assemble and Verify.
func PolicyDigest(allowlist Allowlist, installPrefix string, nodes []resolution.Node) (string, error) {
	prefix, err := normalizeInstallPrefix(installPrefix)
	if err != nil {
		return "", err
	}
	byName := make(map[string]resolution.Node, len(nodes))
	for _, node := range nodes {
		if err := validatePathComponent(node.Name); err != nil {
			return "", fmt.Errorf("unsafe Formula name %q: %w", node.Name, err)
		}
		if err := validatePathComponent(node.PkgVersion); err != nil {
			return "", fmt.Errorf("unsafe PkgVersion %q: %w", node.PkgVersion, err)
		}
		byName[node.Name] = node
	}
	if !allowlist.Cellar || !allowlist.Opt {
		return "", fmt.Errorf("Cellar and opt must be explicitly allowlisted")
	}
	n := normalizedAllowlist{Cellar: allowlist.Cellar, Opt: allowlist.Opt, Bin: allowlist.Bin, Sbin: allowlist.Sbin, Lib: allowlist.Lib, Share: allowlist.Share}
	n.Etc, err = normalizeRules(allowlist.Etc, "etc", prefix, byName, false)
	if err != nil {
		return "", err
	}
	n.Var, err = normalizeRules(allowlist.Var, "var", prefix, byName, true)
	if err != nil {
		return "", err
	}
	n.Owners, err = normalizeOwnerRules(allowlist.Owners, prefix, byName, n)
	if err != nil {
		return "", err
	}
	return digestNormalizedPolicy(prefix, n)
}

func normalizeInstallPrefix(value string) (string, error) {
	if value == "" {
		value = DefaultInstallPrefix
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("install prefix %q is not absolute", value)
	}
	clean := path.Clean(value)
	if clean == "/" || clean == "." || clean != value && strings.HasSuffix(value, "/..") {
		return "", fmt.Errorf("unsafe install prefix %q", value)
	}
	return strings.TrimSuffix(clean, "/"), nil
}

func normalizeRules(input []PathRule, root, installPrefix string, nodes map[string]resolution.Node, allowWritable bool) ([]normalizedRule, error) {
	out := make([]normalizedRule, 0, len(input))
	seen := map[string]normalizedRule{}
	for _, rule := range input {
		p, err := normalizeRulePath(rule.Path, root, installPrefix)
		if err != nil {
			return nil, runtimeError(CodeInvalidOptions, rule.Path, "%v", err)
		}
		if p != root && !strings.HasPrefix(p, root+"/") {
			return nil, runtimeError(CodeInvalidOptions, rule.Path, "path must be below %s", root)
		}
		if _, ok := nodes[rule.Package]; !ok {
			return nil, runtimeError(CodeInvalidOptions, rule.Path, "unknown package %q", rule.Package)
		}
		if rule.Writable && !allowWritable {
			return nil, runtimeError(CodeInvalidOptions, rule.Path, "only var paths may be writable")
		}
		n := normalizedRule{Path: p, Package: rule.Package, Writable: rule.Writable, Required: rule.Required}
		if previous, ok := seen[p]; ok {
			if previous != n {
				return nil, runtimeError(CodeInvalidOptions, rule.Path, "conflicting duplicate path rule")
			}
			continue
		}
		seen[p] = n
		out = append(out, n)
	}
	sortRules(out)
	if err := validateRuleOverlaps(out); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeOwnerRules(input []PathRule, installPrefix string, nodes map[string]resolution.Node, allowlist normalizedAllowlist) ([]normalizedRule, error) {
	out := make([]normalizedRule, 0, len(input))
	seen := map[string]normalizedRule{}
	for _, rule := range input {
		p, err := normalizeRulePath(rule.Path, "", installPrefix)
		if err != nil {
			return nil, runtimeError(CodeInvalidOptions, rule.Path, "%v", err)
		}
		root := firstSegment(p)
		if !globalRootEnabled(root, allowlist) {
			return nil, runtimeError(CodeInvalidOptions, rule.Path, "owner override is not below an enabled global root")
		}
		if _, ok := nodes[rule.Package]; !ok {
			return nil, runtimeError(CodeInvalidOptions, rule.Path, "unknown package %q", rule.Package)
		}
		if rule.Writable {
			return nil, runtimeError(CodeInvalidOptions, rule.Path, "global owner rules cannot be writable")
		}
		n := normalizedRule{Path: p, Package: rule.Package, Required: rule.Required}
		if previous, ok := seen[p]; ok {
			if previous != n {
				return nil, runtimeError(CodeInvalidOptions, rule.Path, "conflicting duplicate owner rule")
			}
			continue
		}
		seen[p] = n
		out = append(out, n)
	}
	sortRules(out)
	if err := validateRuleOverlaps(out); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeRulePath(value, root, installPrefix string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "", fmt.Errorf("empty path rule")
	}
	if strings.HasPrefix(value, "/") {
		if value == installPrefix {
			return "", fmt.Errorf("rule cannot approve the entire prefix")
		}
		prefix := installPrefix + "/"
		if !strings.HasPrefix(value, prefix) {
			return "", fmt.Errorf("absolute path is outside install prefix %q", installPrefix)
		}
		value = strings.TrimPrefix(value, prefix)
	} else if root != "" && value != root && !strings.HasPrefix(value, root+"/") {
		value = root + "/" + value
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("unsafe path rule %q", value)
	}
	if err := validateRelativePath(clean); err != nil {
		return "", err
	}
	return clean, nil
}

func sortRules(rules []normalizedRule) {
	slices.SortFunc(rules, func(a, b normalizedRule) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		if a.Writable != b.Writable {
			if a.Writable {
				return 1
			}
			return -1
		}
		return 0
	})
}

func validateRuleOverlaps(rules []normalizedRule) error {
	for i := range rules {
		for j := i + 1; j < len(rules); j++ {
			if !isWithin(rules[j].Path, rules[i].Path) {
				continue
			}
			if rules[i].Package != rules[j].Package || rules[i].Writable != rules[j].Writable {
				return runtimeError(CodeInvalidOptions, rules[j].Path, "overlaps %q with different ownership policy", rules[i].Path)
			}
		}
	}
	return nil
}

func digestNormalizedPolicy(installPrefix string, allowlist normalizedAllowlist) (string, error) {
	value := struct {
		SchemaVersion string              `json:"schema_version"`
		InstallPrefix string              `json:"install_prefix"`
		Allowlist     normalizedAllowlist `json:"allowlist"`
	}{
		SchemaVersion: "dalec-homebrew-pruning-policy/v1",
		InstallPrefix: installPrefix,
		Allowlist:     allowlist,
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return "", err
	}
	return digest.FromBytes(bytes.TrimSuffix(b.Bytes(), []byte("\n"))).String(), nil
}

func sameStringSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func globalRootEnabled(root string, allowlist normalizedAllowlist) bool {
	switch root {
	case "bin":
		return allowlist.Bin
	case "sbin":
		return allowlist.Sbin
	case "lib":
		return allowlist.Lib
	case "share":
		return allowlist.Share
	default:
		return false
	}
}

func firstSegment(p string) string {
	if before, _, ok := strings.Cut(p, "/"); ok {
		return before
	}
	return p
}

func isWithin(candidate, root string) bool {
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func matchingRule(p string, rules []normalizedRule) (normalizedRule, bool) {
	var best normalizedRule
	found := false
	for _, rule := range rules {
		if isWithin(p, rule.Path) && (!found || len(rule.Path) > len(best.Path)) {
			best = rule
			found = true
		}
	}
	return best, found
}

func isRuleAncestor(p string, rules []normalizedRule) bool {
	for _, rule := range rules {
		if isWithin(rule.Path, p) {
			return true
		}
	}
	return false
}
