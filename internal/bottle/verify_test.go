package bottle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type archiveMember struct {
	header tar.Header
	body   []byte
}

func TestVerifyValidBottleAndDeterministicInventory(t *testing.T) {
	t.Parallel()
	members := validMembers(false)
	blobA := makeArchive(t, members)
	resultA, err := Verify(bytes.NewReader(blobA), expectationFor(blobA), Options{})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	reordered := append([]archiveMember(nil), members...)
	slices.Reverse(reordered)
	blobB := makeArchive(t, reordered)
	resultB, err := Verify(bytes.NewReader(blobB), expectationFor(blobB), Options{})
	if err != nil {
		t.Fatalf("Verify(reordered) error = %v", err)
	}

	if resultA.KegPrefix != "hello/1.0" {
		t.Fatalf("KegPrefix = %q", resultA.KegPrefix)
	}
	if resultA.Formula.ClassName != "Hello" || resultA.Formula.Path != "hello/1.0/.brew/hello.rb" {
		t.Fatalf("Formula = %#v", resultA.Formula)
	}
	if resultA.Receipt != nil {
		t.Fatalf("Receipt = %#v, want nil", resultA.Receipt)
	}
	if resultA.InventorySHA256 != resultB.InventorySHA256 {
		t.Fatalf("inventory digest depends on tar order: %s != %s", resultA.InventorySHA256, resultB.InventorySHA256)
	}
	if !reflect.DeepEqual(resultA.Inventory, resultB.Inventory) {
		t.Fatalf("inventory depends on tar order:\n%#v\n%#v", resultA.Inventory, resultB.Inventory)
	}
	for i := 1; i < len(resultA.Inventory); i++ {
		if resultA.Inventory[i-1].Path >= resultA.Inventory[i].Path {
			t.Fatalf("inventory is not sorted: %q before %q", resultA.Inventory[i-1].Path, resultA.Inventory[i].Path)
		}
	}
	assertInventoryEntry(t, resultA.Inventory, "hello/1.0/bin/hello", EntryRegular, "sha256:"+sha256Hex([]byte("hello\n")))
	assertInventoryEntry(t, resultA.Inventory, "hello/1.0/bin/hello-hard", EntryHardlink, "sha256:"+sha256Hex([]byte("hello\n")))
	assertInventoryEntry(t, resultA.Inventory, "hello/1.0/bin/hello-link", EntrySymlink, "")
}

func TestVerifyValidReceipt(t *testing.T) {
	t.Parallel()
	blob := makeArchive(t, validMembers(true))
	result, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{
		Policy: Policy{RequirePreInstallReceipt: true},
	})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if result.Receipt == nil {
		t.Fatal("Receipt is nil")
	}
	if result.Receipt.FormulaVersion != "1.0" || !result.Receipt.BuiltAsBottle || result.Receipt.RuntimeDepCount != 1 {
		t.Fatalf("Receipt = %#v", result.Receipt)
	}
}

func TestVerifyNodeProjectsAuthenticatedExpectation(t *testing.T) {
	t.Parallel()
	blob := makeArchive(t, validMembers(true))
	expected := expectationFor(blob)
	node := resolution.Node{
		Name:              expected.Name,
		FullName:          expected.FullName,
		FormulaVersion:    expected.FormulaVersion,
		FormulaRevision:   expected.FormulaRevision,
		PkgVersion:        expected.PkgVersion,
		VersionScheme:     expected.VersionScheme,
		BottleRebuild:     expected.BottleRebuild,
		UpstreamFormulaID: "homebrew/core/hello",
		Bottle: resolution.Bottle{
			Tag:            expected.BottleTag,
			HomebrewSHA256: expected.HomebrewSHA256,
			Layer: resolution.Descriptor{
				Digest: expected.CompressedSHA256,
				Size:   expected.CompressedSize,
			},
			Tab: resolution.BottleTab{
				HomebrewVersion: expected.HomebrewVersion,
				Arch:            expected.Arch,
				Compiler:        expected.Compiler,
				Dependencies: []resolution.RuntimeDependency{{
					FullName:         "libfoo",
					Version:          "2.0",
					PkgVersion:       "2.0",
					DeclaredDirectly: true,
				}},
			},
		},
	}
	result, err := VerifyNode(bytes.NewReader(blob), node, Options{Policy: Policy{RequirePreInstallReceipt: true}})
	if err != nil {
		t.Fatalf("VerifyNode() error = %v", err)
	}
	if result.Name != node.Name || result.Receipt == nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestVerifyAuthenticatesCompressedBytesBeforeTarParsing(t *testing.T) {
	t.Parallel()
	blob := makeArchive(t, validMembers(false))

	tests := []struct {
		name   string
		mutate func(*Expectation)
		code   ErrorCode
	}{
		{
			name: "size",
			mutate: func(e *Expectation) {
				e.CompressedSize++
			},
			code: CodeSizeMismatch,
		},
		{
			name: "oci digest",
			mutate: func(e *Expectation) {
				e.CompressedSHA256 = "sha256:" + strings.Repeat("0", 64)
			},
			code: CodeDigestMismatch,
		},
		{
			name: "homebrew checksum",
			mutate: func(e *Expectation) {
				e.HomebrewSHA256 = strings.Repeat("f", 64)
			},
			code: CodeHomebrewMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expected := expectationFor(blob)
			tt.mutate(&expected)
			_, err := Verify(bytes.NewReader(blob), expected, Options{})
			assertErrorCode(t, err, tt.code)
		})
	}
}

func TestVerifyRejectsUnsafePathsAndCollisions(t *testing.T) {
	t.Parallel()
	formula := formulaMember(validFormula())
	tests := []struct {
		name    string
		members []archiveMember
		code    ErrorCode
	}{
		{"absolute", []archiveMember{formula, regularMember("/hello/1.0/evil", 0o644, "x")}, CodeUnsafePath},
		{"traversal", []archiveMember{formula, regularMember("hello/1.0/../../evil", 0o644, "x")}, CodeUnsafePath},
		{"dot component", []archiveMember{formula, regularMember("hello/1.0/./evil", 0o644, "x")}, CodeUnsafePath},
		{"backslash", []archiveMember{formula, regularMember(`hello/1.0/bin\\evil`, 0o644, "x")}, CodeUnsafePath},
		{"different keg", []archiveMember{formula, regularMember("hello/2.0/bin/evil", 0o644, "x")}, CodeUnsafePath},
		{"different root", []archiveMember{formula, regularMember("other/1.0/bin/evil", 0o644, "x")}, CodeUnsafePath},
		{"duplicate", []archiveMember{formula, regularMember("hello/1.0/bin/x", 0o644, "a"), regularMember("hello/1.0/bin/x", 0o644, "b")}, CodePathCollision},
		{"symlink ancestor", []archiveMember{formula, symlinkMember("hello/1.0/lib", "bin"), regularMember("hello/1.0/lib/plugin", 0o644, "x")}, CodePathCollision},
		{"regular ancestor", []archiveMember{formula, regularMember("hello/1.0/share", 0o644, "x"), regularMember("hello/1.0/share/data", 0o644, "x")}, CodePathCollision},
		{"non-directory keg root", []archiveMember{formula, regularMember("hello", 0o644, "x")}, CodeUnsafePath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob := makeArchive(t, tt.members)
			_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
			assertErrorCode(t, err, tt.code)
		})
	}
}

func TestVerifyRejectsUnsafeLinks(t *testing.T) {
	t.Parallel()
	formula := formulaMember(validFormula())
	tests := []struct {
		name    string
		members []archiveMember
	}{
		{"absolute symlink", []archiveMember{formula, symlinkMember("hello/1.0/bin/x", "/etc/passwd")}},
		{"escaping symlink", []archiveMember{formula, symlinkMember("hello/1.0/bin/x", "../../../etc/passwd")}},
		{"backslash symlink", []archiveMember{formula, symlinkMember("hello/1.0/bin/x", `..\\evil`)}},
		{"escaping hardlink", []archiveMember{formula, hardlinkMember("hello/1.0/bin/x", "../outside")}},
		{"missing hardlink", []archiveMember{formula, hardlinkMember("hello/1.0/bin/x", "hello/1.0/bin/missing")}},
		{"hardlink to symlink", []archiveMember{formula, symlinkMember("hello/1.0/bin/target", "hello"), hardlinkMember("hello/1.0/bin/x", "hello/1.0/bin/target")}},
		{"hardlink cycle", []archiveMember{formula, hardlinkMember("hello/1.0/bin/a", "hello/1.0/bin/b"), hardlinkMember("hello/1.0/bin/b", "hello/1.0/bin/a")}},
		{"symlink cycle", []archiveMember{formula, symlinkMember("hello/1.0/bin/a", "b"), symlinkMember("hello/1.0/bin/b", "a")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob := makeArchive(t, tt.members)
			_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
			assertErrorCode(t, err, CodeUnsafeLink)
		})
	}
}

func TestVerifyRejectsSpecialTypesSetIDAndSparse(t *testing.T) {
	t.Parallel()
	formula := formulaMember(validFormula())
	tests := []struct {
		name   string
		member archiveMember
		code   ErrorCode
	}{
		{"character device", specialMember("hello/1.0/dev/char", tar.TypeChar), CodeUnsafeType},
		{"block device", specialMember("hello/1.0/dev/block", tar.TypeBlock), CodeUnsafeType},
		{"fifo", specialMember("hello/1.0/run/fifo", tar.TypeFifo), CodeUnsafeType},
		{"setuid", regularMember("hello/1.0/bin/setuid", 0o4755, "x"), CodeUnsafeMode},
		{"setgid", regularMember("hello/1.0/bin/setgid", 0o2755, "x"), CodeUnsafeMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob := makeArchive(t, []archiveMember{formula, tt.member})
			_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
			assertErrorCode(t, err, tt.code)
		})
	}

	t.Run("socket type", func(t *testing.T) {
		t.Parallel()
		blob := makeArchive(t, []archiveMember{regularMember("hello/1.0/socket", 0o644, ""), formula})
		blob = patchFirstTarType(t, blob, 's')
		_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
		assertErrorCode(t, err, CodeUnsafeType)
	})
	t.Run("gnu sparse type", func(t *testing.T) {
		t.Parallel()
		blob := makeArchive(t, []archiveMember{regularMember("hello/1.0/sparse", 0o644, ""), formula})
		blob = patchFirstTarType(t, blob, tar.TypeGNUSparse)
		_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
		assertOneOfCodes(t, err, CodeUnsafeType, CodeInvalidTar)
	})
}

func TestVerifyRejectsSecurityXattrsAndUnsupportedPAX(t *testing.T) {
	t.Parallel()
	formula := formulaMember(validFormula())
	tests := []struct {
		name string
		pax  map[string]string
	}{
		{"capability", map[string]string{"SCHILY.xattr.security.capability": "cap"}},
		{"selinux", map[string]string{"SCHILY.xattr.security.selinux": "label"}},
		{"trusted", map[string]string{"SCHILY.xattr.trusted.overlay.opaque": "y"}},
		{"acl", map[string]string{"SCHILY.acl.access": "acl"}},
		{"sparse pax", map[string]string{"SCHILY.realsize": "999"}},
		{"unknown pax", map[string]string{"EVIL.unhandled": "value"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			member := regularMember("hello/1.0/share/data", 0o644, "x")
			member.header.Format = tar.FormatPAX
			member.header.PAXRecords = tt.pax
			blob := makeArchive(t, []archiveMember{formula, member})
			_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
			assertErrorCode(t, err, CodeUnsafeMetadata)
		})
	}

	t.Run("bounded user xattr is retained", func(t *testing.T) {
		t.Parallel()
		member := regularMember("hello/1.0/share/data", 0o644, "x")
		member.header.Format = tar.FormatPAX
		member.header.PAXRecords = map[string]string{"SCHILY.xattr.user.comment": "safe"}
		blob := makeArchive(t, []archiveMember{formula, member})
		result, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		entry := findInventoryEntry(t, result.Inventory, member.header.Name)
		if !reflect.DeepEqual(entry.Xattrs, []Xattr{{Name: "user.comment", Value: "safe"}}) {
			t.Fatalf("Xattrs = %#v", entry.Xattrs)
		}
	})
}

func TestVerifyEnforcesArchiveLimits(t *testing.T) {
	t.Parallel()
	members := validMembers(false)
	blob := makeArchive(t, members)
	formulaBytes := int64(len(validFormula()))

	tests := []struct {
		name   string
		limits Limits
	}{
		{"compressed", Limits{MaxCompressedBytes: int64(len(blob) - 1)}},
		{"expanded", Limits{MaxExpandedBytes: formulaBytes - 1}},
		{"files", Limits{MaxFiles: 1}},
		{"depth", Limits{MaxDepth: 3}},
		{"file bytes", Limits{MaxFileBytes: formulaBytes - 1}},
		{"metadata", Limits{MaxMetadataBytes: 8}},
		{"path", Limits{MaxPathBytes: 8}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{Limits: tt.limits})
			switch tt.name {
			case "compressed":
				assertErrorCode(t, err, CodeInvalidExpectation)
			case "metadata":
				assertErrorCode(t, err, CodeUnsafeMetadata)
			case "path":
				assertErrorCode(t, err, CodeUnsafePath)
			default:
				assertErrorCode(t, err, CodeArchiveLimit)
			}
		})
	}

	t.Run("per-file xattr bytes", func(t *testing.T) {
		t.Parallel()
		member := regularMember("hello/1.0/share/data", 0o644, "x")
		member.header.Format = tar.FormatPAX
		member.header.PAXRecords = map[string]string{"SCHILY.xattr.user.comment": strings.Repeat("x", 64)}
		limitedBlob := makeArchive(t, []archiveMember{formulaMember(validFormula()), member})
		_, err := Verify(bytes.NewReader(limitedBlob), expectationFor(limitedBlob), Options{Limits: Limits{MaxXattrBytes: 8}})
		assertErrorCode(t, err, CodeUnsafeMetadata)
	})
}

func TestVerifyRequiresAndValidatesFormulaSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		members []archiveMember
		code    ErrorCode
	}{
		{"missing", []archiveMember{regularMember("hello/1.0/bin/hello", 0o755, "x")}, CodeMissingFormula},
		{"wrong class", []archiveMember{formulaMember("class Goodbye < Formula\nend\n")}, CodeInvalidFormula},
		{"multiple classes", []archiveMember{formulaMember("class Hello < Formula\nend\nclass Other < Formula\nend\n")}, CodeInvalidFormula},
		{"unclosed class", []archiveMember{formulaMember("class Hello < Formula\n  desc \"truncated\"\n")}, CodeInvalidFormula},
		{"binary", []archiveMember{formulaMember(string([]byte{'c', 'l', 'a', 's', 's', ' ', 0xff}))}, CodeInvalidFormula},
		{"formula is symlink", []archiveMember{symlinkMember("hello/1.0/.brew/hello.rb", "../bin/hello"), regularMember("hello/1.0/bin/hello", 0o755, "x")}, CodeMissingFormula},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob := makeArchive(t, tt.members)
			_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
			assertErrorCode(t, err, tt.code)
		})
	}

	t.Run("versioned Formula class identity", func(t *testing.T) {
		t.Parallel()
		member := archiveMember{
			header: tar.Header{Name: "openssl@1.1/1.1.1/.brew/openssl@1.1.rb", Typeflag: tar.TypeReg, Mode: 0o644},
			body:   []byte("class OpensslAT11 < Formula\nend\n"),
		}
		blob := makeArchive(t, []archiveMember{member})
		expected := expectationFor(blob)
		expected.Name = "openssl@1.1"
		expected.FullName = "homebrew/core/openssl@1.1"
		expected.FormulaVersion = "1.1.1"
		expected.PkgVersion = "1.1.1"
		result, err := Verify(bytes.NewReader(blob), expected, Options{})
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if result.Formula.ClassName != "OpensslAT11" {
			t.Fatalf("Formula class = %q", result.Formula.ClassName)
		}
	})
}

func TestVerifyReceiptPolicyAndIdentity(t *testing.T) {
	t.Parallel()
	withoutReceipt := makeArchive(t, validMembers(false))
	_, err := Verify(bytes.NewReader(withoutReceipt), expectationFor(withoutReceipt), Options{Policy: Policy{RequirePreInstallReceipt: true}})
	assertErrorCode(t, err, CodeMissingReceipt)

	tests := []struct {
		name    string
		receipt string
	}{
		{"wrong version", strings.Replace(validReceipt(), `"stable":"1.0"`, `"stable":"9.9"`, 1)},
		{"not built as bottle", strings.Replace(validReceipt(), `"built_as_bottle":true`, `"built_as_bottle":false`, 1)},
		{"wrong tap", strings.Replace(validReceipt(), `"tap":"homebrew/core"`, `"tap":"evil/tap"`, 1)},
		{"dependency mismatch", strings.Replace(validReceipt(), `"pkg_version":"2.0"`, `"pkg_version":"3.0"`, 1)},
		{"duplicate JSON key", strings.Replace(validReceipt(), `"built_as_bottle":true`, `"built_as_bottle":true,"built_as_bottle":true`, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			members := validMembers(false)
			members = append(members, receiptMember(tt.receipt))
			blob := makeArchive(t, members)
			_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
			assertErrorCode(t, err, CodeInvalidReceipt)
		})
	}
}

func TestVerifyRejectsMalformedOrAmbiguousGzipTar(t *testing.T) {
	t.Parallel()
	valid := makeArchive(t, validMembers(false))

	t.Run("concatenated gzip members", func(t *testing.T) {
		t.Parallel()
		blob := append(append([]byte(nil), valid...), valid...)
		_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
		assertErrorCode(t, err, CodeInvalidGzip)
	})
	t.Run("trailing compressed bytes", func(t *testing.T) {
		t.Parallel()
		blob := append(append([]byte(nil), valid...), 0x42)
		_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
		assertErrorCode(t, err, CodeInvalidGzip)
	})
	t.Run("truncated gzip", func(t *testing.T) {
		t.Parallel()
		blob := append([]byte(nil), valid[:len(valid)-4]...)
		_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
		assertOneOfCodes(t, err, CodeInvalidGzip, CodeInvalidTar)
	})
	t.Run("standard zero record padding", func(t *testing.T) {
		t.Parallel()
		raw := decompressGzip(t, valid)
		raw = append(raw, make([]byte, 9*tarBlockSize)...)
		blob := compressGzip(t, raw)
		if _, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{}); err != nil {
			t.Fatalf("Verify() with zero tar padding error = %v", err)
		}
	})
	t.Run("missing tar end markers", func(t *testing.T) {
		t.Parallel()
		raw := decompressGzip(t, valid)
		raw = raw[:len(raw)-2*tarBlockSize]
		blob := compressGzip(t, raw)
		_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
		assertErrorCode(t, err, CodeInvalidTar)
	})
	t.Run("data after tar end marker", func(t *testing.T) {
		t.Parallel()
		raw := decompressGzip(t, valid)
		raw = append(raw, []byte("trailing decompressed payload")...)
		blob := compressGzip(t, raw)
		_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
		assertErrorCode(t, err, CodeInvalidTar)
	})
}

func TestCanonicalInventoryAndExpectationIdentity(t *testing.T) {
	t.Parallel()
	entries := []InventoryEntry{
		{Path: "hello/1.0/z", Type: EntryRegular, Xattrs: []Xattr{{Name: "user.z", Value: "z"}, {Name: "user.a", Value: "a"}}},
		{Path: "hello/1.0/a", Type: EntryDirectory},
	}
	forward, err := CanonicalInventory(entries)
	if err != nil {
		t.Fatalf("CanonicalInventory() error = %v", err)
	}
	slices.Reverse(entries)
	reverse, err := CanonicalInventory(entries)
	if err != nil {
		t.Fatalf("CanonicalInventory(reversed) error = %v", err)
	}
	if !bytes.Equal(forward, reverse) {
		t.Fatalf("canonical inventory depends on input ordering:\n%s\n%s", forward, reverse)
	}

	blob := makeArchive(t, validMembers(false))
	expected := expectationFor(blob)
	expected.FormulaIdentity = "homebrew/core/not-hello"
	_, err = Verify(bytes.NewReader(blob), expected, Options{})
	assertErrorCode(t, err, CodeInvalidExpectation)
}

func validMembers(includeReceipt bool) []archiveMember {
	members := []archiveMember{
		formulaMember(validFormula()),
		regularMember("hello/1.0/bin/hello", 0o755, "hello\n"),
		symlinkMember("hello/1.0/bin/hello-link", "hello"),
		hardlinkMember("hello/1.0/bin/hello-hard", "hello/1.0/bin/hello"),
		regularMember("hello/1.0/share/doc/readme", 0o644, "docs\n"),
	}
	if includeReceipt {
		members = append(members, receiptMember(validReceipt()))
	}
	return members
}

func validFormula() string {
	return "class Hello < Formula\n  desc \"test formula\"\n  url \"https://example.invalid/hello-1.0.tar.gz\"\n  version \"1.0\"\nend\n"
}

func validReceipt() string {
	return `{"homebrew_version":"4.3.0","built_as_bottle":true,"poured_from_bottle":false,"arch":"x86_64","compiler":"gcc-11","runtime_dependencies":[{"full_name":"libfoo","version":"2.0","revision":0,"bottle_rebuild":0,"pkg_version":"2.0","declared_directly":true}],"source":{"spec":"stable","tap":"homebrew/core","versions":{"stable":"1.0","head":null,"version_scheme":0}}}`
}

func expectationFor(blob []byte) Expectation {
	sum := sha256.Sum256(blob)
	hexSum := hex.EncodeToString(sum[:])
	return Expectation{
		Name:             "hello",
		FullName:         "homebrew/core/hello",
		FormulaVersion:   "1.0",
		PkgVersion:       "1.0",
		BottleTag:        "x86_64_linux",
		CompressedSHA256: "sha256:" + hexSum,
		CompressedSize:   int64(len(blob)),
		HomebrewSHA256:   hexSum,
		HomebrewVersion:  "4.3.0",
		Arch:             "x86_64",
		Compiler:         "gcc-11",
		ExpectedTap:      "homebrew/core",
		Dependencies: []ReceiptDependency{{
			FullName:         "libfoo",
			Version:          "2.0",
			PkgVersion:       "2.0",
			DeclaredDirectly: true,
		}},
	}
}

func formulaMember(source string) archiveMember {
	return regularMember("hello/1.0/.brew/hello.rb", 0o644, source)
}

func receiptMember(receipt string) archiveMember {
	return regularMember("hello/1.0/INSTALL_RECEIPT.json", 0o644, receipt)
}

func regularMember(name string, mode int64, body string) archiveMember {
	return archiveMember{
		header: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Uid: 1000, Gid: 1000},
		body:   []byte(body),
	}
}

func symlinkMember(name, target string) archiveMember {
	return archiveMember{header: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: target, Uid: 1000, Gid: 1000}}
}

func hardlinkMember(name, target string) archiveMember {
	return archiveMember{header: tar.Header{Name: name, Typeflag: tar.TypeLink, Mode: 0o644, Linkname: target, Uid: 1000, Gid: 1000}}
}

func specialMember(name string, typeflag byte) archiveMember {
	return archiveMember{header: tar.Header{Name: name, Typeflag: typeflag, Mode: 0o600, Devmajor: 1, Devminor: 3}}
}

func makeArchive(t *testing.T, members []archiveMember) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Header.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	for _, member := range members {
		h := member.header
		if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA || h.Typeflag == 0 {
			h.Size = int64(len(member.body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("WriteHeader(%q): %v", h.Name, err)
		}
		if len(member.body) > 0 {
			if _, err := tw.Write(member.body); err != nil {
				t.Fatalf("Write(%q): %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close(): %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close(): %v", err)
	}
	return compressed.Bytes()
}

func patchFirstTarType(t *testing.T, blob []byte, typeflag byte) []byte {
	t.Helper()
	raw := decompressGzip(t, blob)
	if len(raw) < 512 {
		t.Fatal("tar is shorter than one header")
	}
	raw[156] = typeflag
	for i := 148; i < 156; i++ {
		raw[i] = ' '
	}
	var checksum int64
	for _, b := range raw[:512] {
		checksum += int64(b)
	}
	copy(raw[148:156], fmt.Sprintf("%06o\x00 ", checksum))
	return compressGzip(t, raw)
}

func decompressGzip(t *testing.T, blob []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("gzip.NewReader(): %v", err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("ReadAll(gzip): %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close(): %v", err)
	}
	return raw
}

func compressGzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	gz.Header.ModTime = time.Unix(0, 0)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip Write(): %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close(): %v", err)
	}
	return out.Bytes()
}

func assertErrorCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("Verify() error = nil, want code %q", want)
	}
	var verificationErr *VerificationError
	if !errors.As(err, &verificationErr) {
		t.Fatalf("error %T %v is not VerificationError", err, err)
	}
	if verificationErr.Code != want {
		t.Fatalf("error code = %q, want %q; error: %v", verificationErr.Code, want, err)
	}
}

func assertOneOfCodes(t *testing.T, err error, wants ...ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("Verify() error = nil, want one of %v", wants)
	}
	var verificationErr *VerificationError
	if !errors.As(err, &verificationErr) {
		t.Fatalf("error %T %v is not VerificationError", err, err)
	}
	if !slices.Contains(wants, verificationErr.Code) {
		t.Fatalf("error code = %q, want one of %v; error: %v", verificationErr.Code, wants, err)
	}
}

func assertInventoryEntry(t *testing.T, entries []InventoryEntry, name string, entryType EntryType, digest string) {
	t.Helper()
	entry := findInventoryEntry(t, entries, name)
	if entry.Type != entryType || entry.SHA256 != digest {
		t.Fatalf("entry %q = %#v, want type=%s digest=%q", name, entry, entryType, digest)
	}
}

func findInventoryEntry(t *testing.T, entries []InventoryEntry, name string) InventoryEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.Path == name {
			return entry
		}
	}
	t.Fatalf("inventory entry %q not found", name)
	return InventoryEntry{}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestRejectsSymlinkEscapeThroughIntermediateSymlink(t *testing.T) {
	members := validMembers(false)
	members = append(members,
		symlinkMember("hello/1.0/a/b/t", "../../c"),
		symlinkMember("hello/1.0/s", "a/b/t/../../../outside"),
	)
	blob := makeArchive(t, members)
	_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
	assertErrorCode(t, err, CodeUnsafeLink)
}

func TestExpandedLimitIncludesTarHeadersAndMetadata(t *testing.T) {
	members := validMembers(false)
	var payload int64
	for _, member := range members {
		payload += int64(len(member.body))
	}
	blob := makeArchive(t, members)
	_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{Limits: Limits{MaxExpandedBytes: payload + 1}})
	assertErrorCode(t, err, CodeArchiveLimit)
}

func TestInstalledReceiptRequiresPouredIdentity(t *testing.T) {
	node := resolution.Node{Name: "hello", FullName: "homebrew/core/hello", FormulaVersion: "1.0", PkgVersion: "1.0", VersionScheme: 0, Bottle: resolution.Bottle{Tab: resolution.BottleTab{Arch: "x86_64", Compiler: "gcc-12"}}}
	if _, err := VerifyInstalledReceipt([]byte(`{"built_as_bottle":true}`), node); err == nil {
		t.Fatal("minimal receipt should not bind installed identity")
	}
	valid := []byte(`{"built_as_bottle":true,"poured_from_bottle":true,"arch":"x86_64","compiler":"gcc","runtime_dependencies":[],"source":{"spec":"stable","tap":"homebrew/core","versions":{"stable":"1.0","version_scheme":0}}}`)
	if _, err := VerifyInstalledReceipt(valid, node); err != nil {
		t.Fatal(err)
	}
	wrong := bytes.Replace(valid, []byte(`"compiler":"gcc"`), []byte(`"compiler":"clang"`), 1)
	if _, err := VerifyInstalledReceipt(wrong, node); err == nil {
		t.Fatal("unrelated installed compiler family accepted")
	}
}

func TestNumericLeadingCanonicalFormulaUsesEmbeddedClassIdentity(t *testing.T) {
	members := []archiveMember{regularMember("4ti2/1.0/.brew/4ti2.rb", 0o644, "class Fourti2 < Formula\nend\n"), regularMember("4ti2/1.0/bin/tool", 0o755, "x")}
	blob := makeArchive(t, members)
	expected := expectationFor(blob)
	expected.Name = "4ti2"
	expected.FullName = "homebrew/core/4ti2"
	expected.FormulaIdentity = expected.FullName
	if _, err := Verify(bytes.NewReader(blob), expected, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectExpandedLimitOverflow(t *testing.T) {
	blob := makeArchive(t, validMembers(false))
	_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{Limits: Limits{MaxExpandedBytes: math.MaxInt64}})
	assertErrorCode(t, err, CodeInvalidExpectation)
}

func TestHardlinkInventoryUsesTargetInodeMetadata(t *testing.T) {
	blob := makeArchive(t, validMembers(false))
	result, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var regular, hard *InventoryEntry
	for i := range result.Inventory {
		switch result.Inventory[i].Path {
		case "hello/1.0/bin/hello":
			regular = &result.Inventory[i]
		case "hello/1.0/bin/hello-hard":
			hard = &result.Inventory[i]
		}
	}
	if regular == nil || hard == nil {
		t.Fatal("missing inventory entries")
	}
	if hard.Mode != regular.Mode || hard.SHA256 != regular.SHA256 || hard.Size != regular.Size {
		t.Fatalf("regular=%+v hard=%+v", regular, hard)
	}
}

func TestRejectHardlinkChainBeyondDepthLimit(t *testing.T) {
	members := validMembers(false)
	members = append(members, hardlinkMember("hello/1.0/h1", "hello/1.0/h2"), hardlinkMember("hello/1.0/h2", "hello/1.0/h3"), hardlinkMember("hello/1.0/h3", "hello/1.0/h4"), hardlinkMember("hello/1.0/h4", "hello/1.0/h5"), hardlinkMember("hello/1.0/h5", "hello/1.0/h6"), hardlinkMember("hello/1.0/h6", "hello/1.0/bin/hello"))
	blob := makeArchive(t, members)
	_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{Limits: Limits{MaxDepth: 5}})
	assertErrorCode(t, err, CodeUnsafeLink)
}

func TestRejectDanglingGNUMetadataWithSingleEndBlock(t *testing.T) {
	var raw bytes.Buffer
	body := make([]byte, 1024)
	raw.Write(rawTarHeader("././@LongLink", int64(len(body)), tar.TypeGNULongName))
	raw.Write(body)
	raw.Write(make([]byte, tarBlockSize))
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	blob := compressed.Bytes()
	_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{})
	assertErrorCode(t, err, CodeInvalidTar)
}

func TestPlaceholderDetectorAcrossWrites(t *testing.T) {
	detector := &placeholderDetector{}
	for _, chunk := range [][]byte{[]byte("prefix=@@HOMEBREW_"), []byte("PREFIX@@/bin")} {
		if _, err := detector.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if !detector.found {
		t.Fatal("split Homebrew relocation placeholder was not detected")
	}

	clean := &placeholderDetector{}
	if _, err := clean.Write([]byte("ordinary payload")); err != nil {
		t.Fatal(err)
	}
	if clean.found {
		t.Fatal("ordinary payload was marked relocatable")
	}
}

func rawTarHeader(name string, size int64, typeflag byte) []byte {
	block := make([]byte, tarBlockSize)
	copy(block[:100], name)
	writeTarOctal(block[100:108], 0o644)
	writeTarOctal(block[108:116], 0)
	writeTarOctal(block[116:124], 0)
	writeTarOctal(block[124:136], size)
	writeTarOctal(block[136:148], 0)
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}
	block[156] = typeflag
	copy(block[257:263], []byte("ustar\x00"))
	copy(block[263:265], []byte("00"))
	sum := 0
	for _, value := range block {
		sum += int(value)
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return block
}
func writeTarOctal(field []byte, value int64) {
	text := fmt.Sprintf("%0*o", len(field)-1, value)
	copy(field, text)
	field[len(field)-1] = 0
}

func TestGzipMetadataLimitIsEnforcedBeforeArchiveParsing(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, member := range validMembers(false) {
		header := member.header
		header.Size = int64(len(member.body))
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Name = strings.Repeat("n", 1024)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	blob := compressed.Bytes()
	_, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{Limits: Limits{MaxMetadataBytes: 64}})
	assertErrorCode(t, err, CodeArchiveLimit)
}
