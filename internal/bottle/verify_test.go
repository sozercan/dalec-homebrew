package bottle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	wantFormulaSource := []byte(validFormula())
	if !bytes.Equal(resultA.FormulaSource, wantFormulaSource) || !bytes.Equal(resultB.FormulaSource, wantFormulaSource) {
		t.Fatalf("transient Formula source was not captured exactly")
	}
	encoded, err := json.Marshal(resultA)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, wantFormulaSource) || bytes.Contains(encoded, []byte("formula_source")) || bytes.Contains(encoded, []byte("FormulaSource")) {
		t.Fatalf("transient Formula source leaked into JSON evidence: %s", encoded)
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

func TestVerifyAcceptsHomebrewPAXHeaderWithUTF8PhysicalName(t *testing.T) {
	t.Parallel()
	blob, member := homebrewUTF8PAXBottle(t)
	assertTarHeaderFormat(t, blob, member.header.Name, tar.FormatUnknown)

	result, err := Verify(bytes.NewReader(blob), goExpectationFor(blob), Options{})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	assertInventoryEntry(t, result.Inventory, member.header.Name, EntryRegular, "sha256:"+sha256Hex(member.body))
}

func TestVerifyAcceptsHomebrewPAXHeaderWithExact100BytePhysicalName(t *testing.T) {
	t.Parallel()
	blob, member := homebrewExact100ByteUTF8PAXBottle(t)
	if got := len([]byte(member.header.Name)); got != 100 {
		t.Fatalf("physical name length = %d, want 100", got)
	}
	assertTarHeaderFormat(t, blob, member.header.Name, tar.FormatUnknown)

	result, err := Verify(bytes.NewReader(blob), goExpectationFor(blob), Options{})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	assertInventoryEntry(t, result.Inventory, member.header.Name, EntryRegular, "sha256:"+sha256Hex(member.body))
}

func TestVerifyAcceptsKnownTarFormats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		format tar.Format
		path   string
	}{
		{name: "ustar", format: tar.FormatUSTAR, path: "hello/1.0/share/ustar"},
		{name: "pax", format: tar.FormatPAX, path: "hello/1.0/share/Þpax"},
		{name: "gnu", format: tar.FormatGNU, path: "hello/1.0/share/gnu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			member := regularMember(tt.path, 0o644, tt.name)
			member.header.Format = tt.format
			blob := makeArchive(t, []archiveMember{formulaMember(validFormula()), member})
			assertTarHeaderFormat(t, blob, tt.path, tt.format)
			if _, err := Verify(bytes.NewReader(blob), expectationFor(blob), Options{}); err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
		})
	}
}

func TestVerifyRejectsMalformedUnknownTarFormats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		goBottle bool
		mutate   func(*testing.T) []byte
	}{
		{
			name:     "pax raw name differs",
			goBottle: true,
			mutate: func(t *testing.T) []byte {
				blob, _ := homebrewUTF8PAXBottle(t)
				return patchRawTarHeader(t, blob,
					"go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Þfoo.go",
					func(block []byte) {
						clear(block[:100])
						copy(block[:100], "go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Ðfoo.go")
					},
				)
			},
		},
		{
			name:     "non-nul numeric field",
			goBottle: true,
			mutate: func(t *testing.T) []byte {
				blob, _ := homebrewUTF8PAXBottle(t)
				return patchRawTarHeader(t, blob,
					"go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Þfoo.go",
					func(block []byte) { block[107] = ' ' },
				)
			},
		},
		{
			name:     "non-ascii owner metadata",
			goBottle: true,
			mutate: func(t *testing.T) []byte {
				blob, _ := homebrewUTF8PAXBottle(t)
				return patchRawTarHeader(t, blob,
					"go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Þfoo.go",
					func(block []byte) { copy(block[265:297], "Þ") },
				)
			},
		},
		{
			name:     "non-empty physical prefix",
			goBottle: true,
			mutate: func(t *testing.T) []byte {
				blob, _ := homebrewUTF8PAXBottle(t)
				blob = patchRawTarHeader(t, blob,
					"go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Þfoo.go",
					func(block []byte) {
						clear(block[345:500])
						copy(block[345:500], "../../escape")
					},
				)
				assertTarHeaderFormat(t, blob, "go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Þfoo.go", tar.FormatUnknown)
				return blob
			},
		},
		{
			name:     "signed-only checksum",
			goBottle: true,
			mutate: func(t *testing.T) []byte {
				blob, _ := homebrewUTF8PAXBottle(t)
				blob = patchRawTarHeaderWithChecksum(t, blob,
					"go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Þfoo.go",
					func([]byte) {},
					writeSignedTarChecksum,
				)
				assertTarHeaderFormat(t, blob, "go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Þfoo.go", tar.FormatUnknown)
				return blob
			},
		},
		{
			name: "utf8 ustar without pax",
			mutate: func(t *testing.T) []byte {
				blob := makeArchive(t, []archiveMember{
					formulaMember(validFormula()),
					regularMember("hello/1.0/bin/tool", 0o755, "x"),
				})
				return patchRawTarHeader(t, blob, "hello/1.0/bin/tool", func(block []byte) {
					clear(block[:100])
					copy(block[:100], "hello/1.0/bin/Þtool")
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob := tt.mutate(t)
			expected := expectationFor(blob)
			if tt.goBottle {
				expected = goExpectationFor(blob)
			}
			_, err := Verify(bytes.NewReader(blob), expected, Options{})
			assertErrorCode(t, err, CodeInvalidTar)
		})
	}
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

func assertTarHeaderFormat(t *testing.T, blob []byte, name string, want tar.Format) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("tar entry %q not found", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == name {
			if hdr.Format != want {
				t.Fatalf("tar entry %q format = %v, want %v", name, hdr.Format, want)
			}
			return
		}
	}
}

func homebrewUTF8PAXBottle(t *testing.T) ([]byte, archiveMember) {
	t.Helper()
	const logicalName = "go/1.26.5/libexec/test/fixedbugs/issue27836.dir/Þfoo.go"
	member := regularMember(logicalName, 0o644, "package Þfoo\n")
	member.header.Format = tar.FormatPAX
	blob := makeArchive(t, []archiveMember{
		regularMember("go/1.26.5/.brew/go.rb", 0o644, "class Go < Formula\nend\n"),
		member,
	})
	blob = patchRawTarHeader(t, blob,
		"go/1.26.5/libexec/test/fixedbugs/issue27836.dir/foo.go",
		func(block []byte) {
			clear(block[:100])
			copy(block[:100], logicalName)
		},
	)
	return blob, member
}

func homebrewExact100ByteUTF8PAXBottle(t *testing.T) ([]byte, archiveMember) {
	t.Helper()
	logicalName := "go/1.26.5/" + strings.Repeat("a", 81) + "/Þfoo.go"
	if len([]byte(logicalName)) != 100 {
		t.Fatalf("test path length = %d, want 100", len([]byte(logicalName)))
	}
	member := regularMember(logicalName, 0o644, "package Þfoo\n")
	member.header.Format = tar.FormatPAX
	blob := makeArchive(t, []archiveMember{
		regularMember("go/1.26.5/.brew/go.rb", 0o644, "class Go < Formula\nend\n"),
		member,
	})
	blob = patchRawTarHeader(t, blob, strings.Replace(logicalName, "Þ", "", 1), func(block []byte) {
		clear(block[:100])
		copy(block[:100], logicalName)
	})
	return blob, member
}

func patchRawTarHeader(t *testing.T, blob []byte, rawName string, mutate func([]byte)) []byte {
	t.Helper()
	return patchRawTarHeaderWithChecksum(t, blob, rawName, mutate, writeUnsignedTarChecksum)
}

func patchRawTarHeaderWithChecksum(t *testing.T, blob []byte, rawName string, mutate, writeChecksum func([]byte)) []byte {
	t.Helper()
	raw := decompressGzip(t, blob)
	for offset := 0; offset+tarBlockSize <= len(raw); offset += tarBlockSize {
		block := raw[offset : offset+tarBlockSize]
		name := string(bytes.TrimRight(block[:100], "\x00"))
		if name != rawName || !bytes.Equal(block[257:263], []byte("ustar\x00")) {
			continue
		}
		mutate(block)
		writeChecksum(block)
		return compressGzip(t, raw)
	}
	t.Fatalf("raw tar header %q not found", rawName)
	return nil
}

func writeUnsignedTarChecksum(block []byte) {
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}
	var checksum int64
	for _, b := range block {
		checksum += int64(b)
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", checksum))
}

func writeSignedTarChecksum(block []byte) {
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}
	var checksum int64
	for _, b := range block {
		checksum += int64(int8(b))
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", checksum))
}

func goExpectationFor(blob []byte) Expectation {
	expected := expectationFor(blob)
	expected.Name = "go"
	expected.FullName = "homebrew/core/go"
	expected.FormulaVersion = "1.26.5"
	expected.PkgVersion = "1.26.5"
	return expected
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
	if _, err := VerifyInstalledReceipt([]byte(`{"built_as_bottle":true}`), node, []resolution.Node{node}); err == nil {
		t.Fatal("minimal receipt should not bind installed identity")
	}
	valid := []byte(`{"built_as_bottle":true,"poured_from_bottle":true,"arch":"x86_64","compiler":"gcc","runtime_dependencies":[],"source":{"spec":"stable","tap":"homebrew/core","versions":{"stable":"1.0","version_scheme":0}}}`)
	if _, err := VerifyInstalledReceipt(valid, node, []resolution.Node{node}); err != nil {
		t.Fatal(err)
	}
	wrong := bytes.Replace(valid, []byte(`"compiler":"gcc"`), []byte(`"compiler":"clang"`), 1)
	if _, err := VerifyInstalledReceipt(wrong, node, []resolution.Node{node}); err == nil {
		t.Fatal("unrelated installed compiler family accepted")
	}
}

func TestInstalledReceiptAllowsHostCompilerForAllBottleOnly(t *testing.T) {
	node := resolution.Node{
		Name:           "ca-certificates",
		FullName:       "homebrew/core/ca-certificates",
		FormulaVersion: "2026-07-16",
		PkgVersion:     "2026-07-16",
		Bottle: resolution.Bottle{
			Tag: "all",
			Tab: resolution.BottleTab{Arch: "x86_64", Compiler: "clang"},
		},
	}
	data := installedReceiptData(t, node, "gcc", nil)
	if _, err := VerifyInstalledReceipt(data, node, []resolution.Node{node}); err != nil {
		t.Fatalf("architecture-neutral compiler normalization rejected: %v", err)
	}

	archSpecific := node
	archSpecific.Bottle.Tag = "x86_64_linux"
	if _, err := VerifyInstalledReceipt(data, archSpecific, []resolution.Node{archSpecific}); err == nil {
		t.Fatal("cross-family compiler change accepted for architecture-specific bottle")
	}
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, node, "malicious", nil), node, []resolution.Node{node}); err == nil {
		t.Fatal("unknown compiler accepted for architecture-neutral bottle")
	}
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, node, "gcc-.", nil), node, []resolution.Node{node}); err == nil {
		t.Fatal("malformed compiler version accepted for architecture-neutral bottle")
	}
}

func TestInstalledReceiptUsesRootSpecificRecursiveSignedGraph(t *testing.T) {
	root := resolvedReceiptNode("little-cms2", "2.17", 0, "2.17")
	root.Bottle.Tag = "x86_64_linux"
	root.Bottle.Tab = resolution.BottleTab{
		Arch:     "x86_64",
		Compiler: "gcc",
		// The historical bottle tab contains a stale transitive dependency and
		// does not describe the current recursive signed graph.
		Dependencies: []resolution.RuntimeDependency{
			{FullName: "jpeg-turbo", Version: "3.0.4", PkgVersion: "3.0.4", DeclaredDirectly: true},
			{FullName: "libtiff", Version: "4.7.0", PkgVersion: "4.7.0", DeclaredDirectly: true},
			{FullName: "obsolete-codec", Version: "1.0", PkgVersion: "1.0"},
		},
	}
	root.Dependencies = []resolution.Requirement{
		{Name: "jpeg-turbo", Minimum: "3.0.4", Direct: true},
		{Name: "libtiff", Minimum: "4.7.0", Direct: true},
	}

	jpeg := resolvedReceiptNode("jpeg-turbo", "3.1.0", 0, "3.1.0")
	libtiff := resolvedReceiptNode("libtiff", "4.7.1", 0, "4.7.1")
	libtiff.Bottle.Tab.Dependencies = []resolution.RuntimeDependency{
		{FullName: "lz4", Version: "1.9.4", PkgVersion: "1.9.4"},
		{FullName: "xz", Version: "5.6.4", PkgVersion: "5.6.4"},
		{FullName: "zlib", Version: "1.3.1", PkgVersion: "1.3.1"},
		{FullName: "zstd", Version: "1.5.6", PkgVersion: "1.5.6"},
	}
	libtiff.Dependencies = []resolution.Requirement{
		{Name: "lz4", Minimum: "1.9.4", Direct: true},
		{Name: "xz", Minimum: "5.6.4", Direct: true},
		{Name: "zlib", Minimum: "1.3.1", Direct: true},
		{Name: "zstd", Minimum: "1.5.6", Direct: true},
	}

	closure := []resolution.Node{
		root,
		jpeg,
		libtiff,
		resolvedReceiptNode("lz4", "1.10.0", 0, "1.10.0"),
		resolvedReceiptNode("xz", "5.8.3", 0, "5.8.3"),
		resolvedReceiptNode("zlib", "1.3.2", 0, "1.3.2"),
		resolvedReceiptNode("zstd", "1.5.7", 1, "1.5.7_1"),
		resolvedReceiptNode("other-root", "9.0", 0, "9.0"),
	}
	actual := []ReceiptDependency{
		{FullName: "jpeg-turbo", Version: "3.1.0", PkgVersion: "3.1.0", DeclaredDirectly: true},
		{FullName: "libtiff", Version: "4.7.1", PkgVersion: "4.7.1", DeclaredDirectly: true},
		{FullName: "lz4", Version: "1.10.0", PkgVersion: "1.10.0"},
		{FullName: "xz", Version: "5.8.3", PkgVersion: "5.8.3"},
		{FullName: "zlib", Version: "1.3.2", PkgVersion: "1.3.2"},
		{FullName: "zstd", Version: "1.5.7", Revision: 1, PkgVersion: "1.5.7_1"},
	}
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, root, "gcc", actual), root, closure); err != nil {
		t.Fatalf("current recursive signed graph rejected: %v", err)
	}

	tests := map[string]func([]ReceiptDependency, []resolution.Node) ([]ReceiptDependency, []resolution.Node){
		"tampered formula version": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			deps[0].Version = "999"
			return deps, nodes
		},
		"tampered revision": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			deps[0].Revision = 999
			return deps, nodes
		},
		"tampered pkg version": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			deps[0].PkgVersion = "999"
			return deps, nodes
		},
		"missing recursive dependency": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			return deps[1:], nodes
		},
		"cross-root dependency": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			return append(deps, ReceiptDependency{FullName: "other-root", Version: "9.0", PkgVersion: "9.0"}), nodes
		},
		"duplicate receipt dependency": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			return append(deps, deps[0]), nodes
		},
		"duplicate closure node": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			return deps, append(nodes, nodes[3])
		},
		"duplicate graph edge": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			nodes[0].Dependencies = append(nodes[0].Dependencies, nodes[0].Dependencies[0])
			return deps, nodes
		},
		"dependency cycle": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			nodes[3].Dependencies = []resolution.Requirement{{Name: "little-cms2", Minimum: "2.17"}}
			return deps, nodes
		},
		"missing graph node": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			return deps, append(nodes[:3], nodes[4:]...)
		},
		"inconsistent authenticated minimum": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			nodes[0].Dependencies[0].Minimum = "3.0.3"
			return deps, nodes
		},
		"inconsistent authenticated revision": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			nodes[0].Dependencies[0].Revision = 1
			return deps, nodes
		},
		"inconsistent authenticated rebuild": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			nodes[0].Dependencies[0].BottleRebuild = 1
			return deps, nodes
		},
		"duplicate historical minimum": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			nodes[0].Bottle.Tab.Dependencies = append(nodes[0].Bottle.Tab.Dependencies, nodes[0].Bottle.Tab.Dependencies[0])
			return deps, nodes
		},
		"selected version below minimum": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			nodes[3].PkgVersion = "1.8.0"
			nodes[3].FormulaVersion = "1.8.0"
			return deps, nodes
		},
		"invented bottle rebuild": func(deps []ReceiptDependency, nodes []resolution.Node) ([]ReceiptDependency, []resolution.Node) {
			deps[0].BottleRebuild = 999
			return deps, nodes
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			deps := append([]ReceiptDependency(nil), actual...)
			nodes := cloneResolutionNodes(t, closure)
			deps, nodes = mutate(deps, nodes)
			if _, err := VerifyInstalledReceipt(installedReceiptData(t, root, "gcc", deps), root, nodes); err == nil {
				t.Fatal("invalid generated receipt or signed graph accepted")
			}
		})
	}
}

func TestInstalledReceiptIncludesSelectedHistoricalDirectFormulaRoots(t *testing.T) {
	webp := resolvedReceiptNode("webp", "1.6.0", 0, "1.6.0")
	webp.Bottle.Tag = "x86_64_linux"
	webp.Bottle.Tab = resolution.BottleTab{
		Arch:     "x86_64",
		Compiler: "gcc",
		Dependencies: []resolution.RuntimeDependency{
			{FullName: "giflib", Version: "5.2.2", PkgVersion: "5.2.2", DeclaredDirectly: true},
			{FullName: "jpeg-turbo", Version: "3.1.1", PkgVersion: "3.1.1", DeclaredDirectly: true},
			{FullName: "libpng", Version: "1.6.50", PkgVersion: "1.6.50", DeclaredDirectly: true},
			// The embedded historical WebP Formula still declares libtiff,
			// while the current signed WebP graph no longer does. Deno selects
			// libtiff independently through little-cms2.
			{FullName: "libtiff", Version: "4.7.0", PkgVersion: "4.7.0", DeclaredDirectly: true},
			{FullName: "obsolete-codec", Version: "1.0", PkgVersion: "1.0"},
		},
	}
	webp.Dependencies = []resolution.Requirement{
		{Name: "giflib", Minimum: "5.2.2", Direct: true},
		{Name: "jpeg-turbo", Minimum: "3.1.1", Direct: true},
		{Name: "libpng", Minimum: "1.6.50", Direct: true},
	}

	giflib := resolvedReceiptNode("giflib", "6.1.3", 0, "6.1.3")
	jpeg := resolvedReceiptNode("jpeg-turbo", "3.2.0", 0, "3.2.0")
	libpng := resolvedReceiptNode("libpng", "1.6.58", 0, "1.6.58")
	libpng.Dependencies = []resolution.Requirement{{Name: "zlib-ng-compat", Minimum: "2.3.3_1", Revision: 1, Direct: true}}
	zlib := resolvedReceiptNode("zlib-ng-compat", "2.3.3", 1, "2.3.3_1")
	libtiff := resolvedReceiptNode("libtiff", "4.7.2", 0, "4.7.2")
	libtiff.Dependencies = []resolution.Requirement{
		{Name: "jpeg-turbo", Minimum: "3.1.1", Direct: true},
		{Name: "xz", Minimum: "5.8.1", Direct: true},
		{Name: "lz4", Minimum: "1.10.0", BottleRebuild: 1, Direct: true},
		{Name: "zstd", Minimum: "1.5.7", Direct: true},
		{Name: "zlib-ng-compat", Minimum: "2.3.3_1", Revision: 1, Direct: true},
	}
	lz4 := resolvedReceiptNode("lz4", "1.10.0", 0, "1.10.0")
	lz4.BottleRebuild = 1
	xz := resolvedReceiptNode("xz", "5.8.3", 0, "5.8.3")
	zstd := resolvedReceiptNode("zstd", "1.5.7", 1, "1.5.7_1")
	obsolete := resolvedReceiptNode("obsolete-codec", "1.0", 0, "1.0")
	unrelated := resolvedReceiptNode("little-cms2", "2.19", 0, "2.19")
	closure := []resolution.Node{webp, giflib, jpeg, libpng, zlib, libtiff, lz4, xz, zstd, obsolete, unrelated}

	actual := []ReceiptDependency{
		{FullName: "giflib", Version: "6.1.3", PkgVersion: "6.1.3", DeclaredDirectly: true},
		{FullName: "jpeg-turbo", Version: "3.2.0", PkgVersion: "3.2.0", DeclaredDirectly: true},
		{FullName: "libpng", Version: "1.6.58", PkgVersion: "1.6.58", DeclaredDirectly: true},
		{FullName: "zlib-ng-compat", Version: "2.3.3", Revision: 1, PkgVersion: "2.3.3_1"},
		{FullName: "libtiff", Version: "4.7.2", PkgVersion: "4.7.2", DeclaredDirectly: true},
		{FullName: "lz4", Version: "1.10.0", BottleRebuild: 1, PkgVersion: "1.10.0"},
		{FullName: "xz", Version: "5.8.3", PkgVersion: "5.8.3"},
		{FullName: "zstd", Version: "1.5.7", Revision: 1, PkgVersion: "1.5.7_1"},
	}
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, webp, "gcc", actual), webp, closure); err != nil {
		t.Fatalf("selected historical direct Formula root rejected: %v", err)
	}

	missingDirect := cloneResolutionNodes(t, closure)
	missingDirect[0].Bottle.Tab.Dependencies = append(missingDirect[0].Bottle.Tab.Dependencies,
		resolution.RuntimeDependency{FullName: "missing-direct", Version: "1.0", PkgVersion: "1.0", DeclaredDirectly: true})
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, missingDirect[0], "gcc", actual), missingDirect[0], missingDirect); err != nil {
		t.Fatalf("out-of-closure historical direct Formula should remain evidence-only: %v", err)
	}

	historicalCurrentCycle := cloneResolutionNodes(t, closure)
	historicalCurrentCycle[5].Dependencies = append(historicalCurrentCycle[5].Dependencies,
		resolution.Requirement{Name: "webp", Minimum: "1.6.0", Direct: true})
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, webp, "gcc", actual), webp, historicalCurrentCycle); err != nil {
		t.Fatalf("historical branch back-edge to receipt root rejected: %v", err)
	}

	withUnrelated := append(append([]ReceiptDependency(nil), actual...), ReceiptDependency{FullName: "little-cms2", Version: "2.19", PkgVersion: "2.19"})
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, webp, "gcc", withUnrelated), webp, closure); err == nil {
		t.Fatal("unrelated selected closure node was accepted")
	}
	withHistoricalTransitive := append(append([]ReceiptDependency(nil), actual...), ReceiptDependency{FullName: "obsolete-codec", Version: "1.0", PkgVersion: "1.0"})
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, webp, "gcc", withHistoricalTransitive), webp, closure); err == nil {
		t.Fatal("historical transitive bottle entry was accepted as a receipt root")
	}
	missingHistoricalRoot := slices.DeleteFunc(append([]ReceiptDependency(nil), actual...), func(dep ReceiptDependency) bool { return dep.FullName == "libtiff" })
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, webp, "gcc", missingHistoricalRoot), webp, closure); err == nil {
		t.Fatal("selected historical direct Formula root was allowed to disappear")
	}
}

func TestNormalizeInstalledReceiptDependenciesUsesExactVerifiedPolicy(t *testing.T) {
	root, closure, expected := normalizationReceiptFixture()
	incomplete := []ReceiptDependency{expected[0]}
	input := installedReceiptData(t, root, "gcc", incomplete)

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		t.Fatal(err)
	}
	fields["name"] = json.RawMessage(`"cairo"`)
	fields["full_name"] = json.RawMessage(`"homebrew/core/cairo"`)
	fields["pkg_version"] = json.RawMessage(`"1.18.4"`)
	fields["custom_homebrew_metadata"] = json.RawMessage(`{"retained":true,"number":1}`)
	input, err := json.MarshalIndent(fields, "", "    ")
	if err != nil {
		t.Fatal(err)
	}

	normalized, err := NormalizeInstalledReceiptDependencies(input, root, closure)
	if err != nil {
		t.Fatal(err)
	}
	if !normalized.Changed || normalized.BeforeDependencyCount != 1 || normalized.AfterDependencyCount != len(expected) {
		t.Fatalf("normalization = %#v", normalized)
	}
	if _, err := VerifyInstalledReceipt(normalized.Data, root, closure); err != nil {
		t.Fatalf("normalized receipt did not pass strict verification: %v", err)
	}

	var beforeObject, afterObject map[string]any
	if err := json.Unmarshal(input, &beforeObject); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(normalized.Data, &afterObject); err != nil {
		t.Fatal(err)
	}
	delete(beforeObject, "runtime_dependencies")
	delete(afterObject, "runtime_dependencies")
	if !reflect.DeepEqual(beforeObject, afterObject) {
		t.Fatalf("non-dependency receipt fields changed:\nbefore=%#v\nafter=%#v", beforeObject, afterObject)
	}

	var receipt installReceipt
	if err := json.Unmarshal(normalized.Data, &receipt); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(receipt.RuntimeDeps, expected) {
		t.Fatalf("normalized dependencies = %#v, want %#v", receipt.RuntimeDeps, expected)
	}
	second, err := NormalizeInstalledReceiptDependencies(input, root, closure)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(normalized.Data, second.Data) {
		t.Fatal("normalization output is not deterministic")
	}

	valid := installedReceiptData(t, root, "gcc", expected)
	unchanged, err := NormalizeInstalledReceiptDependencies(valid, root, closure)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Changed || unchanged.Data != nil {
		t.Fatalf("valid receipt was rewritten: %#v", unchanged)
	}
}

func TestNormalizeInstalledReceiptDependenciesRejectsTampering(t *testing.T) {
	root, closure, expected := normalizationReceiptFixture()
	validIncomplete := []ReceiptDependency{expected[0]}
	tests := map[string]func([]byte) []byte{
		"extra dependency": func(_ []byte) []byte {
			return installedReceiptData(t, root, "gcc", []ReceiptDependency{
				expected[0],
				{FullName: "unrelated", Version: "9", PkgVersion: "9"},
			})
		},
		"wrong version": func(_ []byte) []byte {
			deps := append([]ReceiptDependency(nil), validIncomplete...)
			deps[0].Version = "999"
			return installedReceiptData(t, root, "gcc", deps)
		},
		"wrong revision": func(_ []byte) []byte {
			deps := append([]ReceiptDependency(nil), validIncomplete...)
			deps[0].Revision++
			return installedReceiptData(t, root, "gcc", deps)
		},
		"wrong pkg version": func(_ []byte) []byte {
			deps := append([]ReceiptDependency(nil), validIncomplete...)
			deps[0].PkgVersion = "999"
			return installedReceiptData(t, root, "gcc", deps)
		},
		"invented bottle rebuild": func(_ []byte) []byte {
			deps := append([]ReceiptDependency(nil), validIncomplete...)
			deps[0].BottleRebuild = expected[0].BottleRebuild + 7
			return installedReceiptData(t, root, "gcc", deps)
		},
		"duplicate dependency": func(_ []byte) []byte {
			return installedReceiptData(t, root, "gcc", append(append([]ReceiptDependency(nil), validIncomplete...), validIncomplete[0]))
		},
		"complete wrong set": func(_ []byte) []byte {
			deps := append([]ReceiptDependency(nil), expected...)
			deps[len(deps)-1].FullName = "unrelated"
			return installedReceiptData(t, root, "gcc", deps)
		},
		"tampered architecture": func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"arch":"x86_64"`), []byte(`"arch":"arm64"`), 1)
		},
		"tampered source identity": func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"stable":"1.18.4"`), []byte(`"stable":"999"`), 1)
		},
		"missing runtime dependencies": func(data []byte) []byte {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, "runtime_dependencies")
			out, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			return out
		},
		"null runtime dependencies": func(data []byte) []byte {
			return bytes.Replace(data, mustJSON(t, validIncomplete), []byte("null"), 1)
		},
		"object runtime dependencies": func(data []byte) []byte {
			return bytes.Replace(data, mustJSON(t, validIncomplete), []byte(`{}`), 1)
		},
		"duplicate JSON key": func(data []byte) []byte {
			return append([]byte(`{"runtime_dependencies":[] ,`), data[1:]...)
		},
	}
	base := installedReceiptData(t, root, "gcc", validIncomplete)
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := mutate(append([]byte(nil), base...))
			if result, err := NormalizeInstalledReceiptDependencies(input, root, closure); err == nil {
				t.Fatalf("tampered receipt normalized: %#v", result)
			}
		})
	}
}

func TestNormalizeInstalledReceiptDependenciesDoesNotRelaxPreinstallVerification(t *testing.T) {
	root, _, expected := normalizationReceiptFixture()
	expectation := ExpectationFromNode(root)
	expectation.Dependencies = expected
	incomplete := installedReceiptData(t, root, "gcc", expected[:1])
	if _, err := validateReceipt(incomplete, expectation); err == nil {
		t.Fatal("pre-install receipt verification accepted an incomplete dependency set")
	}
}

func normalizationReceiptFixture() (resolution.Node, []resolution.Node, []ReceiptDependency) {
	root := resolvedReceiptNode("cairo", "1.18.4", 0, "1.18.4")
	root.Bottle.Tag = "x86_64_linux"
	root.Bottle.Tab = resolution.BottleTab{
		Arch:     "x86_64",
		Compiler: "gcc",
		Dependencies: []resolution.RuntimeDependency{
			{FullName: "libpng", Version: "1.6.50", PkgVersion: "1.6.50", DeclaredDirectly: true},
			{FullName: "obsolete-transitive", Version: "1", PkgVersion: "1"},
		},
	}
	root.Dependencies = []resolution.Requirement{{Name: "pixman", Minimum: "0.46.4", Direct: true}}
	pixman := resolvedReceiptNode("pixman", "0.46.4", 0, "0.46.4")
	pixman.BottleRebuild = 1
	libpng := resolvedReceiptNode("libpng", "1.6.50", 0, "1.6.50")
	libpng.Dependencies = []resolution.Requirement{{Name: "zlib-ng-compat", Minimum: "2.2.4", Direct: true}}
	zlib := resolvedReceiptNode("zlib-ng-compat", "2.3.3", 1, "2.3.3_1")
	closure := []resolution.Node{root, pixman, libpng, zlib, resolvedReceiptNode("unrelated", "9", 0, "9")}
	expected := []ReceiptDependency{
		{FullName: "libpng", Version: "1.6.50", PkgVersion: "1.6.50", DeclaredDirectly: true},
		{FullName: "pixman", Version: "0.46.4", BottleRebuild: 1, PkgVersion: "0.46.4", DeclaredDirectly: true},
		{FullName: "zlib-ng-compat", Version: "2.3.3", Revision: 1, PkgVersion: "2.3.3_1"},
	}
	return root, closure, expected
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInstalledReceiptValidatesNestedHistoricalDirectEdges(t *testing.T) {
	root := resolvedReceiptNode("root", "1", 0, "1")
	root.Bottle.Tag = "x86_64_linux"
	root.Bottle.Tab = resolution.BottleTab{
		Arch:         "x86_64",
		Compiler:     "gcc",
		Dependencies: []resolution.RuntimeDependency{{FullName: "parent", Version: "1", PkgVersion: "1", DeclaredDirectly: true}},
	}
	root.Dependencies = []resolution.Requirement{{Name: "parent", Minimum: "1", Direct: true}}

	parent := resolvedReceiptNode("parent", "1", 0, "1")
	parent.Bottle.Tab.Dependencies = []resolution.RuntimeDependency{
		{FullName: "bridge", Version: "1", PkgVersion: "1", DeclaredDirectly: true},
		// stale was direct when this bottle was built, but remains selected
		// recursively through bridge in the current signed graph.
		{FullName: "stale", Version: "1", PkgVersion: "1", DeclaredDirectly: true},
	}
	parent.Dependencies = []resolution.Requirement{{Name: "bridge", Minimum: "1", Direct: true}}

	bridge := resolvedReceiptNode("bridge", "1", 0, "1")
	bridge.Bottle.Tab.Dependencies = []resolution.RuntimeDependency{{FullName: "stale", Version: "1", PkgVersion: "1", DeclaredDirectly: true}}
	bridge.Dependencies = []resolution.Requirement{{Name: "stale", Minimum: "1", Direct: true}}

	stale := resolvedReceiptNode("stale", "1", 0, "1")
	stale.Bottle.Tab.Dependencies = []resolution.RuntimeDependency{{FullName: "leaf", Version: "1", PkgVersion: "1", DeclaredDirectly: true}}
	stale.Dependencies = []resolution.Requirement{{Name: "leaf", Minimum: "1", Direct: true}}
	leaf := resolvedReceiptNode("leaf", "1", 0, "1")
	other := resolvedReceiptNode("other-root", "1", 0, "1")

	closure := []resolution.Node{root, parent, bridge, stale, leaf, other}
	actual := []ReceiptDependency{
		{FullName: "parent", Version: "1", PkgVersion: "1"},
		{FullName: "bridge", Version: "1", PkgVersion: "1"},
		{FullName: "stale", Version: "1", PkgVersion: "1"},
		{FullName: "leaf", Version: "1", PkgVersion: "1"},
	}
	data := installedReceiptData(t, root, "gcc", actual)
	if _, err := VerifyInstalledReceipt(data, root, closure); err != nil {
		t.Fatalf("selected nested stale direct dependency rejected: %v", err)
	}

	t.Run("stale direct minimum is authenticated", func(t *testing.T) {
		nodes := cloneResolutionNodes(t, closure)
		nodes[1].Bottle.Tab.Dependencies[1].PkgVersion = "2"
		nodes[1].Bottle.Tab.Dependencies[1].Version = "2"
		if _, err := VerifyInstalledReceipt(data, root, nodes); err == nil {
			t.Fatal("unsatisfied stale direct minimum accepted")
		}
	})

	t.Run("historical branch may terminate at receipt root", func(t *testing.T) {
		nodes := cloneResolutionNodes(t, closure)
		nodes[4].Bottle.Tab.Dependencies = []resolution.RuntimeDependency{{FullName: "root", Version: "1", PkgVersion: "1", DeclaredDirectly: true}}
		if _, err := VerifyInstalledReceipt(data, root, nodes); err != nil {
			t.Fatalf("historical back-edge to receipt root rejected: %v", err)
		}
	})

	t.Run("selected historical direct edge justifies another closure node", func(t *testing.T) {
		nodes := cloneResolutionNodes(t, closure)
		nodes[1].Bottle.Tab.Dependencies = append(nodes[1].Bottle.Tab.Dependencies,
			resolution.RuntimeDependency{FullName: "other-root", Version: "1", PkgVersion: "1", DeclaredDirectly: true})
		withOther := append(append([]ReceiptDependency(nil), actual...), ReceiptDependency{FullName: "other-root", Version: "1", PkgVersion: "1"})
		if _, err := VerifyInstalledReceipt(installedReceiptData(t, root, "gcc", withOther), root, nodes); err != nil {
			t.Fatalf("selected historical direct edge rejected: %v", err)
		}
	})

	t.Run("out-of-closure historical direct edge remains evidence-only", func(t *testing.T) {
		nodes := cloneResolutionNodes(t, closure)
		nodes[1].Bottle.Tab.Dependencies = append(nodes[1].Bottle.Tab.Dependencies,
			resolution.RuntimeDependency{FullName: "missing", Version: "1", PkgVersion: "1", DeclaredDirectly: true})
		if _, err := VerifyInstalledReceipt(data, root, nodes); err != nil {
			t.Fatalf("out-of-closure historical edge should not broaden receipt: %v", err)
		}
	})

	t.Run("historical cycle among selected closure nodes is finite", func(t *testing.T) {
		nodes := cloneResolutionNodes(t, closure)
		nodes[4].Bottle.Tab.Dependencies = []resolution.RuntimeDependency{{FullName: "parent", Version: "1", PkgVersion: "1", DeclaredDirectly: true}}
		if _, err := VerifyInstalledReceipt(data, root, nodes); err != nil {
			t.Fatalf("authenticated historical cycle rejected: %v", err)
		}
	})

	t.Run("current-only cycle is rejected", func(t *testing.T) {
		nodes := cloneResolutionNodes(t, closure)
		nodes[4].Dependencies = []resolution.Requirement{{Name: "parent", Minimum: "1", Direct: true}}
		if _, err := VerifyInstalledReceipt(data, root, nodes); err == nil {
			t.Fatal("current signed dependency cycle accepted")
		}
	})
}

func TestInstalledReceiptRejectsCurrentCycleAfterHistoricalMemoization(t *testing.T) {
	root := resolvedReceiptNode("root", "1", 0, "1")
	root.Bottle.Tag = "x86_64_linux"
	root.Bottle.Tab = resolution.BottleTab{
		Arch:         "x86_64",
		Compiler:     "gcc",
		Dependencies: []resolution.RuntimeDependency{{FullName: "a", Version: "1", PkgVersion: "1", DeclaredDirectly: true}},
	}
	root.Dependencies = []resolution.Requirement{{Name: "z", Minimum: "1", Direct: true}}
	a := resolvedReceiptNode("a", "1", 0, "1")
	a.Dependencies = []resolution.Requirement{{Name: "root", Minimum: "1", Direct: true}}
	z := resolvedReceiptNode("z", "1", 0, "1")
	z.Dependencies = []resolution.Requirement{{Name: "a", Minimum: "1", Direct: true}}
	actual := []ReceiptDependency{
		{FullName: "a", Version: "1", PkgVersion: "1"},
		{FullName: "z", Version: "1", PkgVersion: "1"},
	}
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, root, "gcc", actual), root, []resolution.Node{root, a, z}); err == nil {
		t.Fatal("current cycle hidden by historical traversal memoization was accepted")
	}
}

func TestInstalledReceiptAllowsUnspecifiedCurrentMinimums(t *testing.T) {
	root := resolvedReceiptNode("root", "1", 0, "1")
	root.Bottle.Tag = "x86_64_linux"
	root.Bottle.Tab = resolution.BottleTab{
		Arch:         "x86_64",
		Compiler:     "gcc",
		Dependencies: []resolution.RuntimeDependency{{FullName: "child", Version: "2", PkgVersion: "2", DeclaredDirectly: true}},
	}
	root.Dependencies = []resolution.Requirement{{Name: "child"}}
	child := resolvedReceiptNode("child", "2", 0, "2")
	actual := []ReceiptDependency{{FullName: "child", Version: "2", PkgVersion: "2"}}
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, root, "gcc", actual), root, []resolution.Node{root, child}); err != nil {
		t.Fatalf("unspecified current minimum did not inherit authenticated bottle minimum: %v", err)
	}

	unbounded := root
	unbounded.Bottle.Tab.Dependencies = nil
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, unbounded, "gcc", actual), unbounded, []resolution.Node{unbounded, child}); err != nil {
		t.Fatalf("unbounded current dependency was rejected: %v", err)
	}

	partial := root
	partial.Dependencies = []resolution.Requirement{{Name: "child", Revision: 7}}
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, partial, "gcc", actual), partial, []resolution.Node{partial, child}); err == nil {
		t.Fatal("revision without a minimum version was accepted")
	}
}

func TestInstalledReceiptRejectsCurrentCycleAfterHistoricalPrefix(t *testing.T) {
	root := resolvedReceiptNode("root", "1", 0, "1")
	root.Bottle.Tag = "x86_64_linux"
	root.Bottle.Tab = resolution.BottleTab{
		Arch:     "x86_64",
		Compiler: "gcc",
		Dependencies: []resolution.RuntimeDependency{
			{FullName: "historical-root", Version: "1", PkgVersion: "1", DeclaredDirectly: true},
		},
	}
	historicalRoot := resolvedReceiptNode("historical-root", "1", 0, "1")
	historicalRoot.Dependencies = []resolution.Requirement{{Name: "cycle-a", Minimum: "1", Direct: true}}
	cycleA := resolvedReceiptNode("cycle-a", "1", 0, "1")
	cycleA.Dependencies = []resolution.Requirement{{Name: "cycle-b", Minimum: "1", Direct: true}}
	cycleB := resolvedReceiptNode("cycle-b", "1", 0, "1")
	cycleB.Dependencies = []resolution.Requirement{{Name: "cycle-a", Minimum: "1", Direct: true}}
	closure := []resolution.Node{root, historicalRoot, cycleA, cycleB}
	actual := []ReceiptDependency{
		{FullName: "historical-root", Version: "1", PkgVersion: "1"},
		{FullName: "cycle-a", Version: "1", PkgVersion: "1"},
		{FullName: "cycle-b", Version: "1", PkgVersion: "1"},
	}
	if _, err := VerifyInstalledReceipt(installedReceiptData(t, root, "gcc", actual), root, closure); err == nil {
		t.Fatal("current-only cycle hidden behind historical prefix was accepted")
	}
}

func cloneResolutionNodes(t *testing.T, nodes []resolution.Node) []resolution.Node {
	t.Helper()
	data, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	var cloned []resolution.Node
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func installedReceiptData(t *testing.T, node resolution.Node, compiler string, deps []ReceiptDependency) []byte {
	t.Helper()
	built, poured := true, true
	versionScheme := node.VersionScheme
	data, err := json.Marshal(installReceipt{
		BuiltAsBottle:    &built,
		PouredFromBottle: &poured,
		Arch:             node.Bottle.Tab.Arch,
		Compiler:         compiler,
		RuntimeDeps:      deps,
		Source: receiptSource{
			Spec: "stable",
			Tap:  "homebrew/core",
			Versions: receiptVersions{
				Stable:        node.FormulaVersion,
				VersionScheme: &versionScheme,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func resolvedReceiptNode(name, version string, revision int, pkgVersion string) resolution.Node {
	return resolution.Node{
		Name:            name,
		FullName:        "homebrew/core/" + name,
		FormulaVersion:  version,
		FormulaRevision: revision,
		PkgVersion:      pkgVersion,
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
