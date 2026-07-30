// Package runtimecheck performs static checks over materialized payload. It
// never executes untrusted binaries (in particular it never invokes ldd).
package runtimecheck

import (
	"bufio"
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

type Options struct {
	Root          string
	Prefix        string
	Arch          string
	CPUBaseline   string
	LogicalPrefix string
	SearchPATH    []string
}

func Verify(opts Options) error {
	if opts.Root == "" {
		opts.Root = "/"
	}
	if opts.Prefix == "" {
		opts.Prefix = "/home/linuxbrew/.linuxbrew"
	}
	if opts.LogicalPrefix == "" {
		opts.LogicalPrefix = "/home/linuxbrew/.linuxbrew"
	}
	if !filepath.IsAbs(opts.Prefix) || !filepath.IsAbs(opts.LogicalPrefix) {
		return fmt.Errorf("runtime prefix and logical prefix must be absolute")
	}
	var errs []error
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return err
	}
	root = filepath.Clean(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve runtime root: %w", err)
	}
	prefixCandidate := filepath.Join(root, strings.TrimPrefix(filepath.Clean(opts.Prefix), string(filepath.Separator)))
	rel, err := filepath.Rel(root, prefixCandidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("runtime prefix %q escapes root %q", opts.Prefix, root)
	}
	prefix, err := filepath.EvalSymlinks(prefixCandidate)
	if err != nil {
		return fmt.Errorf("runtime prefix %s is unavailable: %w", prefixCandidate, err)
	}
	rel, err = filepath.Rel(root, prefix)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolved runtime prefix %q escapes root %q", prefix, root)
	}
	info, err := os.Lstat(prefix)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime prefix %s is not a real directory", prefix)
	}
	err = filepath.WalkDir(prefix, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			errs = append(errs, walkErr)
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 {
			errs = append(errs, fmt.Errorf("setid path %s", p))
		}
		if info.Mode()&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket) != 0 {
			errs = append(errs, fmt.Errorf("special file %s", p))
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := verifyLink(root, prefix, opts.LogicalPrefix, p); err != nil {
				errs = append(errs, err)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := verifyNoForbiddenReferences(p); err != nil {
			errs = append(errs, err)
		}
		f, err := os.Open(p)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		defer f.Close()
		var magic [4]byte
		n, _ := io.ReadFull(f, magic[:])
		_, _ = f.Seek(0, io.SeekStart)
		if n == 4 && string(magic[:]) == "\x7fELF" {
			if err := verifyELF(root, prefix, opts.LogicalPrefix, p, opts.Arch, opts.CPUBaseline); err != nil {
				errs = append(errs, err)
			}
			return nil
		}
		if n >= 2 && magic[0] == '#' && magic[1] == '!' && info.Mode().Perm()&0o111 != 0 {
			if err := verifyShebang(root, prefix, opts.LogicalPrefix, opts.SearchPATH, opts.Arch, f, p); err != nil {
				errs = append(errs, err)
			}
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func verifyELF(root, prefix, logicalPrefix, filename, arch, baseline string) error {
	f, err := elf.Open(filename)
	if err != nil {
		return fmt.Errorf("parse ELF %s: %w", filename, err)
	}
	defer f.Close()
	want := elf.EM_NONE
	switch arch {
	case "amd64":
		want = elf.EM_X86_64
	case "arm64":
		want = elf.EM_AARCH64
	default:
		return fmt.Errorf("unsupported ELF architecture %q", arch)
	}
	if f.Machine != want {
		return fmt.Errorf("ELF %s has machine %s, expected %s", filename, f.Machine, want)
	}
	if err := verifyCPUProperties(f, arch, baseline); err != nil {
		return fmt.Errorf("ELF %s CPU baseline: %w", filename, err)
	}
	var interp string
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(p.Open(), 4096))
		if err != nil {
			return err
		}
		interp = strings.TrimRight(string(b), "\x00")
		if interp == "" || !filepath.IsAbs(interp) {
			return fmt.Errorf("ELF %s has invalid interpreter %q", filename, interp)
		}
		interpreterPath, err := resolveInRoot(root, prefix, logicalPrefix, interp)
		if err != nil {
			return fmt.Errorf("ELF %s interpreter %s escapes runtime root: %w", filename, interp, err)
		}
		if err := verifyExecutable(interpreterPath, arch); err != nil {
			return fmt.Errorf("ELF %s interpreter %s is unusable: %w", filename, interp, err)
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("read ELF dependencies %s: %w", filename, err)
	}
	if len(libs) == 0 {
		return nil
	}
	var dirs []string
	values, _ := f.DynString(elf.DT_RUNPATH)
	if len(values) == 0 {
		values, _ = f.DynString(elf.DT_RPATH)
	}
	originRel, err := filepath.Rel(prefix, filepath.Dir(filename))
	if err != nil {
		return err
	}
	logicalOrigin := filepath.Join(logicalPrefix, originRel)
	for _, value := range values {
		for _, dir := range strings.Split(value, ":") {
			dir = strings.ReplaceAll(dir, "${ORIGIN}", logicalOrigin)
			dir = strings.ReplaceAll(dir, "$ORIGIN", logicalOrigin)
			if !filepath.IsAbs(dir) {
				return fmt.Errorf("ELF %s has relative runtime library path %q", filename, dir)
			}
			dirs = appendUnique(dirs, filepath.Clean(dir))
		}
	}
	systemDirs, err := systemLibraryDirs(root, arch)
	if err != nil {
		return fmt.Errorf("read dynamic loader configuration: %w", err)
	}
	dirs = appendUnique(dirs, systemDirs...)
	var missing []string
	for _, lib := range libs {
		if lib == "" || filepath.Base(lib) != lib || strings.ContainsAny(lib, "/\\") {
			return fmt.Errorf("ELF %s has unsafe DT_NEEDED value %q", filename, lib)
		}
		found := false
		for _, dir := range dirs {
			candidate, err := resolveInRoot(root, prefix, logicalPrefix, filepath.Join(dir, lib))
			if err != nil {
				continue
			}
			if err := verifyLibrary(candidate, arch); err == nil {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, lib)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("ELF %s has unresolved libraries: %s", filename, strings.Join(missing, ", "))
	}
	return nil
}

const (
	gnuPropertyType0           = 5
	gnuPropertyX86ISA1Needed   = 0xc0008002
	gnuPropertyX86ISA1Baseline = 1
)

func verifyCPUProperties(f *elf.File, arch, baseline string) error {
	if arch == "arm64" {
		if baseline != "" && baseline != "armv8" {
			return fmt.Errorf("unsupported arm64 baseline %q", baseline)
		}
		return nil
	}
	if arch != "amd64" {
		return fmt.Errorf("unsupported architecture %q", arch)
	}
	if baseline != "" && baseline != "core2" {
		return fmt.Errorf("unsupported amd64 baseline %q", baseline)
	}
	section := f.Section(".note.gnu.property")
	if section == nil {
		return nil
	}
	if section.Size > 1<<20 {
		return fmt.Errorf("GNU property note exceeds 1 MiB")
	}
	data, err := section.Data()
	if err != nil {
		return err
	}
	order := f.ByteOrder
	align := func(value, multiple int) int { return (value + multiple - 1) &^ (multiple - 1) }
	for offset := 0; offset < len(data); {
		if len(data)-offset < 12 {
			return fmt.Errorf("truncated GNU note header")
		}
		namesz := int(order.Uint32(data[offset:]))
		descsz := int(order.Uint32(data[offset+4:]))
		noteType := order.Uint32(data[offset+8:])
		offset += 12
		if namesz < 0 || descsz < 0 || offset+align(namesz, 4) > len(data) {
			return fmt.Errorf("invalid GNU note name")
		}
		name := data[offset : offset+namesz]
		offset += align(namesz, 4)
		if offset+align(descsz, 4) > len(data) {
			return fmt.Errorf("invalid GNU note descriptor")
		}
		desc := data[offset : offset+descsz]
		offset += align(descsz, 4)
		if noteType != gnuPropertyType0 || !bytes.HasPrefix(name, []byte("GNU")) {
			continue
		}
		for pos := 0; pos < len(desc); {
			if len(desc)-pos < 8 {
				return fmt.Errorf("truncated GNU property")
			}
			propertyType := order.Uint32(desc[pos:])
			propertySize := int(order.Uint32(desc[pos+4:]))
			pos += 8
			if propertySize < 0 || pos+propertySize > len(desc) {
				return fmt.Errorf("invalid GNU property size")
			}
			if propertyType == gnuPropertyX86ISA1Needed {
				if propertySize < 4 {
					return fmt.Errorf("invalid x86 ISA property")
				}
				needed := order.Uint32(desc[pos:])
				if needed&^uint32(gnuPropertyX86ISA1Baseline) != 0 {
					return fmt.Errorf("requires x86 ISA level bits %#x beyond core2 baseline", needed)
				}
			}
			pos += align(propertySize, 8)
		}
	}
	return nil
}

func verifyShebang(root, prefix, logicalPrefix string, searchPATH []string, arch string, f *os.File, filename string) error {
	_, _ = f.Seek(0, io.SeekStart)
	line, err := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "#!"))
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return fmt.Errorf("script %s has an empty shebang", filename)
	}
	interpreter := fields[0]
	if !filepath.IsAbs(interpreter) {
		return fmt.Errorf("script %s has non-absolute interpreter %q", filename, interpreter)
	}
	interpreterPath, err := resolveInRoot(root, prefix, logicalPrefix, interpreter)
	if err != nil {
		return fmt.Errorf("script %s interpreter %s escapes runtime root: %w", filename, interpreter, err)
	}
	if err := verifyExecutable(interpreterPath, arch); err != nil {
		return fmt.Errorf("script %s interpreter %s is unusable: %w", filename, interpreter, err)
	}
	if interpreter == "/usr/bin/env" || interpreter == "/bin/env" {
		if len(fields) != 2 || strings.HasPrefix(fields[1], "-") {
			return fmt.Errorf("script %s uses unsupported env shebang %q", filename, line)
		}
		resolved, err := resolvePATHCommand(root, prefix, logicalPrefix, searchPATH, fields[1])
		if err != nil {
			return fmt.Errorf("script %s env interpreter %q is unavailable: %w", filename, fields[1], err)
		}
		if err := verifyExecutable(resolved, arch); err != nil {
			return fmt.Errorf("script %s env interpreter %q is unusable: %w", filename, fields[1], err)
		}
	}
	return nil
}

func verifyExecutable(filename, arch string) error {
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not executable")
	}
	f, err := elf.Open(filename)
	if err != nil {
		return fmt.Errorf("not a valid ELF executable: %w", err)
	}
	defer f.Close()
	want := elf.EM_X86_64
	if arch == "arm64" {
		want = elf.EM_AARCH64
	} else if arch != "amd64" {
		return fmt.Errorf("unsupported architecture %q", arch)
	}
	if f.Machine != want {
		return fmt.Errorf("ELF machine %s does not match %s", f.Machine, want)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("ELF type %s is not executable", f.Type)
	}
	loadable := false
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_LOAD && prog.Flags&elf.PF_X != 0 {
			loadable = true
			break
		}
	}
	if !loadable {
		return fmt.Errorf("ELF has no executable PT_LOAD segment")
	}
	return nil
}

func verifyLibrary(filename, arch string) error {
	info, err := os.Stat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular library")
	}
	f, err := elf.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	want := elf.EM_X86_64
	if arch == "arm64" {
		want = elf.EM_AARCH64
	} else if arch != "amd64" {
		return fmt.Errorf("unsupported architecture %q", arch)
	}
	if f.Machine != want {
		return fmt.Errorf("wrong ELF machine %s", f.Machine)
	}
	if f.Type != elf.ET_DYN {
		return fmt.Errorf("ELF type %s is not a shared object", f.Type)
	}
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_LOAD {
			return nil
		}
	}
	return fmt.Errorf("ELF has no PT_LOAD segment")
}

func resolvePATHCommand(root, prefix, logicalPrefix string, searchPATH []string, command string) (string, error) {
	if command == "" || strings.ContainsRune(command, filepath.Separator) {
		return "", fmt.Errorf("invalid command name")
	}
	for _, dir := range searchPATH {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate, err := resolveInRoot(root, prefix, logicalPrefix, filepath.Join(dir, command))
		if err != nil {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

func verifyLink(root, prefix, logicalPrefix, filename string) error {
	target, err := os.Readlink(filename)
	if err != nil {
		return err
	}
	var resolved string
	if filepath.IsAbs(target) {
		resolved, err = resolveInRoot(root, prefix, logicalPrefix, target)
	} else {
		resolved, err = resolveHostWithinRoot(root, filepath.Join(filepath.Dir(filename), target))
	}
	if err != nil {
		return fmt.Errorf("symlink %s escapes runtime root via %q: %w", filename, target, err)
	}
	if _, err := os.Stat(resolved); err != nil {
		return fmt.Errorf("symlink %s points to unavailable target %q: %w", filename, target, err)
	}
	return nil
}

func appendUnique(dst []string, values ...string) []string {
	for _, v := range values {
		if v != "" && !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}

func resolveInRoot(root, prefix, logicalPrefix, value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("path %q is not absolute", value)
	}
	clean := filepath.Clean(value)
	logicalPrefix = filepath.Clean(logicalPrefix)
	var candidate string
	if clean == logicalPrefix {
		candidate = prefix
	} else if strings.HasPrefix(clean, logicalPrefix+string(filepath.Separator)) {
		candidate = filepath.Join(prefix, strings.TrimPrefix(clean, logicalPrefix+string(filepath.Separator)))
	} else {
		candidate = filepath.Join(root, strings.TrimPrefix(clean, string(filepath.Separator)))
	}
	return resolveHostWithinRoot(root, candidate)
}

func resolveHostWithinRoot(root, candidate string) (string, error) {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes root", candidate)
	}
	queue := strings.Split(filepath.ToSlash(rel), "/")
	current := root
	links := 0
	for len(queue) > 0 {
		component := queue[0]
		queue = queue[1:]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if current == root {
				return "", fmt.Errorf("path escapes root")
			}
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, filepath.FromSlash(component))
		info, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		links++
		if links > 64 {
			return "", fmt.Errorf("too many symlinks")
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			current = root
			queue = append(strings.Split(filepath.ToSlash(strings.TrimPrefix(filepath.Clean(target), string(filepath.Separator))), "/"), queue...)
		} else {
			queue = append(strings.Split(filepath.ToSlash(target), "/"), queue...)
		}
	}
	resolvedRel, err := filepath.Rel(root, current)
	if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("resolved path escapes root")
	}
	return current, nil
}

func verifyNoForbiddenReferences(filename string) error {
	patterns := [][]byte{
		[]byte("@@HOMEBREW_PREFIX@@"),
		[]byte("@@HOMEBREW_CELLAR@@"),
		[]byte("@@HOMEBREW_REPOSITORY@@"),
		[]byte("@@HOMEBREW_LIBRARY@@"),
		[]byte("@@HOMEBREW_PERL@@"),
		[]byte("@@HOMEBREW_JAVA@@"),
		[]byte("/home/linuxbrew/.cache/Homebrew"),
		[]byte("/home/linuxbrew/.linuxbrew/Homebrew/Library/Homebrew"),
	}
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	const chunkSize = 64 << 10
	const overlap = 256
	buffer := make([]byte, chunkSize+overlap)
	carry := 0
	for {
		n, readErr := f.Read(buffer[carry:])
		total := carry + n
		for _, pattern := range patterns {
			if bytes.Contains(buffer[:total], pattern) {
				return fmt.Errorf("retained file %s references materializer-only path %q", filename, pattern)
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if total > overlap {
			copy(buffer[:overlap], buffer[total-overlap:total])
			carry = overlap
		} else {
			carry = total
		}
	}
}

func systemLibraryDirs(root, arch string) ([]string, error) {
	var logical []string
	visited := map[string]bool{}
	filesRead := 0
	var readConfig func(string) error
	readConfig = func(configPath string) error {
		clean := path.Clean(filepath.ToSlash(configPath))
		if !strings.HasPrefix(clean, "/") {
			return fmt.Errorf("loader config path %q is not absolute", configPath)
		}
		resolved, err := resolveInRoot(root, filepath.Join(root, ".__unused"), "/.__unused", filepath.FromSlash(clean))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("loader config %q escapes runtime root", configPath)
		}
		if visited[resolved] {
			return nil
		}
		visited[resolved] = true
		filesRead++
		if filesRead > 256 {
			return fmt.Errorf("loader configuration includes too many files")
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("loader config %q is not a regular file", configPath)
		}
		if info.Size() > 1<<20 {
			return fmt.Errorf("loader config %q exceeds 1 MiB", configPath)
		}
		f, err := os.Open(resolved)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, 1<<20+1))
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(data) > 1<<20 {
			return fmt.Errorf("loader config %q exceeds 1 MiB", configPath)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "include ") {
				rawPattern := strings.TrimSpace(strings.TrimPrefix(line, "include "))
				if containsTraversalComponent(rawPattern) {
					return fmt.Errorf("loader include %q contains traversal", rawPattern)
				}
				pattern := path.Clean(rawPattern)
				if !strings.HasPrefix(pattern, "/") {
					return fmt.Errorf("loader include %q is not absolute", pattern)
				}
				matches, err := rootGlob(root, pattern)
				if err != nil {
					return err
				}
				for _, logicalMatch := range matches {
					if err := readConfig(logicalMatch); err != nil {
						return err
					}
				}
				continue
			}
			if !strings.HasPrefix(line, "/") || containsTraversalComponent(line) {
				return fmt.Errorf("loader path %q is invalid", line)
			}
			logical = append(logical, path.Clean(line))
		}
		return nil
	}
	if err := readConfig("/etc/ld.so.conf"); err != nil {
		return nil, err
	}
	logical = append(logical, "/lib", "/usr/lib", "/lib64", "/usr/lib64")
	triplet := "x86_64-linux-gnu"
	if arch == "arm64" {
		triplet = "aarch64-linux-gnu"
	}
	logical = append(logical, "/lib/"+triplet, "/usr/lib/"+triplet)
	var out []string
	for _, dir := range logical {
		clean := path.Clean(dir)
		if !strings.HasPrefix(clean, "/") {
			return nil, fmt.Errorf("loader path %q is not absolute", dir)
		}
		out = appendUnique(out, filepath.FromSlash(clean))
	}
	return out, nil
}

func rootGlob(root, pattern string) ([]string, error) {
	dirPattern, base := path.Dir(pattern), path.Base(pattern)
	if strings.ContainsAny(dirPattern, "*?[") {
		return nil, fmt.Errorf("loader include wildcards are only supported in the basename")
	}
	resolvedDir, err := resolveInRoot(root, filepath.Join(root, ".__unused"), "/.__unused", filepath.FromSlash(dirPattern))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(resolvedDir, filepath.FromSlash(base)))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		resolved, err := resolveHostWithinRoot(root, match)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("loader include match escapes root")
		}
		out = append(out, "/"+filepath.ToSlash(rel))
	}
	slices.Sort(out)
	return out, nil
}

func containsTraversalComponent(value string) bool {
	for _, part := range strings.Split(filepath.ToSlash(value), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}
