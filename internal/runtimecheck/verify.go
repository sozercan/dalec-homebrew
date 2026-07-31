// Package runtimecheck performs static checks over materialized payload. It
// never executes untrusted binaries (in particular it never invokes ldd).
package runtimecheck

import (
	"bufio"
	"bytes"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type Options struct {
	Root          string
	Prefix        string
	Arch          string
	CPUBaseline   string
	LogicalPrefix string
	SearchPATH    []string
}

type scriptScope struct {
	required  map[string]struct{}
	auxiliary map[string]string
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
	exposedExecutables, err := discoverExposedExecutables(root, prefix, opts.LogicalPrefix, opts.SearchPATH)
	if err != nil {
		return err
	}
	scope, bound, err := runtimeScriptScope(root, prefix, opts)
	if err != nil {
		return err
	}
	if !bound {
		scope.required = exposedExecutables
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
		f, err := os.Open(p)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		defer f.Close()
		var magic [8]byte
		n, _ := io.ReadFull(f, magic[:])
		_, _ = f.Seek(0, io.SeekStart)
		isELF := n >= 4 && string(magic[:4]) == "\x7fELF"
		if err := verifyNoForbiddenReferences(p); err != nil {
			errs = append(errs, err)
		}
		if isELF {
			clean := filepath.Clean(p)
			_, required := scope.required[clean]
			_, exposed := exposedExecutables[clean]
			objectData := relocatableObjectDataPath(p)
			executableObject := info.Mode().Perm()&0o111 != 0 && !objectData
			runtimeCandidate := required || exposed || executableObject
			if err := verifyELF(root, prefix, opts.LogicalPrefix, p, opts.Arch, opts.CPUBaseline, runtimeCandidate, objectData); err != nil {
				errs = append(errs, err)
			}
			return nil
		}
		if n >= 2 && magic[0] == '#' && magic[1] == '!' && info.Mode().Perm()&0o111 != 0 {
			clean := filepath.Clean(p)
			_, required := scope.required[clean]
			_, exposed := exposedExecutables[clean]
			expectedAuxiliaryShebang := ""
			if !required && !exposed {
				expectedAuxiliaryShebang = scope.auxiliary[clean]
			}
			if err := verifyShebang(root, prefix, opts.LogicalPrefix, opts.SearchPATH, opts.Arch, f, p, expectedAuxiliaryShebang); err != nil {
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

func relocatableObjectDataPath(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".a", ".o", ".lo", ".syso":
		return true
	default:
		return false
	}
}

func verifyELF(root, prefix, logicalPrefix, filename, arch, baseline string, runtimeCandidate, objectData bool) error {
	f, err := elf.Open(filename)
	if err != nil {
		return fmt.Errorf("parse ELF %s: %w", filename, err)
	}
	defer f.Close()
	if f.Type == elf.ET_REL {
		// Relocatable objects are runtime inputs, not host-loadable programs, but
		// only at authenticated object-data paths. A .so/plugin or arbitrary helper
		// containing ET_REL is malformed even when it is not executable or exposed.
		if runtimeCandidate || !objectData {
			return fmt.Errorf("ELF %s is a relocatable object exposed as a runtime executable or stored outside an object-data path", filename)
		}
		return nil
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("ELF %s has unsupported runtime type %s", filename, f.Type)
	}
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
		if ref := forbiddenRuntimeReference(interp); ref != "" {
			return fmt.Errorf("ELF %s interpreter references materializer-only path %q", filename, ref)
		}
		interpreterPath, err := resolveInRoot(root, prefix, logicalPrefix, interp)
		if err != nil {
			return fmt.Errorf("ELF %s interpreter %s escapes runtime root: %w", filename, interp, err)
		}
		if err := verifyExecutable(interpreterPath, arch); err != nil {
			return fmt.Errorf("ELF %s interpreter %s is unusable: %w", filename, interp, err)
		}
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
		if ref := forbiddenRuntimeReference(value); ref != "" {
			return fmt.Errorf("ELF %s runtime library path references materializer-only path %q", filename, ref)
		}
		for _, dir := range strings.Split(value, ":") {
			dir = strings.ReplaceAll(dir, "${ORIGIN}", logicalOrigin)
			dir = strings.ReplaceAll(dir, "$ORIGIN", logicalOrigin)
			if !filepath.IsAbs(dir) {
				return fmt.Errorf("ELF %s has relative runtime library path %q", filename, dir)
			}
			dirs = appendUnique(dirs, filepath.Clean(dir))
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("read ELF dependencies %s: %w", filename, err)
	}
	if len(libs) == 0 {
		return nil
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

func verifyShebang(root, prefix, logicalPrefix string, searchPATH []string, arch string, f *os.File, filename, expectedAuxiliaryShebang string) error {
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
	auxiliary := expectedAuxiliaryShebang != ""
	if auxiliary && line != expectedAuxiliaryShebang {
		return fmt.Errorf("script %s auxiliary shebang %q does not match authenticated fixture %q", filename, line, expectedAuxiliaryShebang)
	}
	interpreter := fields[0]
	if !filepath.IsAbs(interpreter) {
		if auxiliary {
			return nil
		}
		return fmt.Errorf("script %s has non-absolute interpreter %q", filename, interpreter)
	}
	isEnv := interpreter == "/usr/bin/env" || interpreter == "/bin/env"
	if isEnv && (len(fields) != 2 || strings.HasPrefix(fields[1], "-")) {
		return fmt.Errorf("script %s uses unsupported env shebang %q", filename, line)
	}
	interpreterPath, err := resolveInRoot(root, prefix, logicalPrefix, interpreter)
	if err != nil {
		if auxiliary && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("script %s interpreter %s escapes runtime root: %w", filename, interpreter, err)
	}
	if err := verifyExecutable(interpreterPath, arch); err != nil {
		return fmt.Errorf("script %s interpreter %s is unusable: %w", filename, interpreter, err)
	}
	if isEnv {
		resolved, err := resolvePATHCommand(root, prefix, logicalPrefix, searchPATH, fields[1])
		if err != nil {
			if auxiliary && errors.Is(err, os.ErrNotExist) {
				return nil
			}
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

func discoverExposedExecutables(root, prefix, logicalPrefix string, searchPATH []string) (map[string]struct{}, error) {
	dirs := appendUnique(nil, filepath.Join(logicalPrefix, "bin"), filepath.Join(logicalPrefix, "sbin"))
	dirs = appendUnique(dirs, searchPATH...)
	exposed := map[string]struct{}{}
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		resolvedDir, err := resolveInRoot(root, prefix, logicalPrefix, dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve executable search directory %q: %w", dir, err)
		}
		rel, err := filepath.Rel(prefix, resolvedDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			// Runtime-base PATH entries are outside the Homebrew prefix and are
			// not part of the tree walked by Verify.
			continue
		}
		info, err := os.Stat(resolvedDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat executable search directory %q: %w", dir, err)
		}
		if !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(resolvedDir)
		if err != nil {
			return nil, fmt.Errorf("read executable search directory %q: %w", dir, err)
		}
		for _, entry := range entries {
			candidate := filepath.Join(resolvedDir, entry.Name())
			resolved, err := resolveHostWithinRoot(root, candidate)
			if err != nil {
				// The main walk reports unsafe or dangling links with their full
				// source path. They cannot expand the exposed-script allowlist.
				continue
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				continue
			}
			exposed[filepath.Clean(resolved)] = struct{}{}
		}
	}
	return exposed, nil
}

func runtimeScriptScope(root, prefix string, opts Options) (scriptScope, bool, error) {
	resolutionData, resolutionFound, err := readRuntimeEvidence(root, prefix, opts.LogicalPrefix, "/usr/share/dalec-homebrew/resolution.json", 16<<20)
	if err != nil {
		return scriptScope{}, false, err
	}
	manifestData, manifestFound, err := readRuntimeEvidence(root, prefix, opts.LogicalPrefix, "/usr/share/dalec-homebrew/manifest.json", 16<<20)
	if err != nil {
		return scriptScope{}, false, err
	}
	if resolutionFound != manifestFound {
		return scriptScope{}, false, fmt.Errorf("runtime resolution and manifest evidence must either both exist or both be absent")
	}
	if !resolutionFound {
		return scriptScope{required: map[string]struct{}{}, auxiliary: map[string]string{}}, false, nil
	}
	record, err := resolution.Decode(resolutionData)
	if err != nil {
		return scriptScope{}, false, fmt.Errorf("decode runtime resolution evidence: %w", err)
	}
	digest, err := resolution.Digest(record)
	if err != nil {
		return scriptScope{}, false, fmt.Errorf("digest runtime resolution evidence: %w", err)
	}
	var manifest struct {
		SchemaVersion    string              `json:"schema_version"`
		ResolutionDigest string              `json:"resolution_digest"`
		Platform         resolution.Platform `json:"platform"`
		Prefix           string              `json:"prefix"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return scriptScope{}, false, fmt.Errorf("decode runtime manifest evidence: %w", err)
	}
	if manifest.SchemaVersion != "dalec-homebrew-runtime-manifest/v1" || manifest.ResolutionDigest != digest.String() {
		return scriptScope{}, false, fmt.Errorf("runtime manifest does not bind the embedded resolution")
	}
	if manifest.Prefix != opts.LogicalPrefix || manifest.Platform != record.Input.Platform || record.Input.Platform.Architecture != opts.Arch {
		return scriptScope{}, false, fmt.Errorf("runtime resolution scope does not match verification target")
	}
	if record.Runtime.CPUBaseline != opts.CPUBaseline || !slices.Equal(record.Runtime.GeneratedPATH, opts.SearchPATH) {
		return scriptScope{}, false, fmt.Errorf("runtime resolution policy does not match verification options")
	}
	byName := make(map[string]resolution.Node, len(record.Nodes))
	for _, node := range record.Nodes {
		byName[node.Name] = node
	}
	requestedNames := make(map[string]struct{}, len(record.Requested))
	scope := scriptScope{required: map[string]struct{}{}, auxiliary: map[string]string{}}
	for _, requested := range record.Requested {
		requestedNames[requested.Canonical] = struct{}{}
		node, ok := byName[requested.Canonical]
		if !ok {
			return scriptScope{}, false, fmt.Errorf("requested runtime root %q is missing from resolution", requested.Canonical)
		}
		for _, executable := range node.ExecutablePaths {
			executable = filepath.ToSlash(executable)
			clean := path.Clean(executable)
			if clean != executable || clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				return scriptScope{}, false, fmt.Errorf("requested executable path %q is unsafe", executable)
			}
			logical := path.Join(filepath.ToSlash(opts.LogicalPrefix), "Cellar", node.Name, node.PkgVersion, clean)
			resolved, err := resolveInRoot(root, prefix, opts.LogicalPrefix, filepath.FromSlash(logical))
			if err != nil {
				return scriptScope{}, false, fmt.Errorf("resolve requested executable %q: %w", logical, err)
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				return scriptScope{}, false, fmt.Errorf("requested executable %q is unavailable or unusable", logical)
			}
			scope.required[filepath.Clean(resolved)] = struct{}{}
		}
	}
	add := func(node resolution.Node, subpath, expected string) error {
		logical := path.Join(filepath.ToSlash(opts.LogicalPrefix), "Cellar", node.Name, node.PkgVersion, subpath)
		resolved, err := resolveInRoot(root, prefix, opts.LogicalPrefix, filepath.FromSlash(logical))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("resolve authenticated auxiliary script %q: %w", logical, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("stat authenticated auxiliary script %q: %w", logical, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("authenticated auxiliary script %q is not a regular file", logical)
		}
		if info.Mode().Perm()&0o111 == 0 {
			// Some upstream packages carry shebang-bearing examples or modules as
			// ordinary data. The main walk does not execute or shebang-check them, so
			// they do not need an interpreter exception.
			return nil
		}
		resolved = filepath.Clean(resolved)
		if previous := scope.auxiliary[resolved]; previous != "" && previous != expected {
			return fmt.Errorf("conflicting auxiliary shebang policy for %q", logical)
		}
		scope.auxiliary[resolved] = expected
		return nil
	}
	for _, node := range record.Nodes {
		switch {
		case node.Name == "go":
			for _, name := range []string{"all.rc", "clean.rc", "make.rc", "run.rc"} {
				if err := add(node, path.Join("libexec/src", name), "/bin/rc -e"); err != nil {
					return scriptScope{}, false, err
				}
			}
		case node.Name == "ncurses":
			for _, name := range []string{"debian", "debian-mingw", "debian-mingw64"} {
				if err := add(node, path.Join("share/ncurses/test/package", name, "rules"), "/usr/bin/make -f"); err != nil {
					return scriptScope{}, false, err
				}
			}
		case strings.HasPrefix(node.Name, "python@3."):
			minor := strings.TrimPrefix(node.Name, "python@")
			pythonAuxiliary := map[string]string{
				path.Join("lib/python"+minor, "idlelib/idle_test/example_noext"):             "usr/bin/env python",
				path.Join("lib/python"+minor, "encodings/rot_13.py"):                         "/usr/bin/env python",
				path.Join("lib/python"+minor, "site-packages/pip/_vendor/distro/distro.py"):  "/usr/bin/env python",
				path.Join("lib/python"+minor, "site-packages/pip/_vendor/requests/certs.py"): "/usr/bin/env python",
			}
			for subpath, expected := range pythonAuxiliary {
				if err := add(node, subpath, expected); err != nil {
					return scriptScope{}, false, err
				}
			}
		case node.Name == "dbus":
			if err := add(node, "share/doc/dbus/examples/GetAllMatchRules.py", "/usr/bin/env python"); err != nil {
				return scriptScope{}, false, err
			}
		case node.Name == "llvm" || strings.HasPrefix(node.Name, "llvm@"):
			if _, requested := requestedNames[node.Name]; requested {
				continue
			}
			major := strings.TrimPrefix(node.Name, "llvm@")
			if major == "llvm" || major == "" {
				major, _, _ = strings.Cut(node.FormulaVersion, ".")
			}
			llvmScripts := map[string]string{
				"bin/analyze-build":    "/usr/bin/env python3",
				"bin/git-clang-format": "/usr/bin/env python3",
				"bin/hmaptool":         "/usr/bin/env python3",
				"bin/intercept-build":  "/usr/bin/env python3",
				"bin/run-clang-tidy":   "/usr/bin/env python3",
				"bin/scan-build-py":    "/usr/bin/env python3",
				"bin/scan-view":        "/usr/bin/env python",
				path.Join("lib/clang", major, "bin/hwasan_symbolize"): "/usr/bin/env python3",
				"libexec/analyze-c++":                 "/usr/bin/env python3",
				"libexec/analyze-cc":                  "/usr/bin/env python3",
				"libexec/intercept-c++":               "/usr/bin/env python3",
				"libexec/intercept-cc":                "/usr/bin/env python3",
				"share/clang/clang-format-diff.py":    "/usr/bin/env python3",
				"share/clang/clang-tidy-diff.py":      "/usr/bin/env python3",
				"share/clang/run-find-all-symbols.py": "/usr/bin/env python",
				"share/opt-viewer/opt-diff.py":        "/usr/bin/env python",
				"share/opt-viewer/opt-stats.py":       "/usr/bin/env python",
				"share/opt-viewer/opt-viewer.py":      "/usr/bin/env python",
				"share/opt-viewer/optrecord.py":       "/usr/bin/env python",
			}
			for subpath, expected := range llvmScripts {
				if err := add(node, subpath, expected); err != nil {
					return scriptScope{}, false, err
				}
			}
		}
	}
	return scope, true, nil
}

func readRuntimeEvidence(root, prefix, logicalPrefix, logicalPath string, limit int64) ([]byte, bool, error) {
	resolved, err := resolveInRoot(root, prefix, logicalPrefix, logicalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("resolve runtime evidence %q: %w", logicalPath, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, false, fmt.Errorf("runtime evidence %q is not a bounded regular file", logicalPath)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) != info.Size() || int64(len(data)) > limit {
		return nil, false, fmt.Errorf("runtime evidence %q changed while reading", logicalPath)
	}
	return data, true, nil
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

const cargoRegistryPrefix = homebrewCachePath + "/cargo_cache/registry/src/"

var cargoReadOnlyProvenancePattern = regexp.MustCompile(`/home/linuxbrew/\.cache/Homebrew/cargo_cache/registry/src/[A-Za-z0-9._-]+/[A-Za-z0-9._+~-]+(?:/[A-Za-z0-9._+~/-]+\.rs(?::[0-9]+)?|\x00)`)
var cargoDebugProvenancePattern = regexp.MustCompile(`/home/linuxbrew/\.cache/Homebrew/cargo_cache/registry/src/[A-Za-z0-9._-]+/[A-Za-z0-9._+~-]+(?:/[A-Za-z0-9._+~@:/-]+)?\x00`)

var unresolvedRelocationPatterns = [][]byte{
	[]byte("@@HOMEBREW_PREFIX@@"),
	[]byte("@@HOMEBREW_CELLAR@@"),
	[]byte("@@HOMEBREW_REPOSITORY@@"),
	[]byte("@@HOMEBREW_LIBRARY@@"),
	[]byte("@@HOMEBREW_PERL@@"),
	[]byte("@@HOMEBREW_JAVA@@"),
}

const (
	homebrewCachePath      = "/home/linuxbrew/.cache/Homebrew"
	homebrewRepositoryPath = "/home/linuxbrew/.linuxbrew/Homebrew/Library/Homebrew"
)

type fileRange struct {
	start int64
	end   int64
	debug bool
}

func verifyNoForbiddenReferences(filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	for _, pattern := range append(append([][]byte{}, unresolvedRelocationPatterns...), []byte(homebrewRepositoryPath)) {
		offsets, err := findPatternOffsets(f, info.Size(), pattern, 1)
		if err != nil {
			return err
		}
		if len(offsets) > 0 {
			return fmt.Errorf("retained file %s references materializer-only path %q", filename, pattern)
		}
	}
	cacheOffsets, err := findPatternOffsets(f, info.Size(), []byte(homebrewCachePath), 1_000_000)
	if err != nil || len(cacheOffsets) == 0 {
		return err
	}
	var magic [8]byte
	n, readErr := f.ReadAt(magic[:], 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	switch {
	case n >= 4 && string(magic[:4]) == "\x7fELF":
		ef, err := elf.NewFile(f)
		if err != nil {
			return fmt.Errorf("retained ELF %s with build path is malformed: %w", filename, err)
		}
		defer ef.Close()
		allowed, err := elfProvenanceRanges(ef, info.Size())
		if err != nil {
			return fmt.Errorf("retained ELF %s provenance sections: %w", filename, err)
		}
		return validateCargoSourceProvenance(f, info.Size(), cacheOffsets, allowed)
	case n == len(magic) && string(magic[:]) == "!<arch>\n":
		return verifyARProvenance(f, info.Size(), len(cacheOffsets))
	default:
		return fmt.Errorf("retained file %s references materializer-only path %q", filename, homebrewCachePath)
	}
}

func elfProvenanceRanges(file *elf.File, size int64) ([]fileRange, error) {
	allowed := make([]fileRange, 0)
	for _, section := range file.Sections {
		if section.Type != elf.SHT_PROGBITS || section.Flags&(elf.SHF_WRITE|elf.SHF_EXECINSTR) != 0 {
			continue
		}
		readOnlyRuntimeData := section.Flags&elf.SHF_ALLOC != 0 && (section.Name == ".rodata" || strings.HasPrefix(section.Name, ".rodata."))
		debugProvenance := section.Flags&elf.SHF_ALLOC == 0 && (strings.HasPrefix(section.Name, ".debug_") || strings.HasPrefix(section.Name, ".zdebug_"))
		if !readOnlyRuntimeData && !debugProvenance {
			continue
		}
		if section.Offset > uint64(size) || section.Size > uint64(size)-section.Offset {
			return nil, fmt.Errorf("invalid provenance section %q", section.Name)
		}
		allowed = append(allowed, fileRange{start: int64(section.Offset), end: int64(section.Offset + section.Size), debug: debugProvenance})
	}
	return allowed, nil
}

func validateCargoSourceProvenance(r io.ReaderAt, size int64, offsets []int64, allowed []fileRange) error {
	for _, offset := range offsets {
		var containing *fileRange
		for i := range allowed {
			if offset >= allowed[i].start && offset < allowed[i].end {
				containing = &allowed[i]
				break
			}
		}
		if containing == nil {
			return fmt.Errorf("build cache reference at offset %d is outside authenticated provenance data", offset)
		}
		remaining := size - offset
		if remaining <= 0 {
			return fmt.Errorf("invalid build cache reference offset %d", offset)
		}
		window := int64(4096)
		if remaining < window {
			window = remaining
		}
		data := make([]byte, window)
		n, err := r.ReadAt(data, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		pattern := cargoReadOnlyProvenancePattern
		if containing.debug {
			pattern = cargoDebugProvenancePattern
		}
		match := pattern.FindIndex(data[:n])
		if match == nil || match[0] != 0 {
			return fmt.Errorf("build cache reference at offset %d is not authenticated Cargo source provenance", offset)
		}
		continuation := []byte(nil)
		if !containing.debug && data[match[1]-1] != 0 {
			continuation, err = readCargoContinuation(r, offset+int64(match[1]), min(containing.end, size))
			if err != nil {
				return fmt.Errorf("build cache reference at offset %d continuation: %w", offset, err)
			}
		}
		if err := validateCargoProvenanceReference(data[:n], match[1], containing.debug, continuation); err != nil {
			return fmt.Errorf("build cache reference at offset %d: %w", offset, err)
		}
		if offset+int64(match[1]) > containing.end {
			return fmt.Errorf("Cargo source provenance at offset %d crosses its provenance section", offset)
		}
	}
	return nil
}

func readCargoContinuation(r io.ReaderAt, start, end int64) ([]byte, error) {
	if start >= end {
		return nil, nil
	}
	const maxContinuation = 1 << 20
	length := end - start
	if length > maxContinuation {
		length = maxContinuation
	}
	data := make([]byte, length)
	n, err := r.ReadAt(data, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	data = data[:n]
	delimiter := len(data)
	if nul := bytes.IndexByte(data, 0); nul >= 0 && nul < delimiter {
		delimiter = nul
	}
	if next := bytes.Index(data, []byte(cargoRegistryPrefix)); next >= 0 && next < delimiter {
		delimiter = next
	}
	if delimiter == len(data) && start+int64(n) < end {
		return nil, fmt.Errorf("compiler provenance continuation exceeds %d bytes without a boundary", maxContinuation)
	}
	return data[:delimiter], nil
}

func cargoContinuationTraverses(data []byte) bool {
	text := filepath.ToSlash(string(data))
	return strings.HasPrefix(text, "../") || strings.Contains(text, "/../") || strings.HasSuffix(text, "/..") ||
		strings.HasPrefix(text, "./") || strings.Contains(text, "/./") || strings.HasSuffix(text, "/.")
}

func validateCargoProvenanceReference(window []byte, matchEnd int, debug bool, continuation []byte) error {
	if matchEnd <= 0 || matchEnd > len(window) {
		return fmt.Errorf("invalid Cargo provenance match")
	}
	matched := window[:matchEnd]
	terminated := matched[len(matched)-1] == 0
	if terminated {
		matched = matched[:len(matched)-1]
	}
	if !bytes.HasPrefix(matched, []byte(cargoRegistryPrefix)) {
		return fmt.Errorf("Cargo provenance has an invalid registry prefix")
	}
	relative := string(matched[len(cargoRegistryPrefix):])
	if relative == "" || path.IsAbs(relative) || strings.ContainsRune(relative, '\\') {
		return fmt.Errorf("Cargo provenance path %q is invalid", relative)
	}
	components := strings.Split(relative, "/")
	if len(components) < 2 || components[0] == "" || components[1] == "" || components[0] == "." || components[0] == ".." || components[1] == "." || components[1] == ".." {
		return fmt.Errorf("Cargo provenance path %q lacks a safe index or crate identity", relative)
	}
	depth := 0
	for _, component := range components[2:] {
		switch component {
		case "":
			return fmt.Errorf("Cargo provenance path %q contains an empty component", relative)
		case ".":
			continue
		case "..":
			if depth == 0 {
				return fmt.Errorf("Cargo provenance path %q escapes its crate root", relative)
			}
			depth--
		default:
			depth++
		}
	}
	if debug && !terminated {
		return fmt.Errorf("debug Cargo provenance is not NUL-terminated")
	}
	if !debug && !terminated && len(continuation) > 0 {
		// Rust binaries concatenate file!() source strings with symbol or diagnostic
		// text. The caller reads that complete suffix through its NUL, section end,
		// or next authenticated Cargo prefix. It may be opaque compiler data, but it
		// must not continue the matched .rs path or introduce path traversal.
		if continuation[0] == '.' || continuation[0] == '/' || continuation[0] == '\\' || cargoContinuationTraverses(continuation) {
			return fmt.Errorf("Cargo source path continues beyond its .rs boundary")
		}
	}
	return nil
}

func verifyARProvenance(f *os.File, size int64, expectedCacheReferences int) error {
	if size < 8 {
		return fmt.Errorf("malformed ar archive: truncated global header")
	}
	var global [8]byte
	if _, err := f.ReadAt(global[:], 0); err != nil || string(global[:]) != "!<arch>\n" {
		return fmt.Errorf("malformed ar archive: invalid global header")
	}
	offset := int64(8)
	handled := 0
	for offset < size {
		if size-offset < 60 {
			return fmt.Errorf("malformed ar archive: truncated member header at offset %d", offset)
		}
		var header [60]byte
		if _, err := f.ReadAt(header[:], offset); err != nil {
			return err
		}
		if string(header[58:60]) != "`\n" {
			return fmt.Errorf("malformed ar archive: invalid member trailer at offset %d", offset)
		}
		if _, err := parseARNumber("mtime", header[16:28], 10, true); err != nil {
			return err
		}
		if _, err := parseARNumber("uid", header[28:34], 10, true); err != nil {
			return err
		}
		if _, err := parseARNumber("gid", header[34:40], 10, true); err != nil {
			return err
		}
		if _, err := parseARNumber("mode", header[40:48], 8, true); err != nil {
			return err
		}
		memberSize, err := parseARNumber("size", header[48:58], 10, false)
		if err != nil {
			return err
		}
		dataOffset := offset + 60
		if memberSize < 0 || dataOffset > size || memberSize > size-dataOffset {
			return fmt.Errorf("malformed ar archive: member at offset %d exceeds archive", offset)
		}
		name := strings.TrimSpace(string(header[:16]))
		contentOffset, contentSize := dataOffset, memberSize
		if strings.HasPrefix(name, "#1/") {
			nameSize, err := strconv.ParseInt(strings.TrimPrefix(name, "#1/"), 10, 64)
			if err != nil || nameSize <= 0 || nameSize > contentSize {
				return fmt.Errorf("malformed ar archive: invalid BSD member name at offset %d", offset)
			}
			contentOffset += nameSize
			contentSize -= nameSize
		}
		member := io.NewSectionReader(f, contentOffset, contentSize)
		offsets, err := findPatternOffsets(member, contentSize, []byte(homebrewCachePath), 1_000_000)
		if err != nil {
			return err
		}
		handled += len(offsets)
		if len(offsets) > 0 {
			if name == "/" || name == "//" || name == "/SYM64/" || strings.HasPrefix(name, "__.SYMDEF") {
				return fmt.Errorf("ar metadata member %q contains a build cache reference", name)
			}
			ef, err := elf.NewFile(member)
			if err != nil {
				return fmt.Errorf("ar member %q with build path is not valid ELF: %w", name, err)
			}
			if ef.Type != elf.ET_REL {
				_ = ef.Close()
				return fmt.Errorf("ar member %q with build path is ELF type %s, expected ET_REL", name, ef.Type)
			}
			allowed, rangeErr := elfProvenanceRanges(ef, contentSize)
			_ = ef.Close()
			if rangeErr != nil {
				return fmt.Errorf("ar member %q provenance sections: %w", name, rangeErr)
			}
			if err := validateCargoSourceProvenance(member, contentSize, offsets, allowed); err != nil {
				return fmt.Errorf("ar member %q: %w", name, err)
			}
		}
		offset = dataOffset + memberSize
		if memberSize%2 != 0 {
			if offset >= size {
				return fmt.Errorf("malformed ar archive: missing alignment byte")
			}
			var padding [1]byte
			if _, err := f.ReadAt(padding[:], offset); err != nil || padding[0] != '\n' {
				return fmt.Errorf("malformed ar archive: invalid alignment byte")
			}
			offset++
		}
	}
	if offset != size || handled != expectedCacheReferences {
		return fmt.Errorf("malformed ar archive: unaccounted build cache references")
	}
	return nil
}

func parseARNumber(label string, field []byte, base int, allowEmpty bool) (int64, error) {
	value := strings.TrimSpace(string(field))
	if value == "" {
		if allowEmpty {
			return 0, nil
		}
		return 0, fmt.Errorf("malformed ar archive: empty %s", label)
	}
	parsed, err := strconv.ParseInt(value, base, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("malformed ar archive: invalid %s %q", label, value)
	}
	return parsed, nil
}

func findPatternOffsets(r io.ReaderAt, size int64, pattern []byte, limit int) ([]int64, error) {
	if len(pattern) == 0 || size <= 0 {
		return nil, nil
	}
	const chunkSize = 64 << 10
	overlap := len(pattern) - 1
	buffer := make([]byte, chunkSize+overlap)
	var offsets []int64
	var position int64
	carry := 0
	next := int64(0)
	for position < size {
		want := chunkSize
		if remaining := size - position; remaining < int64(want) {
			want = int(remaining)
		}
		n, err := r.ReadAt(buffer[carry:carry+want], position)
		total := carry + n
		base := position - int64(carry)
		for search := 0; search < total; {
			index := bytes.Index(buffer[search:total], pattern)
			if index < 0 {
				break
			}
			absolute := base + int64(search+index)
			if absolute >= next {
				offsets = append(offsets, absolute)
				if len(offsets) > limit {
					return nil, fmt.Errorf("too many occurrences of %q", pattern)
				}
				next = absolute + 1
			}
			search += index + 1
		}
		position += int64(n)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if n == 0 {
			break
		}
		carry = overlap
		if carry > total {
			carry = total
		}
		copy(buffer[:carry], buffer[total-carry:total])
	}
	return offsets, nil
}

func forbiddenRuntimeReference(value string) string {
	for _, pattern := range []string{
		"@@HOMEBREW_PREFIX@@",
		"@@HOMEBREW_CELLAR@@",
		"@@HOMEBREW_REPOSITORY@@",
		"@@HOMEBREW_LIBRARY@@",
		"@@HOMEBREW_PERL@@",
		"@@HOMEBREW_JAVA@@",
		"/home/linuxbrew/.cache/Homebrew",
		"/home/linuxbrew/.linuxbrew/Homebrew/Library/Homebrew",
	} {
		if strings.Contains(value, pattern) {
			return pattern
		}
	}
	return ""
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
