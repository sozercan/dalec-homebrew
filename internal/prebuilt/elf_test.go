package prebuilt

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"testing"
)

func TestELFRejectsWrongClassTypeMachineAndEntry(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	tests := []struct {
		name    string
		payload []byte
		arch    string
	}{
		{name: "wrong machine", payload: payload, arch: "arm64"},
		{name: "ELF32", payload: mutateByte(payload, 4, 1), arch: "amd64"},
		{name: "relocatable", payload: mutateUint16(payload, 16, uint16(elf.ET_REL)), arch: "amd64"},
		{name: "entry outside executable load", payload: mutateUint64(payload, 24, 0), arch: "amd64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := makeSourceArchive(t, baseEntries(test.payload))
			_, err := Derive(bytes.NewReader(source), formula, profileFor(source, formula, test.arch))
			requireErrorCode(t, err, CodeInvalidELF)
		})
	}
}

func TestELFRejectsInterpreterImportedLibrariesAndWritableExecutableSegments(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	withInterpreter := mutateProgramHeader(t, payload, func(programType, flags uint32) (uint32, uint32, bool) {
		return uint32(elf.PT_INTERP), flags, true
	})
	withWritableExecutable := mutateProgramHeader(t, payload, func(programType, flags uint32) (uint32, uint32, bool) {
		if elf.ProgType(programType) == elf.PT_LOAD && elf.ProgFlag(flags)&elf.PF_X != 0 {
			return programType, flags | uint32(elf.PF_W), true
		}
		return 0, 0, false
	})
	withDynamicSection := minimalDynamicELF()
	withDynamicProgram := minimalDynamicProgramELF()
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "PT_INTERP", payload: withInterpreter},
		{name: "writable executable segment", payload: withWritableExecutable},
		{name: "DT_NEEDED section", payload: withDynamicSection},
		{name: "sectionless PT_DYNAMIC DT_NEEDED", payload: withDynamicProgram},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := makeSourceArchive(t, baseEntries(test.payload))
			_, err := Derive(bytes.NewReader(source), formula, profileFor(source, formula, "amd64"))
			requireErrorCode(t, err, CodeInvalidELF)
		})
	}
}

func TestELFRequiresGoBuildInfoAndExactSettings(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	tests := []struct {
		name       string
		payload    []byte
		mutateProf func(*Profile)
	}{
		{name: "missing build info", payload: minimalStaticELF()},
		{name: "module", payload: payload, mutateProf: func(profile *Profile) { profile.GoBuild.ModulePath = "example.com/other" }},
		{name: "GOOS", payload: replaceBuildSetting(t, payload, "GOOS=linux", "GOOS=xxxxx")},
		{name: "GOARCH", payload: replaceBuildSetting(t, payload, "GOARCH=amd64", "GOARCH=arm64")},
		{name: "CGO_ENABLED value", payload: replaceBuildSetting(t, payload, "CGO_ENABLED=0", "CGO_ENABLED=1")},
		{name: "CGO_ENABLED missing", payload: replaceBuildSetting(t, payload, "CGO_ENABLED=0", "BAD_ENABLED=0")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := makeSourceArchive(t, baseEntries(test.payload))
			profile := profileFor(source, formula, "amd64")
			if test.mutateProf != nil {
				test.mutateProf(&profile)
			}
			_, err := Derive(bytes.NewReader(source), formula, profile)
			requireErrorCode(t, err, CodeGoBuildMismatch)
		})
	}
}

func minimalStaticELF() []byte {
	data := minimalDynamicELF()
	binary.LittleEndian.PutUint64(data[40:48], 0)
	binary.LittleEndian.PutUint16(data[60:62], 0)
	binary.LittleEndian.PutUint16(data[62:64], 0)
	return data
}

func minimalDynamicProgramELF() []byte {
	data := minimalDynamicELF()
	binary.LittleEndian.PutUint16(data[56:58], 2)
	program := data[64+56 : 64+2*56]
	binary.LittleEndian.PutUint32(program[0:4], uint32(elf.PT_DYNAMIC))
	binary.LittleEndian.PutUint32(program[4:8], uint32(elf.PF_R))
	binary.LittleEndian.PutUint64(program[8:16], 0x200)
	binary.LittleEndian.PutUint64(program[16:24], 0x400200)
	binary.LittleEndian.PutUint64(program[24:32], 0x400200)
	binary.LittleEndian.PutUint64(program[32:40], 32)
	binary.LittleEndian.PutUint64(program[40:48], 32)
	binary.LittleEndian.PutUint64(program[48:56], 8)
	binary.LittleEndian.PutUint64(data[40:48], 0)
	binary.LittleEndian.PutUint16(data[60:62], 0)
	binary.LittleEndian.PutUint16(data[62:64], 0)
	return data
}

func mutateByte(data []byte, offset int, value byte) []byte {
	out := append([]byte(nil), data...)
	out[offset] = value
	return out
}

func mutateUint16(data []byte, offset int, value uint16) []byte {
	out := append([]byte(nil), data...)
	binary.LittleEndian.PutUint16(out[offset:offset+2], value)
	return out
}

func mutateUint64(data []byte, offset int, value uint64) []byte {
	out := append([]byte(nil), data...)
	binary.LittleEndian.PutUint64(out[offset:offset+8], value)
	return out
}
