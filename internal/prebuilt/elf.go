package prebuilt

import (
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	"fmt"
	"slices"
)

func inspectPayload(payload []byte, profile Profile) (ELFEvidence, GoBuildEvidence, error) {
	file, err := elf.NewFile(bytes.NewReader(payload))
	if err != nil {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "parse ELF: %v", err)
	}
	defer file.Close()
	if file.Class != elf.ELFCLASS64 {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "ELF class %s is not ELF64", file.Class)
	}
	if file.Data != elf.ELFDATA2LSB {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "ELF data encoding %s is not little-endian", file.Data)
	}
	if file.Version != elf.EV_CURRENT {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "unsupported ELF version %s", file.Version)
	}
	expectedMachine := elf.EM_X86_64
	if profile.Target.Arch == "arm64" {
		expectedMachine = elf.EM_AARCH64
	}
	if file.Machine != expectedMachine {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "ELF machine %s does not match target %s", file.Machine, profile.Target.Arch)
	}
	if file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "ELF type %s is not an executable", file.Type)
	}

	executableLoad := false
	entryInExecutableLoad := false
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "PT_INTERP is forbidden for a static executable")
		}
		if program.Flags&elf.PF_W != 0 && program.Flags&elf.PF_X != 0 {
			return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "writable and executable program segment is forbidden")
		}
		if program.Type == elf.PT_LOAD && program.Flags&elf.PF_X != 0 {
			executableLoad = true
			if file.Entry >= program.Vaddr && file.Entry-program.Vaddr < program.Memsz {
				entryInExecutableLoad = true
			}
		}
	}
	if !executableLoad || !entryInExecutableLoad {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "ELF entry point is not contained in an executable PT_LOAD segment")
	}
	dynamicTags, err := forbiddenDynamicTags(payload, file)
	if err != nil {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "inspect PT_DYNAMIC: %v", err)
	}
	if dynamicTags.needed {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "DT_NEEDED entries are forbidden")
	}
	if dynamicTags.rpath {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "DT_RPATH entries are forbidden")
	}
	if dynamicTags.runpath {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "DT_RUNPATH entries are forbidden")
	}
	libraries, err := file.ImportedLibraries()
	if err != nil {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "read imported libraries: %v", err)
	}
	slices.Sort(libraries)
	if len(libraries) != 0 {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeInvalidELF, profile.PayloadPath, "imported libraries are forbidden: %v", libraries)
	}

	info, err := buildinfo.Read(bytes.NewReader(payload))
	if err != nil {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "read Go build information: %v", err)
	}
	if info.Main.Path != profile.GoBuild.ModulePath {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "main module %q does not match required module %q", info.Main.Path, profile.GoBuild.ModulePath)
	}
	if info.Main.Replace != nil {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "main module replacement is forbidden")
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		if _, duplicate := settings[setting.Key]; duplicate {
			return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "duplicate Go build setting %q", setting.Key)
		}
		settings[setting.Key] = setting.Value
	}
	goos, ok := settings["GOOS"]
	if !ok || goos != profile.Target.OS {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "GOOS %q does not match target %q", goos, profile.Target.OS)
	}
	goarch, ok := settings["GOARCH"]
	if !ok || goarch != profile.Target.Arch {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "GOARCH %q does not match target %q", goarch, profile.Target.Arch)
	}
	cgoRaw, ok := settings["CGO_ENABLED"]
	if !ok {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "CGO_ENABLED build setting is absent")
	}
	cgoValue, err := parseCGOValue(cgoRaw)
	if err != nil {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "%v", err)
	}
	if cgoValue != profile.GoBuild.CGOEnabled {
		return ELFEvidence{}, GoBuildEvidence{}, verificationError(CodeGoBuildMismatch, profile.PayloadPath, "CGO_ENABLED=%s does not match required value %t", cgoRaw, profile.GoBuild.CGOEnabled)
	}

	return ELFEvidence{
			Class:                      file.Class.String(),
			Data:                       file.Data.String(),
			Type:                       file.Type.String(),
			Machine:                    file.Machine.String(),
			Entry:                      file.Entry,
			ProgramHeaderCount:         len(file.Progs),
			Interpreter:                "",
			ImportedLibraries:          []string{},
			WritableExecutableSegments: 0,
		}, GoBuildEvidence{
			GoVersion:     info.GoVersion,
			MainPackage:   info.Path,
			ModulePath:    info.Main.Path,
			ModuleVersion: info.Main.Version,
			ModuleSum:     info.Main.Sum,
			GOOS:          goos,
			GOARCH:        goarch,
			CGOEnabled:    cgoValue,
		}, nil
}

type dynamicTagSet struct {
	needed  bool
	rpath   bool
	runpath bool
}

func forbiddenDynamicTags(payload []byte, file *elf.File) (dynamicTagSet, error) {
	var found dynamicTagSet
	for _, program := range file.Progs {
		if program.Type != elf.PT_DYNAMIC {
			continue
		}
		if program.Off > uint64(len(payload)) || program.Filesz > uint64(len(payload))-program.Off {
			return dynamicTagSet{}, fmt.Errorf("PT_DYNAMIC extends beyond the payload")
		}
		if program.Filesz == 0 || program.Filesz%16 != 0 {
			return dynamicTagSet{}, fmt.Errorf("PT_DYNAMIC size %d is not a positive multiple of 16", program.Filesz)
		}
		dynamic := payload[int(program.Off):int(program.Off+program.Filesz)]
		terminated := false
		for len(dynamic) >= 16 {
			tag := elf.DynTag(file.ByteOrder.Uint64(dynamic[:8]))
			dynamic = dynamic[16:]
			switch tag {
			case elf.DT_NULL:
				terminated = true
				dynamic = nil
			case elf.DT_NEEDED:
				found.needed = true
			case elf.DT_RPATH:
				found.rpath = true
			case elf.DT_RUNPATH:
				found.runpath = true
			}
		}
		if !terminated {
			return dynamicTagSet{}, fmt.Errorf("PT_DYNAMIC is not terminated by DT_NULL")
		}
	}
	for _, dynamic := range []struct {
		tag     elf.DynTag
		present *bool
	}{
		{tag: elf.DT_NEEDED, present: &found.needed},
		{tag: elf.DT_RPATH, present: &found.rpath},
		{tag: elf.DT_RUNPATH, present: &found.runpath},
	} {
		values, err := file.DynString(dynamic.tag)
		if err != nil {
			return dynamicTagSet{}, err
		}
		if len(values) != 0 {
			*dynamic.present = true
		}
	}
	return found, nil
}

func parseCGOValue(value string) (bool, error) {
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("invalid CGO_ENABLED value %q", value)
	}
}
