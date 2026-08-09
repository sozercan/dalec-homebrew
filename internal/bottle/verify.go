package bottle

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const tarBlockSize = 512

var safeFormulaName = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.@-]*$`)

var relocationPlaceholders = [][]byte{
	[]byte("@@HOMEBREW_PREFIX@@"),
	[]byte("@@HOMEBREW_CELLAR@@"),
	[]byte("@@HOMEBREW_REPOSITORY@@"),
	[]byte("@@HOMEBREW_LIBRARY@@"),
	[]byte("@@HOMEBREW_PERL@@"),
	[]byte("@@HOMEBREW_JAVA@@"),
}

var allowedPAXKeys = map[string]struct{}{
	"path": {}, "linkpath": {}, "size": {}, "uid": {}, "gid": {},
	"uname": {}, "gname": {}, "mtime": {}, "atime": {}, "ctime": {},
}

type scannedEntry struct {
	inventory InventoryEntry
	formula   []byte
	receipt   []byte
}

type placeholderDetector struct {
	tail  []byte
	found bool
}

func (d *placeholderDetector) Write(p []byte) (int, error) {
	if d.found {
		return len(p), nil
	}
	combined := make([]byte, 0, len(d.tail)+len(p))
	combined = append(combined, d.tail...)
	combined = append(combined, p...)
	for _, placeholder := range relocationPlaceholders {
		if bytes.Contains(combined, placeholder) {
			d.found = true
			break
		}
	}
	keep := maxRelocationPlaceholderBytes() - 1
	if keep > len(combined) {
		keep = len(combined)
	}
	d.tail = append(d.tail[:0], combined[len(combined)-keep:]...)
	return len(p), nil
}

func maxRelocationPlaceholderBytes() int {
	longest := 0
	for _, placeholder := range relocationPlaceholders {
		if len(placeholder) > longest {
			longest = len(placeholder)
		}
	}
	return longest
}

func containsRelocationPlaceholder(value []byte) bool {
	for _, placeholder := range relocationPlaceholders {
		if bytes.Contains(value, placeholder) {
			return true
		}
	}
	return false
}

type countingTailReader struct {
	r     io.Reader
	count int64
	tail  []byte
	pos   int
	full  bool
}

func newCountingTailReader(r io.Reader, tailBytes int) *countingTailReader {
	return &countingTailReader{r: r, tail: make([]byte, tailBytes)}
}

func (r *countingTailReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	for _, b := range p[:n] {
		r.tail[r.pos] = b
		r.pos++
		if r.pos == len(r.tail) {
			r.pos = 0
			r.full = true
		}
	}
	r.count += int64(n)
	return n, err
}

func (r *countingTailReader) last(n int) ([]byte, bool) {
	if n <= 0 || n > len(r.tail) || int64(n) > r.count {
		return nil, false
	}
	out := make([]byte, n)
	start := r.pos - n
	if start < 0 {
		start += len(r.tail)
	}
	for i := range out {
		out[i] = r.tail[(start+i)%len(r.tail)]
	}
	return out, true
}

func (r *countingTailReader) tailAllZero() bool {
	if !r.full {
		return false
	}
	for _, b := range r.tail {
		if b != 0 {
			return false
		}
	}
	return true
}

// Verify authenticates the fetched compressed bytes, validates every gzip/tar
// structure, and returns a sorted inventory. The reader is spooled to a private
// temporary file so no archive parsing occurs until both authenticated hashes
// and the exact compressed size have passed.
func Verify(r io.Reader, expected Expectation, opts Options) (*Result, error) {
	return verifyWithReceiptValidator(r, expected, opts, validateReceipt)
}

type receiptValidator func([]byte, Expectation) (ReceiptEvidence, error)

func verifyWithReceiptValidator(r io.Reader, expected Expectation, opts Options, validate receiptValidator) (result *Result, retErr error) {
	if r == nil {
		return nil, verificationError(CodeInvalidExpectation, "", "nil bottle reader")
	}
	limits := opts.Limits.withDefaults()
	if err := validateLimits(limits); err != nil {
		return nil, verificationError(CodeInvalidExpectation, "", "invalid limits: %v", err)
	}
	normalized, err := normalizeExpectation(expected, limits)
	if err != nil {
		return nil, verificationError(CodeInvalidExpectation, "", "%v", err)
	}
	expected = normalized

	spool, err := os.CreateTemp("", "dalec-homebrew-bottle-*.tar.gz")
	if err != nil {
		return nil, verificationError(CodeInvalidGzip, "", "create private spool: %v", err)
	}
	spoolName := spool.Name()
	defer func() {
		closeErr := spool.Close()
		removeErr := os.Remove(spoolName)
		if retErr == nil && closeErr != nil {
			result = nil
			retErr = verificationError(CodeInvalidGzip, "", "close private spool: %v", closeErr)
		}
		if retErr == nil && removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = nil
			retErr = verificationError(CodeInvalidGzip, "", "remove private spool: %v", removeErr)
		}
	}()
	if err := spool.Chmod(0o600); err != nil {
		return nil, verificationError(CodeInvalidGzip, "", "secure private spool: %v", err)
	}

	compressedHash := sha256.New()
	limited := &io.LimitedReader{R: r, N: limits.MaxCompressedBytes + 1}
	n, err := io.Copy(io.MultiWriter(spool, compressedHash), limited)
	if err != nil {
		return nil, verificationError(CodeInvalidGzip, "", "read compressed bottle: %v", err)
	}
	if n > limits.MaxCompressedBytes {
		return nil, verificationError(CodeCompressedLimit, "", "compressed bottle exceeds %d bytes", limits.MaxCompressedBytes)
	}
	if n != expected.CompressedSize {
		return nil, verificationError(CodeSizeMismatch, "", "compressed size %d does not match authenticated size %d", n, expected.CompressedSize)
	}
	actualHex := hex.EncodeToString(compressedHash.Sum(nil))
	actualDigest := "sha256:" + actualHex
	if actualDigest != expected.CompressedSHA256 {
		return nil, verificationError(CodeDigestMismatch, "", "compressed digest %s does not match authenticated OCI digest %s", actualDigest, expected.CompressedSHA256)
	}
	if actualHex != expected.HomebrewSHA256 {
		return nil, verificationError(CodeHomebrewMismatch, "", "compressed digest %s does not match authenticated Homebrew checksum %s", actualHex, expected.HomebrewSHA256)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, verificationError(CodeInvalidGzip, "", "rewind private spool: %v", err)
	}

	inventory, expanded, formula, receipt, err := scanArchive(spool, expected, limits)
	if err != nil {
		return nil, err
	}
	if formula == nil {
		return nil, verificationError(CodeMissingFormula, expectedFormulaPath(expected), "required embedded Formula source is absent")
	}
	formulaClass, err := validateFormulaSource(expected.Name, formula.formula)
	if err != nil {
		return nil, verificationError(CodeInvalidFormula, formula.inventory.Path, "%v", err)
	}
	formulaEvidence := FormulaEvidence{
		Path:      formula.inventory.Path,
		ClassName: formulaClass,
		SHA256:    formula.inventory.SHA256,
		Size:      formula.inventory.Size,
	}

	var receiptEvidence *ReceiptEvidence
	if receipt != nil {
		ev, err := validate(receipt.receipt, expected)
		if err != nil {
			return nil, verificationError(CodeInvalidReceipt, receipt.inventory.Path, "%v", err)
		}
		ev.Path = receipt.inventory.Path
		ev.SHA256 = receipt.inventory.SHA256
		ev.Size = receipt.inventory.Size
		receiptEvidence = &ev
	} else if opts.Policy.RequirePreInstallReceipt {
		return nil, verificationError(CodeMissingReceipt, expectedReceiptPath(expected), "policy requires a pre-install receipt")
	}

	inventoryDigest, err := digestInventory(inventory)
	if err != nil {
		return nil, verificationError(CodeInvalidTar, "", "canonicalize inventory: %v", err)
	}
	return &Result{
		Name:             expected.Name,
		PkgVersion:       expected.PkgVersion,
		KegPrefix:        expectedKegPrefix(expected),
		CompressedSHA256: actualDigest,
		CompressedSize:   n,
		HomebrewSHA256:   actualHex,
		ExpandedSize:     expanded,
		InventorySHA256:  inventoryDigest,
		Inventory:        inventory,
		Formula:          formulaEvidence,
		Receipt:          receiptEvidence,
		FormulaSource:    bytes.Clone(formula.formula),
	}, nil
}

var errGzipMetadataLimit = errors.New("gzip metadata exceeds configured limit")

func boundedGzipStream(r io.Reader, limit int64) (io.Reader, error) {
	var prefix bytes.Buffer
	read := func(n int) ([]byte, error) {
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		_, _ = prefix.Write(data)
		return data, nil
	}
	fixed, err := read(10)
	if err != nil {
		return nil, err
	}
	if fixed[0] != 0x1f || fixed[1] != 0x8b || fixed[2] != 8 {
		return nil, fmt.Errorf("invalid gzip fixed header")
	}
	flags := fixed[3]
	if flags&0xe0 != 0 {
		return nil, fmt.Errorf("gzip reserved flags are set")
	}
	metadata := int64(0)
	consume := func(n int) error {
		if n < 0 || metadata > limit-int64(n) {
			return errGzipMetadataLimit
		}
		metadata += int64(n)
		_, err := read(n)
		return err
	}
	if flags&0x04 != 0 {
		lengthBytes, err := read(2)
		if err != nil {
			return nil, err
		}
		length := int(binary.LittleEndian.Uint16(lengthBytes))
		if err := consume(length); err != nil {
			return nil, err
		}
	}
	consumeCString := func() error {
		for {
			if metadata >= limit {
				return errGzipMetadataLimit
			}
			value, err := read(1)
			if err != nil {
				return err
			}
			metadata++
			if value[0] == 0 {
				return nil
			}
		}
	}
	if flags&0x08 != 0 {
		if err := consumeCString(); err != nil {
			return nil, err
		}
	}
	if flags&0x10 != 0 {
		if err := consumeCString(); err != nil {
			return nil, err
		}
	}
	if flags&0x02 != 0 {
		if err := consume(2); err != nil {
			return nil, err
		}
	}
	return io.MultiReader(bytes.NewReader(prefix.Bytes()), r), nil
}

func scanArchive(r io.Reader, expected Expectation, limits Limits) ([]InventoryEntry, int64, *scannedEntry, *scannedEntry, error) {
	replayed, err := boundedGzipStream(r, limits.MaxMetadataBytes)
	if err != nil {
		code := CodeInvalidGzip
		if errors.Is(err, errGzipMetadataLimit) {
			code = CodeArchiveLimit
		}
		return nil, 0, nil, nil, verificationError(code, "", "validate gzip header: %v", err)
	}
	buffered := bufio.NewReader(replayed)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return nil, 0, nil, nil, verificationError(CodeInvalidGzip, "", "open gzip stream: %v", err)
	}
	gz.Multistream(false)
	gzipMetadata := int64(len(gz.Name) + len(gz.Comment) + len(gz.Extra))
	if gzipMetadata > limits.MaxMetadataBytes {
		return nil, 0, nil, nil, verificationError(CodeArchiveLimit, "", "gzip metadata exceeds %d bytes", limits.MaxMetadataBytes)
	}

	tarBudget := &io.LimitedReader{R: gz, N: limits.MaxExpandedBytes + 1}
	tarBytes := newCountingTailReader(tarBudget, 2*tarBlockSize)
	tr := tar.NewReader(tarBytes)
	entries := make(map[string]*scannedEntry)
	ordered := make([]*scannedEntry, 0)
	var expanded int64
	var formula, receipt *scannedEntry
	var finalNextBytes int64
	for {
		beforeNext := tarBytes.count
		hdr, err := tr.Next()
		if tarBytes.count > limits.MaxExpandedBytes {
			return nil, 0, nil, nil, verificationError(CodeArchiveLimit, "", "total expanded tar bytes exceed %d", limits.MaxExpandedBytes)
		}
		if errors.Is(err, io.EOF) {
			finalNextBytes = tarBytes.count - beforeNext
			break
		}
		if err != nil {
			return nil, 0, nil, nil, verificationError(CodeInvalidTar, "", "read tar header: %v", err)
		}
		if len(ordered) >= limits.MaxFiles {
			return nil, 0, nil, nil, verificationError(CodeArchiveLimit, hdr.Name, "archive exceeds %d entries", limits.MaxFiles)
		}
		rawHeader, ok := tarBytes.last(tarBlockSize)
		if !ok {
			return nil, 0, nil, nil, verificationError(CodeInvalidTar, hdr.Name, "missing physical tar header")
		}
		entry, err := validateHeader(hdr, rawHeader, expected, limits)
		if err != nil {
			return nil, 0, nil, nil, err
		}
		if previous, ok := entries[entry.Path]; ok {
			return nil, 0, nil, nil, verificationError(CodePathCollision, entry.Path, "duplicates prior %s entry", previous.inventory.Type)
		}

		scanned := &scannedEntry{inventory: entry}
		if entry.Type == EntryRegular {
			if hdr.Size > limits.MaxFileBytes {
				return nil, 0, nil, nil, verificationError(CodeArchiveLimit, entry.Path, "file size %d exceeds %d", hdr.Size, limits.MaxFileBytes)
			}
			if expanded > limits.MaxExpandedBytes-hdr.Size {
				return nil, 0, nil, nil, verificationError(CodeArchiveLimit, entry.Path, "expanded bytes exceed %d", limits.MaxExpandedBytes)
			}
			expanded += hdr.Size

			h := sha256.New()
			detector := &placeholderDetector{}
			var capture *bytes.Buffer
			switch entry.Path {
			case expectedFormulaPath(expected):
				if hdr.Size > limits.MaxFormulaBytes {
					return nil, 0, nil, nil, verificationError(CodeArchiveLimit, entry.Path, "Formula source exceeds %d bytes", limits.MaxFormulaBytes)
				}
				capture = bytes.NewBuffer(make([]byte, 0, hdr.Size))
			case expectedReceiptPath(expected):
				if hdr.Size > limits.MaxReceiptBytes {
					return nil, 0, nil, nil, verificationError(CodeArchiveLimit, entry.Path, "receipt exceeds %d bytes", limits.MaxReceiptBytes)
				}
				capture = bytes.NewBuffer(make([]byte, 0, hdr.Size))
			}
			writer := io.Writer(io.MultiWriter(h, detector))
			if capture != nil {
				writer = io.MultiWriter(h, detector, capture)
			}
			copied, err := io.Copy(writer, tr)
			if tarBytes.count > limits.MaxExpandedBytes {
				return nil, 0, nil, nil, verificationError(CodeArchiveLimit, entry.Path, "total expanded tar bytes exceed %d", limits.MaxExpandedBytes)
			}
			if err != nil {
				return nil, 0, nil, nil, verificationError(CodeInvalidTar, entry.Path, "read file payload: %v", err)
			}
			if copied != hdr.Size {
				return nil, 0, nil, nil, verificationError(CodeInvalidTar, entry.Path, "read %d payload bytes, expected %d", copied, hdr.Size)
			}
			scanned.inventory.SHA256 = "sha256:" + hex.EncodeToString(h.Sum(nil))
			scanned.inventory.Relocatable = detector.found
			if capture != nil {
				if entry.Path == expectedFormulaPath(expected) {
					scanned.formula = capture.Bytes()
					formula = scanned
				} else {
					scanned.receipt = capture.Bytes()
					receipt = scanned
				}
			}
		}
		entries[entry.Path] = scanned
		ordered = append(ordered, scanned)
	}

	if tarBytes.count > limits.MaxExpandedBytes {
		return nil, 0, nil, nil, verificationError(CodeArchiveLimit, "", "total expanded tar bytes exceed %d", limits.MaxExpandedBytes)
	}
	expanded = tarBytes.count
	if finalNextBytes < 2*tarBlockSize || finalNextBytes > 3*tarBlockSize-1 || !tarBytes.tailAllZero() {
		return nil, 0, nil, nil, verificationError(CodeInvalidTar, "", "tar archive is missing its two zero end-marker blocks")
	}

	// Reading through gzip EOF validates its CRC and uncompressed size. POSIX
	// permits zero padding after the two end-marker blocks (GNU tar normally
	// pads to a record boundary), but non-zero trailing payload is ambiguous.
	var paddingBytes int64
	padding := make([]byte, 32<<10)
	for {
		n, trailingErr := gz.Read(padding)
		if n > 0 {
			if paddingBytes > limits.MaxTarPaddingBytes-int64(n) {
				return nil, 0, nil, nil, verificationError(CodeArchiveLimit, "", "tar end padding exceeds %d bytes", limits.MaxTarPaddingBytes)
			}
			paddingBytes += int64(n)
			for _, b := range padding[:n] {
				if b != 0 {
					return nil, 0, nil, nil, verificationError(CodeInvalidTar, "", "gzip member contains non-zero data after tar end marker")
				}
			}
		}
		if errors.Is(trailingErr, io.EOF) {
			break
		}
		if trailingErr != nil {
			return nil, 0, nil, nil, verificationError(CodeInvalidGzip, "", "validate gzip trailer: %v", trailingErr)
		}
	}
	if err := gz.Close(); err != nil {
		return nil, 0, nil, nil, verificationError(CodeInvalidGzip, "", "close gzip stream: %v", err)
	}
	if _, err := buffered.ReadByte(); err == nil {
		return nil, 0, nil, nil, verificationError(CodeInvalidGzip, "", "concatenated gzip members or trailing compressed data are unsupported")
	} else if !errors.Is(err, io.EOF) {
		return nil, 0, nil, nil, verificationError(CodeInvalidGzip, "", "inspect gzip trailing data: %v", err)
	}

	if err := validateCollisionsAndHardlinks(entries, expected, limits); err != nil {
		return nil, 0, nil, nil, err
	}
	inventory := make([]InventoryEntry, 0, len(ordered))
	for _, entry := range ordered {
		inventory = append(inventory, entry.inventory)
	}
	slices.SortFunc(inventory, func(a, b InventoryEntry) int { return strings.Compare(a.Path, b.Path) })
	return inventory, expanded, formula, receipt, nil
}

func supportedTarFormat(hdr *tar.Header, rawHeader []byte) bool {
	switch hdr.Format {
	case tar.FormatUSTAR, tar.FormatPAX, tar.FormatGNU:
		return true
	case tar.FormatUnknown:
		return validPAXUTF8PhysicalName(hdr, rawHeader)
	default:
		return false
	}
}

// validPAXUTF8PhysicalName recognizes the narrow libarchive/Homebrew encoding
// where a valid PAX path is also copied verbatim into the physical USTAR name
// field. archive/tar parses the entry but reports FormatUnknown because USTAR
// forbids non-ASCII header bytes. Require the raw header to be otherwise strict
// USTAR so FormatUnknown cannot mask malformed numeric or metadata fields.
func validPAXUTF8PhysicalName(hdr *tar.Header, rawHeader []byte) bool {
	if hdr == nil || len(rawHeader) != tarBlockSize {
		return false
	}
	paxPath, ok := hdr.PAXRecords["path"]
	if !ok || paxPath == "" || paxPath != hdr.Name || !utf8.ValidString(paxPath) {
		return false
	}
	if !bytes.Equal(rawHeader[257:263], []byte("ustar\x00")) || !bytes.Equal(rawHeader[263:265], []byte("00")) {
		return false
	}
	// archive/tar accepts legacy signed checksums. This exception requires the
	// canonical unsigned checksum so high-bit UTF-8 bytes cannot select the
	// more permissive interpretation.
	if !canonicalUnsignedTarChecksum(rawHeader) || rawHeader[156] != hdr.Typeflag {
		return false
	}
	// archive/tar joins a physical USTAR prefix before PAX replaces hdr.Name.
	// Require the unused prefix field to be empty so no hidden alternate path
	// survives in the physical header.
	for _, b := range rawHeader[345:500] {
		if b != 0 {
			return false
		}
	}
	for _, field := range [][2]int{
		{100, 108}, {108, 116}, {116, 124}, {124, 136},
		{136, 148}, {329, 337}, {337, 345},
	} {
		if rawHeader[field[1]-1] != 0 {
			return false
		}
	}

	nameField := rawHeader[:100]
	nul := bytes.IndexByte(nameField, 0)
	rawName := nameField
	if nul >= 0 {
		for _, b := range nameField[nul:] {
			if b != 0 {
				return false
			}
		}
		rawName = nameField[:nul]
	}
	if !bytes.Equal(rawName, []byte(paxPath)) {
		return false
	}
	hasNonASCII := false
	for i, b := range rawHeader {
		if b < utf8.RuneSelf {
			continue
		}
		if i >= len(rawName) {
			return false
		}
		hasNonASCII = true
	}
	return hasNonASCII
}

func canonicalUnsignedTarChecksum(rawHeader []byte) bool {
	if len(rawHeader) != tarBlockSize {
		return false
	}
	field := rawHeader[148:156]
	if field[6] != 0 || field[7] != ' ' {
		return false
	}
	var recorded int64
	for _, b := range field[:6] {
		if b < '0' || b > '7' {
			return false
		}
		recorded = recorded*8 + int64(b-'0')
	}

	var computed int64
	for i, b := range rawHeader {
		if i >= 148 && i < 156 {
			computed += int64(' ')
		} else {
			computed += int64(b)
		}
	}
	return recorded == computed
}

func validateHeader(hdr *tar.Header, rawHeader []byte, expected Expectation, limits Limits) (InventoryEntry, error) {
	if hdr == nil {
		return InventoryEntry{}, verificationError(CodeInvalidTar, "", "nil tar header")
	}
	if !supportedTarFormat(hdr, rawHeader) {
		return InventoryEntry{}, verificationError(CodeInvalidTar, hdr.Name, "unsupported tar format %v", hdr.Format)
	}
	if hdr.Uid < 0 || hdr.Gid < 0 {
		return InventoryEntry{}, verificationError(CodeUnsafeMetadata, hdr.Name, "negative uid/gid")
	}
	if !utf8.ValidString(hdr.Uname) || !utf8.ValidString(hdr.Gname) || containsControl(hdr.Uname) || containsControl(hdr.Gname) {
		return InventoryEntry{}, verificationError(CodeUnsafeMetadata, hdr.Name, "invalid user/group metadata")
	}
	entryType, err := classifyType(hdr.Typeflag)
	if err != nil {
		return InventoryEntry{}, verificationError(CodeUnsafeType, hdr.Name, "%v", err)
	}
	name, err := canonicalArchivePath(hdr.Name, entryType == EntryDirectory, limits.MaxPathBytes)
	if err != nil {
		return InventoryEntry{}, verificationError(CodeUnsafePath, hdr.Name, "%v", err)
	}
	if depth := strings.Count(name, "/") + 1; depth > limits.MaxDepth {
		return InventoryEntry{}, verificationError(CodeArchiveLimit, name, "path depth %d exceeds %d", depth, limits.MaxDepth)
	}
	if err := validateKegPath(name, entryType, expected); err != nil {
		return InventoryEntry{}, verificationError(CodeUnsafePath, name, "%v", err)
	}
	if hdr.Mode < 0 || hdr.Mode&0o6000 != 0 || hdr.Mode&^int64(0o7777) != 0 {
		return InventoryEntry{}, verificationError(CodeUnsafeMode, name, "setuid/setgid or invalid mode %#o", hdr.Mode)
	}
	if hdr.Size < 0 {
		return InventoryEntry{}, verificationError(CodeInvalidTar, name, "negative file size")
	}
	if entryType != EntryRegular && hdr.Size != 0 {
		return InventoryEntry{}, verificationError(CodeInvalidTar, name, "%s entry has non-zero size %d", entryType, hdr.Size)
	}
	if err := validatePAX(hdr, limits); err != nil {
		return InventoryEntry{}, verificationError(CodeUnsafeMetadata, name, "%v", err)
	}
	xattrs, err := allowedXattrs(hdr, limits)
	if err != nil {
		return InventoryEntry{}, verificationError(CodeUnsafeMetadata, name, "%v", err)
	}

	entry := InventoryEntry{
		Path:    name,
		KegPath: strings.TrimPrefix(strings.TrimPrefix(name, expectedKegPrefix(expected)), "/"),
		Type:    entryType,
		Mode:    uint32(hdr.Mode & 0o7777),
		Size:    hdr.Size,
		UID:     hdr.Uid,
		GID:     hdr.Gid,
		Xattrs:  xattrs,
	}
	switch entryType {
	case EntrySymlink:
		resolved, prefixTarget, err := validateSymlink(name, hdr.Linkname, expected, limits)
		if err != nil {
			return InventoryEntry{}, err
		}
		entry.SymlinkTarget = hdr.Linkname
		entry.ResolvedTarget = resolved
		entry.PrefixTarget = prefixTarget
		entry.Relocatable = containsRelocationPlaceholder([]byte(hdr.Linkname))
	case EntryHardlink:
		target, err := validateHardlink(hdr.Linkname, expected, limits)
		if err != nil {
			return InventoryEntry{}, verificationError(CodeUnsafeLink, name, "%v", err)
		}
		entry.HardlinkTarget = target
		entry.ResolvedTarget = target
	default:
		if hdr.Linkname != "" {
			return InventoryEntry{}, verificationError(CodeUnsafeLink, name, "%s entry has unexpected link target", entryType)
		}
	}
	return entry, nil
}

func classifyType(typeflag byte) (EntryType, error) {
	switch typeflag {
	case tar.TypeReg, tar.TypeRegA:
		return EntryRegular, nil
	case tar.TypeDir:
		return EntryDirectory, nil
	case tar.TypeSymlink:
		return EntrySymlink, nil
	case tar.TypeLink:
		return EntryHardlink, nil
	case tar.TypeChar, tar.TypeBlock:
		return "", fmt.Errorf("device nodes are forbidden")
	case tar.TypeFifo:
		return "", fmt.Errorf("FIFOs are forbidden")
	case tar.TypeGNUSparse:
		return "", fmt.Errorf("sparse files are forbidden")
	case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
		return "", fmt.Errorf("standalone archive metadata entry is forbidden")
	default:
		return "", fmt.Errorf("unsupported entry type %#x (including sockets)", typeflag)
	}
}

func canonicalArchivePath(raw string, directory bool, maxBytes int) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty path")
	}
	if len(raw) > maxBytes {
		return "", fmt.Errorf("path exceeds %d bytes", maxBytes)
	}
	if !utf8.ValidString(raw) || containsControl(raw) {
		return "", fmt.Errorf("path is not safe UTF-8 text")
	}
	if strings.Contains(raw, "\\") {
		return "", fmt.Errorf("backslashes are forbidden")
	}
	if strings.HasPrefix(raw, "/") || path.IsAbs(raw) {
		return "", fmt.Errorf("absolute path is forbidden")
	}
	name := raw
	if directory && strings.HasSuffix(name, "/") {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || strings.HasSuffix(name, "/") {
		return "", fmt.Errorf("non-canonical trailing slash")
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("empty, dot, or traversal component is forbidden")
		}
	}
	if clean := path.Clean(name); clean != name || clean == "." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("non-canonical or traversing path")
	}
	return name, nil
}

func validateKegPath(name string, entryType EntryType, expected Expectation) error {
	keg := expectedKegPrefix(expected)
	if name == expected.Name || name == keg {
		if entryType != EntryDirectory {
			return fmt.Errorf("keg ancestor must be a directory")
		}
		return nil
	}
	if !strings.HasPrefix(name, keg+"/") {
		return fmt.Errorf("entry is outside the only allowed keg %q", keg)
	}
	return nil
}

func validateSymlink(name, target string, expected Expectation, limits Limits) (string, string, error) {
	if target == "" {
		return "", "", verificationError(CodeUnsafeLink, name, "empty symlink target")
	}
	if len(target) > limits.MaxLinkBytes || !utf8.ValidString(target) || containsControl(target) {
		return "", "", verificationError(CodeUnsafeLink, name, "invalid or overlong symlink target")
	}
	if strings.Contains(target, "\\") || strings.HasPrefix(target, "/") || path.IsAbs(target) {
		return "", "", verificationError(CodeUnsafeLink, name, "absolute or backslash symlink target is forbidden")
	}

	prefixTarget := path.Clean(path.Join("Cellar", path.Dir(name), target))
	installedKeg := path.Join("Cellar", expectedKegPrefix(expected))
	if prefixTarget == installedKeg || strings.HasPrefix(prefixTarget, installedKeg+"/") {
		return strings.TrimPrefix(prefixTarget, "Cellar/"), "", nil
	}
	if canonicalExternalTarget, ok := canonicalExternalSymlinkTarget(name, target, expected); ok && canonicalExternalTarget == prefixTarget {
		return "", prefixTarget, nil
	}
	return "", "", verificationError(CodeUnsafeLink, name, "symlink target %q escapes keg %q", target, expectedKegPrefix(expected))
}

func externalSymlinkKegPath(name string, expected Expectation) (string, bool) {
	kegPrefix := expectedKegPrefix(expected) + "/"
	kegPath := strings.TrimPrefix(name, kegPrefix)
	return kegPath, kegPath != name
}

func canonicalExternalSymlinkTarget(name, target string, expected Expectation) (string, bool) {
	base := strings.Split(path.Join("Cellar", path.Dir(name)), "/")
	components := strings.Split(target, "/")
	if len(components) < len(base)+2 {
		return "", false
	}
	for index := range base {
		if components[index] != ".." {
			return "", false
		}
	}
	components = components[len(base):]
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", false
		}
	}
	candidate := strings.Join(components, "/")
	return candidate, allowedExternalSymlink(name, candidate, expected)
}

func allowedExternalSymlink(name, target string, expected Expectation) bool {
	kegPath, ok := externalSymlinkKegPath(name, expected)
	if !ok {
		return false
	}
	if strings.HasPrefix(kegPath, "libexec/") {
		for _, formula := range expected.AllowedExternalSymlinkFormulae {
			root := path.Join("opt", formula)
			if target == root || strings.HasPrefix(target, root+"/") {
				return true
			}
		}
	}
	return slices.Contains(expected.AllowedExternalSymlinkRules, ExternalSymlinkRuleCertifiSharedCA) &&
		IsCertifiSharedCACertKegPath(kegPath) && target == CertifiSharedCATarget
}

func validateHardlink(target string, expected Expectation, limits Limits) (string, error) {
	if target == "" || len(target) > limits.MaxLinkBytes || !utf8.ValidString(target) || containsControl(target) {
		return "", fmt.Errorf("invalid or overlong hardlink target")
	}
	if strings.Contains(target, "\\") || strings.HasPrefix(target, "/") || path.IsAbs(target) {
		return "", fmt.Errorf("absolute or backslash hardlink target is forbidden")
	}
	canonical, err := canonicalArchivePath(target, false, limits.MaxLinkBytes)
	if err != nil {
		return "", fmt.Errorf("invalid hardlink target: %w", err)
	}
	if !withinKeg(canonical, expected) {
		return "", fmt.Errorf("hardlink target %q escapes keg %q", target, expectedKegPrefix(expected))
	}
	return canonical, nil
}

func validatePAX(hdr *tar.Header, limits Limits) error {
	if len(hdr.PAXRecords) > limits.MaxPAXRecords {
		return fmt.Errorf("PAX record count %d exceeds %d", len(hdr.PAXRecords), limits.MaxPAXRecords)
	}
	metadata := int64(len(hdr.Name) + len(hdr.Linkname) + len(hdr.Uname) + len(hdr.Gname))
	keys := make([]string, 0, len(hdr.PAXRecords))
	for key := range hdr.PAXRecords {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := hdr.PAXRecords[key]
		if strings.HasPrefix(key, "GNU.sparse.") || key == "SCHILY.realsize" || strings.HasPrefix(key, "SCHILY.acl.") {
			return fmt.Errorf("sparse or security-sensitive PAX key %q is forbidden", key)
		}
		if _, ok := allowedPAXKeys[key]; !ok && !strings.HasPrefix(key, "SCHILY.xattr.") {
			return fmt.Errorf("unsupported PAX key %q", key)
		}
		if len(key) == 0 || len(value) == 0 || !utf8.ValidString(key) || !utf8.ValidString(value) {
			return fmt.Errorf("invalid PAX record %q", key)
		}
		if int64(len(key)) > limits.MaxMetadataBytes-metadata {
			return fmt.Errorf("per-file metadata exceeds %d bytes", limits.MaxMetadataBytes)
		}
		metadata += int64(len(key))
		if int64(len(value)) > limits.MaxMetadataBytes-metadata {
			return fmt.Errorf("per-file metadata exceeds %d bytes", limits.MaxMetadataBytes)
		}
		metadata += int64(len(value))
	}
	if metadata > limits.MaxMetadataBytes {
		return fmt.Errorf("per-file metadata exceeds %d bytes", limits.MaxMetadataBytes)
	}
	return nil
}

func allowedXattrs(hdr *tar.Header, limits Limits) ([]Xattr, error) {
	attrs := make(map[string]string)
	for name, value := range hdr.Xattrs {
		attrs[name] = value
	}
	for key, value := range hdr.PAXRecords {
		const prefix = "SCHILY.xattr."
		if strings.HasPrefix(key, prefix) {
			name := strings.TrimPrefix(key, prefix)
			if previous, ok := attrs[name]; ok && previous != value {
				return nil, fmt.Errorf("conflicting xattr %q", name)
			}
			attrs[name] = value
		}
	}
	if len(attrs) > limits.MaxXattrs {
		return nil, fmt.Errorf("xattr count %d exceeds %d", len(attrs), limits.MaxXattrs)
	}
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	slices.Sort(names)
	var total int64
	out := make([]Xattr, 0, len(names))
	for _, name := range names {
		value := attrs[name]
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "user.") || strings.HasPrefix(lower, "security.") || strings.HasPrefix(lower, "trusted.") || strings.Contains(lower, "capability") {
			return nil, fmt.Errorf("security or unsupported xattr %q is forbidden", name)
		}
		if name == "" || !utf8.ValidString(name) || !utf8.ValidString(value) || containsControl(name) {
			return nil, fmt.Errorf("invalid xattr %q", name)
		}
		if int64(len(name)+len(value)) > limits.MaxXattrBytes-total {
			return nil, fmt.Errorf("xattr bytes exceed %d", limits.MaxXattrBytes)
		}
		total += int64(len(name) + len(value))
		out = append(out, Xattr{Name: name, Value: value})
	}
	return out, nil
}

func validateCollisionsAndHardlinks(entries map[string]*scannedEntry, expected Expectation, limits Limits) error {
	paths := make([]string, 0, len(entries))
	for name := range entries {
		paths = append(paths, name)
	}
	slices.Sort(paths)
	for _, name := range paths {
		for parent := path.Dir(name); parent != "." && parent != "/"; parent = path.Dir(parent) {
			if ancestor, ok := entries[parent]; ok && ancestor.inventory.Type != EntryDirectory {
				return verificationError(CodePathCollision, name, "ancestor %q is a %s, not a directory", parent, ancestor.inventory.Type)
			}
		}
	}

	resolvePath := func(base, target, owner string) (string, string, error) {
		rootParts := strings.Split(expectedKegPrefix(expected), "/")
		stack := []string{}
		if base != "" && base != "." {
			stack = strings.Split(base, "/")
		}
		queue := strings.Split(target, "/")
		followed := map[string]bool{}
		steps := 0
		for len(queue) > 0 {
			component := queue[0]
			queue = queue[1:]
			switch component {
			case "", ".":
				continue
			case "..":
				if len(stack) <= len(rootParts) {
					return "", "", verificationError(CodeUnsafeLink, owner, "link target %q escapes keg %q", target, expectedKegPrefix(expected))
				}
				stack = stack[:len(stack)-1]
				continue
			}
			stack = append(stack, component)
			candidate := strings.Join(stack, "/")
			if !withinKeg(candidate, expected) && len(stack) >= len(rootParts) {
				return "", "", verificationError(CodeUnsafeLink, owner, "link target %q escapes keg %q", target, expectedKegPrefix(expected))
			}
			entry, ok := entries[candidate]
			if !ok || entry.inventory.Type != EntrySymlink {
				continue
			}
			steps++
			if steps > limits.MaxDepth {
				return "", "", verificationError(CodeUnsafeLink, owner, "symlink resolution exceeds %d steps", limits.MaxDepth)
			}
			if followed[candidate] {
				return "", "", verificationError(CodeUnsafeLink, owner, "symlink cycle through %q", candidate)
			}
			followed[candidate] = true
			if entry.inventory.PrefixTarget != "" {
				if len(queue) != 0 {
					return "", "", verificationError(CodeUnsafeLink, owner, "external symlink %q has unresolved path suffix", candidate)
				}
				return "", entry.inventory.PrefixTarget, nil
			}
			stack = stack[:len(stack)-1]
			queue = append(strings.Split(entry.inventory.SymlinkTarget, "/"), queue...)
		}
		resolved := strings.Join(stack, "/")
		if !withinKeg(resolved, expected) {
			return "", "", verificationError(CodeUnsafeLink, owner, "link target %q escapes keg %q", target, expectedKegPrefix(expected))
		}
		return resolved, "", nil
	}

	for _, name := range paths {
		entry := entries[name]
		if entry.inventory.Type != EntrySymlink {
			continue
		}
		if entry.inventory.PrefixTarget != "" {
			continue
		}
		resolved, prefixTarget, err := resolvePath(path.Dir(name), entry.inventory.SymlinkTarget, name)
		if err != nil {
			return err
		}
		if prefixTarget != "" && !allowedExternalSymlink(name, prefixTarget, expected) {
			return verificationError(CodeUnsafeLink, name, "symlink chain reaches an unauthorized prefix target")
		}
		entry.inventory.ResolvedTarget = resolved
		entry.inventory.PrefixTarget = prefixTarget
	}

	hardResolved := make(map[string]string)
	visiting := make(map[string]bool)
	var resolveHardlink func(string, string, int) (string, error)
	resolveHardlink = func(name, owner string, depth int) (string, error) {
		if depth > limits.MaxDepth {
			return "", verificationError(CodeUnsafeLink, owner, "hardlink chain exceeds %d", limits.MaxDepth)
		}
		if target, ok := hardResolved[name]; ok {
			return target, nil
		}
		if visiting[name] {
			return "", verificationError(CodeUnsafeLink, owner, "hardlink cycle through %q", name)
		}
		entry, ok := entries[name]
		if !ok {
			return "", verificationError(CodeUnsafeLink, owner, "hardlink target %q is missing", name)
		}
		visiting[name] = true
		defer delete(visiting, name)
		switch entry.inventory.Type {
		case EntryRegular:
			hardResolved[name] = name
			return name, nil
		case EntryHardlink:
			resolvedPath, prefixTarget, err := resolvePath("", entry.inventory.HardlinkTarget, owner)
			if err != nil {
				return "", err
			}
			if prefixTarget != "" {
				return "", verificationError(CodeUnsafeLink, owner, "hardlink target resolves outside keg")
			}
			target, err := resolveHardlink(resolvedPath, owner, depth+1)
			if err != nil {
				return "", err
			}
			hardResolved[name] = target
			return target, nil
		default:
			return "", verificationError(CodeUnsafeLink, owner, "hardlink resolves to %s instead of a regular file", entry.inventory.Type)
		}
	}
	for _, name := range paths {
		entry := entries[name]
		if entry.inventory.Type != EntryHardlink {
			continue
		}
		resolvedPath, prefixTarget, err := resolvePath("", entry.inventory.HardlinkTarget, name)
		if err != nil {
			return err
		}
		if prefixTarget != "" {
			return verificationError(CodeUnsafeLink, name, "hardlink target resolves outside keg")
		}
		target, err := resolveHardlink(resolvedPath, name, 1)
		if err != nil {
			return err
		}
		entry.inventory.ResolvedTarget = target
		targetEntry := entries[target]
		entry.inventory.SHA256 = targetEntry.inventory.SHA256
		entry.inventory.Size = targetEntry.inventory.Size
		entry.inventory.Mode = targetEntry.inventory.Mode
		entry.inventory.UID = targetEntry.inventory.UID
		entry.inventory.GID = targetEntry.inventory.GID
		entry.inventory.Xattrs = append([]Xattr(nil), targetEntry.inventory.Xattrs...)
		entry.inventory.Relocatable = targetEntry.inventory.Relocatable
	}
	return nil
}

func normalizeExpectation(e Expectation, limits Limits) (Expectation, error) {
	if !safeFormulaName.MatchString(e.Name) || strings.ContainsAny(e.Name, `/\\`) {
		return e, fmt.Errorf("invalid Formula name %q", e.Name)
	}
	if err := validateComponent("PkgVersion", e.PkgVersion); err != nil {
		return e, err
	}
	if e.FormulaVersion == "" || !utf8.ValidString(e.FormulaVersion) || containsControl(e.FormulaVersion) {
		return e, fmt.Errorf("invalid FormulaVersion %q", e.FormulaVersion)
	}
	if e.FormulaRevision < 0 || e.VersionScheme < 0 || e.BottleRebuild < 0 {
		return e, fmt.Errorf("negative Formula revision, version scheme, or bottle rebuild")
	}
	if e.CompressedSize <= 0 {
		return e, fmt.Errorf("compressed size must be positive")
	}
	if e.CompressedSize > limits.MaxCompressedBytes {
		return e, fmt.Errorf("authenticated compressed size %d exceeds configured maximum %d", e.CompressedSize, limits.MaxCompressedBytes)
	}
	oci, err := normalizeSHA256(e.CompressedSHA256, true)
	if err != nil {
		return e, fmt.Errorf("invalid compressed SHA-256: %w", err)
	}
	homebrew, err := normalizeSHA256(e.HomebrewSHA256, false)
	if err != nil {
		return e, fmt.Errorf("invalid Homebrew SHA-256: %w", err)
	}
	e.CompressedSHA256 = "sha256:" + oci
	e.HomebrewSHA256 = homebrew
	if e.FullName == "" {
		e.FullName = e.Name
	}
	if !utf8.ValidString(e.FullName) || containsControl(e.FullName) || strings.HasPrefix(e.FullName, "/") || strings.HasSuffix(e.FullName, "/") {
		return e, fmt.Errorf("invalid full Formula identity %q", e.FullName)
	}
	fullParts := strings.Split(e.FullName, "/")
	if fullParts[len(fullParts)-1] != e.Name {
		return e, fmt.Errorf("full Formula identity %q does not name %q", e.FullName, e.Name)
	}
	for _, part := range fullParts {
		if part == "" || part == "." || part == ".." {
			return e, fmt.Errorf("invalid full Formula identity %q", e.FullName)
		}
	}
	if e.FormulaIdentity != "" && e.FormulaIdentity != e.FullName {
		return e, fmt.Errorf("upstream Formula identity %q does not match %q", e.FormulaIdentity, e.FullName)
	}
	if e.ExpectedTap == "" {
		e.ExpectedTap = tapFromFullName(e.FullName)
	}
	if e.ExpectedTap == "" {
		e.ExpectedTap = "homebrew/core"
	}
	if !utf8.ValidString(e.ExpectedTap) || containsControl(e.ExpectedTap) || strings.HasPrefix(e.ExpectedTap, "/") || strings.HasSuffix(e.ExpectedTap, "/") {
		return e, fmt.Errorf("invalid expected tap %q", e.ExpectedTap)
	}
	if len(fullParts) > 1 && e.ExpectedTap+"/"+e.Name != e.FullName {
		return e, fmt.Errorf("tap %q does not match full Formula identity %q", e.ExpectedTap, e.FullName)
	}
	if e.BottleTag != "" {
		if err := validateComponent("BottleTag", e.BottleTag); err != nil {
			return e, err
		}
	}
	for _, value := range []struct {
		label string
		value string
	}{
		{"HomebrewVersion", e.HomebrewVersion},
		{"Arch", e.Arch},
		{"Compiler", e.Compiler},
	} {
		if value.value != "" && (!utf8.ValidString(value.value) || containsControl(value.value)) {
			return e, fmt.Errorf("invalid %s %q", value.label, value.value)
		}
	}
	e.AllowedExternalSymlinkFormulae = slices.Clone(e.AllowedExternalSymlinkFormulae)
	slices.Sort(e.AllowedExternalSymlinkFormulae)
	for index, formula := range e.AllowedExternalSymlinkFormulae {
		if !safeFormulaName.MatchString(formula) || strings.ContainsAny(formula, `/\`) || formula == e.Name {
			return e, fmt.Errorf("invalid allowed external symlink Formula %q", formula)
		}
		if index > 0 && e.AllowedExternalSymlinkFormulae[index-1] == formula {
			return e, fmt.Errorf("duplicate allowed external symlink Formula %q", formula)
		}
	}
	e.AllowedExternalSymlinkRules = slices.Clone(e.AllowedExternalSymlinkRules)
	slices.Sort(e.AllowedExternalSymlinkRules)
	for index, rule := range e.AllowedExternalSymlinkRules {
		if rule != ExternalSymlinkRuleCertifiSharedCA {
			return e, fmt.Errorf("unknown allowed external symlink rule %q", rule)
		}
		if index > 0 && e.AllowedExternalSymlinkRules[index-1] == rule {
			return e, fmt.Errorf("duplicate allowed external symlink rule %q", rule)
		}
	}
	if slices.Contains(e.AllowedExternalSymlinkRules, ExternalSymlinkRuleCertifiSharedCA) &&
		(e.Name != "certifi" || e.FullName != "homebrew/core/certifi" || !slices.Contains(e.AllowedExternalSymlinkFormulae, "ca-certificates")) {
		return e, fmt.Errorf("external symlink rule %q requires exact core certifi with a signed direct ca-certificates dependency", ExternalSymlinkRuleCertifiSharedCA)
	}

	seenDeps := make(map[string]struct{}, len(e.Dependencies))
	for _, dep := range e.Dependencies {
		if dep.FullName == "" || dep.Version == "" || dep.PkgVersion == "" ||
			!utf8.ValidString(dep.FullName) || !utf8.ValidString(dep.Version) || !utf8.ValidString(dep.PkgVersion) ||
			containsControl(dep.FullName) || containsControl(dep.Version) || containsControl(dep.PkgVersion) {
			return e, fmt.Errorf("invalid receipt dependency %#v", dep)
		}
		if dep.Revision < 0 || dep.BottleRebuild < 0 {
			return e, fmt.Errorf("negative receipt dependency revision for %q", dep.FullName)
		}
		if _, ok := seenDeps[dep.FullName]; ok {
			return e, fmt.Errorf("duplicate receipt dependency %q", dep.FullName)
		}
		seenDeps[dep.FullName] = struct{}{}
	}
	return e, nil
}

func normalizeSHA256(value string, requireAlgorithm bool) (string, error) {
	if strings.TrimSpace(value) != value || value == "" {
		return "", fmt.Errorf("empty or padded digest")
	}
	hexValue := value
	if strings.Contains(value, ":") {
		algorithm, encoded, ok := strings.Cut(value, ":")
		if !ok || algorithm != "sha256" {
			return "", fmt.Errorf("only sha256 is supported")
		}
		hexValue = encoded
	} else if requireAlgorithm {
		return "", fmt.Errorf("OCI digest must include sha256: prefix")
	}
	if len(hexValue) != sha256.Size*2 {
		return "", fmt.Errorf("expected 64 hex characters")
	}
	decoded, err := hex.DecodeString(hexValue)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("invalid SHA-256 hex")
	}
	return strings.ToLower(hexValue), nil
}

func validateComponent(label, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) || !utf8.ValidString(value) || containsControl(value) {
		return fmt.Errorf("invalid %s %q", label, value)
	}
	return nil
}

func validateLimits(l Limits) error {
	if l.MaxCompressedBytes == math.MaxInt64 || l.MaxExpandedBytes == math.MaxInt64 {
		return fmt.Errorf("compressed and expanded byte limits must be less than MaxInt64")
	}
	for label, value := range map[string]int64{
		"MaxCompressedBytes": l.MaxCompressedBytes,
		"MaxExpandedBytes":   l.MaxExpandedBytes,
		"MaxFileBytes":       l.MaxFileBytes,
		"MaxMetadataBytes":   l.MaxMetadataBytes,
		"MaxTarPaddingBytes": l.MaxTarPaddingBytes,
		"MaxXattrBytes":      l.MaxXattrBytes,
		"MaxFormulaBytes":    l.MaxFormulaBytes,
		"MaxReceiptBytes":    l.MaxReceiptBytes,
	} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", label)
		}
	}
	for label, value := range map[string]int{
		"MaxFiles":      l.MaxFiles,
		"MaxDepth":      l.MaxDepth,
		"MaxPathBytes":  l.MaxPathBytes,
		"MaxLinkBytes":  l.MaxLinkBytes,
		"MaxPAXRecords": l.MaxPAXRecords,
		"MaxXattrs":     l.MaxXattrs,
	} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", label)
		}
	}
	return nil
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxCompressedBytes == 0 {
		l.MaxCompressedBytes = d.MaxCompressedBytes
	}
	if l.MaxExpandedBytes == 0 {
		l.MaxExpandedBytes = d.MaxExpandedBytes
	}
	if l.MaxFiles == 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxFileBytes == 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxMetadataBytes == 0 {
		l.MaxMetadataBytes = d.MaxMetadataBytes
	}
	if l.MaxTarPaddingBytes == 0 {
		l.MaxTarPaddingBytes = d.MaxTarPaddingBytes
	}
	if l.MaxPathBytes == 0 {
		l.MaxPathBytes = d.MaxPathBytes
	}
	if l.MaxLinkBytes == 0 {
		l.MaxLinkBytes = d.MaxLinkBytes
	}
	if l.MaxPAXRecords == 0 {
		l.MaxPAXRecords = d.MaxPAXRecords
	}
	if l.MaxXattrs == 0 {
		l.MaxXattrs = d.MaxXattrs
	}
	if l.MaxXattrBytes == 0 {
		l.MaxXattrBytes = d.MaxXattrBytes
	}
	if l.MaxFormulaBytes == 0 {
		l.MaxFormulaBytes = d.MaxFormulaBytes
	}
	if l.MaxReceiptBytes == 0 {
		l.MaxReceiptBytes = d.MaxReceiptBytes
	}
	return l
}

// CanonicalInventory returns deterministic JSON independent of input ordering.
// It is suitable for transport to the materializer and for stable hashing.
func CanonicalInventory(entries []InventoryEntry) ([]byte, error) {
	canonical := append([]InventoryEntry(nil), entries...)
	for i := range canonical {
		canonical[i].Xattrs = append([]Xattr(nil), canonical[i].Xattrs...)
		slices.SortFunc(canonical[i].Xattrs, func(a, b Xattr) int {
			if c := strings.Compare(a.Name, b.Name); c != 0 {
				return c
			}
			return strings.Compare(a.Value, b.Value)
		})
		for j := 1; j < len(canonical[i].Xattrs); j++ {
			if canonical[i].Xattrs[j-1].Name == canonical[i].Xattrs[j].Name {
				return nil, fmt.Errorf("duplicate xattr %q on inventory path %q", canonical[i].Xattrs[j].Name, canonical[i].Path)
			}
		}
	}
	slices.SortFunc(canonical, func(a, b InventoryEntry) int { return strings.Compare(a.Path, b.Path) })
	for i := 1; i < len(canonical); i++ {
		if canonical[i-1].Path == canonical[i].Path {
			return nil, fmt.Errorf("duplicate inventory path %q", canonical[i].Path)
		}
	}
	return json.Marshal(canonical)
}

func digestInventory(entries []InventoryEntry) (string, error) {
	data, err := CanonicalInventory(entries)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func expectedKegPrefix(e Expectation) string { return e.Name + "/" + e.PkgVersion }
func expectedFormulaPath(e Expectation) string {
	return expectedKegPrefix(e) + "/.brew/" + e.Name + ".rb"
}
func expectedReceiptPath(e Expectation) string { return expectedKegPrefix(e) + "/INSTALL_RECEIPT.json" }

func withinKeg(name string, expected Expectation) bool {
	keg := expectedKegPrefix(expected)
	return name == keg || strings.HasPrefix(name, keg+"/")
}

func tapFromFullName(fullName string) string {
	parts := strings.Split(fullName, "/")
	if len(parts) >= 3 {
		return strings.Join(parts[:len(parts)-1], "/")
	}
	return ""
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
