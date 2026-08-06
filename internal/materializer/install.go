package materializer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
)

const DefaultPrefix = "/home/linuxbrew/.linuxbrew"

const pourScriptPath = "/usr/local/libexec/dalec-homebrew-pour.rb"

type Config struct {
	Record     *resolution.Record
	BottlesDir string
	Prefix     string
	User       string
	Timeout    time.Duration
	Runner     Runner

	// Tests can stage into a caller-owned fixture and model a distinct runtime
	// identity. Production leaves these at zero: the tap owner is root and the
	// untrusted runtime identity comes from the authenticated resolution.
	formulaTapUID        int
	formulaTapGID        int
	formulaTapRuntimeUID int
	formulaTapRuntimeGID int
}

type Runner interface {
	Run(context.Context, Command) error
}

type Command struct {
	Path      string
	Args, Env []string
	Dir, User string
}

type Evidence struct {
	VerifiedBottles       []bottle.Result                `json:"verified_bottles"`
	StagedFormulae        []StagedFormulaEvidence        `json:"staged_formulae"`
	ReceiptNormalizations []ReceiptNormalizationEvidence `json:"receipt_normalizations,omitempty"`
	InstallDeltas         []InstallDelta                 `json:"install_deltas"`
}

type InstallDelta struct {
	Formula string   `json:"formula"`
	Changes []Change `json:"changes"`
}
type Change struct{ Path, Kind, Classification string }

func Install(ctx context.Context, cfg Config) (*Evidence, error) {
	if cfg.Record == nil {
		return nil, errors.New("nil resolution record")
	}
	if err := resolution.ValidateForMaterialization(cfg.Record); err != nil {
		return nil, fmt.Errorf("verify resolution before materialization: %w", err)
	}
	prefix, err := normalizeMaterializerPrefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	cfg.Prefix = prefix
	if cfg.User == "" {
		cfg.User = "linuxbrew"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if cfg.Runner == nil {
		cfg.Runner = OSRunner{}
	}
	if cfg.BottlesDir == "" {
		return nil, errors.New("bottles directory is required")
	}

	bottlesDir, err := filepath.Abs(cfg.BottlesDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(bottlesDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("bottles directory must be a real directory")
	}
	bottleDirFile, err := os.Open(bottlesDir)
	if err != nil {
		return nil, err
	}
	defer bottleDirFile.Close()
	openedDirInfo, err := bottleDirFile.Stat()
	if err != nil || !openedDirInfo.IsDir() {
		return nil, errors.New("opened bottle root is not a directory")
	}
	stagedDir, err := os.MkdirTemp("", "dalec-homebrew-verified-bottles-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stagedDir)
	if err := os.Chmod(stagedDir, 0o755); err != nil {
		return nil, err
	}
	installPaths := map[string]string{}
	// Copy each input through an already-open rooted descriptor, then verify and later install that immutable private copy.
	evidence := &Evidence{}
	verifiedByName := map[string]bottle.Result{}
	formulaSources := map[string][]byte{}
	defer func() {
		for name, source := range formulaSources {
			clear(source)
			delete(formulaSources, name)
		}
	}()
	for _, name := range cfg.Record.InstallOrder {
		node, ok := nodeByName(cfg.Record, name)
		if !ok {
			return nil, fmt.Errorf("install node %q is absent", name)
		}
		filename := node.Bottle.Filename
		if filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, "/\\") {
			return nil, fmt.Errorf("invalid bottle filename %q", filename)
		}
		source, err := openBottleNoFollow(bottleDirFile, filename)
		if err != nil {
			return nil, err
		}
		fileInfo, err := source.Stat()
		if err != nil {
			source.Close()
			return nil, err
		}
		if !fileInfo.Mode().IsRegular() {
			source.Close()
			return nil, fmt.Errorf("bottle %q is not a regular no-follow file", filename)
		}
		stagedPath := filepath.Join(stagedDir, filename)
		destination, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
		if err != nil {
			source.Close()
			return nil, err
		}
		copied, copyErr := io.Copy(destination, io.LimitReader(source, node.Bottle.Layer.Size+1))
		sourceClose := source.Close()
		destinationClose := destination.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if sourceClose != nil {
			return nil, sourceClose
		}
		if destinationClose != nil {
			return nil, destinationClose
		}
		if copied != node.Bottle.Layer.Size {
			return nil, fmt.Errorf("bottle %q copied %d bytes, expected %d", filename, copied, node.Bottle.Layer.Size)
		}
		if err := os.Chmod(stagedPath, 0o444); err != nil {
			return nil, err
		}
		staged, err := os.Open(stagedPath)
		if err != nil {
			return nil, err
		}
		verified, verifyErr := bottle.VerifyNode(staged, node, bottle.Options{Policy: bottle.Policy{RequirePreInstallReceipt: false}})
		closeErr := staged.Close()
		if verifyErr != nil {
			return nil, fmt.Errorf("verify bottle %q: %w", name, verifyErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		installPaths[name] = stagedPath
		formulaSources[name] = verified.FormulaSource
		verified.FormulaSource = nil
		evidence.VerifiedBottles = append(evidence.VerifiedBottles, *verified)
		verifiedByName[name] = *verified
	}
	formulaTapOptions := formulaTapStageOptions{
		ownerUID:   cfg.formulaTapUID,
		ownerGID:   cfg.formulaTapGID,
		runtimeUID: cfg.Record.Runtime.UID,
		runtimeGID: cfg.Record.Runtime.GID,
	}
	if cfg.formulaTapRuntimeUID != 0 || cfg.formulaTapRuntimeGID != 0 {
		formulaTapOptions.runtimeUID = cfg.formulaTapRuntimeUID
		formulaTapOptions.runtimeGID = cfg.formulaTapRuntimeGID
	}
	stagedFormulae, err := stageVerifiedFormulaClosure(cfg.Prefix, cfg.Record, verifiedByName, formulaSources, formulaTapOptions)
	if err != nil {
		return nil, fmt.Errorf("stage verified Formula closure: %w", err)
	}
	evidence.StagedFormulae = stagedFormulae
	for name, source := range formulaSources {
		clear(source)
		delete(formulaSources, name)
	}

	for _, name := range cfg.Record.InstallOrder {
		node, _ := nodeByName(cfg.Record, name)
		if err := validateNoPrefixBrewEnv(cfg.Prefix); err != nil {
			return nil, fmt.Errorf("validate Homebrew environment before %q: %w", name, err)
		}
		if err := validateProtectedHomebrewRepository(cfg.Prefix, formulaTapOptions, true); err != nil {
			return nil, fmt.Errorf("validate protected Homebrew repository before %q: %w", name, err)
		}
		stepCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		before, err := snapshotContext(stepCtx, cfg.Prefix)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("snapshot before %q: %w", name, err)
		}
		if err := validatePreinstallSymlinks(cfg.Prefix, before, cfg.Record); err != nil {
			cancel()
			return nil, fmt.Errorf("validate prefix before %q: %w", name, err)
		}
		if err := validateExternalBottleSymlinkTargets(cfg.Prefix, before, node, verifiedByName[name], cfg.Record.Nodes); err != nil {
			cancel()
			return nil, fmt.Errorf("validate bottle dependency links before %q: %w", name, err)
		}
		var priorGdkPixbufCache []byte
		if state, ok := before[gdkPixbufLoadersCachePath]; ok {
			if state.Type != "regular" {
				cancel()
				return nil, fmt.Errorf("validate prefix before %q: gdk-pixbuf loader cache is not a regular file", name)
			}
			priorGdkPixbufCache, err = readStableSnapshotFile(cfg.Prefix, gdkPixbufLoadersCachePath, state)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("capture gdk-pixbuf loader cache before %q: %w", name, err)
			}
		}
		err = cfg.Runner.Run(stepCtx, Command{Path: filepath.Join(cfg.Prefix, filepath.FromSlash(protectedHomebrewBrew)), Args: []string{"ruby", pourScriptPath, installPaths[name]}, Env: installEnv(cfg.Prefix), Dir: "/home/linuxbrew", User: cfg.User})
		if err != nil {
			cancel()
			return nil, fmt.Errorf("offline install %q: %w", name, err)
		}
		if err := validateProtectedHomebrewRepository(cfg.Prefix, formulaTapOptions, true); err != nil {
			cancel()
			return nil, fmt.Errorf("validate protected Homebrew repository after %q: %w", name, err)
		}
		if err := validateNoPrefixBrewEnv(cfg.Prefix); err != nil {
			cancel()
			return nil, fmt.Errorf("validate Homebrew environment after %q: %w", name, err)
		}
		normalization, err := normalizeInstalledReceipt(cfg.Prefix, node, cfg.Record.Nodes, cfg.Record.SourceDateEpoch)
		if err != nil {
			cancel()
			return nil, err
		}
		if normalization != nil {
			evidence.ReceiptNormalizations = append(evidence.ReceiptNormalizations, *normalization)
		}
		after, err := snapshotContext(stepCtx, cfg.Prefix)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("snapshot after %q: %w", name, err)
		}
		select {
		case <-stepCtx.Done():
			cancel()
			return nil, stepCtx.Err()
		case <-time.After(25 * time.Millisecond):
		}
		stable, err := snapshotContext(stepCtx, cfg.Prefix)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("stable snapshot after %q: %w", name, err)
		}
		if !maps.Equal(after, stable) {
			return nil, fmt.Errorf("installer descendants continued mutating prefix after %q", name)
		}
		changes := diff(before, after)
		if err := classify(cfg.Prefix, node, before, after, changes, classifyOptions{
			optNames:            optNamesForNode(cfg.Record, node.Name),
			closureKegs:         resolvedClosureKegs(cfg.Record),
			verified:            verifiedByName[name],
			runtimeUID:          uint32(cfg.Record.Runtime.UID),
			runtimeGID:          uint32(cfg.Record.Runtime.GID),
			priorGdkPixbufCache: priorGdkPixbufCache,
		}); err != nil {
			return nil, fmt.Errorf("contain install %q: %w", name, err)
		}
		if err := reconcileInstalledKeg(cfg.Prefix, node, verifiedByName[name], after, reconcileKegOptions{closure: cfg.Record.Nodes}); err != nil {
			return nil, fmt.Errorf("reconcile installed keg %q: %w", name, err)
		}
		evidence.InstallDeltas = append(evidence.InstallDeltas, InstallDelta{Formula: name, Changes: changes})
		if err := verifyInstalledSubset(cfg.Prefix, cfg.Record, name); err != nil {
			return nil, err
		}
	}
	if err := verifyClosure(cfg.Prefix, cfg.Record); err != nil {
		return nil, err
	}
	return evidence, nil
}

func normalizeMaterializerPrefix(value string) (string, error) {
	if value == "" {
		value = DefaultPrefix
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("materializer prefix %q must be absolute", value)
	}
	clean := filepath.Clean(value)
	if clean == "/" {
		return "", errors.New("materializer prefix cannot be root")
	}
	return clean, nil
}

func validateNoPrefixBrewEnv(prefix string) error {
	directory := filepath.Join(prefix, "etc", "homebrew")
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("prefix Homebrew environment directory is not a real directory")
	}
	if _, err := os.Lstat(filepath.Join(directory, "brew.env")); err == nil {
		return fmt.Errorf("prefix Homebrew environment override is forbidden")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func installEnv(prefix string) []string {
	return []string{
		"HOME=/home/linuxbrew", "USER=linuxbrew", "LOGNAME=linuxbrew", "PATH=" + prefix + "/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOMEBREW_PREFIX=" + prefix, "HOMEBREW_REPOSITORY=" + prefix + "/Homebrew", "HOMEBREW_CELLAR=" + prefix + "/Cellar", "HOMEBREW_CACHE=/home/linuxbrew/.cache/Homebrew",
		"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_SYSTEM_ENV_TAKES_PRIORITY=1", "HOMEBREW_NO_INSTALL_FROM_API=1", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1",
	}
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, command Command) error {
	program := command.Path
	args := append([]string(nil), command.Args...)
	if command.User != "" {
		u, err := user.Lookup(command.User)
		if err != nil {
			return err
		}
		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			return err
		}
		gid, err := strconv.Atoi(u.Gid)
		if err != nil {
			return err
		}
		args = append([]string{"--reuid=" + strconv.Itoa(uid), "--regid=" + strconv.Itoa(gid), "--clear-groups", "--pdeathsig", "keep", "--", command.Path}, args...)
		program = "/usr/bin/setpriv"
	}
	cmd, cleanup, err := installerSupervisorCommand(ctx, program, args)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Env = append([]string(nil), command.Env...)
	cmd.Dir = command.Dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type fileState struct {
	Type           string
	Mode           fs.FileMode
	Size           int64
	Digest, Link   string
	Inode          string
	Links          uint64
	UID, GID       uint32
	OwnershipKnown bool
}

func snapshot(root string) (map[string]fileState, error) {
	return snapshotContext(context.Background(), root)
}
func snapshotContext(ctx context.Context, root string) (map[string]fileState, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("snapshot root must be a real directory")
	}
	limits := bottle.DefaultLimits()
	rootState := fileState{Type: "directory", Mode: rootInfo.Mode()}
	rootState.Inode, rootState.Links = snapshotInodeMeta(rootInfo)
	rootState.UID, rootState.GID, rootState.OwnershipKnown = snapshotOwnership(rootInfo)
	out := map[string]fileState{".": rootState}
	hashes := map[string]string{}
	files := 0
	var uniqueBytes int64
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == root {
			return nil
		}
		files++
		if files > limits.MaxFiles {
			return fmt.Errorf("snapshot exceeds %d entries", limits.MaxFiles)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		state := fileState{Mode: info.Mode()}
		state.Inode, state.Links = snapshotInodeMeta(info)
		state.UID, state.GID, state.OwnershipKnown = snapshotOwnership(info)
		switch {
		case info.Mode().IsDir():
			state.Type = "directory"
		case info.Mode().IsRegular():
			state.Type = "regular"
			state.Size = info.Size()
			if state.Size < 0 || state.Size > limits.MaxFileBytes {
				return fmt.Errorf("snapshot file %s exceeds %d bytes", p, limits.MaxFileBytes)
			}
			key := state.Inode
			if digest, ok := hashes[key]; ok && key != "" {
				state.Digest = digest
			} else {
				if uniqueBytes > limits.MaxExpandedBytes-state.Size {
					return fmt.Errorf("snapshot exceeds %d unique bytes", limits.MaxExpandedBytes)
				}
				uniqueBytes += state.Size
				sum, err := hashFileContext(ctx, p, state.Size)
				if err != nil {
					return err
				}
				state.Digest = sum
				if key != "" {
					hashes[key] = sum
				}
			}
		case info.Mode()&os.ModeSymlink != 0:
			state.Type = "symlink"
			state.Link, err = os.Readlink(p)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("special file %s", p)
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return fmt.Errorf("setid file %s", p)
		}
		out[filepath.ToSlash(rel)] = state
		return nil
	})
	return out, err
}
func snapshotInodeMeta(info os.FileInfo) (string, uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), uint64(stat.Nlink)
	}
	return "", 0
}

func snapshotOwnership(info os.FileInfo) (uint32, uint32, bool) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid, stat.Gid, true
	}
	return 0, 0, false
}
func hashFileContext(ctx context.Context, p string, expected int64) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > expected {
				return "", fmt.Errorf("file %s grew during snapshot", p)
			}
			if _, err := h.Write(buffer[:n]); err != nil {
				return "", err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if total != expected {
		return "", fmt.Errorf("file %s changed size during snapshot", p)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func diff(before, after map[string]fileState) []Change {
	keys := map[string]struct{}{}
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}
	var out []Change
	for k := range keys {
		b, bok := before[k]
		a, aok := after[k]
		kind := ""
		switch {
		case !bok:
			kind = "created"
		case !aok:
			kind = "removed"
		case !snapshotStatesEqual(b, a):
			kind = "modified"
		}
		if kind != "" {
			out = append(out, Change{Path: k, Kind: kind})
		}
	}
	slices.SortFunc(out, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return out
}

func snapshotStatesEqual(before, after fileState) bool {
	if before.Type == "directory" && after.Type == "directory" {
		// A directory's link count and storage size are structural metadata: both
		// may change solely because a verified install added or removed a child.
		// Preserve inode identity, mode, type, and ownership while excluding only
		// link count and storage size, which can change when children are added.
		before.Links = 0
		after.Links = 0
		before.Size = 0
		after.Size = 0
	}
	return before == after
}

func validateSharedDirectoryExpansions(prefix, currentKeg string, before, after map[string]fileState, changes []Change) (map[string]struct{}, error) {
	allowed := map[string]struct{}{}
	for _, change := range changes {
		if change.Kind != "modified" || isGlobalRoot(change.Path) || !inGlobal(change.Path) {
			continue
		}
		prior, existed := before[change.Path]
		current, remains := after[change.Path]
		if !existed || !remains || prior.Type != "symlink" || current.Type != "directory" {
			continue
		}
		paths, err := validateSharedDirectoryExpansion(prefix, currentKeg, change.Path, before, after)
		if err != nil {
			return nil, fmt.Errorf("validate shared directory expansion %s: %w", change.Path, err)
		}
		maps.Copy(allowed, paths)
	}
	return allowed, nil
}

func validateSharedDirectoryExpansion(prefix, currentKeg, globalRoot string, before, after map[string]fileState) (map[string]struct{}, error) {
	priorLink := before[globalRoot]
	expandedRoot := after[globalRoot]
	priorRoot, err := resolveSnapshotPath(prefix, before, globalRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve prior link: %w", err)
	}
	parts := strings.Split(priorRoot, "/")
	if len(parts) < 4 || parts[0] != "Cellar" || snapshotPathWithin(priorRoot, currentKeg) {
		return nil, fmt.Errorf("prior link does not resolve into a different installed keg")
	}
	priorRootState, ok := before[priorRoot]
	if !ok || priorRootState.Type != "directory" {
		return nil, fmt.Errorf("prior keg directory %s is absent", priorRoot)
	}
	currentRootPath := path.Join(currentKeg, globalRoot)
	currentRootState, ok := after[currentRootPath]
	if !ok || currentRootState.Type != "directory" {
		return nil, fmt.Errorf("current keg directory %s is absent", currentRootPath)
	}
	if !sameDirectorySecurity(priorRootState, expandedRoot) || !sameDirectorySecurity(currentRootState, expandedRoot) {
		return nil, fmt.Errorf("expanded directory mode or ownership differs from its keg directories")
	}
	if !sameSnapshotOwnership(priorLink, expandedRoot) {
		return nil, fmt.Errorf("expanded directory ownership differs from prior global link")
	}

	priorPaths := map[string]string{}
	for candidate := range before {
		if !strings.HasPrefix(candidate, priorRoot+"/") {
			continue
		}
		relative := strings.TrimPrefix(candidate, priorRoot+"/")
		priorPaths[path.Join(globalRoot, relative)] = candidate
	}
	allowed := map[string]struct{}{globalRoot: {}}
	for globalPath, priorPath := range priorPaths {
		priorState := before[priorPath]
		currentState, ok := after[globalPath]
		if !ok {
			return nil, fmt.Errorf("prior keg path %s is not represented at %s", priorPath, globalPath)
		}
		switch priorState.Type {
		case "directory":
			if !sameDirectorySecurity(priorState, currentState) {
				return nil, fmt.Errorf("preserved directory %s changed mode or ownership", globalPath)
			}
			if overlapping, exists := after[path.Join(currentKeg, globalPath)]; exists && !sameDirectorySecurity(overlapping, currentState) {
				return nil, fmt.Errorf("current keg conflicts with preserved directory %s", globalPath)
			}
		case "regular", "symlink":
			if currentState.Type != "symlink" || !sameSnapshotOwnership(expandedRoot, currentState) {
				return nil, fmt.Errorf("preserved path %s is not an owner-matched symlink", globalPath)
			}
			want, err := resolveSnapshotPath(prefix, before, priorPath)
			if err != nil {
				return nil, fmt.Errorf("resolve prior path %s: %w", priorPath, err)
			}
			got, err := resolveSnapshotPath(prefix, after, globalPath)
			if err != nil || got != want {
				return nil, fmt.Errorf("preserved link %s no longer resolves to %s", globalPath, want)
			}
			if _, overlaps := after[path.Join(currentKeg, globalPath)]; overlaps {
				return nil, fmt.Errorf("current keg overlaps preserved non-directory %s", globalPath)
			}
		default:
			return nil, fmt.Errorf("unsupported prior path type %s at %s", priorState.Type, priorPath)
		}
		allowed[globalPath] = struct{}{}
	}

	addedCurrent := 0
	for globalPath, currentState := range after {
		if !strings.HasPrefix(globalPath, globalRoot+"/") {
			continue
		}
		if _, preserved := priorPaths[globalPath]; preserved {
			continue
		}
		sourcePath := path.Join(currentKeg, globalPath)
		sourceState, ok := after[sourcePath]
		if !ok {
			return nil, fmt.Errorf("expanded path %s has no current-keg source", globalPath)
		}
		switch currentState.Type {
		case "directory":
			if !sameDirectorySecurity(sourceState, currentState) {
				return nil, fmt.Errorf("expanded directory %s differs from current-keg source", globalPath)
			}
		case "symlink":
			if !sameSnapshotOwnership(expandedRoot, currentState) {
				return nil, fmt.Errorf("expanded link %s has unexpected ownership", globalPath)
			}
			direct, err := directSnapshotSymlinkTarget(prefix, after, globalPath)
			if err != nil || direct != sourcePath {
				return nil, fmt.Errorf("expanded link %s bypasses current-keg source %s", globalPath, sourcePath)
			}
			want, err := resolveSnapshotPath(prefix, after, sourcePath)
			if err != nil {
				return nil, fmt.Errorf("resolve current-keg source %s: %w", sourcePath, err)
			}
			got, err := resolveSnapshotPath(prefix, after, globalPath)
			if err != nil || got != want {
				return nil, fmt.Errorf("expanded link %s does not resolve to current-keg source", globalPath)
			}
		default:
			return nil, fmt.Errorf("expanded path %s has unsafe type %s", globalPath, currentState.Type)
		}
		allowed[globalPath] = struct{}{}
		addedCurrent++
	}
	if addedCurrent == 0 {
		return nil, fmt.Errorf("expansion added no current-keg paths")
	}
	return allowed, nil
}

func sameDirectorySecurity(expected, actual fileState) bool {
	return expected.Type == "directory" && actual.Type == "directory" && expected.Mode == actual.Mode && sameSnapshotOwnership(expected, actual)
}

func sameSnapshotOwnership(expected, actual fileState) bool {
	if expected.OwnershipKnown != actual.OwnershipKnown {
		return false
	}
	return !expected.OwnershipKnown || (expected.UID == actual.UID && expected.GID == actual.GID)
}

type classifyOptions struct {
	optNames            map[string]struct{}
	closureKegs         map[string]struct{}
	verified            bottle.Result
	runtimeUID          uint32
	runtimeGID          uint32
	priorGdkPixbufCache []byte
}

func classify(prefix string, node resolution.Node, before, after map[string]fileState, changes []Change, optionsArg ...classifyOptions) error {
	keg := filepath.ToSlash(filepath.Join("Cellar", node.Name, node.PkgVersion))
	opt := filepath.ToSlash(filepath.Join("opt", node.Name))
	optNames := map[string]struct{}{node.Name: {}}
	options := classifyOptions{}
	if len(optionsArg) > 0 {
		options = optionsArg[0]
		for name := range options.optNames {
			optNames[name] = struct{}{}
		}
	}
	expandedSharedPaths, err := validateSharedDirectoryExpansions(prefix, keg, before, after, changes)
	if err != nil {
		return err
	}
	sharedMimeGenerated := map[string]struct{}{}
	if node.Name != sharedMimeInfoFormula && changesContainPathRoot(changes, sharedMimeDatabaseRoot) {
		return fmt.Errorf("formula %q changed the generated shared MIME database without a controlled refresh", node.Name)
	}
	if node.Name == sharedMimeInfoFormula && changesContainPathRoot(changes, sharedMimeDatabaseRoot) {
		sharedMimeGenerated, err = validateSharedMimeInfoDatabase(prefix, node, before, after, options)
		if err != nil {
			return fmt.Errorf("validate shared MIME database: %w", err)
		}
	}
	nodeNPMGenerated := map[string]struct{}{}
	if node.Name == nodeFormula {
		nodeNPMGenerated, err = validateNodeNPMRuntime(prefix, node, before, after, options)
		if err != nil {
			return fmt.Errorf("validate Node npm runtime: %w", err)
		}
	} else if changesContainPathRoot(changes, nodeNPMRuntimeRoot) {
		return fmt.Errorf("formula %q changed the Node npm runtime tree", node.Name)
	}
	gdkPixbufCacheValidated := false
	if changesContainPathRoot(changes, gdkPixbufLoadersDirectoryPath) || changesContainExactPath(changes, gdkPixbufLoadersCachePath) {
		kind, ok := changeKind(changes, gdkPixbufLoadersCachePath)
		if !ok {
			return fmt.Errorf("gdk-pixbuf loader set changed without refreshing %s", gdkPixbufLoadersCachePath)
		}
		state, ok := after[gdkPixbufLoadersCachePath]
		if !ok || state.Type != "regular" {
			return fmt.Errorf("gdk-pixbuf loader cache is absent or not a regular file after %s", node.Name)
		}
		if err := validateGdkPixbufLoadersCache(prefix, node, gdkPixbufLoadersCachePath, kind, before, state, after, options); err != nil {
			return fmt.Errorf("validate gdk-pixbuf loader cache: %w", err)
		}
		gdkPixbufCacheValidated = true
	}
	for i := range changes {
		c := &changes[i]
		p := c.Path
		_, existed := before[p]
		_, safeSharedExpansion := expandedSharedPaths[p]
		if existed && c.Kind != "created" && !safeSharedExpansion && !isPackageManagerState(p) && !isBrewedLoaderMutation(prefix, node, p, after) && !isControlledGdkPixbufLoadersCacheMutation(node, p, c.Kind, options.verified) && (p == "." || inGlobal(p) || p == "Cellar" || p == "etc" || strings.HasPrefix(p, "etc/") || p == "var" || strings.HasPrefix(p, "var/") || p == "opt" || strings.HasPrefix(p, "opt/")) {
			return fmt.Errorf("install modified or removed pre-existing shared path %s", p)
		}
		switch {
		case p == ".":
			c.Classification = "prefix-root"
		case p == "Cellar":
			c.Classification = "prefix-structure"
		case p == "opt":
			if a, ok := after[p]; !ok || a.Type != "directory" {
				return fmt.Errorf("opt root is not a real directory")
			}
			c.Classification = "prefix-structure"
		case p == filepath.ToSlash(filepath.Join("Cellar", node.Name)):
			c.Classification = "current-rack"
		case p == keg || strings.HasPrefix(p, keg+"/"):
			c.Classification = "current-keg"
		case strings.HasPrefix(p, "opt/"):
			parts := strings.Split(p, "/")
			if len(parts) != 2 {
				return fmt.Errorf("unexpected descendant below opt link %s", p)
			}
			if _, ok := optNames[parts[1]]; !ok {
				return fmt.Errorf("unexpected opt alias %s", p)
			}
			a, ok := after[p]
			if !ok || a.Type != "symlink" {
				return fmt.Errorf("opt entry %s is not a symlink", p)
			}
			resolved, err := resolveSnapshotPath(prefix, after, p)
			if err != nil || resolved != keg {
				return fmt.Errorf("opt link %s does not resolve exactly to %s", p, keg)
			}
			c.Classification = "current-opt"
		case p == "Homebrew" || strings.HasPrefix(p, "Homebrew/") || p == "bin/brew":
			return fmt.Errorf("protected Homebrew tooling changed at %s", p)
		case strings.HasPrefix(p, "Cellar/"):
			return fmt.Errorf("another keg changed at %s", p)
		case p == "etc":
			if a, ok := after[p]; !ok || a.Type != "directory" {
				return fmt.Errorf("etc root is not a real directory")
			}
			c.Classification = "configuration"
		case strings.HasPrefix(p, "etc/"):
			c.Classification = "configuration"
			if a, ok := after[p]; ok && a.Type == "symlink" {
				resolved, err := resolveSnapshotPath(prefix, after, p)
				if err != nil || (!snapshotPathWithin(resolved, "etc") && !isCurrentKegBashCompletionLink(prefix, after, p, resolved, keg) && !isCurrentGlibcLoaderConfigurationLink(prefix, node, options.verified, after, p, resolved, keg)) {
					return fmt.Errorf("configuration symlink %s escapes etc", p)
				}
			}
		case isPackageManagerState(p):
			c.Classification = "package-manager-state"
			if a, ok := after[p]; ok && a.Type == "symlink" {
				resolved, err := resolveSnapshotPath(prefix, after, p)
				if err != nil || !snapshotPathWithin(resolved, keg) {
					return fmt.Errorf("package-manager symlink %s does not resolve into current keg", p)
				}
			}
			if a, ok := after[p]; ok && a.Type == "regular" && a.Mode.Perm()&0o111 != 0 {
				return fmt.Errorf("package-manager state contains unexpected executable at %s", p)
			}
		case hasPath(nodeNPMGenerated, p):
			c.Classification = "node-npm-runtime"
		case p == "var":
			if a, ok := after[p]; !ok || a.Type != "directory" {
				return fmt.Errorf("var root is not a real directory")
			}
			c.Classification = "runtime-state"
		case strings.HasPrefix(p, "var/"):
			c.Classification = "runtime-state"
			if a, ok := after[p]; ok && a.Type == "symlink" {
				resolved, err := resolveSnapshotPath(prefix, after, p)
				if err != nil || !snapshotPathWithin(resolved, "var") {
					return fmt.Errorf("runtime-state symlink %s escapes var", p)
				}
			}
		case inGlobal(p):
			if isGlobalRoot(p) {
				if a, ok := after[p]; !ok || a.Type != "directory" {
					return fmt.Errorf("global root %s is not a real directory", p)
				}
				c.Classification = "global-root"
				break
			}
			c.Classification = "global-link-or-data"
			if a, ok := after[p]; ok && a.Type == "regular" {
				root := strings.SplitN(p, "/", 2)[0]
				if root != "share" {
					if p != gdkPixbufLoadersCachePath || !gdkPixbufCacheValidated {
						return fmt.Errorf("unexpected regular file in global %s tree at %s", root, p)
					}
					c.Classification = "gdk-pixbuf-loader-cache"
				}
				if snapshotPathWithin(p, sharedMimeDatabaseRoot) {
					if _, generated := sharedMimeGenerated[p]; generated {
						c.Classification = "shared-mime-database"
					} else if !globalPathMatchesVerifiedBottle(prefix, node, options.verified, p, a, after) {
						return fmt.Errorf("unverified regular file in shared MIME database at %s", p)
					}
				}
				if a.Mode.Perm()&0o111 != 0 {
					return fmt.Errorf("unexpected executable outside current keg at %s", p)
				}
			}
			if a, ok := after[p]; ok && a.Type == "symlink" {
				resolved, err := resolveSnapshotPath(prefix, after, p)
				_, preservedByExpansion := expandedSharedPaths[p]
				nodeNPMLink := err == nil && isNodeNPMGlobalLink(prefix, node, keg, p, resolved, after, nodeNPMGenerated)
				if err != nil || (!snapshotPathWithin(resolved, keg) && !preservedByExpansion && !nodeNPMLink) {
					return fmt.Errorf("global link %s does not resolve into current keg", p)
				}
				if nodeNPMLink {
					c.Classification = "node-npm-link"
				}
			}
		default:
			return fmt.Errorf("unexpected prefix change at %s", p)
		}
	}
	for _, root := range []string{"etc", "var"} {
		if a, ok := after[root]; !ok || a.Type != "directory" {
			return fmt.Errorf("%s root is absent or not a directory", root)
		}
	}
	if a, ok := after["opt"]; !ok || a.Type != "directory" {
		return fmt.Errorf("opt root is absent or not a directory")
	}
	if a, ok := after[opt]; !ok || a.Type != "symlink" {
		return fmt.Errorf("canonical opt link %s is absent", opt)
	} else {
		resolved, err := resolveSnapshotPath(prefix, after, opt)
		if err != nil || resolved != keg {
			return fmt.Errorf("canonical opt link %s does not resolve exactly to %s", opt, keg)
		}
	}
	return nil
}

const (
	sharedMimeInfoFormula              = "shared-mime-info"
	sharedMimeDatabaseRoot             = "share/mime"
	sharedMimeVerifiedSourcePath       = "share/mime/packages/freedesktop.org.xml"
	sharedMimeDatabaseMaxEntries       = 4096
	sharedMimeDatabaseMaxFiles         = 2048
	sharedMimeDatabaseMaxBytes   int64 = 32 << 20
	sharedMimeDatabaseMaxFile    int64 = 8 << 20

	gdkPixbufFormula               = "gdk-pixbuf"
	gdkPixbufLoadersCachePath      = "lib/gdk-pixbuf-2.0/2.10.0/loaders.cache"
	gdkPixbufLoadersDirectoryPath  = "lib/gdk-pixbuf-2.0/2.10.0/loaders"
	gdkPixbufLoadersCacheMaxBytes  = int64(1 << 20)
	gdkPixbufLoadersCacheMaxModule = 256

	nodeFormula                    = "node"
	nodeNPMSourceRoot              = "libexec/lib/node_modules/npm"
	nodeNPMRuntimeParent           = "lib/node_modules"
	nodeNPMRuntimeRoot             = "lib/node_modules/npm"
	nodeNPMRuntimeMaxEntries       = 10_000
	nodeNPMRuntimeMaxBytes   int64 = 256 << 20
)

var sharedMimeFixedOutputs = map[string]struct{}{
	"XMLnamespaces": {},
	"aliases":       {},
	"generic-icons": {},
	"globs":         {},
	"globs2":        {},
	"icons":         {},
	"magic":         {},
	"mime.cache":    {},
	"subclasses":    {},
	"treemagic":     {},
	"types":         {},
	"version":       {},
}

var sharedMimeGeneratedTypes = map[string]struct{}{
	"application": {},
	"audio":       {},
	"chemical":    {},
	"font":        {},
	"image":       {},
	"inode":       {},
	"message":     {},
	"model":       {},
	"multipart":   {},
	"text":        {},
	"video":       {},
	"x-content":   {},
	"x-epoc":      {},
}

func validateNodeNPMRuntime(prefix string, node resolution.Node, before, after map[string]fileState, options classifyOptions) (map[string]struct{}, error) {
	if node.Name != nodeFormula || !verifiedBottleMatchesNode(node, options.verified) {
		return nil, fmt.Errorf("verified Node bottle identity is absent")
	}
	if options.runtimeUID == 0 || options.runtimeGID == 0 {
		return nil, fmt.Errorf("authenticated runtime identity is absent")
	}
	for rel := range before {
		if snapshotPathWithin(rel, nodeNPMRuntimeRoot) {
			return nil, fmt.Errorf("Node npm runtime root existed before Node installation")
		}
	}

	keg := path.Join("Cellar", node.Name, node.PkgVersion)
	sourceRoot := path.Join(keg, nodeNPMSourceRoot)
	sourceRootState, ok := after[sourceRoot]
	if !ok || sourceRootState.Type != "directory" {
		return nil, fmt.Errorf("verified Node npm source root is absent")
	}
	parent, ok := after[nodeNPMRuntimeParent]
	if !ok || !sameDirectorySecurity(sourceRootState, parent) {
		return nil, fmt.Errorf("Node npm runtime parent does not match the verified source directory")
	}
	runtimeRoot, ok := after[nodeNPMRuntimeRoot]
	if !ok || !sameDirectorySecurity(sourceRootState, runtimeRoot) {
		return nil, fmt.Errorf("Node npm runtime root does not match the verified source directory")
	}

	npmrcPath := path.Join(nodeNPMRuntimeRoot, "npmrc")
	allowed := map[string]struct{}{nodeNPMRuntimeParent: {}, nodeNPMRuntimeRoot: {}}
	expected := map[string]struct{}{nodeNPMRuntimeRoot: {}}
	sourceEntries := 0
	for sourcePath, source := range after {
		if !snapshotPathWithin(sourcePath, sourceRoot) {
			continue
		}
		suffix := strings.TrimPrefix(sourcePath, sourceRoot)
		destinationPath := nodeNPMRuntimeRoot + suffix
		destination, ok := after[destinationPath]
		if !ok {
			return nil, fmt.Errorf("Node npm runtime copy is missing %s", destinationPath)
		}
		if destinationPath != npmrcPath {
			if err := validateNodeNPMCopiedEntry(prefix, after, sourceRoot, nodeNPMRuntimeRoot, sourcePath, destinationPath, source, destination); err != nil {
				return nil, err
			}
		}
		expected[destinationPath] = struct{}{}
		allowed[destinationPath] = struct{}{}
		sourceEntries++
		if sourceEntries > nodeNPMRuntimeMaxEntries {
			return nil, fmt.Errorf("Node npm runtime exceeds %d entries", nodeNPMRuntimeMaxEntries)
		}
	}
	if sourceEntries <= 1 {
		return nil, fmt.Errorf("verified Node npm source tree is empty")
	}

	npmrc, ok := after[npmrcPath]
	expectedNPMRC := []byte("prefix = " + filepath.ToSlash(prefix) + "\n")
	npmrcDigest := sha256.Sum256(expectedNPMRC)
	if !ok || npmrc.Type != "regular" || npmrc.Mode.Perm() != 0o644 || npmrc.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || npmrc.Size != int64(len(expectedNPMRC)) || npmrc.Digest != hex.EncodeToString(npmrcDigest[:]) || npmrc.Links != 1 || !npmrc.OwnershipKnown || npmrc.UID != options.runtimeUID || npmrc.GID != options.runtimeGID {
		return nil, fmt.Errorf("generated Node npmrc is missing or invalid")
	}
	allowed[npmrcPath] = struct{}{}
	expected[npmrcPath] = struct{}{}

	if len(expected) > nodeNPMRuntimeMaxEntries {
		return nil, fmt.Errorf("Node npm runtime exceeds %d entries", nodeNPMRuntimeMaxEntries)
	}
	var totalBytes int64
	for destinationPath := range expected {
		destination := after[destinationPath]
		if destination.Type != "regular" {
			continue
		}
		if destination.Size < 0 || totalBytes > nodeNPMRuntimeMaxBytes-destination.Size {
			return nil, fmt.Errorf("Node npm runtime exceeds %d bytes", nodeNPMRuntimeMaxBytes)
		}
		totalBytes += destination.Size
	}

	for destinationPath := range after {
		if !snapshotPathWithin(destinationPath, nodeNPMRuntimeRoot) {
			continue
		}
		if _, ok := expected[destinationPath]; !ok {
			return nil, fmt.Errorf("Node npm runtime contains unverified path %s", destinationPath)
		}
	}
	return allowed, nil
}

func validateNodeNPMCopiedEntry(prefix string, snapshot map[string]fileState, sourceRoot, destinationRoot, sourcePath, destinationPath string, source, destination fileState) error {
	if source.Type != destination.Type || source.Mode != destination.Mode || !sameSnapshotOwnership(source, destination) {
		return fmt.Errorf("Node npm runtime path %s differs from verified source metadata", destinationPath)
	}
	switch source.Type {
	case "directory":
		if !sameDirectorySecurity(source, destination) {
			return fmt.Errorf("Node npm runtime directory %s differs from verified source", destinationPath)
		}
	case "regular":
		if source.Size != destination.Size || source.Digest != destination.Digest || source.Links != 1 || destination.Links != 1 {
			return fmt.Errorf("Node npm runtime file %s differs from verified source", destinationPath)
		}
	case "symlink":
		if source.Link != destination.Link {
			return fmt.Errorf("Node npm runtime symlink %s differs from verified source", destinationPath)
		}
		sourceResolved, err := resolveSnapshotPath(prefix, snapshot, sourcePath)
		if err != nil || !snapshotPathWithin(sourceResolved, sourceRoot) {
			return fmt.Errorf("verified Node npm source symlink %s escapes its source tree", sourcePath)
		}
		destinationResolved, err := resolveSnapshotPath(prefix, snapshot, destinationPath)
		if err != nil || !snapshotPathWithin(destinationResolved, destinationRoot) {
			return fmt.Errorf("Node npm runtime symlink %s escapes its runtime tree", destinationPath)
		}
		if strings.TrimPrefix(sourceResolved, sourceRoot) != strings.TrimPrefix(destinationResolved, destinationRoot) {
			return fmt.Errorf("Node npm runtime symlink %s resolves differently from verified source", destinationPath)
		}
	default:
		return fmt.Errorf("Node npm runtime path %s has unsafe type %s", destinationPath, source.Type)
	}
	return nil
}

func isNodeNPMGlobalLink(prefix string, node resolution.Node, keg, rel, resolved string, snapshot map[string]fileState, generated map[string]struct{}) bool {
	if node.Name != nodeFormula || len(generated) == 0 {
		return false
	}
	commands := map[string]string{
		"bin/npm": path.Join(nodeNPMRuntimeRoot, "bin/npm-cli.js"),
		"bin/npx": path.Join(nodeNPMRuntimeRoot, "bin/npx-cli.js"),
	}
	if target, ok := commands[rel]; ok {
		sourcePath := path.Join(keg, rel)
		direct, err := directSnapshotSymlinkTarget(prefix, snapshot, rel)
		if err != nil || direct != sourcePath {
			return false
		}
		sourceResolved, err := resolveSnapshotPath(prefix, snapshot, sourcePath)
		if err != nil || sourceResolved != target || resolved != target {
			return false
		}
		state, ok := snapshot[target]
		_, generatedTarget := generated[target]
		return ok && generatedTarget && state.Type == "regular" && state.Mode.Perm()&0o111 != 0
	}

	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != "share" || parts[1] != "man" || (parts[2] != "man1" && parts[2] != "man5" && parts[2] != "man7") {
		return false
	}
	target := path.Join(nodeNPMRuntimeRoot, "man", parts[2], parts[3])
	direct, err := directSnapshotSymlinkTarget(prefix, snapshot, rel)
	if err != nil || direct != target || resolved != target {
		return false
	}
	state, ok := snapshot[target]
	_, generatedTarget := generated[target]
	return ok && generatedTarget && state.Type == "regular" && state.Mode.Perm()&0o111 == 0
}

func hasPath(values map[string]struct{}, value string) bool {
	_, ok := values[value]
	return ok
}

func changesContainPathRoot(changes []Change, root string) bool {
	for _, change := range changes {
		if snapshotPathWithin(change.Path, root) {
			return true
		}
	}
	return false
}

func changesContainExactPath(changes []Change, target string) bool {
	_, ok := changeKind(changes, target)
	return ok
}

func changeKind(changes []Change, target string) (string, bool) {
	for _, change := range changes {
		if change.Path == target {
			return change.Kind, true
		}
	}
	return "", false
}

func validateSharedMimeInfoDatabase(prefix string, node resolution.Node, before, after map[string]fileState, options classifyOptions) (map[string]struct{}, error) {
	if node.Name != sharedMimeInfoFormula || !verifiedBottleMatchesNode(node, options.verified) {
		return nil, fmt.Errorf("verified shared-mime-info bottle identity is absent")
	}
	if options.runtimeUID == 0 || options.runtimeGID == 0 {
		return nil, fmt.Errorf("authenticated runtime identity is absent")
	}
	if _, exists := before[sharedMimeDatabaseRoot]; exists {
		return nil, fmt.Errorf("shared MIME database root existed before shared-mime-info installation")
	}
	root, ok := after[sharedMimeDatabaseRoot]
	if !ok || root.Type != "directory" {
		return nil, fmt.Errorf("shared MIME database root is absent or not a directory")
	}
	if err := validateSharedMimeGeneratedDirectory(sharedMimeDatabaseRoot, root, options.runtimeUID, options.runtimeGID); err != nil {
		return nil, err
	}
	source, ok := after[sharedMimeVerifiedSourcePath]
	if !ok || !globalPathMatchesVerifiedBottle(prefix, node, options.verified, sharedMimeVerifiedSourcePath, source, after) {
		return nil, fmt.Errorf("verified freedesktop.org.xml source is not linked from the shared-mime-info keg")
	}

	generated := map[string]struct{}{}
	fixedSeen := map[string]struct{}{}
	typeCounts := map[string]int{}
	entries := 0
	files := 0
	var totalBytes int64
	for rel, state := range after {
		if !snapshotPathWithin(rel, sharedMimeDatabaseRoot) || rel == sharedMimeDatabaseRoot {
			continue
		}
		entries++
		if entries > sharedMimeDatabaseMaxEntries {
			return nil, fmt.Errorf("shared MIME database exceeds %d entries", sharedMimeDatabaseMaxEntries)
		}
		if rel == sharedMimeVerifiedSourcePath {
			continue
		}
		if state.Type == "directory" {
			if err := validateSharedMimeGeneratedDirectory(rel, state, options.runtimeUID, options.runtimeGID); err != nil {
				return nil, err
			}
			continue
		}
		if state.Type != "regular" || !isSharedMimeGeneratedFilePath(rel) {
			return nil, fmt.Errorf("unexpected shared MIME database entry %s of type %s", rel, state.Type)
		}
		if err := validateSharedMimeGeneratedFileMetadata(rel, state, options.runtimeUID, options.runtimeGID); err != nil {
			return nil, err
		}
		if totalBytes > sharedMimeDatabaseMaxBytes-state.Size {
			return nil, fmt.Errorf("shared MIME database exceeds %d bytes", sharedMimeDatabaseMaxBytes)
		}
		totalBytes += state.Size
		files++
		if files > sharedMimeDatabaseMaxFiles {
			return nil, fmt.Errorf("shared MIME database exceeds %d generated files", sharedMimeDatabaseMaxFiles)
		}
		data, err := readStableSnapshotFile(prefix, rel, state)
		if err != nil {
			return nil, fmt.Errorf("read generated shared MIME file %s: %w", rel, err)
		}
		sub := strings.TrimPrefix(rel, sharedMimeDatabaseRoot+"/")
		if !strings.Contains(sub, "/") {
			fixedSeen[sub] = struct{}{}
		} else {
			mimeType := strings.SplitN(sub, "/", 2)[0]
			typeCounts[mimeType]++
			if err := validateGeneratedSharedMimeXML(rel, data); err != nil {
				return nil, err
			}
		}
		generated[rel] = struct{}{}
	}
	for name := range sharedMimeFixedOutputs {
		if _, ok := fixedSeen[name]; !ok {
			return nil, fmt.Errorf("shared MIME database is missing required output %s", name)
		}
	}
	for name := range sharedMimeGeneratedTypes {
		if typeCounts[name] == 0 {
			return nil, fmt.Errorf("shared MIME database is missing generated XML for type %s", name)
		}
	}
	return generated, nil
}

func validateSharedMimeGeneratedDirectory(rel string, state fileState, runtimeUID, runtimeGID uint32) error {
	if state.Type != "directory" || !state.Mode.IsDir() {
		return fmt.Errorf("shared MIME path %s is not a directory", rel)
	}
	if state.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || state.Mode.Perm()&0o022 != 0 {
		return fmt.Errorf("shared MIME directory %s has unsafe permissions", rel)
	}
	if !state.OwnershipKnown || state.UID != runtimeUID || state.GID != runtimeGID {
		return fmt.Errorf("shared MIME directory %s owner does not match runtime uid/gid %d:%d", rel, runtimeUID, runtimeGID)
	}
	return nil
}

func validateSharedMimeGeneratedFileMetadata(rel string, state fileState, runtimeUID, runtimeGID uint32) error {
	if state.Type != "regular" || !state.Mode.IsRegular() {
		return fmt.Errorf("shared MIME output %s is not an ordinary regular file", rel)
	}
	if state.Size < 0 || state.Size > sharedMimeDatabaseMaxFile {
		return fmt.Errorf("shared MIME output %s size %d exceeds %d bytes", rel, state.Size, sharedMimeDatabaseMaxFile)
	}
	if state.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || state.Mode.Perm()&0o133 != 0 {
		return fmt.Errorf("shared MIME output %s has unsafe permissions", rel)
	}
	if state.Links != 1 {
		return fmt.Errorf("shared MIME output %s link count is %d, expected 1", rel, state.Links)
	}
	if !state.OwnershipKnown || state.UID != runtimeUID || state.GID != runtimeGID {
		return fmt.Errorf("shared MIME output %s owner does not match runtime uid/gid %d:%d", rel, runtimeUID, runtimeGID)
	}
	if len(state.Digest) != sha256.Size*2 {
		return fmt.Errorf("shared MIME output %s digest is absent or malformed", rel)
	}
	if _, err := hex.DecodeString(state.Digest); err != nil {
		return fmt.Errorf("shared MIME output %s digest is malformed: %w", rel, err)
	}
	return nil
}

func isSharedMimeGeneratedFilePath(rel string) bool {
	if !strings.HasPrefix(rel, sharedMimeDatabaseRoot+"/") {
		return false
	}
	sub := strings.TrimPrefix(rel, sharedMimeDatabaseRoot+"/")
	if _, ok := sharedMimeFixedOutputs[sub]; ok {
		return true
	}
	parts := strings.Split(sub, "/")
	if len(parts) != 2 {
		return false
	}
	if _, ok := sharedMimeGeneratedTypes[parts[0]]; !ok || !strings.HasSuffix(parts[1], ".xml") {
		return false
	}
	base := strings.TrimSuffix(parts[1], ".xml")
	if base == "" {
		return false
	}
	for _, r := range base {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._+@-", r) {
			return false
		}
	}
	return true
}

func validateGeneratedSharedMimeXML(rel string, data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("generated shared MIME XML %s is not valid UTF-8", rel)
	}
	sub := strings.TrimPrefix(rel, sharedMimeDatabaseRoot+"/")
	if !strings.HasSuffix(sub, ".xml") || !strings.Contains(sub, "/") {
		return fmt.Errorf("generated shared MIME XML %s has an invalid path", rel)
	}
	expectedType := strings.TrimSuffix(sub, ".xml")
	const namespace = "http://www.freedesktop.org/standards/shared-mime-info"
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	rootCount := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("parse generated shared MIME XML %s: %w", rel, err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				rootCount++
				if rootCount != 1 || value.Name.Local != "mime-type" || value.Name.Space != namespace {
					return fmt.Errorf("generated shared MIME XML %s has an unexpected root element", rel)
				}
				mimeType := ""
				typeSeen := false
				for _, attr := range value.Attr {
					if attr.Name.Space == "" && attr.Name.Local == "type" {
						if typeSeen {
							return fmt.Errorf("generated shared MIME XML %s repeats the type attribute", rel)
						}
						typeSeen = true
						mimeType = attr.Value
					}
				}
				// update-mime-database lower-cases generated filenames even when
				// the registered MIME subtype retains mixed case (for example,
				// macroEnabled). MIME type tokens are case-insensitive.
				if !equalASCIIFold(mimeType, expectedType) {
					return fmt.Errorf("generated shared MIME XML %s declares type %q, expected %q", rel, mimeType, expectedType)
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 {
				return fmt.Errorf("generated shared MIME XML %s has an invalid element boundary", rel)
			}
		case xml.CharData:
			if depth == 0 && !isXMLWhitespace(value) {
				return fmt.Errorf("generated shared MIME XML %s has character data outside the root element", rel)
			}
		}
	}
	if rootCount != 1 || depth != 0 {
		return fmt.Errorf("generated shared MIME XML %s has no unique complete root element", rel)
	}
	return nil
}

func equalASCIIFold(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range len(left) {
		a, b := left[i], right[i]
		if a >= utf8.RuneSelf || b >= utf8.RuneSelf {
			return false
		}
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

func isXMLWhitespace(data []byte) bool {
	for _, value := range data {
		switch value {
		case ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	return true
}

func globalPathMatchesVerifiedBottle(prefix string, node resolution.Node, verified bottle.Result, rel string, global fileState, snapshot map[string]fileState) bool {
	if !verifiedBottleMatchesNode(node, verified) {
		return false
	}
	var inventory *bottle.InventoryEntry
	for i := range verified.Inventory {
		entry := &verified.Inventory[i]
		if entry.KegPath == rel && entry.Type == bottle.EntryRegular {
			inventory = entry
			break
		}
	}
	if inventory == nil {
		return false
	}
	keg := path.Join("Cellar", node.Name, node.PkgVersion)
	sourceRel := path.Join(keg, rel)
	source, ok := snapshot[sourceRel]
	if !ok || source.Type != "regular" || source.Size != inventory.Size || source.Mode.Perm() != os.FileMode(inventory.Mode).Perm() || source.Digest != strings.TrimPrefix(inventory.SHA256, "sha256:") {
		return false
	}
	switch global.Type {
	case "symlink":
		resolved, err := resolveSnapshotPath(prefix, snapshot, rel)
		return err == nil && resolved == sourceRel
	case "regular":
		return global.Size == source.Size && global.Mode.Perm() == source.Mode.Perm() && global.Digest == source.Digest
	default:
		return false
	}
}

func isControlledGdkPixbufLoadersCacheMutation(node resolution.Node, rel, kind string, verified bottle.Result) bool {
	if rel != gdkPixbufLoadersCachePath || !verifiedBottleMatchesNode(node, verified) {
		return false
	}
	if node.Name == gdkPixbufFormula {
		return kind == "created"
	}
	return kind == "modified" && nodeDependsOn(node, gdkPixbufFormula) && len(verifiedGdkPixbufLoaders(node, verified)) > 0
}

func verifiedBottleMatchesNode(node resolution.Node, verified bottle.Result) bool {
	return node.Name != "" && node.PkgVersion != "" &&
		verified.Name == node.Name && verified.PkgVersion == node.PkgVersion &&
		verified.KegPrefix == path.Join(node.Name, node.PkgVersion)
}

func nodeDependsOn(node resolution.Node, dependency string) bool {
	for _, requirement := range node.Dependencies {
		if requirement.Name == dependency {
			return true
		}
	}
	return false
}

func verifiedGdkPixbufLoaders(node resolution.Node, verified bottle.Result) map[string]struct{} {
	loaders := map[string]struct{}{}
	if !verifiedBottleMatchesNode(node, verified) {
		return loaders
	}
	for _, entry := range verified.Inventory {
		if entry.Type != bottle.EntryRegular || path.Dir(entry.KegPath) != gdkPixbufLoadersDirectoryPath {
			continue
		}
		if isGdkPixbufLoaderBasename(path.Base(entry.KegPath)) {
			loaders[entry.KegPath] = struct{}{}
		}
	}
	return loaders
}

func validateGdkPixbufLoadersCache(prefix string, node resolution.Node, rel, kind string, before map[string]fileState, state fileState, after map[string]fileState, options classifyOptions) error {
	if !isControlledGdkPixbufLoadersCacheMutation(node, rel, kind, options.verified) {
		return fmt.Errorf("verified formula %q may not %s %s", node.Name, kind, rel)
	}
	if options.runtimeUID == 0 || options.runtimeGID == 0 {
		return fmt.Errorf("authenticated runtime identity is absent")
	}
	if kind == "created" {
		if _, ok := before[rel]; ok {
			return fmt.Errorf("creation unexpectedly replaced a pre-existing cache")
		}
	} else {
		prior, ok := before[rel]
		if !ok {
			return fmt.Errorf("refresh requires a pre-existing cache")
		}
		if err := validateGdkPixbufLoadersCacheMetadata("pre-existing", prior, options.runtimeUID, options.runtimeGID); err != nil {
			return err
		}
	}
	if err := validateGdkPixbufLoadersCacheMetadata("generated", state, options.runtimeUID, options.runtimeGID); err != nil {
		return err
	}
	if len(options.closureKegs) == 0 {
		return fmt.Errorf("resolved closure keg set is empty")
	}

	data, err := readStableSnapshotFile(prefix, rel, state)
	if err != nil {
		return err
	}
	cacheModules := map[string]gdkPixbufCacheModule{}
	if err := validateGdkPixbufLoadersCacheContent(prefix, data, after, options.closureKegs, cacheModules); err != nil {
		return err
	}
	afterModules, err := installedGlobalGdkPixbufLoaders(prefix, after, options.closureKegs)
	if err != nil {
		return err
	}
	if !gdkPixbufCacheTargetsEqual(cacheModules, afterModules) {
		return fmt.Errorf("cache module set does not exactly match installed global loader symlinks")
	}
	beforeModules, err := installedGlobalGdkPixbufLoaders(prefix, before, options.closureKegs)
	if err != nil {
		return fmt.Errorf("validate pre-existing global loader symlinks: %w", err)
	}
	for module, target := range beforeModules {
		if afterTarget, ok := afterModules[module]; !ok || afterTarget != target {
			return fmt.Errorf("pre-existing loader module %s was removed or retargeted", module)
		}
	}
	if kind == "created" {
		if len(options.priorGdkPixbufCache) != 0 {
			return fmt.Errorf("initial cache creation unexpectedly has captured prior content")
		}
		if len(beforeModules) != 0 {
			return fmt.Errorf("initial gdk-pixbuf cache creation found pre-existing global loaders")
		}
		return nil
	}
	if len(options.priorGdkPixbufCache) == 0 {
		return fmt.Errorf("cache refresh is missing captured pre-install content")
	}
	priorCacheModules := map[string]gdkPixbufCacheModule{}
	if err := validateGdkPixbufLoadersCacheContent(prefix, options.priorGdkPixbufCache, before, options.closureKegs, priorCacheModules); err != nil {
		return fmt.Errorf("validate pre-install gdk-pixbuf loader cache: %w", err)
	}
	for module, prior := range priorCacheModules {
		current, ok := cacheModules[module]
		if !ok {
			return fmt.Errorf("pre-existing cache module %s was removed", module)
		}
		if current.resolved != prior.resolved || current.block != prior.block {
			return fmt.Errorf("pre-existing cache module %s was rewritten", module)
		}
	}

	contributed := verifiedGdkPixbufLoaders(node, options.verified)
	currentKeg := path.Join("Cellar", node.Name, node.PkgVersion)
	newLoaders := 0
	for module, target := range afterModules {
		if _, existed := beforeModules[module]; existed {
			continue
		}
		if !snapshotPathWithin(target, currentKeg) {
			return fmt.Errorf("new loader module %s does not resolve into modifier keg", module)
		}
		kegPath := strings.TrimPrefix(target, currentKeg+"/")
		if _, ok := contributed[kegPath]; !ok {
			return fmt.Errorf("new loader module %s is absent from the verified bottle inventory", module)
		}
		newLoaders++
	}
	if newLoaders == 0 {
		return fmt.Errorf("verified modifier registered no new loader module")
	}
	return nil
}

type gdkPixbufCacheModule struct {
	resolved string
	block    string
}

func gdkPixbufCacheTargetsEqual(cache map[string]gdkPixbufCacheModule, installed map[string]string) bool {
	if len(cache) != len(installed) {
		return false
	}
	for module, target := range installed {
		entry, ok := cache[module]
		if !ok || entry.resolved != target {
			return false
		}
	}
	return true
}

func validateGdkPixbufLoadersCacheMetadata(label string, state fileState, runtimeUID, runtimeGID uint32) error {
	if state.Type != "regular" || !state.Mode.IsRegular() {
		return fmt.Errorf("%s cache is not an ordinary regular file", label)
	}
	if state.Size <= 0 || state.Size > gdkPixbufLoadersCacheMaxBytes {
		return fmt.Errorf("%s cache size %d is outside 1..%d bytes", label, state.Size, gdkPixbufLoadersCacheMaxBytes)
	}
	if state.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%s cache has special permission bits", label)
	}
	if state.Mode.Perm()&0o111 != 0 {
		return fmt.Errorf("%s cache is executable", label)
	}
	if state.Mode.Perm()&0o022 != 0 {
		return fmt.Errorf("%s cache is group/other writable", label)
	}
	if state.Links != 1 {
		return fmt.Errorf("%s cache link count is %d, expected 1", label, state.Links)
	}
	if !state.OwnershipKnown || state.UID != runtimeUID || state.GID != runtimeGID {
		return fmt.Errorf("%s cache owner does not match authenticated runtime uid/gid %d:%d", label, runtimeUID, runtimeGID)
	}
	if len(state.Digest) != sha256.Size*2 {
		return fmt.Errorf("%s cache digest is absent or malformed", label)
	}
	if _, err := hex.DecodeString(state.Digest); err != nil {
		return fmt.Errorf("%s cache digest is malformed: %w", label, err)
	}
	return nil
}

func readStableSnapshotFile(prefix, rel string, state fileState) ([]byte, error) {
	filename := filepath.Join(prefix, filepath.FromSlash(rel))
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, fmt.Errorf("lstat generated file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode() != state.Mode || info.Size() != state.Size {
		return nil, fmt.Errorf("generated file no longer matches its snapshot metadata")
	}
	inode, links := snapshotInodeMeta(info)
	if links != 1 || (state.Inode != "" && inode != state.Inode) {
		return nil, fmt.Errorf("generated file no longer matches its snapshot inode")
	}

	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open generated file: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened generated file: %w", err)
	}
	openedInode, openedLinks := snapshotInodeMeta(openedInfo)
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode() != state.Mode || openedInfo.Size() != state.Size || openedLinks != 1 || (inode != "" && openedInode != inode) {
		return nil, fmt.Errorf("opened generated file differs from its no-follow path")
	}
	data, err := io.ReadAll(io.LimitReader(file, state.Size+1))
	if err != nil {
		return nil, fmt.Errorf("read generated file: %w", err)
	}
	if int64(len(data)) != state.Size {
		return nil, fmt.Errorf("generated file changed size while reading")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != state.Digest {
		return nil, fmt.Errorf("generated file content differs from its stable snapshot")
	}
	return data, nil
}

type gdkPixbufCacheToken struct {
	value  string
	quoted bool
}

func validateGdkPixbufLoadersCacheContent(prefix string, data []byte, after map[string]fileState, closureKegs map[string]struct{}, modules map[string]gdkPixbufCacheModule) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("cache is not valid UTF-8")
	}
	for offset, value := range data {
		if (value < 0x20 && value != '\n') || value == 0x7f {
			return fmt.Errorf("cache contains control byte 0x%02x at offset %d", value, offset)
		}
	}
	content := string(data)
	for _, marker := range []string{
		"/__dalec_homebrew",
		"/run/dalec-homebrew",
		"/usr/local/bin/dalec-homebrew-materializer",
		"/usr/local/bin/dalec-homebrew-test-runner",
		pourScriptPath,
		"dalec-homebrew-verified-bottles-",
	} {
		if strings.Contains(content, marker) {
			return fmt.Errorf("cache contains materializer-only path marker %q", marker)
		}
	}
	if strings.Contains(content, "../") || strings.Contains(content, `..\`) {
		return fmt.Errorf("cache contains path traversal")
	}

	lines := strings.Split(content, "\n")
	if len(lines) == 0 || lines[0] != "# GdkPixbuf Image Loader Modules file" {
		return fmt.Errorf("cache header is missing or unsupported")
	}
	const (
		cachePhaseHeader = iota
		cachePhaseModule
		cachePhaseMetadata
		cachePhaseMIME
		cachePhaseExtensions
		cachePhaseSignatures
	)
	phase := cachePhaseHeader
	loaderDirectorySeen := false
	moduleCount := 0
	currentModule := ""
	currentResolved := ""
	var currentBlock []string
	finalizeBlock := func() error {
		if currentModule == "" || currentResolved == "" || len(currentBlock) == 0 {
			return fmt.Errorf("cache contains an incomplete loader block")
		}
		if _, duplicate := modules[currentModule]; duplicate {
			return fmt.Errorf("cache repeats loader module %s", currentModule)
		}
		modules[currentModule] = gdkPixbufCacheModule{resolved: currentResolved, block: strings.Join(currentBlock, "\n")}
		currentModule = ""
		currentResolved = ""
		currentBlock = nil
		return nil
	}

	for lineNumber, line := range lines[1:] {
		lineNumber += 2
		if phase == cachePhaseHeader {
			if strings.HasPrefix(line, "# LoaderDir = ") {
				if loaderDirectorySeen {
					return fmt.Errorf("line %d repeats LoaderDir", lineNumber)
				}
				if err := validateGdkPixbufLoaderDirectory(prefix, strings.TrimPrefix(line, "# LoaderDir = "), after); err != nil {
					return fmt.Errorf("line %d: %w", lineNumber, err)
				}
				loaderDirectorySeen = true
				continue
			}
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !loaderDirectorySeen {
				return fmt.Errorf("line %d starts modules before LoaderDir", lineNumber)
			}
			phase = cachePhaseModule
		}

		if phase == cachePhaseModule && line == "" {
			continue
		}
		if phase == cachePhaseSignatures && line == "" {
			if err := finalizeBlock(); err != nil {
				return fmt.Errorf("line %d: %w", lineNumber, err)
			}
			phase = cachePhaseModule
			continue
		}
		tokens, err := parseGdkPixbufCacheLine(line)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNumber, err)
		}
		switch phase {
		case cachePhaseModule:
			if len(tokens) != 1 || !tokens[0].quoted || tokens[0].value == "" || strings.Contains(tokens[0].value, `\`) {
				return fmt.Errorf("line %d is not a plain loader module path", lineNumber)
			}
			resolved, err := validateGdkPixbufLoaderModule(prefix, tokens[0].value, after, closureKegs)
			if err != nil {
				return fmt.Errorf("line %d: %w", lineNumber, err)
			}
			if _, duplicate := modules[tokens[0].value]; duplicate {
				return fmt.Errorf("line %d repeats loader module %s", lineNumber, tokens[0].value)
			}
			currentModule = tokens[0].value
			currentResolved = resolved
			currentBlock = []string{line}
			moduleCount++
			if moduleCount > gdkPixbufLoadersCacheMaxModule {
				return fmt.Errorf("cache exceeds %d loader modules", gdkPixbufLoadersCacheMaxModule)
			}
			phase = cachePhaseMetadata
		case cachePhaseMetadata:
			currentBlock = append(currentBlock, line)
			if len(tokens) != 5 || !tokens[0].quoted || tokens[1].quoted || !tokens[2].quoted || !tokens[3].quoted || !tokens[4].quoted {
				return fmt.Errorf("line %d has malformed loader metadata", lineNumber)
			}
			if _, err := strconv.ParseUint(tokens[1].value, 10, 32); err != nil {
				return fmt.Errorf("line %d has invalid loader flags", lineNumber)
			}
			phase = cachePhaseMIME
		case cachePhaseMIME, cachePhaseExtensions:
			currentBlock = append(currentBlock, line)
			if len(tokens) == 0 || !tokens[len(tokens)-1].quoted || tokens[len(tokens)-1].value != "" {
				return fmt.Errorf("line %d lacks an empty-list terminator", lineNumber)
			}
			for _, token := range tokens {
				if !token.quoted {
					return fmt.Errorf("line %d contains an unquoted list value", lineNumber)
				}
			}
			if phase == cachePhaseMIME {
				phase = cachePhaseExtensions
			} else {
				phase = cachePhaseSignatures
			}
		case cachePhaseSignatures:
			currentBlock = append(currentBlock, line)
			if len(tokens) != 3 || !tokens[0].quoted || !tokens[1].quoted || tokens[2].quoted {
				return fmt.Errorf("line %d has malformed loader signature", lineNumber)
			}
			if _, err := strconv.ParseInt(tokens[2].value, 10, 32); err != nil {
				return fmt.Errorf("line %d has invalid signature relevance", lineNumber)
			}
		}
	}
	if !loaderDirectorySeen || moduleCount == 0 {
		return fmt.Errorf("cache does not contain a loader directory and at least one module")
	}
	if phase == cachePhaseMetadata || phase == cachePhaseMIME || phase == cachePhaseExtensions {
		return fmt.Errorf("cache ends in an incomplete loader block")
	}
	if phase == cachePhaseSignatures {
		if err := finalizeBlock(); err != nil {
			return err
		}
	}
	if len(modules) != moduleCount {
		return fmt.Errorf("cache module block count does not match parsed module count")
	}
	return nil
}

func validateGdkPixbufLoaderDirectory(prefix, directory string, after map[string]fileState) error {
	prefix = path.Clean(filepath.ToSlash(prefix))
	expected := path.Join(prefix, gdkPixbufLoadersDirectoryPath)
	if directory != expected || !path.IsAbs(directory) || path.Clean(directory) != directory {
		return fmt.Errorf("LoaderDir %q is not the exact global loader directory %q", directory, expected)
	}
	state, ok := after[gdkPixbufLoadersDirectoryPath]
	if !ok || state.Type != "directory" {
		return fmt.Errorf("global loader directory is absent or not a real directory")
	}
	return nil
}

func validateGdkPixbufLoaderModule(prefix, module string, snapshot map[string]fileState, closureKegs map[string]struct{}) (string, error) {
	prefix = path.Clean(filepath.ToSlash(prefix))
	loaderDirectory := path.Join(prefix, gdkPixbufLoadersDirectoryPath)
	if !path.IsAbs(module) || path.Clean(module) != module || path.Dir(module) != loaderDirectory {
		return "", fmt.Errorf("loader module %q is outside the exact global loader directory", module)
	}
	base := path.Base(module)
	if !isGdkPixbufLoaderBasename(base) {
		return "", fmt.Errorf("loader module %q has an unexpected filename", module)
	}
	prefixWithSlash := strings.TrimSuffix(prefix, "/") + "/"
	if !strings.HasPrefix(module, prefixWithSlash) {
		return "", fmt.Errorf("loader module %q is outside the runtime prefix", module)
	}
	rel := strings.TrimPrefix(module, prefixWithSlash)
	state, ok := snapshot[rel]
	if !ok || state.Type != "symlink" {
		return "", fmt.Errorf("loader module %q is not a global symlink", module)
	}
	resolved, err := resolveSnapshotPath(prefix, snapshot, rel)
	if err != nil {
		return "", fmt.Errorf("resolve loader module %q: %w", module, err)
	}
	keg, ok := closureKegForPath(resolved, closureKegs)
	if !ok {
		return "", fmt.Errorf("loader module %q resolves outside the resolved closure at %s", module, resolved)
	}
	expectedSuffix := strings.TrimPrefix(gdkPixbufLoadersDirectoryPath, "lib/") + "/" + base
	if !strings.HasSuffix(resolved, "/lib/"+expectedSuffix) {
		return "", fmt.Errorf("loader module %q resolves outside a keg loader directory at %s", module, resolved)
	}
	target, ok := snapshot[resolved]
	if !ok || target.Type != "regular" || target.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || target.Mode.Perm()&0o022 != 0 {
		return "", fmt.Errorf("loader module %q does not resolve to an ordinary protected file in %s", module, keg)
	}
	return resolved, nil
}

func installedGlobalGdkPixbufLoaders(prefix string, snapshot map[string]fileState, closureKegs map[string]struct{}) (map[string]string, error) {
	modules := map[string]string{}
	prefix = path.Clean(filepath.ToSlash(prefix))
	for rel, state := range snapshot {
		if path.Dir(rel) != gdkPixbufLoadersDirectoryPath || state.Type != "symlink" || !isGdkPixbufLoaderBasename(path.Base(rel)) {
			continue
		}
		module := path.Join(prefix, rel)
		resolved, err := validateGdkPixbufLoaderModule(prefix, module, snapshot, closureKegs)
		if err != nil {
			return nil, err
		}
		modules[module] = resolved
	}
	return modules, nil
}

func isGdkPixbufLoaderBasename(base string) bool {
	const prefix = "libpixbufloader"
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".so") || len(base) <= len(prefix)+len("-.so") {
		return false
	}
	separator := base[len(prefix)]
	if separator != '-' && separator != '_' {
		return false
	}
	name := base[len(prefix)+1 : len(base)-len(".so")]
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func closureKegForPath(candidate string, closureKegs map[string]struct{}) (string, bool) {
	for keg := range closureKegs {
		if snapshotPathWithin(candidate, keg) {
			return keg, true
		}
	}
	return "", false
}

func parseGdkPixbufCacheLine(line string) ([]gdkPixbufCacheToken, error) {
	var tokens []gdkPixbufCacheToken
	for offset := 0; offset < len(line); {
		for offset < len(line) && line[offset] == ' ' {
			offset++
		}
		if offset == len(line) {
			break
		}
		if line[offset] == '"' {
			start := offset + 1
			offset = start
			closed := false
			for offset < len(line) {
				switch line[offset] {
				case '\\':
					offset += 2
					if offset > len(line) {
						return nil, fmt.Errorf("truncated escape sequence")
					}
				case '"':
					tokens = append(tokens, gdkPixbufCacheToken{value: line[start:offset], quoted: true})
					offset++
					closed = true
				default:
					offset++
				}
				if closed {
					break
				}
			}
			if !closed {
				return nil, fmt.Errorf("unterminated quoted field")
			}
			continue
		}
		start := offset
		for offset < len(line) && line[offset] != ' ' {
			if line[offset] == '"' {
				return nil, fmt.Errorf("quote inside unquoted field")
			}
			offset++
		}
		tokens = append(tokens, gdkPixbufCacheToken{value: line[start:offset]})
	}
	return tokens, nil
}

func isPackageManagerState(p string) bool {
	if p == "share/info/dir" {
		return true
	}
	for _, root := range []string{"var/homebrew", "var/run/homebrew", "var/locks/homebrew"} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

func isBrewedLoaderMutation(prefix string, node resolution.Node, rel string, snapshot map[string]fileState) bool {
	if rel != "lib/ld.so" || node.Name != "glibc" {
		return false
	}
	state, ok := snapshot[rel]
	return ok && state.Type == "symlink" && filepath.ToSlash(state.Link) == brewedLoaderTarget(prefix)
}

func brewedLoaderTarget(prefix string) string {
	return path.Join(filepath.ToSlash(prefix), "opt/glibc/bin/ld.so")
}

func validatePreinstallSymlinks(prefix string, snapshot map[string]fileState, record *resolution.Record) error {
	if record == nil {
		return errors.New("nil resolution record")
	}
	writableRoots := []string{"bin", "sbin", "lib", "share", "include", "etc", "var", "opt", "Cellar"}
	for _, root := range writableRoots {
		if state, ok := snapshot[root]; ok && state.Type != "directory" {
			return fmt.Errorf("installer-writable root %s is not a real directory", root)
		}
	}
	for rel, state := range snapshot {
		if state.Type != "symlink" {
			continue
		}
		if rel == "lib/ld.so" {
			expected, err := runtimefs.RuntimeBaseLoaderTarget(record.Input.Platform.Architecture)
			if err != nil {
				return err
			}
			actual := filepath.ToSlash(state.Link)
			if actual == expected {
				continue
			}
			glibc, ok := nodeByName(record, "glibc")
			if !ok || actual != brewedLoaderTarget(prefix) {
				return fmt.Errorf("pre-existing runtime loader symlink %s targets %q, expected %q", rel, state.Link, expected)
			}
			resolved, err := resolveSnapshotPath(prefix, snapshot, rel)
			want := filepath.ToSlash(filepath.Join("Cellar", "glibc", glibc.PkgVersion, "bin", "ld.so"))
			if err != nil || resolved != want {
				return fmt.Errorf("pre-existing brewed runtime loader %s does not resolve to %s", rel, want)
			}
			continue
		}
		if _, err := resolveSnapshotPath(prefix, snapshot, rel); err != nil {
			return fmt.Errorf("pre-existing symlink %s is unsafe: %w", rel, err)
		}
	}
	return nil
}

func resolveSnapshotPath(prefix string, snapshot map[string]fileState, rel string) (string, error) {
	queue := strings.Split(filepath.ToSlash(rel), "/")
	stack := []string{}
	steps := 0
	finalMustDir := false
	for len(queue) > 0 {
		component := queue[0]
		queue = queue[1:]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if len(stack) == 0 {
				return "", fmt.Errorf("snapshot path escapes prefix")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, component)
		candidate := strings.Join(stack, "/")
		state, ok := snapshot[candidate]
		if !ok {
			return "", fmt.Errorf("snapshot component %s is missing", candidate)
		}
		if state.Type != "symlink" {
			if len(queue) > 0 && state.Type != "directory" {
				return "", fmt.Errorf("snapshot component %s is not a directory", candidate)
			}
			continue
		}
		steps++
		if steps > 64 {
			return "", fmt.Errorf("snapshot symlink expansion exceeds 64 steps near %s", candidate)
		}
		target := filepath.ToSlash(state.Link)
		remaining := len(queue)
		stack = stack[:len(stack)-1]
		mustDir := snapshotTargetMustBeDirectory(target)
		if path.IsAbs(target) {
			components, absoluteMustDir, err := absoluteSnapshotTarget(prefix, target)
			if err != nil {
				return "", err
			}
			mustDir = absoluteMustDir
			stack = nil
			queue = append(components, queue...)
		} else {
			queue = append(strings.Split(target, "/"), queue...)
		}
		if mustDir && remaining == 0 {
			finalMustDir = true
		}
	}
	resolved := strings.Join(stack, "/")
	if resolved == "" {
		resolved = "."
	}
	state, ok := snapshot[resolved]
	if !ok {
		return "", fmt.Errorf("snapshot target %s is missing", resolved)
	}
	if finalMustDir && state.Type != "directory" {
		return "", fmt.Errorf("snapshot target %s must be a directory", resolved)
	}
	return resolved, nil
}

func snapshotTargetMustBeDirectory(target string) bool {
	if strings.HasSuffix(target, "/") {
		return true
	}
	parts := strings.Split(filepath.ToSlash(target), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "" {
			continue
		}
		return parts[i] == "." || parts[i] == ".."
	}
	return false
}

func absoluteSnapshotTarget(prefix, target string) ([]string, bool, error) {
	split := func(value string) []string {
		var out []string
		for _, part := range strings.Split(filepath.ToSlash(value), "/") {
			if part == "" || part == "." {
				continue
			}
			out = append(out, part)
		}
		return out
	}
	targetParts := split(target)
	prefixParts := split(path.Clean(filepath.ToSlash(prefix)))
	if len(targetParts) < len(prefixParts) {
		return nil, false, fmt.Errorf("absolute symlink target escapes prefix")
	}
	for i := range prefixParts {
		if targetParts[i] != prefixParts[i] {
			return nil, false, fmt.Errorf("absolute symlink target escapes prefix")
		}
	}
	return targetParts[len(prefixParts):], snapshotTargetMustBeDirectory(target), nil
}

func snapshotPathWithin(candidate, root string) bool {
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func isValidatedNodeNPMKegLink(prefix string, node resolution.Node, verified bottle.Result, snapshot map[string]fileState, rel string) bool {
	if node.Name != nodeFormula || !verifiedBottleMatchesNode(node, verified) {
		return false
	}
	base := path.Join("Cellar", node.Name, node.PkgVersion)
	kegPath := strings.TrimPrefix(rel, base+"/")
	targets := map[string]string{
		"bin/npm": path.Join(nodeNPMRuntimeRoot, "bin/npm-cli.js"),
		"bin/npx": path.Join(nodeNPMRuntimeRoot, "bin/npx-cli.js"),
	}
	target, ok := targets[kegPath]
	if !ok {
		return false
	}
	direct, err := directSnapshotSymlinkTarget(prefix, snapshot, rel)
	if err != nil || direct != target {
		return false
	}
	resolved, err := resolveSnapshotPath(prefix, snapshot, rel)
	if err != nil || resolved != target {
		return false
	}
	global, ok := snapshot[target]
	if !ok || global.Type != "regular" || global.Mode.Perm()&0o111 == 0 || global.Links != 1 {
		return false
	}
	sourceKegPath := path.Join(nodeNPMSourceRoot, "bin", path.Base(target))
	var sourceInventory *bottle.InventoryEntry
	for index := range verified.Inventory {
		entry := &verified.Inventory[index]
		if entry.KegPath == sourceKegPath && entry.Type == bottle.EntryRegular {
			sourceInventory = entry
			break
		}
	}
	if sourceInventory == nil || sourceInventory.SHA256 != "sha256:"+global.Digest || os.FileMode(sourceInventory.Mode&0o777) != global.Mode.Perm() {
		return false
	}
	sourcePath := path.Join(base, sourceKegPath)
	source, ok := snapshot[sourcePath]
	return ok && source.Type == "regular" && source.Digest == global.Digest && source.Size == global.Size && source.Mode == global.Mode && sameSnapshotOwnership(source, global)
}

func validateExternalBottleSymlinkTargets(prefix string, snapshot map[string]fileState, node resolution.Node, verified bottle.Result, closure []resolution.Node) error {
	seen := map[string]struct{}{}
	for _, entry := range verified.Inventory {
		if entry.PrefixTarget == "" {
			continue
		}
		if _, ok := seen[entry.PrefixTarget]; ok {
			continue
		}
		seen[entry.PrefixTarget] = struct{}{}
		dependencyKeg, err := externalSymlinkDependencyKeg(node, closure, entry.PrefixTarget)
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Path, err)
		}
		resolved, err := resolveSnapshotPath(prefix, snapshot, entry.PrefixTarget)
		if err != nil {
			return fmt.Errorf("%s: resolve dependency target: %w", entry.Path, err)
		}
		if !snapshotPathWithin(resolved, dependencyKeg) {
			return fmt.Errorf("%s: dependency target resolves outside %s", entry.Path, dependencyKeg)
		}
	}
	return nil
}

func externalSymlinkDependencyKeg(node resolution.Node, closure []resolution.Node, target string) (string, error) {
	if target == "" || path.IsAbs(target) || path.Clean(target) != target || target == "." || strings.HasPrefix(target, "../") {
		return "", fmt.Errorf("invalid dependency opt target %q", target)
	}
	parts := strings.Split(target, "/")
	if len(parts) < 2 || parts[0] != "opt" || parts[1] == "" {
		return "", fmt.Errorf("dependency target %q is outside opt", target)
	}
	dependencyName := parts[1]
	optRoot := path.Join("opt", dependencyName)
	if !snapshotPathWithin(target, optRoot) {
		return "", fmt.Errorf("dependency target %q is outside %s", target, optRoot)
	}
	allowed := false
	for _, dependency := range node.Dependencies {
		if dependency.Name == dependencyName && dependency.Direct {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("dependency target %q is not a signed dependency of %q", target, node.Name)
	}
	for _, dependency := range closure {
		if dependency.Name == dependencyName {
			if dependency.PkgVersion == "" {
				return "", fmt.Errorf("dependency %q has no selected package version", dependencyName)
			}
			return path.Join("Cellar", dependency.Name, dependency.PkgVersion), nil
		}
	}
	return "", fmt.Errorf("dependency target %q is absent from the resolved closure", target)
}

func isCurrentKegBashCompletionLink(prefix string, snapshot map[string]fileState, linkPath, resolved, keg string) bool {
	const completionRoot = "etc/bash_completion.d"
	if !strings.HasPrefix(linkPath, completionRoot+"/") {
		return false
	}
	expected := path.Join(keg, linkPath)
	direct, err := directSnapshotSymlinkTarget(prefix, snapshot, linkPath)
	if err != nil || direct != expected {
		return false
	}
	source, ok := snapshot[expected]
	if !ok || (source.Type != "regular" && source.Type != "symlink") {
		return false
	}
	sourceResolved, err := resolveSnapshotPath(prefix, snapshot, expected)
	return err == nil && sourceResolved == resolved && snapshotPathWithin(sourceResolved, keg)
}

func isCurrentGlibcLoaderConfigurationLink(prefix string, node resolution.Node, verified bottle.Result, snapshot map[string]fileState, linkPath, resolved, keg string) bool {
	if node.Name != "glibc" || linkPath != "etc/ld.so.conf" || !verifiedBottleMatchesNode(node, verified) {
		return false
	}
	expected := path.Join(keg, linkPath)
	direct, err := directSnapshotSymlinkTarget(prefix, snapshot, linkPath)
	if err != nil || direct != expected || resolved != expected {
		return false
	}
	source, ok := snapshot[expected]
	if !ok || source.Type != "regular" {
		return false
	}
	for _, entry := range verified.Inventory {
		if entry.KegPath == linkPath && entry.Type == bottle.EntryRegular {
			return source.Mode.Perm() == os.FileMode(entry.Mode).Perm()
		}
	}
	return false
}

func directSnapshotSymlinkTarget(prefix string, snapshot map[string]fileState, linkPath string) (string, error) {
	state, ok := snapshot[linkPath]
	if !ok || state.Type != "symlink" {
		return "", fmt.Errorf("snapshot path %s is not a symlink", linkPath)
	}
	target := filepath.ToSlash(state.Link)
	if path.IsAbs(target) {
		components, _, err := absoluteSnapshotTarget(prefix, target)
		if err != nil {
			return "", err
		}
		return strings.Join(components, "/"), nil
	}
	direct := path.Clean(path.Join(path.Dir(linkPath), target))
	if direct == ".." || strings.HasPrefix(direct, "../") {
		return "", fmt.Errorf("snapshot path escapes prefix")
	}
	return direct, nil
}

func inGlobal(p string) bool {
	for _, root := range []string{"bin", "sbin", "lib", "share", "include"} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}
func isGlobalRoot(p string) bool {
	return p == "bin" || p == "sbin" || p == "lib" || p == "share" || p == "include"
}

type reconcileKegOptions struct {
	closure []resolution.Node
}

func reconcileInstalledKeg(prefix string, node resolution.Node, verified bottle.Result, after map[string]fileState, optional ...reconcileKegOptions) error {
	if len(optional) > 1 {
		return fmt.Errorf("multiple reconcile keg options")
	}
	var options reconcileKegOptions
	if len(optional) == 1 {
		options = optional[0]
	}
	base := filepath.ToSlash(filepath.Join("Cellar", node.Name, node.PkgVersion))
	expected := map[string]bottle.InventoryEntry{}
	allowedDirs := map[string]struct{}{base: {}}
	for _, entry := range verified.Inventory {
		if entry.Path == node.Name || entry.Path == verified.KegPrefix || entry.KegPath == "" {
			continue
		}
		rel := base + "/" + entry.KegPath
		expected[rel] = entry
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))); parent != base && parent != "."; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			allowedDirs[parent] = struct{}{}
		}
	}
	postInstallPaths, err := allowedPostInstallKegPaths(node, base, after)
	if err != nil {
		return err
	}
	changed := map[string]struct{}{}
	for _, value := range node.Bottle.Tab.ChangedFiles {
		changed[value] = struct{}{}
	}
	for rel, entry := range expected {
		actual, ok := after[rel]
		if !ok {
			return fmt.Errorf("verified bottle path %s is missing after install", rel)
		}
		wantType := string(entry.Type)
		if entry.Type == bottle.EntryHardlink {
			wantType = "regular"
		}
		if actual.Type != wantType {
			return fmt.Errorf("verified bottle path %s has type %s, expected %s", rel, actual.Type, wantType)
		}
		_, declaredChanged := changed[entry.KegPath]
		mayChange := declaredChanged
		isFormulaMetadata := entry.KegPath == ".brew" || strings.HasPrefix(entry.KegPath, ".brew/")
		mayChange = mayChange || (entry.Relocatable && !isFormulaMetadata)
		if entry.KegPath == "INSTALL_RECEIPT.json" || entry.KegPath == "sbom.spdx.json" {
			mayChange = true
		}
		if (entry.Type == bottle.EntryRegular || entry.Type == bottle.EntryHardlink) && !mayChange {
			if actual.Digest != strings.TrimPrefix(entry.SHA256, "sha256:") {
				return fmt.Errorf("verified bottle path %s content changed without relocation policy", rel)
			}
		}
		if entry.Type == bottle.EntrySymlink {
			if entry.PrefixTarget != "" && actual.Link != entry.SymlinkTarget {
				return fmt.Errorf("verified bottle external symlink %s target changed", rel)
			}
			nodeNPMRewrite := actual.Link != entry.SymlinkTarget && isValidatedNodeNPMKegLink(prefix, node, verified, after, rel)
			if !mayChange && actual.Link != entry.SymlinkTarget && !nodeNPMRewrite {
				return fmt.Errorf("verified bottle symlink %s target changed", rel)
			}
			resolved, err := resolveSnapshotPath(prefix, after, rel)
			if err != nil {
				return fmt.Errorf("resolve verified bottle symlink %s: %w", rel, err)
			}
			if entry.PrefixTarget == "" {
				if !snapshotPathWithin(resolved, base) && !nodeNPMRewrite {
					return fmt.Errorf("verified bottle symlink %s escapes installed keg", rel)
				}
			} else {
				dependencyKeg, err := externalSymlinkDependencyKeg(node, options.closure, entry.PrefixTarget)
				if err != nil {
					return fmt.Errorf("verified bottle symlink %s: %w", rel, err)
				}
				expectedResolved, err := resolveSnapshotPath(prefix, after, entry.PrefixTarget)
				if err != nil {
					return fmt.Errorf("resolve verified bottle symlink %s dependency target: %w", rel, err)
				}
				if resolved != expectedResolved || !snapshotPathWithin(resolved, dependencyKeg) {
					return fmt.Errorf("verified bottle symlink %s does not resolve inside dependency keg %s", rel, dependencyKeg)
				}
			}
		}
		if entry.Type != bottle.EntrySymlink {
			if actual.Mode&(os.ModeSetuid|os.ModeSetgid) != 0 {
				return fmt.Errorf("verified bottle path %s gained setuid or setgid permissions", rel)
			}
			expectedMode := os.FileMode(entry.Mode & 0o777)
			if entry.Mode&0o1000 != 0 {
				expectedMode |= os.ModeSticky
			}
			actualMode := actual.Mode.Perm()
			if actual.Mode&os.ModeSticky != 0 {
				actualMode |= os.ModeSticky
			}
			if actualMode != expectedMode && !allowsPostInstallOwnerWrite(node, verified, entry, declaredChanged, expectedMode, actualMode) {
				return fmt.Errorf("verified bottle path %s permissions changed", rel)
			}
		}
	}
	declaredGroups := map[string]map[string]struct{}{}
	groupInode := map[string]string{}
	for rel, entry := range expected {
		if entry.Type != bottle.EntryRegular && entry.Type != bottle.EntryHardlink {
			continue
		}
		group := rel
		if entry.Type == bottle.EntryHardlink {
			targetPath := strings.TrimPrefix(entry.ResolvedTarget, verified.KegPrefix+"/")
			group = base + "/" + targetPath
		}
		if declaredGroups[group] == nil {
			declaredGroups[group] = map[string]struct{}{}
		}
		declaredGroups[group][rel] = struct{}{}
		actual := after[rel]
		if actual.Inode == "" {
			continue
		}
		if previous, ok := groupInode[group]; ok && previous != actual.Inode {
			return fmt.Errorf("declared hardlink group %s spans multiple inodes", group)
		}
		groupInode[group] = actual.Inode
	}
	pathsByInode := map[string]map[string]struct{}{}
	linksByInode := map[string]uint64{}
	for rel, state := range after {
		if state.Type != "regular" || state.Inode == "" {
			continue
		}
		if pathsByInode[state.Inode] == nil {
			pathsByInode[state.Inode] = map[string]struct{}{}
		}
		pathsByInode[state.Inode][rel] = struct{}{}
		linksByInode[state.Inode] = state.Links
	}
	for group, expectedPaths := range declaredGroups {
		inode := groupInode[group]
		if inode == "" {
			continue
		}
		actualPaths := pathsByInode[inode]
		if !maps.Equal(actualPaths, expectedPaths) {
			return fmt.Errorf("hardlink group %s has undeclared aliases: %v", group, actualPaths)
		}
		if linksByInode[inode] != uint64(len(expectedPaths)) {
			return fmt.Errorf("hardlink group %s has link count %d, expected %d", group, linksByInode[inode], len(expectedPaths))
		}
	}
	for rel, actual := range after {
		if rel == base || !strings.HasPrefix(rel, base+"/") {
			continue
		}
		if _, ok := expected[rel]; ok {
			continue
		}
		if _, ok := postInstallPaths[rel]; ok {
			continue
		}
		if rel == base+"/INSTALL_RECEIPT.json" {
			if actual.Type != "regular" || actual.Size > bottle.DefaultLimits().MaxReceiptBytes || actual.Mode.Perm()&0o022 != 0 || (actual.Links != 0 && actual.Links != 1) {
				return fmt.Errorf("installed receipt is not a bounded regular file")
			}
			continue
		}
		if actual.Type == "directory" {
			if _, ok := allowedDirs[rel]; ok {
				continue
			}
		}
		return fmt.Errorf("installed keg contains unattributed path %s", rel)
	}
	return nil
}

const (
	glibcLocaleMaxEntries = 4096
	glibcLocaleMaxFile    = 64 << 20
	glibcLocaleMaxTotal   = 128 << 20
)

// allowedPostInstallKegPaths validates narrowly scoped, deterministic data that
// a verified Formula is documented to create after pouring. The raw bottle
// inventory remains authoritative for every other path.
func allowedPostInstallKegPaths(node resolution.Node, base string, after map[string]fileState) (map[string]struct{}, error) {
	allowed := map[string]struct{}{}
	if node.Name != "glibc" {
		return allowed, nil
	}

	// Homebrew glibc's verified post_install invokes its brewed localedef to
	// generate C.utf8/en_US.UTF-8 data below lib/locale. Bound the tree, require
	// ordinary immutable data, and reject links, executables, special modes, or
	// unknown ownership rather than accepting arbitrary Formula output.
	root := base + "/lib/locale"
	rootState, present := after[root]
	if !present {
		return allowed, nil
	}
	if rootState.Type != "directory" {
		return nil, fmt.Errorf("glibc post-install locale root is not a directory")
	}
	var count int
	var total int64
	for rel, state := range after {
		if rel != root && !strings.HasPrefix(rel, root+"/") {
			continue
		}
		count++
		if count > glibcLocaleMaxEntries {
			return nil, fmt.Errorf("glibc post-install locale tree exceeds %d entries", glibcLocaleMaxEntries)
		}
		if !state.OwnershipKnown {
			return nil, fmt.Errorf("glibc post-install locale path %s has unknown ownership", rel)
		}
		if state.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || state.Mode.Perm()&0o022 != 0 {
			return nil, fmt.Errorf("glibc post-install locale path %s has unsafe permissions", rel)
		}
		switch state.Type {
		case "directory":
		case "regular":
			if state.Mode.Perm()&0o111 != 0 {
				return nil, fmt.Errorf("glibc post-install locale path %s is executable", rel)
			}
			if state.Size < 0 || state.Size > glibcLocaleMaxFile {
				return nil, fmt.Errorf("glibc post-install locale path %s exceeds the file limit", rel)
			}
			if state.Links != 0 && state.Links != 1 {
				return nil, fmt.Errorf("glibc post-install locale path %s has unexpected hardlinks", rel)
			}
			total += state.Size
			if total > glibcLocaleMaxTotal {
				return nil, fmt.Errorf("glibc post-install locale tree exceeds the size limit")
			}
		default:
			return nil, fmt.Errorf("glibc post-install locale path %s has unsupported type %s", rel, state.Type)
		}
		allowed[rel] = struct{}{}
	}
	return allowed, nil
}

func allowsPostInstallOwnerWrite(node resolution.Node, verified bottle.Result, entry bottle.InventoryEntry, declaredChanged bool, expectedMode, actualMode fs.FileMode) bool {
	if entry.Type != bottle.EntryRegular && entry.Type != bottle.EntryHardlink {
		return false
	}
	// Homebrew post-install actions may make a verified read-only file writable
	// by its owner. Accept exactly that one-bit transition: executable, group,
	// other, and sticky permissions must remain identical. Content and type are
	// still reconciled independently.
	if expectedMode&0o200 != 0 || actualMode != expectedMode|0o200 {
		return false
	}
	if declaredChanged {
		return true
	}
	return isPythonVenvTemplate(node, verified, entry.KegPath)
}

func isPythonVenvTemplate(node resolution.Node, verified bottle.Result, kegPath string) bool {
	minor, ok := strings.CutPrefix(node.Name, "python@")
	if !ok || !validPythonMinor(minor) {
		return false
	}
	if node.FormulaVersion == "" || (node.FormulaVersion != minor && !strings.HasPrefix(node.FormulaVersion, minor+".")) {
		return false
	}
	expectedFormulaPath := path.Join(verified.KegPrefix, ".brew", node.Name+".rb")
	expectedFormulaClass := "PythonAT" + strings.ReplaceAll(minor, ".", "")
	formulaDigest := strings.TrimPrefix(verified.Formula.SHA256, "sha256:")
	if verified.Formula.Path != expectedFormulaPath || verified.Formula.ClassName != expectedFormulaClass || verified.Formula.Size <= 0 || len(formulaDigest) != sha256.Size*2 {
		return false
	}
	if _, err := hex.DecodeString(formulaDigest); err != nil {
		return false
	}
	root := "lib/python" + minor + "/venv/scripts/"
	return strings.HasPrefix(kegPath, root) && len(kegPath) > len(root)
}

func validPythonMinor(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func resolvedClosureKegs(record *resolution.Record) map[string]struct{} {
	out := map[string]struct{}{}
	if record == nil {
		return out
	}
	for _, node := range record.Nodes {
		if node.Name == "" || node.PkgVersion == "" {
			continue
		}
		out[path.Join("Cellar", node.Name, node.PkgVersion)] = struct{}{}
	}
	return out
}

func optNamesForNode(record *resolution.Record, name string) map[string]struct{} {
	out := map[string]struct{}{name: {}}
	for _, root := range record.Requested {
		if root.Canonical == name {
			out[root.Requested] = struct{}{}
		}
	}
	return out
}

func verifyInstalledSubset(prefix string, record *resolution.Record, through string) error {
	allowed := map[string]struct{}{}
	for _, name := range record.InstallOrder {
		allowed[name] = struct{}{}
		if name == through {
			break
		}
	}
	entries, err := os.ReadDir(filepath.Join(prefix, "Cellar"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			return fmt.Errorf("non-directory Cellar rack %s", e.Name())
		}
		if _, ok := allowed[e.Name()]; !ok {
			return fmt.Errorf("Homebrew added or substituted unexpected keg %q", e.Name())
		}
	}
	return nil
}
func verifyClosure(prefix string, record *resolution.Record) error {
	entries, err := os.ReadDir(filepath.Join(prefix, "Cellar"))
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, e := range entries {
		versions, err := os.ReadDir(filepath.Join(prefix, "Cellar", e.Name()))
		if err != nil {
			return err
		}
		if len(versions) != 1 {
			return fmt.Errorf("rack %q has %d versions", e.Name(), len(versions))
		}
		seen[e.Name()] = struct{}{}
		node, ok := nodeByName(record, e.Name())
		if !ok {
			return fmt.Errorf("extra keg %q", e.Name())
		}
		if versions[0].Name() != node.PkgVersion {
			return fmt.Errorf("keg %q version %q, expected %q", e.Name(), versions[0].Name(), node.PkgVersion)
		}
		if err := verifyReceipt(filepath.Join(prefix, "Cellar", e.Name(), versions[0].Name(), "INSTALL_RECEIPT.json"), node, record.Nodes); err != nil {
			return err
		}
	}
	if len(seen) != len(record.Nodes) {
		return fmt.Errorf("installed %d kegs for %d-node closure", len(seen), len(record.Nodes))
	}
	return nil
}
func verifyReceipt(filename string, node resolution.Node, closure []resolution.Node) error {
	directory, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer directory.Close()
	f, err := openBottleNoFollow(directory, filepath.Base(filename))
	if err != nil {
		return fmt.Errorf("open receipt for %q: %w", node.Name, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	limit := bottle.DefaultLimits().MaxReceiptBytes
	if !info.Mode().IsRegular() || info.Size() > limit {
		f.Close()
		return fmt.Errorf("receipt for %q is not a bounded regular file", node.Name)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	closeErr := f.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("receipt for %q exceeds %d bytes", node.Name, limit)
	}
	if _, err := bottle.VerifyInstalledReceipt(data, node, closure); err != nil {
		return fmt.Errorf("verify installed receipt for %q: %w", node.Name, err)
	}
	return nil
}
func nodeByName(record *resolution.Record, name string) (resolution.Node, bool) {
	for _, node := range record.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return resolution.Node{}, false
}
