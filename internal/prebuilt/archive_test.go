package prebuilt

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestArchiveRequiresExactRegularEntriesAndModes(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	base := baseEntries(payload)
	tests := []struct {
		name    string
		entries []testArchiveEntry
		code    ErrorCode
	}{
		{name: "missing entry", entries: base[:2], code: CodeEntryMismatch},
		{name: "extra entry", entries: append(append([]testArchiveEntry(nil), base...), testArchiveEntry{name: "NOTICE", mode: 0o644, data: []byte("extra")}), code: CodeEntryMismatch},
		{name: "mode mismatch", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o775, data: payload}}, code: CodeEntryMismatch},
		{name: "duplicate", entries: append(append([]testArchiveEntry(nil), base...), testArchiveEntry{name: "a365", mode: 0o755, data: payload}), code: CodeDuplicatePath},
		{name: "traversal", entries: []testArchiveEntry{base[0], base[1], {name: "../a365", mode: 0o755, data: payload}}, code: CodeUnsafePath},
		{name: "absolute", entries: []testArchiveEntry{base[0], base[1], {name: "/a365", mode: 0o755, data: payload}}, code: CodeUnsafePath},
		{name: "backslash", entries: []testArchiveEntry{base[0], base[1], {name: `dir\a365`, mode: 0o755, data: payload}}, code: CodeUnsafePath},
		{name: "directory", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o755, typeflag: tar.TypeDir}}, code: CodeUnsafeType},
		{name: "symlink", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "target"}}, code: CodeUnsafeType},
		{name: "hardlink", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o755, typeflag: tar.TypeLink, linkname: "LICENSE"}}, code: CodeUnsafeType},
		{name: "character device", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o600, typeflag: tar.TypeChar}}, code: CodeUnsafeType},
		{name: "fifo", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o600, typeflag: tar.TypeFifo}}, code: CodeUnsafeType},
		{name: "GNU header", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o755, data: payload, format: tar.FormatGNU}}, code: CodeUnsafeMetadata},
		{name: "PAX header", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o755, data: payload, format: tar.FormatPAX, paxRecords: map[string]string{"comment": "forbidden"}}}, code: CodeUnsafeMetadata},
		{name: "PAX xattr", entries: []testArchiveEntry{base[0], base[1], {name: "a365", mode: 0o755, data: payload, format: tar.FormatPAX, paxRecords: map[string]string{"SCHILY.xattr.user.test": "value"}}}, code: CodeUnsafeMetadata},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := makeSourceArchive(t, test.entries)
			_, err := Derive(bytes.NewReader(source), formula, profileFor(source, formula, "amd64"))
			requireErrorCode(t, err, test.code)
		})
	}
}

func TestArchiveRejectsMalformedPhysicalTar(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	validSource := makeSourceArchive(t, baseEntries(payload))
	validTar := gunzipBytes(t, validSource)
	tests := []struct {
		name   string
		mutate func([]byte) []byte
		code   ErrorCode
	}{
		{
			name: "bad checksum",
			mutate: func(data []byte) []byte {
				data[0] ^= 1
				return data
			},
			code: CodeInvalidTar,
		},
		{
			name: "one terminator block",
			mutate: func(data []byte) []byte {
				return data[:len(data)-tarBlockSize]
			},
			code: CodeInvalidTar,
		},
		{
			name: "missing terminator",
			mutate: func(data []byte) []byte {
				return data[:len(data)-2*tarBlockSize]
			},
			code: CodeInvalidTar,
		},
		{
			name: "nonzero trailing tar data",
			mutate: func(data []byte) []byte {
				trailing := make([]byte, tarBlockSize)
				trailing[0] = 1
				return append(data, trailing...)
			},
			code: CodeInvalidTar,
		},
		{
			name: "nonzero file padding",
			mutate: func(data []byte) []byte {
				for offset := 0; offset+tarBlockSize <= len(data); {
					if allZero(data[offset : offset+tarBlockSize]) {
						t.Fatal("no padded file found")
					}
					size, err := parseTarOctal(data[offset+124:offset+136], "size")
					if err != nil {
						t.Fatal(err)
					}
					dataEnd := offset + tarBlockSize + int(size)
					padding := (tarBlockSize - int(size)%tarBlockSize) % tarBlockSize
					if padding > 0 {
						data[dataEnd] = 1
						return data
					}
					offset = dataEnd + padding
				}
				t.Fatal("no padded file found")
				return nil
			},
			code: CodeInvalidTar,
		},
		{
			name: "base256 mode",
			mutate: func(data []byte) []byte {
				data[100] |= 0x80
				rewriteTarChecksum(data[:tarBlockSize])
				return data
			},
			code: CodeUnsafeMetadata,
		},
		{
			name: "legacy magic",
			mutate: func(data []byte) []byte {
				for index := 257; index < 265; index++ {
					data[index] = 0
				}
				rewriteTarChecksum(data[:tarBlockSize])
				return data
			},
			code: CodeUnsafeMetadata,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := test.mutate(append([]byte(nil), validTar...))
			source := gzipBytes(t, raw, timeZero())
			_, err := Derive(bytes.NewReader(source), formula, profileFor(source, formula, "amd64"))
			requireErrorCode(t, err, test.code)
		})
	}
}

func TestArchiveRejectsTrailingOrInvalidGzipData(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	valid := makeSourceArchive(t, baseEntries(payload))
	secondMember := gzipBytes(t, []byte("not a tar"), timeZero())
	tests := []struct {
		name   string
		source []byte
	}{
		{name: "trailing bytes", source: append(append([]byte(nil), valid...), []byte("trailing")...)},
		{name: "second member", source: append(append([]byte(nil), valid...), secondMember...)},
		{name: "truncated", source: append([]byte(nil), valid[:len(valid)-8]...)},
		{name: "bad crc", source: corruptLastByte(valid)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Derive(bytes.NewReader(test.source), formula, profileFor(test.source, formula, "amd64"))
			requireErrorCode(t, err, CodeInvalidGzip)
		})
	}
}

func TestArchiveSourceBindingsAndLimits(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	valid := makeSourceArchive(t, baseEntries(payload))
	raw := gunzipBytes(t, valid)

	t.Run("size mismatch", func(t *testing.T) {
		profile := profileFor(valid, formula, "amd64")
		profile.Source.Size--
		_, err := Derive(bytes.NewReader(valid), formula, profile)
		requireErrorCode(t, err, CodeSourceSize)
	})
	t.Run("digest mismatch", func(t *testing.T) {
		profile := profileFor(valid, formula, "amd64")
		profile.Source.SHA256 = "sha256:" + strings.Repeat("0", 64)
		_, err := Derive(bytes.NewReader(valid), formula, profile)
		requireErrorCode(t, err, CodeSourceDigest)
	})
	t.Run("compressed limit", func(t *testing.T) {
		profile := profileFor(valid, formula, "amd64")
		profile.Limits.MaxCompressedBytes = int64(len(valid))
		oversized := append(append([]byte(nil), valid...), 0)
		_, err := Derive(bytes.NewReader(oversized), formula, profile)
		requireErrorCode(t, err, CodeSourceLimit)
	})
	t.Run("expanded limit", func(t *testing.T) {
		profile := profileFor(valid, formula, "amd64")
		profile.Limits.MaxExpandedBytes = int64(len(raw) - 1)
		profile.Limits.MaxFileBytes = profile.Limits.MaxExpandedBytes
		_, err := Derive(bytes.NewReader(valid), formula, profile)
		requireErrorCode(t, err, CodeArchiveLimit)
	})
	t.Run("expansion ratio", func(t *testing.T) {
		profile := profileFor(valid, formula, "amd64")
		profile.Limits.MaxExpansionRatio = 1
		_, err := Derive(bytes.NewReader(valid), formula, profile)
		requireErrorCode(t, err, CodeArchiveLimit)
	})
	t.Run("file size", func(t *testing.T) {
		profile := profileFor(valid, formula, "amd64")
		profile.Limits.MaxFileBytes = int64(len(payload) - 1)
		_, err := Derive(bytes.NewReader(valid), formula, profile)
		requireErrorCode(t, err, CodeArchiveLimit)
	})
	t.Run("entry count", func(t *testing.T) {
		entries := append(baseEntries(payload), testArchiveEntry{name: "NOTICE", mode: 0o644, data: []byte("extra")})
		source := makeSourceArchive(t, entries)
		profile := profileFor(source, formula, "amd64")
		profile.Limits.MaxEntries = len(profile.Entries)
		_, err := Derive(bytes.NewReader(source), formula, profile)
		requireErrorCode(t, err, CodeArchiveLimit)
	})
	t.Run("tar padding", func(t *testing.T) {
		expanded := append(append([]byte(nil), raw...), make([]byte, 2*tarBlockSize)...)
		source := gzipBytes(t, expanded, timeZero())
		profile := profileFor(source, formula, "amd64")
		profile.Limits.MaxTarPaddingBytes = 2 * tarBlockSize
		_, err := Derive(bytes.NewReader(source), formula, profile)
		requireErrorCode(t, err, CodeArchiveLimit)
	})
}

func TestFormulaSourceIsExactAndBounded(t *testing.T) {
	payload := goELFFixture(t, "amd64")
	formula := fixtureFormula()
	source := makeSourceArchive(t, baseEntries(payload))

	t.Run("digest", func(t *testing.T) {
		profile := profileFor(source, formula, "amd64")
		_, err := Derive(bytes.NewReader(source), append(append([]byte(nil), formula...), '\n'), profile)
		requireErrorCode(t, err, CodeFormulaMismatch)
	})
	t.Run("size", func(t *testing.T) {
		profile := profileFor(source, formula, "amd64")
		profile.Limits.MaxFormulaBytes = int64(len(formula) - 1)
		_, err := Derive(bytes.NewReader(source), formula, profile)
		requireErrorCode(t, err, CodeFormulaMismatch)
	})
	t.Run("invalid UTF-8", func(t *testing.T) {
		invalid := []byte{0xff}
		profile := profileFor(source, invalid, "amd64")
		_, err := Derive(bytes.NewReader(source), invalid, profile)
		requireErrorCode(t, err, CodeFormulaMismatch)
	})
	t.Run("NUL", func(t *testing.T) {
		invalid := []byte("class A365 < Formula\x00end\n")
		profile := profileFor(source, invalid, "amd64")
		_, err := Derive(bytes.NewReader(source), invalid, profile)
		requireErrorCode(t, err, CodeFormulaMismatch)
	})
}

func rewriteTarChecksum(header []byte) {
	for index := 148; index < 156; index++ {
		header[index] = ' '
	}
	var sum int
	for _, value := range header {
		sum += int(value)
	}
	checksum := []byte(fmtOctal(int64(sum), 6))
	copy(header[148:154], checksum)
	header[154] = 0
	header[155] = ' '
}

func fmtOctal(value int64, width int) string {
	digits := make([]byte, width)
	for index := width - 1; index >= 0; index-- {
		digits[index] = byte('0' + value&7)
		value >>= 3
	}
	return string(digits)
}

func corruptLastByte(data []byte) []byte {
	out := append([]byte(nil), data...)
	out[len(out)-1] ^= 0xff
	return out
}

func timeZero() time.Time { return time.Unix(0, 0).UTC() }
