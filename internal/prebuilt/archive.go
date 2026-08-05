package prebuilt

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
)

const tarBlockSize = 512

type sourceScan struct {
	evidence SourceEvidence
	payload  []byte
}

func verifySource(reader io.Reader, profile Profile) (sourceScan, error) {
	compressed, err := readBounded(reader, profile.Limits.MaxCompressedBytes)
	if err != nil {
		if errors.Is(err, io.ErrShortBuffer) {
			return sourceScan{}, verificationError(CodeSourceLimit, "", "compressed archive exceeds %d bytes", profile.Limits.MaxCompressedBytes)
		}
		return sourceScan{}, verificationError(CodeInvalidGzip, "", "read compressed archive: %v", err)
	}
	if int64(len(compressed)) != profile.Source.Size {
		return sourceScan{}, verificationError(CodeSourceSize, "", "compressed size %d does not match expected size %d", len(compressed), profile.Source.Size)
	}
	compressedDigest := digestBytes(compressed)
	if compressedDigest != profile.Source.SHA256 {
		return sourceScan{}, verificationError(CodeSourceDigest, "", "compressed digest %s does not match expected digest %s", compressedDigest, profile.Source.SHA256)
	}

	compressedReader := bytes.NewReader(compressed)
	gzipReader, err := gzip.NewReader(compressedReader)
	if err != nil {
		return sourceScan{}, verificationError(CodeInvalidGzip, "", "open gzip stream: %v", err)
	}
	gzipReader.Multistream(false)
	var expanded bytes.Buffer
	limited := &io.LimitedReader{R: gzipReader, N: profile.Limits.MaxExpandedBytes + 1}
	if _, err := io.Copy(&expanded, limited); err != nil {
		_ = gzipReader.Close()
		return sourceScan{}, verificationError(CodeInvalidGzip, "", "decompress archive: %v", err)
	}
	if int64(expanded.Len()) > profile.Limits.MaxExpandedBytes {
		_ = gzipReader.Close()
		return sourceScan{}, verificationError(CodeArchiveLimit, "", "expanded archive exceeds %d bytes", profile.Limits.MaxExpandedBytes)
	}
	if err := gzipReader.Close(); err != nil {
		return sourceScan{}, verificationError(CodeInvalidGzip, "", "close gzip stream: %v", err)
	}
	if compressedReader.Len() != 0 {
		return sourceScan{}, verificationError(CodeInvalidGzip, "", "trailing data or an additional gzip member is forbidden")
	}
	inventory, payload, err := scanUSTAR(expanded.Bytes(), profile)
	if err != nil {
		return sourceScan{}, err
	}
	if exceedsRatio(int64(expanded.Len()), int64(len(compressed)), profile.Limits.MaxExpansionRatio) {
		return sourceScan{}, verificationError(CodeArchiveLimit, "", "expansion ratio exceeds %d:1", profile.Limits.MaxExpansionRatio)
	}
	inventoryDigest, err := digestInventory(inventory)
	if err != nil {
		return sourceScan{}, verificationError(CodeInvalidTar, "", "canonicalize source inventory: %v", err)
	}
	return sourceScan{
		evidence: SourceEvidence{
			SHA256:          compressedDigest,
			Size:            int64(len(compressed)),
			ExpandedSHA256:  digestBytes(expanded.Bytes()),
			ExpandedSize:    int64(expanded.Len()),
			InventorySHA256: inventoryDigest,
			Inventory:       inventory,
			PayloadPath:     profile.PayloadPath,
			PayloadSHA256:   digestBytes(payload),
			PayloadSize:     int64(len(payload)),
		},
		payload: payload,
	}, nil
}

func scanUSTAR(data []byte, profile Profile) ([]InventoryEntry, []byte, error) {
	if len(data)%tarBlockSize != 0 {
		return nil, nil, verificationError(CodeInvalidTar, "", "expanded tar size %d is not a multiple of %d", len(data), tarBlockSize)
	}
	expected := make(map[string]EntryProfile, len(profile.Entries))
	for _, entry := range profile.Entries {
		expected[entry.Path] = entry
	}
	seen := make(map[string]struct{}, len(profile.Entries))
	inventory := make([]InventoryEntry, 0, len(profile.Entries))
	var payload []byte

	for offset := 0; ; {
		if offset+tarBlockSize > len(data) {
			return nil, nil, verificationError(CodeInvalidTar, "", "tar archive is missing two zero terminator blocks")
		}
		headerBlock := data[offset : offset+tarBlockSize]
		if allZero(headerBlock) {
			if offset+2*tarBlockSize > len(data) || !allZero(data[offset+tarBlockSize:offset+2*tarBlockSize]) {
				return nil, nil, verificationError(CodeInvalidTar, "", "tar archive has only one zero terminator block")
			}
			trailing := data[offset:]
			if int64(len(trailing)) > profile.Limits.MaxTarPaddingBytes {
				return nil, nil, verificationError(CodeArchiveLimit, "", "tar terminator and padding exceed %d bytes", profile.Limits.MaxTarPaddingBytes)
			}
			if !allZero(trailing) {
				return nil, nil, verificationError(CodeInvalidTar, "", "non-zero data follows tar terminator")
			}
			break
		}
		if len(inventory) >= profile.Limits.MaxEntries {
			return nil, nil, verificationError(CodeArchiveLimit, "", "archive exceeds %d entries", profile.Limits.MaxEntries)
		}

		header, err := parseUSTARHeader(headerBlock)
		if err != nil {
			return nil, nil, err
		}
		if err := validatePortablePath(header.name, profile.Limits.MaxPathBytes, profile.Limits.MaxDepth); err != nil {
			return nil, nil, verificationError(CodeUnsafePath, header.name, "%v", err)
		}
		if _, duplicate := seen[header.name]; duplicate {
			return nil, nil, verificationError(CodeDuplicatePath, header.name, "archive path appears more than once")
		}
		entryProfile, ok := expected[header.name]
		if !ok {
			return nil, nil, verificationError(CodeEntryMismatch, header.name, "archive contains an undeclared entry")
		}
		if header.mode != entryProfile.Mode {
			return nil, nil, verificationError(CodeEntryMismatch, header.name, "mode %#o does not match required mode %#o", header.mode, entryProfile.Mode)
		}
		if header.size > profile.Limits.MaxFileBytes {
			return nil, nil, verificationError(CodeArchiveLimit, header.name, "file size %d exceeds %d", header.size, profile.Limits.MaxFileBytes)
		}
		dataStart := offset + tarBlockSize
		if header.size > int64(len(data)-dataStart) {
			return nil, nil, verificationError(CodeInvalidTar, header.name, "file body extends beyond archive")
		}
		dataEnd := dataStart + int(header.size)
		padding := (tarBlockSize - int(header.size)%tarBlockSize) % tarBlockSize
		paddedEnd := dataEnd + padding
		if paddedEnd > len(data) {
			return nil, nil, verificationError(CodeInvalidTar, header.name, "file padding extends beyond archive")
		}
		if !allZero(data[dataEnd:paddedEnd]) {
			return nil, nil, verificationError(CodeInvalidTar, header.name, "file padding contains non-zero bytes")
		}
		contents := data[dataStart:dataEnd]
		entry := InventoryEntry{
			Path:   header.name,
			Mode:   header.mode,
			Size:   header.size,
			SHA256: digestBytes(contents),
		}
		inventory = append(inventory, entry)
		seen[header.name] = struct{}{}
		if header.name == profile.PayloadPath {
			if len(contents) == 0 {
				return nil, nil, verificationError(CodeEntryMismatch, header.name, "payload is empty")
			}
			payload = append([]byte(nil), contents...)
		}
		offset = paddedEnd
	}

	missing := make([]string, 0)
	for _, entry := range profile.Entries {
		if _, ok := seen[entry.Path]; !ok {
			missing = append(missing, entry.Path)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, nil, verificationError(CodeEntryMismatch, missing[0], "archive is missing required entries: %s", strings.Join(missing, ", "))
	}
	if payload == nil {
		return nil, nil, verificationError(CodeEntryMismatch, profile.PayloadPath, "archive is missing the payload")
	}
	slices.SortFunc(inventory, func(a, b InventoryEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	return inventory, payload, nil
}

type ustarHeader struct {
	name string
	mode uint32
	size int64
}

func parseUSTARHeader(block []byte) (ustarHeader, error) {
	if len(block) != tarBlockSize {
		return ustarHeader{}, verificationError(CodeInvalidTar, "", "physical tar header is not %d bytes", tarBlockSize)
	}
	storedChecksum, err := parseTarOctal(block[148:156], "checksum")
	if err != nil {
		return ustarHeader{}, verificationError(CodeInvalidTar, "", "%v", err)
	}
	var checksum int64
	for index, value := range block {
		if index >= 148 && index < 156 {
			checksum += int64(' ')
		} else {
			checksum += int64(value)
		}
	}
	if storedChecksum != checksum {
		return ustarHeader{}, verificationError(CodeInvalidTar, "", "tar checksum %d does not match computed checksum %d", storedChecksum, checksum)
	}
	if !bytes.Equal(block[257:263], []byte{'u', 's', 't', 'a', 'r', 0}) || !bytes.Equal(block[263:265], []byte{'0', '0'}) {
		return ustarHeader{}, verificationError(CodeUnsafeMetadata, "", "only canonical USTAR headers are supported; GNU, PAX, and legacy headers are forbidden")
	}
	typeflag := block[156]
	if typeflag != 0 && typeflag != '0' {
		code := CodeUnsafeType
		if typeflag == 'x' || typeflag == 'g' || typeflag == 'L' || typeflag == 'K' {
			code = CodeUnsafeMetadata
		}
		return ustarHeader{}, verificationError(code, "", "archive entry type %#x is forbidden; only regular files are supported", typeflag)
	}
	linkName, err := parseTarString(block[157:257], "link name")
	if err != nil {
		return ustarHeader{}, verificationError(CodeUnsafeMetadata, "", "%v", err)
	}
	if linkName != "" {
		return ustarHeader{}, verificationError(CodeUnsafeType, "", "regular file has a link target")
	}
	name, err := parseTarString(block[0:100], "name")
	if err != nil {
		return ustarHeader{}, verificationError(CodeUnsafePath, "", "%v", err)
	}
	prefix, err := parseTarString(block[345:500], "prefix")
	if err != nil {
		return ustarHeader{}, verificationError(CodeUnsafePath, name, "%v", err)
	}
	if prefix != "" {
		name = prefix + "/" + name
	}
	if name == "" {
		return ustarHeader{}, verificationError(CodeUnsafePath, "", "empty archive path")
	}
	modeValue, err := parseTarOctal(block[100:108], "mode")
	if err != nil || modeValue <= 0 || modeValue > 0o777 {
		if err == nil {
			err = fmt.Errorf("mode %#o contains unsupported permission bits", modeValue)
		}
		return ustarHeader{}, verificationError(CodeUnsafeMetadata, name, "%v", err)
	}
	for label, field := range map[string][]byte{
		"uid":          block[108:116],
		"gid":          block[116:124],
		"mtime":        block[136:148],
		"device major": block[329:337],
		"device minor": block[337:345],
	} {
		value, err := parseTarOctal(field, label)
		if err != nil {
			return ustarHeader{}, verificationError(CodeUnsafeMetadata, name, "%v", err)
		}
		if (label == "device major" || label == "device minor") && value != 0 {
			return ustarHeader{}, verificationError(CodeUnsafeMetadata, name, "%s must be zero for a regular file", label)
		}
	}
	for label, field := range map[string][]byte{
		"user name":  block[265:297],
		"group name": block[297:329],
	} {
		value, err := parseTarString(field, label)
		if err != nil {
			return ustarHeader{}, verificationError(CodeUnsafeMetadata, name, "%v", err)
		}
		for _, r := range value {
			if r > unicode.MaxASCII || unicode.IsControl(r) {
				return ustarHeader{}, verificationError(CodeUnsafeMetadata, name, "%s contains non-portable characters", label)
			}
		}
	}
	size, err := parseTarOctal(block[124:136], "size")
	if err != nil {
		return ustarHeader{}, verificationError(CodeInvalidTar, name, "%v", err)
	}
	return ustarHeader{name: name, mode: uint32(modeValue), size: size}, nil
}

func parseTarString(field []byte, label string) (string, error) {
	end := bytes.IndexByte(field, 0)
	if end < 0 {
		end = len(field)
	} else if !allZero(field[end:]) {
		return "", fmt.Errorf("%s contains data after NUL terminator", label)
	}
	value := string(field[:end])
	for _, r := range value {
		if r > unicode.MaxASCII || unicode.IsControl(r) {
			return "", fmt.Errorf("%s contains non-portable characters", label)
		}
	}
	return value, nil
}

func parseTarOctal(field []byte, label string) (int64, error) {
	if len(field) == 0 {
		return 0, fmt.Errorf("empty %s field", label)
	}
	if field[0]&0x80 != 0 {
		return 0, fmt.Errorf("base-256 %s encoding is forbidden", label)
	}
	trimmed := bytes.Trim(field, " \x00")
	if len(trimmed) == 0 {
		return 0, nil
	}
	var value int64
	for _, digit := range trimmed {
		if digit < '0' || digit > '7' {
			return 0, fmt.Errorf("invalid octal %s field", label)
		}
		if value > (int64(^uint64(0)>>1)-int64(digit-'0'))/8 {
			return 0, fmt.Errorf("overflowing octal %s field", label)
		}
		value = value*8 + int64(digit-'0')
	}
	return value, nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}
