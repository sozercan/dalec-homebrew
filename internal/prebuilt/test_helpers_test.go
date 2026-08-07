package prebuilt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const fixtureModule = "example.com/prebuiltfixture"

var fixtureCache = struct {
	sync.Mutex
	binaries map[string][]byte
}{binaries: make(map[string][]byte)}

func goELFFixture(t *testing.T, arch string) []byte {
	t.Helper()
	fixtureCache.Lock()
	defer fixtureCache.Unlock()
	if binary := fixtureCache.binaries[arch]; binary != nil {
		return append([]byte(nil), binary...)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module "+fixtureModule+"\n\ngo 1.25.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "fixture")
	command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", output, ".")
	command.Dir = directory
	command.Env = append(os.Environ(),
		"GOWORK=off",
		"GOFLAGS=",
		"GOOS=linux",
		"GOARCH="+arch,
		"CGO_ENABLED=0",
	)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s ELF fixture: %v\n%s", arch, err, combined)
	}
	binary, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCache.binaries[arch] = append([]byte(nil), binary...)
	return append([]byte(nil), binary...)
}

type testArchiveEntry struct {
	name       string
	mode       int64
	data       []byte
	typeflag   byte
	linkname   string
	format     tar.Format
	paxRecords map[string]string
	uid        int
	gid        int
	modTime    time.Time
}

func makeSourceArchive(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	var tarBuffer bytes.Buffer
	writer := tar.NewWriter(&tarBuffer)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		format := entry.format
		if format == tar.FormatUnknown {
			format = tar.FormatUSTAR
		}
		modTime := entry.modTime
		if modTime.IsZero() {
			modTime = time.Unix(1_700_000_000, 0).UTC()
		}
		header := &tar.Header{
			Name:       entry.name,
			Mode:       entry.mode,
			Size:       int64(len(entry.data)),
			Typeflag:   typeflag,
			Linkname:   entry.linkname,
			Uid:        entry.uid,
			Gid:        entry.gid,
			ModTime:    modTime,
			Format:     format,
			PAXRecords: entry.paxRecords,
		}
		if typeflag != tar.TypeReg && typeflag != tar.TypeRegA {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write test tar header %q: %v", entry.name, err)
		}
		if header.Size > 0 {
			if _, err := writer.Write(entry.data); err != nil {
				t.Fatalf("write test tar body %q: %v", entry.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return gzipBytes(t, tarBuffer.Bytes(), time.Unix(1_600_000_000, 0).UTC())
}

func gzipBytes(t *testing.T, tarBytes []byte, modTime time.Time) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := gzip.NewWriterLevel(&output, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	writer.Header.ModTime = modTime.UTC()
	writer.Header.OS = 255
	if _, err := writer.Write(tarBytes); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func gunzipBytes(t *testing.T, source []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return data
}

func baseEntries(payload []byte) []testArchiveEntry {
	return []testArchiveEntry{
		{name: "LICENSE", mode: 0o644, data: []byte("fixture license\n")},
		{name: "README.md", mode: 0o644, data: []byte("fixture readme\n")},
		{name: "a365", mode: 0o755, data: payload},
	}
}

func fixtureFormula() []byte {
	return []byte("class A365 < Formula\n  desc \"fixture\"\n  def install\n    bin.install \"a365\"\n  end\nend\n")
}

func profileFor(source []byte, formula []byte, arch string) Profile {
	return Profile{
		PolicyVersion: "single-static-elf-archive-v1",
		Name:          "a365",
		PkgVersion:    "0.3.3",
		Target:        Target{OS: "linux", Arch: arch},
		Source: SourceExpectation{
			Size:   int64(len(source)),
			SHA256: digestBytes(source),
		},
		FormulaSHA256: digestBytes(formula),
		Entries: []EntryProfile{
			{Path: "a365", Mode: 0o755},
			{Path: "LICENSE", Mode: 0o644},
			{Path: "README.md", Mode: 0o644},
		},
		PayloadPath:     "a365",
		GoBuild:         GoBuildProfile{ModulePath: fixtureModule, CGOEnabled: false},
		SourceDateEpoch: 1_700_000_123,
		Limits:          DefaultLimits(),
	}
}

func requireErrorCode(t *testing.T, err error, code ErrorCode) *VerificationError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error code %s", code)
	}
	var verification *VerificationError
	if !errors.As(err, &verification) {
		t.Fatalf("expected VerificationError, got %T: %v", err, err)
	}
	if verification.Code != code {
		t.Fatalf("expected error code %s, got %s: %v", code, verification.Code, err)
	}
	return verification
}

func replaceBuildSetting(t *testing.T, payload []byte, oldValue, newValue string) []byte {
	t.Helper()
	if len(oldValue) != len(newValue) {
		t.Fatalf("replacement lengths differ: %q and %q", oldValue, newValue)
	}
	if bytes.Count(payload, []byte(oldValue)) == 0 {
		t.Fatalf("expected %q build setting in fixture", oldValue)
	}
	return bytes.ReplaceAll(append([]byte(nil), payload...), []byte(oldValue), []byte(newValue))
}

func mutateProgramHeader(t *testing.T, payload []byte, mutate func(programType, flags uint32) (uint32, uint32, bool)) []byte {
	t.Helper()
	out := append([]byte(nil), payload...)
	if len(out) < 64 || !bytes.Equal(out[:4], []byte{0x7f, 'E', 'L', 'F'}) || out[4] != 2 || out[5] != 1 {
		t.Fatal("fixture is not little-endian ELF64")
	}
	programOffset := binary.LittleEndian.Uint64(out[32:40])
	entrySize := binary.LittleEndian.Uint16(out[54:56])
	count := binary.LittleEndian.Uint16(out[56:58])
	for index := uint16(0); index < count; index++ {
		offset := int(programOffset) + int(index)*int(entrySize)
		if offset < 0 || offset+8 > len(out) {
			t.Fatal("invalid program-header table in fixture")
		}
		programType := binary.LittleEndian.Uint32(out[offset : offset+4])
		flags := binary.LittleEndian.Uint32(out[offset+4 : offset+8])
		newType, newFlags, ok := mutate(programType, flags)
		if !ok {
			continue
		}
		binary.LittleEndian.PutUint32(out[offset:offset+4], newType)
		binary.LittleEndian.PutUint32(out[offset+4:offset+8], newFlags)
		return out
	}
	t.Fatal("no matching program header")
	return nil
}

func minimalDynamicELF() []byte {
	const (
		base       = uint64(0x400000)
		shstrOff   = 0x180
		dynstrOff  = 0x1c0
		dynamicOff = 0x200
		sectionOff = 0x300
		fileSize   = 0x400
	)
	data := make([]byte, fileSize)
	copy(data[:4], []byte{0x7f, 'E', 'L', 'F'})
	data[4] = 2
	data[5] = 1
	data[6] = 1
	binary.LittleEndian.PutUint16(data[16:18], 2)
	binary.LittleEndian.PutUint16(data[18:20], 62)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	binary.LittleEndian.PutUint64(data[24:32], base)
	binary.LittleEndian.PutUint64(data[32:40], 64)
	binary.LittleEndian.PutUint64(data[40:48], sectionOff)
	binary.LittleEndian.PutUint16(data[52:54], 64)
	binary.LittleEndian.PutUint16(data[54:56], 56)
	binary.LittleEndian.PutUint16(data[56:58], 1)
	binary.LittleEndian.PutUint16(data[58:60], 64)
	binary.LittleEndian.PutUint16(data[60:62], 4)
	binary.LittleEndian.PutUint16(data[62:64], 1)

	program := data[64 : 64+56]
	binary.LittleEndian.PutUint32(program[0:4], 1)
	binary.LittleEndian.PutUint32(program[4:8], 5)
	binary.LittleEndian.PutUint64(program[8:16], 0)
	binary.LittleEndian.PutUint64(program[16:24], base)
	binary.LittleEndian.PutUint64(program[24:32], base)
	binary.LittleEndian.PutUint64(program[32:40], fileSize)
	binary.LittleEndian.PutUint64(program[40:48], fileSize)
	binary.LittleEndian.PutUint64(program[48:56], 0x1000)

	shstr := []byte("\x00.shstrtab\x00.dynstr\x00.dynamic\x00")
	dynstr := []byte("\x00libc.so.6\x00")
	copy(data[shstrOff:], shstr)
	copy(data[dynstrOff:], dynstr)
	binary.LittleEndian.PutUint64(data[dynamicOff:dynamicOff+8], 1)
	binary.LittleEndian.PutUint64(data[dynamicOff+8:dynamicOff+16], 1)
	binary.LittleEndian.PutUint64(data[dynamicOff+16:dynamicOff+24], 0)
	binary.LittleEndian.PutUint64(data[dynamicOff+24:dynamicOff+32], 0)

	putSection := func(index int, name, sectionType uint32, flags, address, offset, size uint64, link uint32, alignment, entrySize uint64) {
		section := data[sectionOff+index*64 : sectionOff+(index+1)*64]
		binary.LittleEndian.PutUint32(section[0:4], name)
		binary.LittleEndian.PutUint32(section[4:8], sectionType)
		binary.LittleEndian.PutUint64(section[8:16], flags)
		binary.LittleEndian.PutUint64(section[16:24], address)
		binary.LittleEndian.PutUint64(section[24:32], offset)
		binary.LittleEndian.PutUint64(section[32:40], size)
		binary.LittleEndian.PutUint32(section[40:44], link)
		binary.LittleEndian.PutUint64(section[48:56], alignment)
		binary.LittleEndian.PutUint64(section[56:64], entrySize)
	}
	putSection(1, 1, 3, 0, 0, shstrOff, uint64(len(shstr)), 0, 1, 0)
	putSection(2, 11, 3, 2, base+dynstrOff, dynstrOff, uint64(len(dynstr)), 0, 1, 0)
	putSection(3, 19, 6, 2, base+dynamicOff, dynamicOff, 32, 2, 8, 16)
	return data
}

func inspectDerivedBottle(t *testing.T, bottle []byte) ([]*tar.Header, map[string][]byte, gzip.Header) {
	t.Helper()
	compressed := bytes.NewReader(bottle)
	reader, err := gzip.NewReader(compressed)
	if err != nil {
		t.Fatal(err)
	}
	reader.Multistream(false)
	header := reader.Header
	tarReader := tar.NewReader(reader)
	var headers []*tar.Header
	contents := make(map[string][]byte)
	for {
		entry, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		copyHeader := *entry
		headers = append(headers, &copyHeader)
		data, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		contents[entry.Name] = data
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if compressed.Len() != 0 {
		t.Fatalf("derived bottle has %d trailing compressed bytes", compressed.Len())
	}
	return headers, contents, header
}
