package runtimefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

type sourceEntry struct {
	rel      string
	abs      string
	info     os.FileInfo
	typeName EntryType
	mode     os.FileMode
	size     int64
	sha256   string
	inode    string

	linkSource   string
	linkOutput   string
	linkResolved string

	retain         bool
	pruneReason    PruneReason
	metadataExport string
	packageName    string
	writable       bool
	statePath      bool
	uid            int
	gid            int
	desiredMode    os.FileMode
	hardlinkTo     string
}

type sourceScan struct {
	root     string
	entries  []*sourceEntry
	byPath   map[string]*sourceEntry
	retained []*sourceEntry
	pruned   []*sourceEntry
	metadata []MetadataExport
}

func scanAndPlan(ctx context.Context, sourceRoot string, record *resolution.Record, policy *normalizedPolicy) (*sourceScan, error) {
	rootInfo, err := os.Lstat(sourceRoot)
	if err != nil {
		return nil, runtimeError(CodeUnsafeSource, "", "stat source prefix: %v", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, runtimeError(CodeUnsafeSource, "", "source prefix must be a real directory")
	}

	scan := &sourceScan{root: sourceRoot, byPath: map[string]*sourceEntry{}}
	err = filepath.WalkDir(sourceRoot, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return runtimeError(CodeUnsafeSource, filepath.ToSlash(current), "walk source: %v", walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if current == sourceRoot {
			return nil
		}
		rel, err := filepath.Rel(sourceRoot, current)
		if err != nil {
			return runtimeError(CodeUnsafeSource, current, "make path relative: %v", err)
		}
		rel = filepath.ToSlash(rel)
		if err := validateRelativePath(rel); err != nil {
			return runtimeError(CodeUnsafeSource, rel, "%v", err)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return runtimeError(CodeUnsafeSource, rel, "lstat: %v", err)
		}
		typeName, err := classifyMode(info.Mode())
		if err != nil {
			return runtimeError(CodeUnsafeType, rel, "%v", err)
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return runtimeError(CodeUnsafeMode, rel, "setuid or setgid mode is forbidden")
		}
		if err := checkSecurityXattrs(current, info.Mode()&os.ModeSymlink != 0); err != nil {
			return runtimeError(CodeUnsafeXAttr, rel, "%v", err)
		}

		entry := &sourceEntry{
			rel:      rel,
			abs:      current,
			info:     info,
			typeName: typeName,
			mode:     info.Mode(),
			size:     info.Size(),
			inode:    inodeKey(info),
		}
		switch typeName {
		case TypeRegular:
			digest, size, err := hashSourceFile(current, info)
			if err != nil {
				return runtimeError(CodeUnsafeSource, rel, "hash source: %v", err)
			}
			entry.sha256 = digest
			entry.size = size
		case TypeSymlink:
			target, err := os.Readlink(current)
			if err != nil {
				return runtimeError(CodeUnsafeLink, rel, "read symlink: %v", err)
			}
			if strings.IndexByte(target, 0) >= 0 || target == "" {
				return runtimeError(CodeUnsafeLink, rel, "empty or NUL-containing target")
			}
			entry.linkSource = target
			entry.size = int64(len(target))
			entry.sha256 = sha256String(target)
		}
		if _, exists := scan.byPath[rel]; exists {
			return runtimeError(CodeUnsafeSource, rel, "duplicate normalized path")
		}
		scan.byPath[rel] = entry
		scan.entries = append(scan.entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(scan.entries, func(a, b *sourceEntry) int { return strings.Compare(a.rel, b.rel) })

	for _, entry := range scan.entries {
		if err := classifyRetention(entry, policy); err != nil {
			return nil, err
		}
	}
	if err := validateExpectedLayout(scan, record, policy); err != nil {
		return nil, err
	}
	if err := normalizeLinks(scan, policy); err != nil {
		return nil, err
	}
	if err := attributeEntries(scan, policy); err != nil {
		return nil, err
	}
	if err := pruneMinimalRuntimeProfile(scan, policy); err != nil {
		return nil, err
	}
	pruneOptionalDependencyTooling(scan, policy)
	if err := validateRetainedLinks(scan); err != nil {
		return nil, err
	}
	if err := validateRequestedExecutables(scan, record, policy); err != nil {
		return nil, err
	}
	if err := planModesAndHardlinks(scan, record); err != nil {
		return nil, err
	}
	if err := collectMetadataExports(scan, record); err != nil {
		return nil, err
	}

	for _, entry := range scan.entries {
		if entry.retain {
			scan.retained = append(scan.retained, entry)
		} else {
			scan.pruned = append(scan.pruned, entry)
		}
	}
	return scan, nil
}

func classifyRetention(entry *sourceEntry, policy *normalizedPolicy) error {
	rel := entry.rel
	if rel == "lib/ld.so" {
		expected, err := RuntimeBaseLoaderTarget(policy.architecture)
		if err != nil {
			return runtimeError(CodeInvalidRecord, rel, "%v", err)
		}
		if entry.typeName != TypeSymlink {
			return runtimeError(CodeUnsafeLink, rel, "runtime loader path must be a symlink")
		}
		actual := filepath.ToSlash(entry.linkSource)
		if actual == expected {
			entry.pruneReason = PruneRuntimeBase
			return nil
		}
		glibc, ok := policy.nodes["glibc"]
		if !ok || !runtimeFSRule(glibc, "brewed-loader", func() bool { return glibc.Name == "glibc" && glibc.FullName == "homebrew/core/glibc" }) || actual != path.Join(policy.installPrefix, "opt/glibc/bin/ld.so") {
			return runtimeError(CodeUnsafeLink, rel, "runtime loader target %q does not match base target %q", entry.linkSource, expected)
		}
		entry.retain = policy.allowlist.Lib
		return nil
	}
	if reason, kind := forcedPrune(rel); reason != "" {
		entry.pruneReason = reason
		entry.metadataExport = kind
		entry.packageName = packageFromCellarPath(rel, policy.nodes)
		return nil
	}

	root := firstSegment(rel)
	switch root {
	case "Cellar":
		parts := strings.Split(rel, "/")
		if len(parts) >= 2 {
			node, ok := policy.nodes[parts[1]]
			if !ok {
				return runtimeError(CodeUnexpectedKeg, rel, "rack %q is not in the resolution closure", parts[1])
			}
			entry.packageName = node.Name
			if len(parts) >= 3 && parts[2] != node.PkgVersion {
				return runtimeError(CodeUnexpectedKeg, rel, "keg version %q does not match resolved %q", parts[2], node.PkgVersion)
			}
		}
		entry.retain = policy.allowlist.Cellar
	case "opt":
		parts := strings.Split(rel, "/")
		if len(parts) >= 2 {
			node, ok := policy.nodes[parts[1]]
			if !ok {
				return runtimeError(CodeUnexpectedKeg, rel, "opt entry %q is not in the resolution closure", parts[1])
			}
			entry.packageName = node.Name
		}
		entry.retain = policy.allowlist.Opt
	case "bin", "sbin", "lib", "share":
		entry.retain = globalRootEnabled(root, policy.allowlist)
	case "etc":
		entry.statePath = true
		if rule, ok := matchingRule(rel, policy.allowlist.Etc); ok {
			entry.retain = true
			entry.packageName = rule.Package
		} else if isRuleAncestor(rel, policy.allowlist.Etc) {
			entry.retain = true
		}
	case "var":
		entry.statePath = true
		if rule, ok := matchingRule(rel, policy.allowlist.Var); ok {
			entry.retain = true
			entry.packageName = rule.Package
			entry.writable = rule.Writable
		} else if isRuleAncestor(rel, policy.allowlist.Var) {
			entry.retain = true
		}
	}
	if !entry.retain {
		entry.pruneReason = PruneNotAllowlisted
	}
	return nil
}

func forcedPrune(rel string) (PruneReason, string) {
	lower := strings.ToLower(rel)
	base := strings.ToLower(path.Base(rel))
	parts := strings.Split(rel, "/")

	if rel == "Homebrew" || strings.HasPrefix(rel, "Homebrew/") || rel == "Library" || strings.HasPrefix(rel, "Library/") || rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return PruneRepository, ""
	}
	if rel == "bin/brew" || rel == "sbin/brew" {
		return PruneBrewExecutable, ""
	}
	if rel == ".cache" || strings.HasPrefix(rel, ".cache/") || rel == "cache" || strings.HasPrefix(rel, "cache/") || rel == "var/cache" || strings.HasPrefix(rel, "var/cache/") {
		return PruneCache, ""
	}
	if rel == "logs" || strings.HasPrefix(rel, "logs/") || rel == "var/log/homebrew" || strings.HasPrefix(rel, "var/log/homebrew/") {
		return PruneLog, ""
	}
	for _, managerRoot := range []string{"var/homebrew", "var/run/homebrew", "var/locks/homebrew", "Caskroom"} {
		if rel == managerRoot || strings.HasPrefix(rel, managerRoot+"/") {
			return PruneManagerState, ""
		}
	}
	for _, managerPath := range []string{
		"share/doc/homebrew",
		"share/info/dir",
		"share/man/man1/brew.1",
		"share/zsh/site-functions/_brew",
		"share/fish/vendor_completions.d/brew.fish",
		"share/bash-completion/completions/brew",
		"etc/bash_completion.d/brew",
	} {
		if rel == managerPath || strings.HasPrefix(rel, managerPath+"/") {
			return PruneManagerState, ""
		}
	}
	for _, toolPath := range []string{"libexec/dalec-homebrew-materializer", "libexec/dalec-homebrew-test-runner", "libexec/dalec-homebrew-record-verify"} {
		if rel == toolPath {
			return PruneTooling, ""
		}
	}
	if rel == "share/dalec-homebrew-tools" || strings.HasPrefix(rel, "share/dalec-homebrew-tools/") {
		return PruneTooling, ""
	}

	inCellar := len(parts) >= 4 && parts[0] == "Cellar"
	if inCellar && parts[3] == ".brew" {
		kind := "homebrew_metadata"
		if strings.HasSuffix(lower, ".rb") {
			kind = "formula"
		} else if looksLikeSBOM(base) {
			kind = "package_manager_sbom"
		}
		reason := PruneFormulaMetadata
		if kind == "package_manager_sbom" {
			reason = PrunePackageSBOM
		}
		return reason, kind
	}
	if inCellar && len(parts) == 4 && strings.EqualFold(parts[3], "INSTALL_RECEIPT.json") {
		return PruneReceipt, "install_receipt"
	}
	return "", ""
}

func looksLikeSBOM(base string) bool {
	base = strings.ToLower(base)
	return strings.Contains(base, "sbom") || strings.HasSuffix(base, ".spdx.json") || strings.HasSuffix(base, ".cdx.json") || strings.HasSuffix(base, ".cyclonedx.json")
}

func validateExpectedLayout(scan *sourceScan, record *resolution.Record, policy *normalizedPolicy) error {
	for _, node := range record.Nodes {
		keg := path.Join("Cellar", node.Name, node.PkgVersion)
		entry, ok := scan.byPath[keg]
		if !ok || entry.typeName != TypeDirectory || !entry.retain {
			return runtimeError(CodeMissingKeg, keg, "resolved keg directory is absent")
		}
		optPath := path.Join("opt", node.Name)
		opt, ok := scan.byPath[optPath]
		if !ok || opt.typeName != TypeSymlink || !opt.retain {
			return runtimeError(CodeInvalidOptLink, optPath, "resolved opt link is absent or is not a symlink")
		}
	}
	for _, group := range [][]normalizedRule{policy.allowlist.Etc, policy.allowlist.Var, policy.allowlist.Owners} {
		for _, rule := range group {
			if !rule.Required {
				continue
			}
			entry, ok := scan.byPath[rule.Path]
			if !ok || !entry.retain {
				return runtimeError(CodeInvalidOptions, rule.Path, "required allowlist path is absent")
			}
			if rule.Writable && entry.typeName != TypeDirectory {
				return runtimeError(CodeInvalidOptions, rule.Path, "writable path root must be a directory")
			}
		}
	}
	return nil
}

func normalizeLinks(scan *sourceScan, policy *normalizedPolicy) error {
	for _, entry := range scan.entries {
		if entry.typeName != TypeSymlink {
			continue
		}
		if entry.pruneReason == PruneRuntimeBase {
			continue
		}
		resolved, outputTarget, err := normalizeLinkTarget(entry.rel, entry.linkSource, scan.root, policy.installPrefix)
		if err != nil {
			return runtimeError(CodeUnsafeLink, entry.rel, "%v", err)
		}
		entry.linkResolved = resolved
		entry.linkOutput = outputTarget
		if entry.retain && pointsToManagerState(resolved) {
			entry.retain = false
			entry.pruneReason = PruneManagerState
		}
	}

	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeSymlink {
			continue
		}
		if _, err := finalLinkTarget(scan.byPath, entry.rel, nil); err != nil {
			return err
		}
	}
	for name, node := range policy.nodes {
		optPath := path.Join("opt", name)
		opt := scan.byPath[optPath]
		if opt == nil || !opt.retain {
			return runtimeError(CodeInvalidOptLink, optPath, "resolved opt link was pruned")
		}
		expected := path.Join("Cellar", name, node.PkgVersion)
		final, err := finalLinkTarget(scan.byPath, optPath, map[string]bool{})
		if err != nil {
			return err
		}
		if final != expected {
			return runtimeError(CodeInvalidOptLink, optPath, "target %q does not resolve to %q", final, expected)
		}
	}
	return nil
}

// RuntimeBaseLoaderTarget returns the system dynamic-loader path bound into the
// matching runtime-base image. That link is omitted from the materialized
// package overlay unless a resolved Homebrew glibc replaces it with the brewed
// loader.
func RuntimeBaseLoaderTarget(architecture string) (string, error) {
	switch architecture {
	case "amd64":
		return "/lib64/ld-linux-x86-64.so.2", nil
	case "arm64":
		return "/lib/ld-linux-aarch64.so.1", nil
	default:
		return "", fmt.Errorf("unsupported runtime loader architecture %q", architecture)
	}
}

func normalizeLinkTarget(rel, target, sourceRoot, installPrefix string) (string, string, error) {
	target = filepath.ToSlash(target)
	if path.IsAbs(target) {
		clean := path.Clean(target)
		if clean == installPrefix {
			return "", "", fmt.Errorf("target resolves to prefix root")
		}
		if strings.HasPrefix(clean, installPrefix+"/") {
			relTarget := strings.TrimPrefix(clean, installPrefix+"/")
			if err := validateRelativePath(relTarget); err != nil {
				return "", "", err
			}
			return relTarget, clean, nil
		}
		if resolvedHost, err := filepath.EvalSymlinks(filepath.FromSlash(clean)); err == nil {
			clean = filepath.ToSlash(resolvedHost)
		}
		sourceSlash := filepath.ToSlash(sourceRoot)
		if sourceRoot != "" && strings.HasPrefix(clean, sourceSlash+"/") {
			relTarget := strings.TrimPrefix(clean, sourceSlash+"/")
			if err := validateRelativePath(relTarget); err != nil {
				return "", "", err
			}
			return relTarget, path.Join(installPrefix, relTarget), nil
		}
		return "", "", fmt.Errorf("absolute target %q escapes install prefix", target)
	}
	cleanTarget := path.Clean(target)
	resolved := path.Clean(path.Join(path.Dir(rel), cleanTarget))
	if err := validateRelativePath(resolved); err != nil {
		return "", "", fmt.Errorf("target %q escapes install prefix: %w", target, err)
	}
	return resolved, cleanTarget, nil
}

func finalLinkTarget(entries map[string]*sourceEntry, rel string, _ map[string]bool) (string, error) {
	queue := strings.Split(rel, "/")
	stack := []string{}
	visited := map[string]bool{}
	steps := 0
	for len(queue) > 0 {
		component := queue[0]
		queue = queue[1:]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if len(stack) == 0 {
				return "", runtimeError(CodeUnsafeLink, rel, "target escapes prefix")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, component)
		candidate := strings.Join(stack, "/")
		entry, ok := entries[candidate]
		if !ok {
			continue
		}
		if entry.typeName != TypeSymlink {
			if len(queue) > 0 && entry.typeName != TypeDirectory {
				return "", runtimeError(CodeDanglingLink, rel, "component %q is not a directory", candidate)
			}
			continue
		}
		steps++
		if steps > 64 || visited[candidate] {
			return "", runtimeError(CodeUnsafeLink, rel, "symlink cycle through %q", candidate)
		}
		visited[candidate] = true
		stack = nil
		queue = append(strings.Split(entry.linkResolved, "/"), queue...)
	}
	resolved := strings.Join(stack, "/")
	entry, ok := entries[resolved]
	if !ok || !entry.retain {
		return "", runtimeError(CodeDanglingLink, rel, "target %q is absent or pruned", resolved)
	}
	return resolved, nil
}

func pointsToManagerState(rel string) bool {
	if rel == "bin/brew" || rel == "sbin/brew" || rel == "Homebrew" || strings.HasPrefix(rel, "Homebrew/") || rel == "Library" || strings.HasPrefix(rel, "Library/") {
		return true
	}
	return rel == "var/homebrew" || strings.HasPrefix(rel, "var/homebrew/")
}

type globalCandidate struct {
	digest     string
	executable bool
}

func attributeEntries(scan *sourceScan, policy *normalizedPolicy) error {
	byInode := map[string]map[string]struct{}{}
	byRelative := map[string]map[string]globalCandidate{}
	for _, entry := range scan.entries {
		if !entry.retain {
			continue
		}
		pkg, sub, ok := kegLocation(entry.rel, policy.nodes)
		if !ok {
			continue
		}
		if entry.typeName != TypeRegular {
			if policy.allowlist.PruningProfile == policyv2.RuntimeProfileMinimalV1 {
				entry.packageName = pkg
			}
			continue
		}
		entry.packageName = pkg
		if entry.inode != "" {
			if byInode[entry.inode] == nil {
				byInode[entry.inode] = map[string]struct{}{}
			}
			byInode[entry.inode][pkg] = struct{}{}
		}
		if byRelative[sub] == nil {
			byRelative[sub] = map[string]globalCandidate{}
		}
		byRelative[sub][pkg] = globalCandidate{digest: entry.sha256, executable: entry.mode.Perm()&0o111 != 0}
	}

	// Attribute regular files before symlinks so a retained link may inherit
	// ownership from a generated global target that is bound by an owner rule.
	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName == TypeDirectory || entry.typeName == TypeSymlink || entry.packageName != "" {
			continue
		}
		if entry.inode != "" {
			entry.packageName = onePackage(byInode[entry.inode])
		}
		if entry.packageName == "" && entry.typeName == TypeRegular {
			entry.packageName = packageForGlobalCopy(entry, byRelative)
		}
		if entry.packageName == "" && entry.typeName == TypeRegular {
			if rule, ok := matchingRule(entry.rel, policy.allowlist.Owners); ok {
				entry.packageName = rule.Package
			}
		}
	}
	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeSymlink || entry.packageName != "" {
			continue
		}
		final, err := finalLinkTarget(scan.byPath, entry.rel, map[string]bool{})
		if err != nil {
			return err
		}
		entry.packageName = scan.byPath[final].packageName
	}
	for _, entry := range scan.entries {
		if entry.retain && entry.typeName != TypeDirectory && entry.packageName == "" {
			return runtimeError(CodeUnattributed, entry.rel, "retained non-directory path cannot be attributed to the resolution closure")
		}
	}

	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeDirectory || entry.packageName != "" {
			continue
		}
		if pkg := packageFromCellarPath(entry.rel, policy.nodes); pkg != "" {
			entry.packageName = pkg
		} else if rule, ok := matchingRule(entry.rel, append(append([]normalizedRule{}, policy.allowlist.Etc...), policy.allowlist.Var...)); ok {
			entry.packageName = rule.Package
		}
	}

	return nil
}

func packageForGlobalCopy(entry *sourceEntry, byRelative map[string]map[string]globalCandidate) string {
	candidates := byRelative[entry.rel]
	if len(candidates) == 0 {
		return ""
	}
	var exact []string
	for pkg, candidate := range candidates {
		if candidate.digest == entry.sha256 && candidate.executable == (entry.mode.Perm()&0o111 != 0) {
			exact = append(exact, pkg)
		}
	}
	if len(exact) == 1 {
		return exact[0]
	}
	return ""
}

func onePackage(values map[string]struct{}) string {
	if len(values) != 1 {
		return ""
	}
	for value := range values {
		return value
	}
	return ""
}

var optionalLLVMGlobalExecutables = map[string]struct{}{
	"bin/analyze-build":    {},
	"bin/git-clang-format": {},
	"bin/hmaptool":         {},
	"bin/intercept-build":  {},
	"bin/run-clang-tidy":   {},
	"bin/scan-build-py":    {},
	"bin/scan-view":        {},
}

func runtimeFSRule(node resolution.Node, rule string, legacy func() bool) bool {
	if node.PolicyFormulaID != "" {
		return policyv2.HasEmbeddedRule(node.PolicyFormulaID, rule)
	}
	return legacy()
}

func optionalDependencyTooling(node resolution.Node, rel string) bool {
	// libpsl exposes a Python-only data-generation helper from its keg even when
	// it is merely transitive. Keep the authenticated keg copy as auxiliary data
	// and omit only this exact global link.
	if rel == "bin/psl-make-dafsa" {
		return runtimeFSRule(node, "optional-libpsl-tooling", func() bool {
			return node.Name == "libpsl" && node.FullName == "homebrew/core/libpsl"
		})
	}
	if _, optional := optionalLLVMGlobalExecutables[rel]; !optional {
		return false
	}
	return runtimeFSRule(node, "optional-llvm-tooling", func() bool {
		return (node.Name == "llvm" || strings.HasPrefix(node.Name, "llvm@")) && node.FullName == "homebrew/core/"+node.Name
	})
}

func pruneOptionalDependencyTooling(scan *sourceScan, policy *normalizedPolicy) {
	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeSymlink {
			continue
		}
		node, ok := policy.nodes[entry.packageName]
		if !ok || !optionalDependencyTooling(node, entry.rel) {
			continue
		}
		if _, requested := policy.requested[entry.packageName]; requested {
			continue
		}
		entry.retain = false
		entry.pruneReason = PruneOptionalTooling
	}
}

func pruneMinimalRuntimeProfile(scan *sourceScan, policy *normalizedPolicy) error {
	if policy.allowlist.PruningProfile != policyv2.RuntimeProfileMinimalV1 {
		return nil
	}

	candidates := make(map[string]PruneReason)
	for _, entry := range scan.entries {
		if !entry.retain {
			continue
		}
		if reason := minimalRuntimePruneReason(entry.rel, entry.packageName, policy); reason != "" && (reason != PruneRuntimeStatic || entry.typeName != TypeDirectory) {
			candidates[entry.rel] = reason
		}
	}
	// A retained exception such as legal text still needs its directory
	// ancestors. Keep only those ancestors; siblings remain independently
	// classified.
	for _, entry := range scan.entries {
		if !entry.retain {
			continue
		}
		if _, pruned := candidates[entry.rel]; pruned {
			continue
		}
		for parent := path.Dir(entry.rel); parent != "."; parent = path.Dir(parent) {
			delete(candidates, parent)
		}
	}

	// Homebrew exposes man pages, Info pages, and build metadata through global
	// hardlinks and copies. Classify those before symlinks so a symlink that
	// resolves through a matching global copy does not depend on lexical order.
	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeRegular {
			continue
		}
		if _, direct := candidates[entry.rel]; direct {
			continue
		}
		node, ok := policy.nodes[entry.packageName]
		if !ok {
			continue
		}
		target := scan.byPath[path.Join("Cellar", node.Name, node.PkgVersion, entry.rel)]
		if target == nil {
			continue
		}
		reason := candidates[target.rel]
		if reason == "" || !minimalRuntimeAliasContentMatches(entry, target) {
			continue
		}
		if reason == "" || !minimalRuntimeAliasPath(entry.rel, reason) || target == nil || entry.packageName == "" || entry.packageName != target.packageName {
			continue
		}
		candidates[entry.rel] = reason
	}

	// Classify symlink aliases only after regular aliases have reached their
	// final candidate state. A symlink remains removable only when its final
	// target and its own path are in the same bounded class for one package.
	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeSymlink {
			continue
		}
		if _, direct := candidates[entry.rel]; direct {
			continue
		}
		final, err := finalLinkTarget(scan.byPath, entry.rel, nil)
		if err != nil {
			return err
		}
		target := scan.byPath[final]
		reason := candidates[final]
		if reason == "" || !minimalRuntimeAliasPath(entry.rel, reason) || target == nil || entry.packageName == "" || entry.packageName != target.packageName {
			continue
		}
		candidates[entry.rel] = reason
	}

	// A protected runtime-data link, or a link owned by an exact requested keg,
	// must remain usable. Remove only already-classified minimal-profile
	// candidates needed by that relationship; this cannot broaden the exact-core
	// or requested-root gates used to create candidates.
	if err := retainMinimalRuntimeAliasTargets(scan, policy, candidates); err != nil {
		return err
	}

	for _, entry := range scan.entries {
		reason, ok := candidates[entry.rel]
		if !ok {
			continue
		}
		entry.retain = false
		entry.pruneReason = reason
	}
	return nil
}

func minimalRuntimePruneReason(rel, packageName string, policy *normalizedPolicy) PruneReason {
	reason, sub := minimalRuntimePathClass(rel, packageName, policy)
	if reason == "" {
		return ""
	}
	if _, retained := policy.toolchainDev[packageName]; retained {
		switch reason {
		case PruneRuntimeHeaders, PruneRuntimeBuild, PruneRuntimeStatic:
			return ""
		}
	}
	// These paths are runtime-bearing regardless of which removable class an
	// ancestor otherwise matches. Keep the predicate path-only so assembly and
	// independent inventory verification enforce the same conservative rule.
	if minimalRuntimeAlwaysRetainedPath(sub) {
		return ""
	}
	// Only archives below lib are eligible. Archive-looking files in headers,
	// documentation, configuration, or other package data remain untouched.
	if strings.EqualFold(path.Ext(sub), ".a") && !staticArchiveRuntimePath(sub) {
		return ""
	}
	return reason
}

func minimalRuntimePathClass(rel, packageName string, policy *normalizedPolicy) (PruneReason, string) {
	if policy == nil || policy.allowlist.PruningProfile != policyv2.RuntimeProfileMinimalV1 {
		return "", ""
	}
	pkg, sub, ok := kegLocation(rel, policy.nodes)
	if !ok || pkg != packageName {
		return "", ""
	}
	if _, requested := policy.requested[pkg]; requested {
		return "", ""
	}
	node, ok := policy.nodes[pkg]
	if !ok || !exactCoreRuntimeProfileNode(node) {
		return "", ""
	}
	if isWithin(sub, "include") {
		return PruneRuntimeHeaders, sub
	}
	if isWithin(sub, "share/man") || isWithin(sub, "share/info") {
		return PruneRuntimeDocs, sub
	}
	for _, root := range []string{"lib/pkgconfig", "share/pkgconfig", "lib/cmake", "share/cmake", "share/aclocal"} {
		if isWithin(sub, root) {
			return PruneRuntimeBuild, sub
		}
	}
	if pythonStdlibTestPath(node, sub) {
		return PruneRuntimeTests, sub
	}
	if shellCompletionRuntimePath(sub) {
		return PruneRuntimeShell, sub
	}
	if staticArchiveRuntimePath(sub) {
		return PruneRuntimeStatic, sub
	}
	return "", sub
}

func exactCoreRuntimeProfileNode(node resolution.Node) bool {
	id := "homebrew/core/" + node.Name
	return node.PolicyFormulaID != "" && node.PolicyFormulaID == id && node.FullName == id
}

func pathContainsLegalText(rel string) bool {
	for _, component := range strings.Split(rel, "/") {
		if looksLikeLegalText(component) {
			return true
		}
	}
	return false
}

func minimalRuntimeAlwaysRetainedPath(rel string) bool {
	return pathContainsLegalText(rel) || minimalRuntimeProtectedDataPath(rel) || sharedObjectRuntimePath(rel)
}

func minimalRuntimeProtectedDataPath(rel string) bool {
	for _, component := range strings.Split(rel, "/") {
		switch strings.ToLower(component) {
		case "libexec",
			"etc",
			"conf",
			"config",
			"configuration",
			"locale",
			"locales",
			"site-packages",
			"ensurepip",
			"venv",
			"plugin",
			"plugins",
			"node_modules":
			return true
		}
	}
	return false
}

func sharedObjectRuntimePath(rel string) bool {
	base := strings.ToLower(path.Base(rel))
	return strings.HasSuffix(base, ".so") || strings.Contains(base, ".so.")
}

func minimalRuntimeAliasRetainsTarget(rel, _ string, policy *normalizedPolicy) bool {
	if minimalRuntimeAlwaysRetainedPath(rel) {
		return true
	}
	if policy == nil {
		return false
	}
	pkg, _, ok := kegLocation(rel, policy.nodes)
	if !ok {
		return false
	}
	_, requested := policy.requested[pkg]
	return requested
}

func pythonStdlibTestPath(node resolution.Node, sub string) bool {
	var minor string
	switch node.PolicyFormulaID {
	case "homebrew/core/python@3.13":
		minor = "3.13"
	case "homebrew/core/python@3.14":
		minor = "3.14"
	default:
		return false
	}
	if !policyv2.HasEmbeddedRule(node.PolicyFormulaID, policyv2.RuntimePrunePythonTestsV1) {
		return false
	}
	testRoot := "lib/python" + minor + "/test"
	return isWithin(sub, testRoot)
}

func minimalRuntimeAliasContentMatches(alias, target *sourceEntry) bool {
	if alias == nil || target == nil || alias.typeName != TypeRegular || target.typeName != TypeRegular {
		return false
	}
	if alias.inode != "" && alias.inode == target.inode {
		return true
	}
	return alias.sha256 != "" && alias.sha256 == target.sha256 && alias.size == target.size && alias.mode.Perm()&0o111 == target.mode.Perm()&0o111
}

func minimalRuntimeAliasPath(rel string, reason PruneReason) bool {
	if minimalRuntimeAlwaysRetainedPath(rel) {
		return false
	}
	switch reason {
	case PruneRuntimeHeaders:
		return headerAliasRuntimePath(rel)
	case PruneRuntimeDocs:
		return isWithin(rel, "share/man") || isWithin(rel, "share/info")
	case PruneRuntimeBuild:
		for _, root := range []string{"lib/pkgconfig", "share/pkgconfig", "lib/cmake", "share/cmake", "share/aclocal"} {
			if isWithin(rel, root) {
				return true
			}
		}
	case PruneRuntimeTests:
		return pythonStdlibTestAliasRuntimePath(rel)
	case PruneRuntimeShell:
		return shellCompletionRuntimePath(rel)
	case PruneRuntimeStatic:
		return staticArchiveAliasRuntimePath(rel)
	}
	return false
}

func pythonStdlibTestAliasRuntimePath(rel string) bool {
	return isWithin(rel, "lib/python3.13/test") || isWithin(rel, "lib/python3.14/test")
}

func headerAliasRuntimePath(rel string) bool {
	if isWithin(rel, "include") {
		return true
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 4 || parts[0] != "Cellar" {
		return false
	}
	for _, component := range parts[3:] {
		if component == "include" {
			return true
		}
	}
	return false
}

func shellCompletionRuntimePath(rel string) bool {
	for _, root := range []string{
		"share/bash-completion/completions",
		"share/fish/vendor_completions.d",
		"share/zsh/site-functions",
	} {
		if isWithin(rel, root) {
			return true
		}
	}
	return false
}

func staticArchiveRuntimePath(rel string) bool {
	if !strings.EqualFold(path.Ext(rel), ".a") || !strings.HasPrefix(rel, "lib/") || minimalRuntimeProtectedDataPath(rel) {
		return false
	}
	return true
}

func staticArchiveAliasRuntimePath(rel string) bool {
	if minimalRuntimeProtectedDataPath(rel) {
		return false
	}
	if staticArchiveRuntimePath(rel) {
		return true
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 5 || parts[0] != "Cellar" || !strings.EqualFold(path.Ext(rel), ".a") {
		return false
	}
	hasLib := false
	for _, component := range parts[3 : len(parts)-1] {
		if strings.EqualFold(component, "lib") {
			hasLib = true
		}
	}
	return hasLib
}

func retainMinimalRuntimeAliasTargets(scan *sourceScan, policy *normalizedPolicy, candidates map[string]PruneReason) error {
	queue := make([]*sourceEntry, 0)
	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeSymlink || !minimalRuntimeAliasRetainsTarget(entry.rel, entry.packageName, policy) {
			continue
		}
		queue = append(queue, entry)
	}

	seen := make(map[string]struct{}, len(queue))
	for len(queue) > 0 {
		entry := queue[0]
		queue = queue[1:]
		if _, ok := seen[entry.rel]; ok {
			continue
		}
		seen[entry.rel] = struct{}{}
		if _, pruned := candidates[entry.rel]; pruned {
			continue
		}
		final, required, err := minimalRuntimeLinkRequirements(scan.byPath, entry.rel)
		if err != nil {
			return err
		}
		for _, rel := range required {
			delete(candidates, rel)
			if requiredEntry := scan.byPath[rel]; requiredEntry != nil && requiredEntry.typeName == TypeSymlink {
				queue = append(queue, requiredEntry)
			}
		}
		target := scan.byPath[final]
		if target == nil || target.typeName != TypeDirectory {
			continue
		}
		for _, descendant := range scan.entries {
			if !isWithin(descendant.rel, final) {
				continue
			}
			delete(candidates, descendant.rel)
			if descendant.typeName == TypeSymlink {
				queue = append(queue, descendant)
			}
		}
	}
	return nil
}

func minimalRuntimeLinkRequirements(entries map[string]*sourceEntry, rel string) (string, []string, error) {
	queue := strings.Split(rel, "/")
	stack := []string{}
	visited := map[string]bool{}
	required := []string{}
	steps := 0
	for len(queue) > 0 {
		component := queue[0]
		queue = queue[1:]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if len(stack) == 0 {
				return "", nil, runtimeError(CodeUnsafeLink, rel, "target escapes prefix")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, component)
		candidate := strings.Join(stack, "/")
		entry, ok := entries[candidate]
		if !ok {
			continue
		}
		required = append(required, candidate)
		if entry.typeName != TypeSymlink {
			if len(queue) > 0 && entry.typeName != TypeDirectory {
				return "", nil, runtimeError(CodeDanglingLink, rel, "component %q is not a directory", candidate)
			}
			continue
		}
		steps++
		if steps > 64 || visited[candidate] {
			return "", nil, runtimeError(CodeUnsafeLink, rel, "symlink cycle through %q", candidate)
		}
		visited[candidate] = true
		stack = nil
		queue = append(strings.Split(entry.linkResolved, "/"), queue...)
	}
	resolved := strings.Join(stack, "/")
	entry, ok := entries[resolved]
	if !ok || !entry.retain {
		return "", nil, runtimeError(CodeDanglingLink, rel, "target %q is absent or pruned", resolved)
	}
	return resolved, required, nil
}

func validateRetainedLinks(scan *sourceScan) error {
	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeSymlink {
			continue
		}
		if _, err := finalLinkTarget(scan.byPath, entry.rel, nil); err != nil {
			return err
		}
	}
	return nil
}

func validateRequestedExecutables(scan *sourceScan, record *resolution.Record, policy *normalizedPolicy) error {
	for _, root := range record.Requested {
		node, ok := policy.nodes[root.Canonical]
		if !ok {
			continue
		}
		for _, executable := range node.ExecutablePaths {
			kegPath := path.Join("Cellar", node.Name, node.PkgVersion, executable)
			entry := scan.byPath[kegPath]
			if entry == nil || !entry.retain {
				return runtimeError(CodeMissingKeg, kegPath, "requested executable is absent")
			}
			final, err := finalLinkTarget(scan.byPath, kegPath, nil)
			if err != nil {
				return err
			}
			target := scan.byPath[final]
			if target.typeName != TypeRegular || target.mode.Perm()&0o111 == 0 {
				return runtimeError(CodeVerification, kegPath, "requested executable is not an executable regular file")
			}
			if !node.KegOnly && (strings.HasPrefix(executable, "bin/") || strings.HasPrefix(executable, "sbin/")) {
				global := scan.byPath[executable]
				if global == nil || !global.retain {
					return runtimeError(CodeVerification, executable, "requested executable is not exposed globally")
				}
				if global.packageName != node.Name {
					return runtimeError(CodeVerification, executable, "requested executable is attributed to %q, expected %q", global.packageName, node.Name)
				}
			}
		}
	}
	return nil
}

func planModesAndHardlinks(scan *sourceScan, record *resolution.Record) error {
	for _, entry := range scan.entries {
		if !entry.retain {
			continue
		}
		entry.statePath = entry.statePath || firstSegment(entry.rel) == "etc" || firstSegment(entry.rel) == "var"
		if entry.writable && (entry.typeName == TypeSymlink || entry.typeName == TypeHardlink || (entry.typeName == TypeRegular && entry.mode.Perm()&0o111 != 0)) {
			return runtimeError(CodeUnexpectedWritable, entry.rel, "writable runtime-state paths cannot contain executable code or links")
		}
		if entry.writable {
			entry.uid = record.Runtime.UID
			entry.gid = record.Runtime.GID
		} else {
			entry.uid = 0
			entry.gid = 0
		}
		entry.desiredMode = desiredMode(entry)
	}

	firstByInode := map[string]*sourceEntry{}
	for _, entry := range scan.entries {
		if !entry.retain || entry.typeName != TypeRegular || entry.inode == "" {
			continue
		}
		if first, ok := firstByInode[entry.inode]; ok {
			if first.packageName != "" && entry.packageName != "" && first.packageName != entry.packageName {
				return runtimeError(CodeVerification, entry.rel, "hardlink crosses package boundary between %q and %q", first.packageName, entry.packageName)
			}
			if first.uid != entry.uid || first.gid != entry.gid || first.desiredMode != entry.desiredMode || first.writable != entry.writable {
				return runtimeError(CodeUnexpectedWritable, entry.rel, "hardlink crosses ownership or write-policy boundary with %q", first.rel)
			}
			entry.hardlinkTo = first.rel
			entry.typeName = TypeHardlink
			continue
		}
		firstByInode[entry.inode] = entry
	}
	return nil
}

func desiredMode(entry *sourceEntry) os.FileMode {
	if entry.typeName == TypeSymlink {
		return 0o777
	}
	if entry.typeName == TypeDirectory {
		if entry.writable || entry.statePath {
			return 0o755
		}
		return 0o555
	}
	executable := entry.mode.Perm()&0o111 != 0
	if entry.writable || entry.statePath {
		if executable {
			return 0o755
		}
		return 0o644
	}
	if executable {
		return 0o555
	}
	return 0o444
}

func collectMetadataExports(scan *sourceScan, record *resolution.Record) error {
	byName := make(map[string]resolution.Node, len(record.Nodes))
	for _, node := range record.Nodes {
		byName[node.Name] = node
	}
	for _, entry := range scan.entries {
		if entry.retain || entry.metadataExport == "" || entry.typeName == TypeDirectory {
			continue
		}
		if entry.typeName != TypeRegular {
			return runtimeError(CodeVerification, entry.rel, "exportable package metadata must be a regular file")
		}
		if entry.packageName == "" {
			entry.packageName = packageFromCellarPath(entry.rel, byName)
		}
		if entry.packageName == "" {
			return runtimeError(CodeUnattributed, entry.rel, "package metadata cannot be attributed")
		}
		if entry.metadataExport == "install_receipt" {
			if err := validateReceiptIdentity(entry, byName[entry.packageName], record.Nodes); err != nil {
				return runtimeError(CodeVerification, entry.rel, "receipt identity: %v", err)
			}
		}
		if entry.metadataExport == "formula" {
			if err := validateFormulaIdentity(entry, entry.packageName); err != nil {
				return runtimeError(CodeVerification, entry.rel, "Formula identity: %v", err)
			}
		}
		scan.metadata = append(scan.metadata, MetadataExport{
			Package:    entry.packageName,
			Kind:       entry.metadataExport,
			SourcePath: entry.rel,
			SHA256:     entry.sha256,
			Size:       entry.size,
		})
	}
	slices.SortFunc(scan.metadata, func(a, b MetadataExport) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.SourcePath, b.SourcePath)
	})
	return nil
}

func packageFromCellarPath(rel string, nodes map[string]resolution.Node) string {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || parts[0] != "Cellar" {
		return ""
	}
	if _, ok := nodes[parts[1]]; ok {
		return parts[1]
	}
	return ""
}

func kegLocation(rel string, nodes map[string]resolution.Node) (pkg, sub string, ok bool) {
	parts := strings.Split(rel, "/")
	if len(parts) < 4 || parts[0] != "Cellar" {
		return "", "", false
	}
	node, ok := nodes[parts[1]]
	if !ok || parts[2] != node.PkgVersion {
		return "", "", false
	}
	return node.Name, strings.Join(parts[3:], "/"), true
}

func validateRelativePath(rel string) error {
	if rel == "" || rel == "." || path.IsAbs(rel) {
		return fmt.Errorf("path is not a non-empty relative path")
	}
	if !utf8.ValidString(rel) || strings.IndexByte(rel, 0) >= 0 {
		return fmt.Errorf("path is not valid UTF-8 text")
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path contains a control character")
		}
	}
	clean := path.Clean(rel)
	if clean != rel || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q is not normalized or escapes the prefix", rel)
	}
	return nil
}

func validatePathComponent(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("value is not a safe path component")
	}
	return validateRelativePath(value)
}

func classifyMode(mode os.FileMode) (EntryType, error) {
	switch {
	case mode.IsDir():
		return TypeDirectory, nil
	case mode.IsRegular():
		return TypeRegular, nil
	case mode&os.ModeSymlink != 0:
		return TypeSymlink, nil
	default:
		return "", fmt.Errorf("special file mode %s is forbidden", mode.String())
	}
}

func hashSourceFile(filename string, expected os.FileInfo) (string, int64, error) {
	f, err := openRegularNoFollow(filename)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return "", 0, fmt.Errorf("path changed while opening")
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	after, err := os.Lstat(filename)
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(expected, after) || after.Size() != n {
		return "", 0, fmt.Errorf("path changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sha256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func readScannedRegular(entry *sourceEntry) ([]byte, error) {
	if entry.size > 4<<20 {
		return nil, fmt.Errorf("metadata file exceeds 4 MiB")
	}
	f, err := openRegularNoFollow(entry.abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(entry.info, opened) {
		return nil, fmt.Errorf("source identity changed")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != entry.size || sha256String(string(data)) != entry.sha256 {
		return nil, fmt.Errorf("source content changed")
	}
	return data, nil
}

func validateReceiptIdentity(entry *sourceEntry, node resolution.Node, closure []resolution.Node) error {
	data, err := readScannedRegular(entry)
	if err != nil {
		return err
	}
	_, err = bottle.VerifyInstalledReceipt(data, node, closure)
	return err
}

var formulaSubclassPattern = regexp.MustCompile(`(?m)^[\t ]*class[\t ]+([A-Z][A-Za-z0-9]*)[\t ]*<[\t ]*Formula(?:[\t ]*(?:#.*)?)?\r?$`)

func validateFormulaIdentity(entry *sourceEntry, name string) error {
	data, err := readScannedRegular(entry)
	if err != nil {
		return err
	}
	if len(data) == 0 || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("Formula source is not valid text")
	}
	matches := formulaSubclassPattern.FindAllSubmatch(data, -1)
	if len(matches) != 1 {
		return fmt.Errorf("expected exactly one Formula subclass, found %d", len(matches))
	}
	expected := formulaClassName(name)
	if actual := string(matches[0][1]); actual != expected {
		return fmt.Errorf("Formula class %q does not match expected %q", actual, expected)
	}
	return nil
}

func validateUniqueJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if err := walkUniqueJSON(dec, token); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func walkUniqueJSON(dec *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			valueToken, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, valueToken); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("unterminated object")
		}
	case '[':
		for dec.More() {
			valueToken, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, valueToken); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func formulaClassName(name string) string {
	if name == "" {
		return ""
	}
	lower := strings.ToLower(name)
	var b strings.Builder
	capitalize := true
	if word, ok := runtimeDigitWord(lower[0]); ok {
		b.WriteString(word)
		lower = lower[1:]
		capitalize = false
	}
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if c == '@' && i+1 < len(lower) && lower[i+1] >= '0' && lower[i+1] <= '9' {
			b.WriteString("AT")
			continue
		}
		if c == '-' || c == '_' || c == '.' || c == ' ' {
			capitalize = true
			continue
		}
		if c == '+' {
			c = 'x'
		}
		if capitalize && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		capitalize = false
		b.WriteByte(c)
	}
	return b.String()
}

func runtimeDigitWord(value byte) (string, bool) {
	words := map[byte]string{'0': "Zero", '1': "One", '2': "Two", '3': "Three", '4': "Four", '5': "Five", '6': "Six", '7': "Seven", '8': "Eight", '9': "Nine"}
	word, ok := words[value]
	return word, ok
}
