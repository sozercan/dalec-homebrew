package materializer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	VerifiedBottles []bottle.Result `json:"verified_bottles"`
	InstallDeltas   []InstallDelta  `json:"install_deltas"`
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
		evidence.VerifiedBottles = append(evidence.VerifiedBottles, *verified)
		verifiedByName[name] = *verified
	}

	for _, name := range cfg.Record.InstallOrder {
		node, _ := nodeByName(cfg.Record, name)
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
		err = cfg.Runner.Run(stepCtx, Command{Path: filepath.Join(cfg.Prefix, "bin/brew"), Args: []string{"ruby", pourScriptPath, installPaths[name]}, Env: installEnv(cfg.Prefix), Dir: "/home/linuxbrew", User: cfg.User})
		if err != nil {
			cancel()
			return nil, fmt.Errorf("offline install %q: %w", name, err)
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
		if err := classify(cfg.Prefix, node, before, after, changes, optNamesForNode(cfg.Record, node.Name)); err != nil {
			return nil, fmt.Errorf("contain install %q: %w", name, err)
		}
		if err := reconcileInstalledKeg(cfg.Prefix, node, verifiedByName[name], after); err != nil {
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

func installEnv(prefix string) []string {
	return []string{
		"HOME=/home/linuxbrew", "USER=linuxbrew", "LOGNAME=linuxbrew", "PATH=" + prefix + "/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOMEBREW_PREFIX=" + prefix, "HOMEBREW_REPOSITORY=" + prefix + "/Homebrew", "HOMEBREW_CELLAR=" + prefix + "/Cellar", "HOMEBREW_CACHE=/home/linuxbrew/.cache/Homebrew",
		"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_NO_INSTALL_FROM_API=1", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1",
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
	Type         string
	Mode         fs.FileMode
	Size         int64
	Digest, Link string
	Inode        string
	Links        uint64
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
	out := map[string]fileState{".": {Type: "directory", Mode: rootInfo.Mode()}}
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
		switch {
		case info.Mode().IsDir():
			state.Type = "directory"
		case info.Mode().IsRegular():
			state.Type = "regular"
			state.Size = info.Size()
			if state.Size < 0 || state.Size > limits.MaxFileBytes {
				return fmt.Errorf("snapshot file %s exceeds %d bytes", p, limits.MaxFileBytes)
			}
			key, links := snapshotInodeMeta(info)
			state.Inode = key
			state.Links = links
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
		case b != a:
			kind = "modified"
		}
		if kind != "" {
			out = append(out, Change{Path: k, Kind: kind})
		}
	}
	slices.SortFunc(out, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return out
}
func classify(prefix string, node resolution.Node, before, after map[string]fileState, changes []Change, optNamesArg ...map[string]struct{}) error {
	keg := filepath.ToSlash(filepath.Join("Cellar", node.Name, node.PkgVersion))
	opt := filepath.ToSlash(filepath.Join("opt", node.Name))
	optNames := map[string]struct{}{node.Name: {}}
	if len(optNamesArg) > 0 {
		for name := range optNamesArg[0] {
			optNames[name] = struct{}{}
		}
	}
	for i := range changes {
		c := &changes[i]
		p := c.Path
		_, existed := before[p]
		if existed && c.Kind != "created" && !isPackageManagerState(p) && !isBrewedLoaderMutation(prefix, node, p, after) && (p == "." || inGlobal(p) || p == "Cellar" || p == "etc" || strings.HasPrefix(p, "etc/") || p == "var" || strings.HasPrefix(p, "var/") || p == "opt" || strings.HasPrefix(p, "opt/")) {
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
				if err != nil || !snapshotPathWithin(resolved, "etc") {
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
					return fmt.Errorf("unexpected regular file in global %s tree at %s", root, p)
				}
				if a.Mode.Perm()&0o111 != 0 {
					return fmt.Errorf("unexpected executable outside current keg at %s", p)
				}
			}
			if a, ok := after[p]; ok && a.Type == "symlink" {
				resolved, err := resolveSnapshotPath(prefix, after, p)
				if err != nil || !snapshotPathWithin(resolved, keg) {
					return fmt.Errorf("global link %s does not resolve into current keg", p)
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

func reconcileInstalledKeg(prefix string, node resolution.Node, verified bottle.Result, after map[string]fileState) error {
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
		_, mayChange := changed[entry.KegPath]
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
			if !mayChange && actual.Link != entry.SymlinkTarget {
				return fmt.Errorf("verified bottle symlink %s target changed", rel)
			}
			resolved, err := resolveSnapshotPath(prefix, after, rel)
			if err != nil || !snapshotPathWithin(resolved, base) {
				return fmt.Errorf("verified bottle symlink %s escapes installed keg", rel)
			}
		}
		if entry.Type != bottle.EntrySymlink {
			expectedMode := os.FileMode(entry.Mode & 0o777)
			if entry.Mode&0o1000 != 0 {
				expectedMode |= os.ModeSticky
			}
			actualMode := actual.Mode.Perm()
			if actual.Mode&os.ModeSticky != 0 {
				actualMode |= os.ModeSticky
			}
			if actualMode != expectedMode {
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
		if err := verifyReceipt(filepath.Join(prefix, "Cellar", e.Name(), versions[0].Name(), "INSTALL_RECEIPT.json"), node); err != nil {
			return err
		}
	}
	if len(seen) != len(record.Nodes) {
		return fmt.Errorf("installed %d kegs for %d-node closure", len(seen), len(record.Nodes))
	}
	return nil
}
func verifyReceipt(filename string, node resolution.Node) error {
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
	if _, err := bottle.VerifyInstalledReceipt(data, node); err != nil {
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
