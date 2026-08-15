package runtimefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

// Assemble creates outputPrefix atomically from the explicitly allowed subset
// of sourcePrefix. It is equivalent to AssembleContext with a background
// context.
func Assemble(sourcePrefix, outputPrefix string, record *resolution.Record, opts Options) (*Result, error) {
	return AssembleContext(context.Background(), sourcePrefix, outputPrefix, record, opts)
}

// AssembleContext creates a clean output prefix, verifies it against the
// generated inventory, and returns canonical evidence. The output must not
// exist or must be an empty real directory. On failure, no partial assembled
// tree is left at outputPrefix.
func AssembleContext(ctx context.Context, sourcePrefix, outputPrefix string, record *resolution.Record, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	policy, err := normalizeOptions(record, opts)
	if err != nil {
		return nil, err
	}
	resolutionDigest, err := resolution.Digest(record)
	if err != nil {
		return nil, runtimeError(CodeInvalidRecord, "", "canonical resolution digest: %v", err)
	}
	return assembleContextWithPolicy(ctx, sourcePrefix, outputPrefix, record, opts, policy, resolutionDigest.String())
}

// AssembleV2 constructs and verifies a runtime filesystem from a validated V2
// record while preserving its canonical digest and full Formula identities in
// evidence. Rack names are used only for filesystem translation.
func AssembleV2(sourcePrefix, outputPrefix string, record *resolution.RecordV2, opts Options) (*Result, error) {
	return AssembleContextV2(context.Background(), sourcePrefix, outputPrefix, record, opts)
}

func AssembleContextV2(ctx context.Context, sourcePrefix, outputPrefix string, record *resolution.RecordV2, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := resolution.ValidateV2(record); err != nil {
		return nil, runtimeError(CodeInvalidRecord, "", "%v", err)
	}
	projected, _, err := resolution.ProjectV2ForRuntime(record)
	if err != nil {
		return nil, err
	}
	policy, err := normalizeOptionsUnchecked(projected, opts)
	if err != nil {
		return nil, err
	}
	resolutionDigest, err := resolution.DigestV2(record)
	if err != nil {
		return nil, runtimeError(CodeInvalidRecord, "", "canonical V2 resolution digest: %v", err)
	}
	return assembleContextWithPolicy(ctx, sourcePrefix, outputPrefix, projected, opts, policy, resolutionDigest.String())
}

func assembleContextWithPolicy(ctx context.Context, sourcePrefix, outputPrefix string, record *resolution.Record, opts Options, policy *normalizedPolicy, resolutionDigest string) (_ *Result, retErr error) {
	sourceRoot, outputRoot, err := normalizeHostRoots(sourcePrefix, outputPrefix)
	if err != nil {
		return nil, err
	}
	outputExisted, err := validateOutputTarget(outputRoot)
	if err != nil {
		return nil, err
	}

	scan, err := scanAndPlan(ctx, sourceRoot, record, policy)
	if err != nil {
		return nil, err
	}
	inventory := buildInventory(scan, record, policy, resolutionDigest)
	prune, err := buildPruneManifest(scan, record, policy, resolutionDigest)
	if err != nil {
		return nil, err
	}

	parent := filepath.Dir(outputRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, runtimeError(CodeCopy, outputRoot, "create output parent: %v", err)
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(outputRoot)+".runtimefs-")
	if err != nil {
		return nil, runtimeError(CodeCopy, outputRoot, "create staging prefix: %v", err)
	}
	defer func() {
		if stage != "" {
			makeTreeRemovable(stage)
			_ = os.RemoveAll(stage)
		}
	}()

	if err := copyPlannedTree(ctx, stage, scan, record, opts); err != nil {
		return nil, err
	}
	if err := verifyWithPolicyDigest(stage, record, &inventory, opts, policy, resolutionDigest); err != nil {
		return nil, err
	}

	result, err := buildResult(outputRoot, record, inventory, prune, scan.metadata, policy)
	if err != nil {
		return nil, err
	}

	if outputExisted {
		if err := os.Remove(outputRoot); err != nil {
			return nil, runtimeError(CodeCopy, outputRoot, "remove empty output directory: %v", err)
		}
	}
	if err := os.Rename(stage, outputRoot); err != nil {
		return nil, runtimeError(CodeCopy, outputRoot, "publish staged prefix: %v", err)
	}
	stage = ""
	return result, nil
}

func normalizeHostRoots(sourcePrefix, outputPrefix string) (string, string, error) {
	if strings.TrimSpace(sourcePrefix) == "" || strings.TrimSpace(outputPrefix) == "" {
		return "", "", runtimeError(CodeInvalidOptions, "", "source and output prefixes are required")
	}
	sourceRoot, err := filepath.Abs(sourcePrefix)
	if err != nil {
		return "", "", runtimeError(CodeInvalidOptions, sourcePrefix, "absolute source path: %v", err)
	}
	outputRoot, err := filepath.Abs(outputPrefix)
	if err != nil {
		return "", "", runtimeError(CodeInvalidOptions, outputPrefix, "absolute output path: %v", err)
	}
	if info, statErr := os.Lstat(filepath.Clean(sourceRoot)); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", "", runtimeError(CodeInvalidOptions, sourcePrefix, "source path itself must not be a symlink")
	}
	if info, statErr := os.Lstat(filepath.Clean(outputRoot)); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", "", runtimeError(CodeInvalidOptions, outputPrefix, "output path itself must not be a symlink")
	}
	sourceRoot, err = filepath.EvalSymlinks(filepath.Clean(sourceRoot))
	if err != nil {
		return "", "", runtimeError(CodeInvalidOptions, sourcePrefix, "resolve source path: %v", err)
	}
	info, err := os.Lstat(sourceRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", runtimeError(CodeInvalidOptions, sourcePrefix, "source must resolve to a real directory")
	}
	outputRoot, err = resolveOutputAncestors(filepath.Clean(outputRoot))
	if err != nil {
		return "", "", runtimeError(CodeInvalidOptions, outputPrefix, "resolve output path: %v", err)
	}
	if sourceRoot == outputRoot || hostPathWithin(outputRoot, sourceRoot) || hostPathWithin(sourceRoot, outputRoot) {
		return "", "", runtimeError(CodeInvalidOptions, outputRoot, "source and output prefixes must not overlap")
	}
	return sourceRoot, outputRoot, nil
}

func resolveOutputAncestors(value string) (string, error) {
	missing := []string{}
	current := value
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	return filepath.Clean(resolved), nil
}

func hostPathWithin(candidate, root string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validateOutputTarget(outputRoot string) (bool, error) {
	info, err := os.Lstat(outputRoot)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, runtimeError(CodeCopy, outputRoot, "stat output: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, runtimeError(CodeCopy, outputRoot, "output must not exist or must be an empty real directory")
	}
	entries, err := os.ReadDir(outputRoot)
	if err != nil {
		return false, runtimeError(CodeCopy, outputRoot, "read output: %v", err)
	}
	if len(entries) != 0 {
		return false, runtimeError(CodeCopy, outputRoot, "output directory is not empty")
	}
	return true, nil
}

func copyPlannedTree(ctx context.Context, stage string, scan *sourceScan, record *resolution.Record, opts Options) error {
	for _, entry := range scan.retained {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parent := path.Dir(entry.rel); parent != "." {
			parentEntry := scan.byPath[parent]
			if parentEntry == nil || !parentEntry.retain || parentEntry.typeName != TypeDirectory {
				return runtimeError(CodeCopy, entry.rel, "retained path has a missing, pruned, or non-directory parent %q", parent)
			}
		}
		if err := verifySourceEntryUnchanged(entry); err != nil {
			return err
		}
		destination := filepath.Join(stage, filepath.FromSlash(entry.rel))
		switch entry.typeName {
		case TypeDirectory:
			if err := os.Mkdir(destination, 0o700); err != nil {
				return runtimeError(CodeCopy, entry.rel, "create directory: %v", err)
			}
		case TypeRegular:
			if err := copyRegular(entry, destination); err != nil {
				return err
			}
		case TypeHardlink:
			primary := filepath.Join(stage, filepath.FromSlash(entry.hardlinkTo))
			if err := os.Link(primary, destination); err != nil {
				return runtimeError(CodeCopy, entry.rel, "create hardlink to %q: %v", entry.hardlinkTo, err)
			}
		case TypeSymlink:
			current, err := os.Readlink(entry.abs)
			if err != nil || current != entry.linkSource {
				if err == nil {
					err = fmt.Errorf("target changed from %q to %q", entry.linkSource, current)
				}
				return runtimeError(CodeSourceChanged, entry.rel, "read source symlink: %v", err)
			}
			if err := os.Symlink(entry.linkOutput, destination); err != nil {
				return runtimeError(CodeCopy, entry.rel, "create symlink: %v", err)
			}
		default:
			return runtimeError(CodeCopy, entry.rel, "unsupported planned type %q", entry.typeName)
		}
	}

	chown := opts.Chown
	if chown == nil {
		chown = defaultChown
	}
	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	for _, entry := range scan.retained {
		destination := filepath.Join(stage, filepath.FromSlash(entry.rel))
		symlink := entry.typeName == TypeSymlink
		if err := chown(destination, entry.uid, entry.gid, symlink); err != nil {
			return runtimeError(CodeOwnership, entry.rel, "set owner %d:%d: %v", entry.uid, entry.gid, err)
		}
		if !symlink {
			if err := os.Chmod(destination, entry.desiredMode); err != nil {
				return runtimeError(CodeCopy, entry.rel, "normalize mode to %04o: %v", entry.desiredMode.Perm(), err)
			}
		}
		if err := setPathMtime(destination, epoch, symlink); err != nil {
			return runtimeError(CodeCopy, entry.rel, "normalize mtime: %v", err)
		}
	}
	if err := chown(stage, 0, 0, false); err != nil {
		return runtimeError(CodeOwnership, "", "set prefix owner 0:0: %v", err)
	}
	if err := os.Chmod(stage, 0o555); err != nil {
		return runtimeError(CodeCopy, "", "normalize prefix mode: %v", err)
	}
	if err := setPathMtime(stage, epoch, false); err != nil {
		return runtimeError(CodeCopy, "", "normalize prefix mtime: %v", err)
	}
	return nil
}

func verifySourceEntryUnchanged(entry *sourceEntry) error {
	current, err := os.Lstat(entry.abs)
	if err != nil {
		return runtimeError(CodeSourceChanged, entry.rel, "lstat source before copy: %v", err)
	}
	if !os.SameFile(entry.info, current) || current.Mode().Type() != entry.info.Mode().Type() {
		return runtimeError(CodeSourceChanged, entry.rel, "source identity or type changed after scan")
	}
	if current.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return runtimeError(CodeUnsafeMode, entry.rel, "source gained setuid or setgid mode after scan")
	}
	return nil
}

func copyRegular(entry *sourceEntry, destination string) error {
	source, err := openRegularNoFollow(entry.abs)
	if err != nil {
		return runtimeError(CodeSourceChanged, entry.rel, "open regular source without following links: %v", err)
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil {
		return runtimeError(CodeSourceChanged, entry.rel, "stat open source: %v", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(entry.info, opened) {
		return runtimeError(CodeSourceChanged, entry.rel, "source identity changed after scan")
	}
	destinationFile, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return runtimeError(CodeCopy, entry.rel, "create destination file: %v", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(destinationFile, h), source)
	closeErr := destinationFile.Close()
	if copyErr != nil {
		return runtimeError(CodeCopy, entry.rel, "copy contents: %v", copyErr)
	}
	if closeErr != nil {
		return runtimeError(CodeCopy, entry.rel, "close destination: %v", closeErr)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if n != entry.size || actual != entry.sha256 {
		return runtimeError(CodeSourceChanged, entry.rel, "source content changed after scan (size %d, digest %s)", n, actual)
	}
	after, err := os.Lstat(entry.abs)
	if err != nil || !os.SameFile(entry.info, after) || after.Size() != entry.size {
		if err == nil {
			err = fmt.Errorf("source identity or size changed")
		}
		return runtimeError(CodeSourceChanged, entry.rel, "%v", err)
	}
	return nil
}

// Verify checks an assembled prefix against a deterministic inventory and the
// same resolution/allowlist policy used to build it. No files are modified.
func Verify(outputPrefix string, record *resolution.Record, inventory *Inventory, opts Options) error {
	policy, err := normalizeOptions(record, opts)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(outputPrefix)
	if err != nil {
		return runtimeError(CodeVerification, outputPrefix, "absolute output path: %v", err)
	}
	resolutionDigest, err := resolution.Digest(record)
	if err != nil {
		return runtimeError(CodeInvalidRecord, "", "canonical resolution digest: %v", err)
	}
	return verifyWithPolicyDigest(filepath.Clean(root), record, inventory, opts, policy, resolutionDigest.String())
}

func verifyWithPolicy(root string, record *resolution.Record, inventory *Inventory, opts Options, policy *normalizedPolicy) error {
	resolutionDigest, err := resolution.Digest(record)
	if err != nil {
		return runtimeError(CodeInvalidRecord, "", "canonical resolution digest: %v", err)
	}
	return verifyWithPolicyDigest(root, record, inventory, opts, policy, resolutionDigest.String())
}

func verifyWithPolicyDigest(root string, record *resolution.Record, inventory *Inventory, opts Options, policy *normalizedPolicy, resolutionDigest string) error {
	if inventory == nil {
		return runtimeError(CodeVerification, "", "nil inventory")
	}
	if inventory.SchemaVersion != inventorySchemaVersion(record) || inventory.PolicyVersion != record.PolicyVersion || inventory.ResolutionDigest != resolutionDigest || inventory.PruningPolicyDigest != policy.digest || inventory.SourceDateEpoch != record.SourceDateEpoch || inventory.Prefix != policy.installPrefix {
		return runtimeError(CodeVerification, "", "inventory identity does not match resolution and pruning policy")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = fmt.Errorf("not a real directory")
		}
		return runtimeError(CodeVerification, "", "invalid output prefix: %v", err)
	}
	if rootInfo.Mode().Perm() != 0o555 {
		return runtimeError(CodeUnexpectedWritable, "", "prefix mode is %04o, want 0555", rootInfo.Mode().Perm())
	}
	if rootInfo.ModTime().Unix() != record.SourceDateEpoch {
		return runtimeError(CodeVerification, "", "prefix mtime %d does not match source_date_epoch %d", rootInfo.ModTime().Unix(), record.SourceDateEpoch)
	}
	if opts.Chown == nil {
		if uid, gid, ok := fileOwner(rootInfo); !ok || uid != 0 || gid != 0 {
			return runtimeError(CodeOwnership, "", "prefix owner is %d:%d, want 0:0", uid, gid)
		}
	}

	expected := make(map[string]InventoryEntry, len(inventory.Entries))
	previous := ""
	for _, entry := range inventory.Entries {
		if err := validateRelativePath(entry.Path); err != nil {
			return runtimeError(CodeVerification, entry.Path, "invalid inventory path: %v", err)
		}
		if previous != "" && strings.Compare(previous, entry.Path) >= 0 {
			return runtimeError(CodeVerification, entry.Path, "inventory entries are not strictly sorted")
		}
		previous = entry.Path
		if _, duplicate := expected[entry.Path]; duplicate {
			return runtimeError(CodeVerification, entry.Path, "duplicate inventory path")
		}
		expected[entry.Path] = entry
	}
	if err := validateInventoryEntriesPolicy(inventory.Entries, record, policy); err != nil {
		return err
	}

	actual := map[string]os.FileInfo{}
	err = filepath.WalkDir(root, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return runtimeError(CodeVerification, filepath.ToSlash(current), "walk output: %v", walkErr)
		}
		if current == root {
			return nil
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return runtimeError(CodeVerification, current, "make output path relative: %v", err)
		}
		rel = filepath.ToSlash(rel)
		expectedEntry, ok := expected[rel]
		if !ok {
			return runtimeError(CodeVerification, rel, "unexpected output path")
		}
		info, err := os.Lstat(current)
		if err != nil {
			return runtimeError(CodeVerification, rel, "lstat output: %v", err)
		}
		actual[rel] = info
		if err := verifyOnePath(current, info, expectedEntry, record, opts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	for rel := range expected {
		if _, ok := actual[rel]; !ok {
			return runtimeError(CodeVerification, rel, "inventory path is missing")
		}
	}

	for _, entry := range inventory.Entries {
		if entry.Type == TypeHardlink {
			primary, ok := actual[entry.HardlinkTo]
			if !ok || !os.SameFile(actual[entry.Path], primary) {
				return runtimeError(CodeVerification, entry.Path, "not hardlinked to %q", entry.HardlinkTo)
			}
		}
		if entry.Type == TypeSymlink {
			if _, err := finalInventoryTarget(expected, entry.Path, map[string]bool{}, policy.installPrefix); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyOnePath(filename string, info os.FileInfo, expected InventoryEntry, record *resolution.Record, opts Options) error {
	actualType, err := classifyMode(info.Mode())
	if err != nil {
		return runtimeError(CodeUnsafeType, expected.Path, "%v", err)
	}
	if expected.Type == TypeHardlink {
		if actualType != TypeRegular {
			return runtimeError(CodeVerification, expected.Path, "type is %q, want hardlink-compatible regular file", actualType)
		}
	} else if actualType != expected.Type {
		return runtimeError(CodeVerification, expected.Path, "type is %q, want %q", actualType, expected.Type)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return runtimeError(CodeUnsafeMode, expected.Path, "setuid or setgid mode is forbidden")
	}
	if err := checkSecurityXattrs(filename, expected.Type == TypeSymlink); err != nil {
		return runtimeError(CodeUnsafeXAttr, expected.Path, "%v", err)
	}
	if expected.Type != TypeSymlink {
		mode, err := strconv.ParseUint(expected.Mode, 8, 32)
		if err != nil || info.Mode().Perm() != os.FileMode(mode) {
			return runtimeError(CodeVerification, expected.Path, "mode is %04o, want %s", info.Mode().Perm(), expected.Mode)
		}
		if info.ModTime().Unix() != record.SourceDateEpoch {
			return runtimeError(CodeVerification, expected.Path, "mtime %d does not match source_date_epoch %d", info.ModTime().Unix(), record.SourceDateEpoch)
		}
	}
	if opts.Chown == nil {
		uid, gid, ok := fileOwner(info)
		if !ok || uid != expected.UID || gid != expected.GID {
			return runtimeError(CodeOwnership, expected.Path, "owner is %d:%d, want %d:%d", uid, gid, expected.UID, expected.GID)
		}
	}
	switch expected.Type {
	case TypeRegular, TypeHardlink:
		digest, size, err := hashSourceFile(filename, info)
		if err != nil {
			return runtimeError(CodeVerification, expected.Path, "hash output: %v", err)
		}
		if digest != expected.SHA256 || size != expected.Size {
			return runtimeError(CodeVerification, expected.Path, "content digest/size does not match inventory")
		}
	case TypeSymlink:
		target, err := os.Readlink(filename)
		if err != nil {
			return runtimeError(CodeVerification, expected.Path, "read output symlink: %v", err)
		}
		if target != expected.LinkTarget || sha256String(target) != expected.SHA256 {
			return runtimeError(CodeVerification, expected.Path, "symlink target %q does not match inventory %q", target, expected.LinkTarget)
		}
	}
	return nil
}

type inventoryAttribution struct {
	path        string
	packageName string
}

type inventoryProfilePlan struct {
	forbidden  map[inventoryAttribution]PruneReason
	exceptions map[inventoryAttribution]struct{}
}

func inventoryEntryAttribution(entry InventoryEntry) inventoryAttribution {
	return inventoryAttribution{path: entry.Path, packageName: entry.Package}
}

func validateInventoryEntriesPolicy(entries []InventoryEntry, record *resolution.Record, policy *normalizedPolicy) error {
	for _, entry := range entries {
		if err := validateInventoryPolicyBase(entry, record, policy); err != nil {
			return err
		}
	}
	plan, err := planMinimalInventoryProfile(entries, policy)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if reason, forbidden := plan.forbidden[inventoryEntryAttribution(entry)]; forbidden {
			return runtimeError(CodeVerification, entry.Path, "inventory retains path forbidden by runtime profile (%s)", reason)
		}
	}
	return nil
}

func validateInventoryPolicy(entry InventoryEntry, record *resolution.Record, policy *normalizedPolicy, profileAncestors map[string]struct{}) error {
	if err := validateInventoryPolicyBase(entry, record, policy); err != nil {
		return err
	}
	if reason := minimalRuntimePruneReason(entry.Path, entry.Package, policy); reason != "" && (reason != PruneRuntimeStatic || entry.Type != TypeDirectory) {
		_, approvedAncestor := profileAncestors[entry.Path]
		if !approvedAncestor || entry.Type != TypeDirectory {
			return runtimeError(CodeVerification, entry.Path, "inventory retains path forbidden by runtime profile (%s)", reason)
		}
	}
	return nil
}

func validateInventoryPolicyBase(entry InventoryEntry, record *resolution.Record, policy *normalizedPolicy) error {
	if reason, _ := forcedPrune(entry.Path); reason != "" {
		return runtimeError(CodeVerification, entry.Path, "inventory retains forbidden package-manager path (%s)", reason)
	}
	if !isV2RuntimeRecord(record) && entry.FormulaID != "" {
		return runtimeError(CodeVerification, entry.Path, "V1 inventory entry has formula identity %q", entry.FormulaID)
	}
	if entry.Package != "" {
		if _, ok := policy.nodes[entry.Package]; !ok {
			return runtimeError(CodeVerification, entry.Path, "inventory names unknown package %q", entry.Package)
		}
		if isV2RuntimeRecord(record) {
			expectedFormulaID := formulaIDForRack(record, entry.Package)
			if expectedFormulaID == "" || entry.FormulaID != expectedFormulaID {
				return runtimeError(CodeVerification, entry.Path, "inventory formula identity %q does not match package %q identity %q", entry.FormulaID, entry.Package, expectedFormulaID)
			}
		}
	} else if entry.Type != TypeDirectory {
		return runtimeError(CodeUnattributed, entry.Path, "non-directory inventory entry has no package")
	} else if isV2RuntimeRecord(record) && entry.FormulaID != "" {
		return runtimeError(CodeVerification, entry.Path, "unattributed inventory directory has formula identity %q", entry.FormulaID)
	}

	root := firstSegment(entry.Path)
	if root == "Cellar" {
		parts := strings.Split(entry.Path, "/")
		if len(parts) < 2 {
			if entry.Package != "" {
				return runtimeError(CodeVerification, entry.Path, "inventory package %q is attributed to the Cellar root", entry.Package)
			}
		} else {
			node, ok := policy.nodes[parts[1]]
			if !ok || len(parts) >= 3 && parts[2] != node.PkgVersion {
				return runtimeError(CodeUnexpectedKeg, entry.Path, "inventory keg is not in the exact resolution closure")
			}
			if isV2RuntimeRecord(record) {
				if entry.Package == "" {
					return runtimeError(CodeUnattributed, entry.Path, "Cellar inventory path has no package attribution")
				}
				if entry.Package != parts[1] {
					return runtimeError(CodeVerification, entry.Path, "inventory package %q does not match Cellar rack %q", entry.Package, parts[1])
				}
			}
		}
	}

	allowed := false
	writable := false
	switch root {
	case "Cellar":
		allowed = policy.allowlist.Cellar
	case "opt":
		allowed = policy.allowlist.Opt
	case "bin", "sbin", "lib", "share":
		allowed = globalRootEnabled(root, policy.allowlist)
	case "etc":
		_, allowed = matchingRule(entry.Path, policy.allowlist.Etc)
		allowed = allowed || isRuleAncestor(entry.Path, policy.allowlist.Etc)
	case "var":
		if rule, ok := matchingRule(entry.Path, policy.allowlist.Var); ok {
			allowed = true
			writable = rule.Writable
		} else {
			allowed = isRuleAncestor(entry.Path, policy.allowlist.Var)
		}
	}
	if !allowed {
		return runtimeError(CodeVerification, entry.Path, "inventory path is not allowlisted")
	}
	if entry.Writable != writable {
		return runtimeError(CodeUnexpectedWritable, entry.Path, "inventory writable flag does not match approved var policy")
	}
	if entry.Writable && root != "var" {
		return runtimeError(CodeUnexpectedWritable, entry.Path, "only approved var paths may be runtime writable")
	}
	if entry.Writable {
		if entry.UID != record.Runtime.UID || entry.GID != record.Runtime.GID {
			return runtimeError(CodeOwnership, entry.Path, "writable path owner is not the runtime identity")
		}
	} else if entry.UID != 0 || entry.GID != 0 {
		return runtimeError(CodeOwnership, entry.Path, "non-writable path is not root-owned")
	}
	mode, err := strconv.ParseUint(entry.Mode, 8, 32)
	if err != nil {
		return runtimeError(CodeVerification, entry.Path, "invalid inventory mode %q", entry.Mode)
	}
	perm := os.FileMode(mode)
	if entry.Type != TypeSymlink && perm&0o022 != 0 {
		return runtimeError(CodeUnexpectedWritable, entry.Path, "group/other write bits are forbidden")
	}
	if !entry.Writable && root != "etc" && root != "var" && perm&0o200 != 0 && entry.Type != TypeSymlink {
		return runtimeError(CodeUnexpectedWritable, entry.Path, "immutable code/data path is owner-writable")
	}
	return nil
}

func planMinimalInventoryProfile(entries []InventoryEntry, policy *normalizedPolicy) (*inventoryProfilePlan, error) {
	plan := &inventoryProfilePlan{
		forbidden:  map[inventoryAttribution]PruneReason{},
		exceptions: map[inventoryAttribution]struct{}{},
	}
	if policy == nil || policy.allowlist.PruningProfile == "" {
		return plan, nil
	}

	byPath := make(map[string]InventoryEntry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
		if reason := minimalRuntimePruneReason(entry.Path, entry.Package, policy); reason != "" && (reason != PruneRuntimeStatic || entry.Type != TypeDirectory) {
			plan.forbidden[inventoryEntryAttribution(entry)] = reason
		}
	}

	for rel := range minimalInventoryExceptionAncestors(entries, policy) {
		entry, ok := byPath[rel]
		if !ok {
			continue
		}
		key := inventoryEntryAttribution(entry)
		if _, candidate := plan.forbidden[key]; candidate {
			delete(plan.forbidden, key)
			plan.exceptions[key] = struct{}{}
		}
	}

	// Attribute matching regular copies before symlinks so a symlink to a
	// matching global copy observes the same candidate set regardless of
	// inventory ordering.
	for _, entry := range entries {
		if entry.Type != TypeRegular && entry.Type != TypeHardlink {
			continue
		}
		key := inventoryEntryAttribution(entry)
		if _, direct := plan.forbidden[key]; direct {
			continue
		}
		node, ok := policy.nodes[entry.Package]
		if !ok {
			continue
		}
		target, ok := byPath[path.Join("Cellar", node.Name, node.PkgVersion, entry.Path)]
		if !ok || target.Package != entry.Package {
			continue
		}
		reason, candidate := plan.forbidden[inventoryEntryAttribution(target)]
		if !candidate || !minimalRuntimeAliasPath(entry.Path, reason) {
			continue
		}
		matches, err := inventoryAliasContentMatches(entry, target)
		if err != nil {
			return nil, err
		}
		if matches {
			plan.forbidden[key] = reason
		}
	}

	for _, entry := range entries {
		if entry.Type != TypeSymlink {
			continue
		}
		key := inventoryEntryAttribution(entry)
		if _, direct := plan.forbidden[key]; direct {
			continue
		}
		trace, err := traceInventoryTarget(byPath, entry.Path, policy.installPrefix)
		if err != nil {
			return nil, err
		}
		target := byPath[trace.final]
		reason, candidate := plan.forbidden[inventoryEntryAttribution(target)]
		if candidate && entry.Package != "" && entry.Package == target.Package && minimalRuntimeAliasPath(entry.Path, reason) {
			plan.forbidden[key] = reason
		}
	}

	// A bounded protected runtime alias may require an otherwise prunable
	// target. Mirror the scanner's fixed point over only the exact link graph
	// reachable from a protected or requested-keg seed. Each exception retains
	// its own independently validated package/formula attribution.
	queue := make([]InventoryEntry, 0)
	for _, entry := range entries {
		if entry.Type == TypeSymlink && minimalRuntimeAliasRetainsTarget(entry.Path, entry.Package, policy) {
			queue = append(queue, entry)
		}
	}
	seen := make(map[string]struct{}, len(queue))
	var descendants *descendantPathIndex
	for len(queue) > 0 {
		entry := queue[0]
		queue = queue[1:]
		if _, ok := seen[entry.Path]; ok {
			continue
		}
		seen[entry.Path] = struct{}{}
		if _, candidate := plan.forbidden[inventoryEntryAttribution(entry)]; candidate {
			continue
		}
		trace, err := traceInventoryTarget(byPath, entry.Path, policy.installPrefix)
		if err != nil {
			return nil, err
		}
		exempt := func(rel string) {
			candidate, ok := byPath[rel]
			if !ok {
				return
			}
			key := inventoryEntryAttribution(candidate)
			if _, forbidden := plan.forbidden[key]; !forbidden {
				return
			}
			delete(plan.forbidden, key)
			plan.exceptions[key] = struct{}{}
		}
		for _, rel := range trace.paths {
			exempt(rel)
			if required := byPath[rel]; required.Type == TypeSymlink {
				queue = append(queue, required)
			}
		}
		target := byPath[trace.final]
		if target.Type == TypeDirectory {
			if descendants == nil {
				paths := make([]string, 0, len(entries))
				for _, candidate := range entries {
					paths = append(paths, candidate.Path)
				}
				descendants = newDescendantPathIndex(paths)
			}
			descendants.consumeDescendants(trace.final, func(rel string) {
				exempt(rel)
				if candidate := byPath[rel]; candidate.Type == TypeSymlink {
					queue = append(queue, candidate)
				}
			})
		}
	}

	return plan, nil
}

func inventoryAliasContentMatches(alias, target InventoryEntry) (bool, error) {
	if (alias.Type != TypeRegular && alias.Type != TypeHardlink) || (target.Type != TypeRegular && target.Type != TypeHardlink) {
		return false, nil
	}
	aliasMode, err := strconv.ParseUint(alias.Mode, 8, 32)
	if err != nil {
		return false, runtimeError(CodeVerification, alias.Path, "invalid inventory mode %q", alias.Mode)
	}
	targetMode, err := strconv.ParseUint(target.Mode, 8, 32)
	if err != nil {
		return false, runtimeError(CodeVerification, target.Path, "invalid inventory mode %q", target.Mode)
	}
	return alias.SHA256 != "" && alias.SHA256 == target.SHA256 && alias.Size == target.Size && (aliasMode&0o111 != 0) == (targetMode&0o111 != 0), nil
}

type inventoryTargetTrace struct {
	final string
	paths []string
}

func traceInventoryTarget(entries map[string]InventoryEntry, rel, installPrefix string) (inventoryTargetTrace, error) {
	queue := strings.Split(rel, "/")
	stack := []string{}
	visited := map[string]bool{}
	traced := map[string]struct{}{}
	trace := inventoryTargetTrace{}
	steps := 0
	for len(queue) > 0 {
		component := queue[0]
		queue = queue[1:]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if len(stack) == 0 {
				return inventoryTargetTrace{}, runtimeError(CodeUnsafeLink, rel, "target escapes prefix")
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
		if _, seen := traced[candidate]; !seen {
			trace.paths = append(trace.paths, candidate)
			traced[candidate] = struct{}{}
		}
		if entry.Type != TypeSymlink {
			if len(queue) > 0 && entry.Type != TypeDirectory {
				return inventoryTargetTrace{}, runtimeError(CodeDanglingLink, rel, "component %q is not a directory", candidate)
			}
			continue
		}
		steps++
		if steps > 64 || visited[candidate] {
			return inventoryTargetTrace{}, runtimeError(CodeUnsafeLink, rel, "inventory symlink cycle through %q", candidate)
		}
		visited[candidate] = true
		resolved, _, err := normalizeLinkTarget(candidate, entry.LinkTarget, "", installPrefix)
		if err != nil {
			return inventoryTargetTrace{}, runtimeError(CodeUnsafeLink, candidate, "%v", err)
		}
		stack = nil
		queue = append(strings.Split(resolved, "/"), queue...)
	}
	trace.final = strings.Join(stack, "/")
	if _, ok := entries[trace.final]; !ok {
		return inventoryTargetTrace{}, runtimeError(CodeDanglingLink, rel, "inventory target %q is absent", trace.final)
	}
	return trace, nil
}

func minimalInventoryExceptionAncestors(entries []InventoryEntry, policy *normalizedPolicy) map[string]struct{} {
	approved := map[string]struct{}{}
	for _, entry := range entries {
		reason, _ := minimalRuntimePathClass(entry.Path, entry.Package, policy)
		if reason == "" || minimalRuntimePruneReason(entry.Path, entry.Package, policy) != "" {
			continue
		}
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			parentReason, _ := minimalRuntimePathClass(parent, entry.Package, policy)
			if parentReason != reason {
				continue
			}
			approved[parent] = struct{}{}
		}
	}
	return approved
}

func finalInventoryTarget(entries map[string]InventoryEntry, rel string, _ map[string]bool, installPrefix string) (string, error) {
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
		if entry.Type != TypeSymlink {
			if len(queue) > 0 && entry.Type != TypeDirectory {
				return "", runtimeError(CodeDanglingLink, rel, "component %q is not a directory", candidate)
			}
			continue
		}
		steps++
		if steps > 64 || visited[candidate] {
			return "", runtimeError(CodeUnsafeLink, rel, "inventory symlink cycle through %q", candidate)
		}
		visited[candidate] = true
		resolved, _, err := normalizeLinkTarget(candidate, entry.LinkTarget, "", installPrefix)
		if err != nil {
			return "", runtimeError(CodeUnsafeLink, candidate, "%v", err)
		}
		stack = nil
		queue = append(strings.Split(resolved, "/"), queue...)
	}
	resolved := strings.Join(stack, "/")
	if _, ok := entries[resolved]; !ok {
		return "", runtimeError(CodeDanglingLink, rel, "inventory target %q is absent", resolved)
	}
	return resolved, nil
}

func modeString(mode os.FileMode) string {
	return fmt.Sprintf("%04o", mode.Perm())
}

func makeTreeRemovable(root string) {
	_ = filepath.WalkDir(root, func(current string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(current, 0o700)
		}
		return nil
	})
}
