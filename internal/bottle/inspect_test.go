package bottle

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestInspectForCatalogDiscoversCanonicalTransientDependencies(t *testing.T) {
	t.Parallel()
	dependencies := []ReceiptDependency{
		{
			FullName:         "libfoo",
			Version:          "2.0",
			PkgVersion:       "2.0",
			DeclaredDirectly: true,
		},
		{
			FullName:      "acme/tools/widget",
			Version:       "3.1",
			Revision:      2,
			BottleRebuild: 1,
			PkgVersion:    "3.1_2",
		},
	}
	blob := catalogBottle(t, catalogReceipt(t, dependencies))
	expected := catalogExpectationFor(blob)
	inspection, err := InspectForCatalog(bytes.NewReader(blob), expected, Options{})
	if err != nil {
		t.Fatalf("InspectForCatalog() error = %v", err)
	}

	want := []ReceiptDependency{
		{
			FullName:      "acme/tools/widget",
			Version:       "3.1",
			Revision:      2,
			BottleRebuild: 1,
			PkgVersion:    "3.1_2",
		},
		{
			FullName:         "homebrew/core/libfoo",
			Version:          "2.0",
			PkgVersion:       "2.0",
			DeclaredDirectly: true,
		},
	}
	if !reflect.DeepEqual(inspection.RuntimeDependencies, want) {
		t.Fatalf("RuntimeDependencies = %#v, want %#v", inspection.RuntimeDependencies, want)
	}
	if inspection.Result == nil || inspection.Receipt == nil || inspection.Receipt.RuntimeDepCount != len(want) {
		t.Fatalf("inspection result = %#v", inspection)
	}
	if len(inspection.Inventory) == 0 || inspection.InventorySHA256 == "" {
		t.Fatalf("static inventory was not returned: %#v", inspection.Result)
	}
	if !bytes.Equal(inspection.FormulaSource, []byte(validFormula())) {
		t.Fatal("exact Formula source was not returned transiently")
	}

	encoded, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte("runtime_dependencies"),
		[]byte("homebrew/core/libfoo"),
		[]byte("acme/tools/widget"),
		[]byte(validFormula()),
	} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("transient catalog input %q leaked into JSON evidence: %s", forbidden, encoded)
		}
	}
}

func TestInspectForCatalogDoesNotRelaxNormalDependencyVerification(t *testing.T) {
	t.Parallel()
	actualDependencies := []ReceiptDependency{{
		FullName:         "libbar",
		Version:          "2.0",
		PkgVersion:       "2.0",
		DeclaredDirectly: true,
	}}
	blob := catalogBottle(t, catalogReceipt(t, actualDependencies))
	expected := expectationFor(blob)

	_, err := Verify(bytes.NewReader(blob), expected, Options{})
	assertErrorCode(t, err, CodeInvalidReceipt)

	node := resolution.Node{
		Name:              expected.Name,
		FullName:          expected.FullName,
		FormulaVersion:    expected.FormulaVersion,
		FormulaRevision:   expected.FormulaRevision,
		PkgVersion:        expected.PkgVersion,
		VersionScheme:     expected.VersionScheme,
		BottleRebuild:     expected.BottleRebuild,
		UpstreamFormulaID: expected.FullName,
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
	_, err = VerifyNode(bytes.NewReader(blob), node, Options{})
	assertErrorCode(t, err, CodeInvalidReceipt)

	inspection, err := InspectForCatalog(bytes.NewReader(blob), catalogExpectationFor(blob), Options{})
	if err != nil {
		t.Fatalf("InspectForCatalog() error = %v", err)
	}
	want := []ReceiptDependency{{
		FullName:         "homebrew/core/libbar",
		Version:          "2.0",
		PkgVersion:       "2.0",
		DeclaredDirectly: true,
	}}
	if !reflect.DeepEqual(inspection.RuntimeDependencies, want) {
		t.Fatalf("RuntimeDependencies = %#v, want %#v", inspection.RuntimeDependencies, want)
	}
}

func TestInspectForCatalogRejectsMalformedOrDuplicateDependencies(t *testing.T) {
	t.Parallel()
	valid := ReceiptDependency{FullName: "libfoo", Version: "2.0", PkgVersion: "2.0"}
	tooMany := make([]ReceiptDependency, maxDiscoveredRuntimeDependencies+1)
	for index := range tooMany {
		tooMany[index] = ReceiptDependency{
			FullName:      "dep" + strconv.Itoa(index),
			Version:       "1",
			PkgVersion:    "1",
			Revision:      0,
			BottleRebuild: 0,
		}
	}
	tests := []struct {
		name         string
		dependencies []ReceiptDependency
	}{
		{name: "missing full name", dependencies: []ReceiptDependency{{Version: "2.0", PkgVersion: "2.0"}}},
		{name: "URL identity", dependencies: []ReceiptDependency{{FullName: "https://example.invalid/dep", Version: "2.0", PkgVersion: "2.0"}}},
		{name: "uppercase identity", dependencies: []ReceiptDependency{{FullName: "Libfoo", Version: "2.0", PkgVersion: "2.0"}}},
		{name: "empty version", dependencies: []ReceiptDependency{{FullName: "libfoo", PkgVersion: "2.0"}}},
		{name: "version whitespace", dependencies: []ReceiptDependency{{FullName: "libfoo", Version: "2. 0", PkgVersion: "2. 0"}}},
		{name: "negative revision", dependencies: []ReceiptDependency{{FullName: "libfoo", Version: "2.0", Revision: -1, PkgVersion: "2.0"}}},
		{name: "negative rebuild", dependencies: []ReceiptDependency{{FullName: "libfoo", Version: "2.0", BottleRebuild: -1, PkgVersion: "2.0"}}},
		{name: "inconsistent package version", dependencies: []ReceiptDependency{{FullName: "libfoo", Version: "2.0", Revision: 1, PkgVersion: "2.0"}}},
		{name: "duplicate exact identity", dependencies: []ReceiptDependency{valid, valid}},
		{name: "duplicate canonical core identity", dependencies: []ReceiptDependency{valid, {FullName: "homebrew/core/libfoo", Version: "2.0", PkgVersion: "2.0"}}},
		{name: "dependency count", dependencies: tooMany},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			blob := catalogBottle(t, catalogReceipt(t, test.dependencies))
			_, err := InspectForCatalog(bytes.NewReader(blob), catalogExpectationFor(blob), Options{})
			assertErrorCode(t, err, CodeInvalidReceipt)
		})
	}
}

func TestInspectForCatalogRequiresDependencyArrayWhenReceiptExists(t *testing.T) {
	t.Parallel()
	validData := catalogReceipt(t, nil)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(validData, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "runtime_dependencies")
	missing, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		receipt []byte
	}{
		{name: "missing", receipt: missing},
		{name: "null", receipt: bytes.Replace(validData, []byte(`"runtime_dependencies":[]`), []byte(`"runtime_dependencies":null`), 1)},
		{name: "object", receipt: bytes.Replace(validData, []byte(`"runtime_dependencies":[]`), []byte(`"runtime_dependencies":{}`), 1)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			blob := catalogBottle(t, test.receipt)
			_, err := InspectForCatalog(bytes.NewReader(blob), catalogExpectationFor(blob), Options{})
			assertErrorCode(t, err, CodeInvalidReceipt)
		})
	}

	withoutReceipt := makeArchive(t, validMembers(false))
	inspection, err := InspectForCatalog(bytes.NewReader(withoutReceipt), catalogExpectationFor(withoutReceipt), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Receipt != nil || len(inspection.RuntimeDependencies) != 0 {
		t.Fatalf("receiptless inspection=%+v", inspection)
	}
}

func TestInspectForCatalogUsesNormalByteArchiveAndFormulaChecks(t *testing.T) {
	t.Parallel()
	blob := catalogBottle(t, catalogReceipt(t, nil))

	t.Run("size", func(t *testing.T) {
		expected := catalogExpectationFor(blob)
		expected.CompressedSize++
		_, err := InspectForCatalog(bytes.NewReader(blob), expected, Options{})
		assertErrorCode(t, err, CodeSizeMismatch)
	})
	t.Run("OCI digest", func(t *testing.T) {
		expected := catalogExpectationFor(blob)
		expected.CompressedSHA256 = "sha256:" + strings.Repeat("0", 64)
		_, err := InspectForCatalog(bytes.NewReader(blob), expected, Options{})
		assertErrorCode(t, err, CodeDigestMismatch)
	})
	t.Run("Homebrew checksum", func(t *testing.T) {
		expected := catalogExpectationFor(blob)
		expected.HomebrewSHA256 = strings.Repeat("f", 64)
		_, err := InspectForCatalog(bytes.NewReader(blob), expected, Options{})
		assertErrorCode(t, err, CodeHomebrewMismatch)
	})
	t.Run("hostile path", func(t *testing.T) {
		hostile := makeArchive(t, []archiveMember{
			formulaMember(validFormula()),
			receiptMember(string(catalogReceipt(t, nil))),
			regularMember("../escape", 0o644, "x"),
		})
		_, err := InspectForCatalog(bytes.NewReader(hostile), catalogExpectationFor(hostile), Options{})
		assertErrorCode(t, err, CodeUnsafePath)
	})
	t.Run("Formula identity", func(t *testing.T) {
		wrongFormula := makeArchive(t, []archiveMember{
			formulaMember("class Goodbye < Formula\nend\n"),
			receiptMember(string(catalogReceipt(t, nil))),
		})
		_, err := InspectForCatalog(bytes.NewReader(wrongFormula), catalogExpectationFor(wrongFormula), Options{})
		assertErrorCode(t, err, CodeInvalidFormula)
	})
	t.Run("receipt identity", func(t *testing.T) {
		receipt := bytes.Replace(catalogReceipt(t, nil), []byte(`"full_name":"homebrew/core/hello"`), []byte(`"full_name":"homebrew/core/goodbye"`), 1)
		mismatched := catalogBottle(t, receipt)
		_, err := InspectForCatalog(bytes.NewReader(mismatched), catalogExpectationFor(mismatched), Options{})
		assertErrorCode(t, err, CodeInvalidReceipt)
	})
	t.Run("predeclared dependencies", func(t *testing.T) {
		expected := expectationFor(blob)
		_, err := InspectForCatalog(bytes.NewReader(blob), expected, Options{})
		assertErrorCode(t, err, CodeInvalidExpectation)
	})
}

func catalogBottle(t *testing.T, receipt []byte) []byte {
	t.Helper()
	return makeArchive(t, append(validMembers(false), receiptMember(string(receipt))))
}

func catalogExpectationFor(blob []byte) Expectation {
	expected := expectationFor(blob)
	expected.Dependencies = nil
	expected.FormulaIdentity = expected.FullName
	return expected
}

func catalogReceipt(t *testing.T, dependencies []ReceiptDependency) []byte {
	t.Helper()
	built, poured := true, false
	revision, rebuild, versionScheme := 0, 0, 0
	if dependencies == nil {
		dependencies = []ReceiptDependency{}
	}
	data, err := json.Marshal(installReceipt{
		Name:             "hello",
		FullName:         "homebrew/core/hello",
		PkgVersion:       "1.0",
		Revision:         &revision,
		BottleRebuild:    &rebuild,
		HomebrewVersion:  "4.3.0",
		BuiltAsBottle:    &built,
		PouredFromBottle: &poured,
		Arch:             "x86_64",
		Compiler:         "gcc-11",
		RuntimeDeps:      dependencies,
		Source: receiptSource{
			Spec: "stable",
			Tap:  "homebrew/core",
			Versions: receiptVersions{
				Stable:        "1.0",
				VersionScheme: &versionScheme,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
