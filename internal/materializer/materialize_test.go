package materializer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
)

func TestMergeRuntimeBaseArtifactsAddsPinnedChecksummedPackage(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "runtime-base-artifacts.tsv")
	checksum := "sha256:" + strings.Repeat("a", 64)
	source := "docker.io/library/golang@sha256:" + strings.Repeat("b", 64)
	row := strings.Join([]string{"deb", "debian", "ca-certificates", "20230311+deb12u1", "all", source, checksum, "/etc/ssl/certs/ca-certificates.crt"}, "\t") + "\n"
	if err := os.WriteFile(filename, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, inventory, err := mergeRuntimeBaseArtifacts(runtimefs.SPDXDocument{}, filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(inventory) != row {
		t.Fatalf("inventory changed: %q", inventory)
	}
	if len(doc.Packages) != 1 || len(doc.DocumentDescribes) != 1 {
		t.Fatalf("unexpected SPDX result: %#v", doc)
	}
	pkg := doc.Packages[0]
	if pkg.Name != "ca-certificates" || pkg.VersionInfo != "20230311+deb12u1" || pkg.PackageFileName != "/etc/ssl/certs/ca-certificates.crt" || pkg.FilesAnalyzed {
		t.Fatalf("unexpected package: %#v", pkg)
	}
	if len(pkg.Checksums) != 1 || pkg.Checksums[0].Algorithm != "SHA256" || pkg.Checksums[0].ChecksumValue != strings.Repeat("a", 64) {
		t.Fatalf("unexpected checksums: %#v", pkg.Checksums)
	}
	if len(pkg.ExternalRefs) != 2 || pkg.ExternalRefs[0].ReferenceType != "purl" || !strings.Contains(pkg.ExternalRefs[0].ReferenceLocator, "pkg:deb/debian/ca-certificates@20230311%2Bdeb12u1") || pkg.ExternalRefs[1].ReferenceLocator != source {
		t.Fatalf("unexpected external refs: %#v", pkg.ExternalRefs)
	}
}

func TestMergeRuntimeBaseArtifactsRejectsUnpinnedOrInvalidRows(t *testing.T) {
	validChecksum := "sha256:" + strings.Repeat("a", 64)
	validSource := "docker.io/library/golang@sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		name string
		row  string
		want string
	}{
		{name: "field count", row: "deb\tdebian\tca-certificates", want: "invalid runtime base artifact row"},
		{name: "kind", row: strings.Join([]string{"rpm", "debian", "ca-certificates", "1", "all", validSource, validChecksum, "/etc/ssl/certs/ca-certificates.crt"}, "\t"), want: "invalid runtime base artifact row"},
		{name: "unpinned source", row: strings.Join([]string{"deb", "debian", "ca-certificates", "1", "all", "docker.io/library/golang:latest", validChecksum, "/etc/ssl/certs/ca-certificates.crt"}, "\t"), want: "source is not digest-pinned"},
		{name: "checksum", row: strings.Join([]string{"deb", "debian", "ca-certificates", "1", "all", validSource, "sha512:" + strings.Repeat("a", 128), "/etc/ssl/certs/ca-certificates.crt"}, "\t"), want: "invalid sha256"},
		{name: "path", row: strings.Join([]string{"deb", "debian", "ca-certificates", "1", "all", validSource, validChecksum, "/etc/ssl/../shadow"}, "\t"), want: "invalid path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "runtime-base-artifacts.tsv")
			if err := os.WriteFile(filename, []byte(tc.row+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := mergeRuntimeBaseArtifacts(runtimefs.SPDXDocument{}, filename)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestMergeRuntimeBaseArtifactsMissingIsNoop(t *testing.T) {
	original := runtimefs.SPDXDocument{Name: "unchanged"}
	doc, inventory, err := mergeRuntimeBaseArtifacts(original, filepath.Join(t.TempDir(), "missing.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != original.Name || inventory != nil {
		t.Fatalf("doc=%#v inventory=%q", doc, inventory)
	}
}

func TestMergeRuntimeBaseSBOMAcceptsChiselPackageEvidence(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "runtime-base-packages.tsv")
	row := strings.Join([]string{"libc6", "2.39-0ubuntu8.7", "amd64", "123456", "sha256:" + strings.Repeat("a", 64)}, "\t") + "\n"
	if err := os.WriteFile(filename, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, inventory, err := mergeRuntimeBaseSBOM(runtimefs.SPDXDocument{}, filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(inventory) != row || len(doc.Packages) != 1 || len(doc.Packages[0].Checksums) != 1 {
		t.Fatalf("inventory=%q doc=%#v", inventory, doc)
	}
	if doc.Packages[0].Checksums[0].ChecksumValue != strings.Repeat("a", 64) {
		t.Fatalf("checksums=%#v", doc.Packages[0].Checksums)
	}
}

func TestMergeRuntimeBaseSBOMRejectsInvalidChiselEvidence(t *testing.T) {
	for _, tc := range []struct{ name, row, want string }{
		{"size", "libc6\t1\tamd64\t-1\tsha256:" + strings.Repeat("a", 64), "invalid selected size"},
		{"digest", "libc6\t1\tamd64\t1\tsha512:" + strings.Repeat("a", 128), "invalid sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "runtime-base-packages.tsv")
			if err := os.WriteFile(filename, []byte(tc.row+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := mergeRuntimeBaseSBOM(runtimefs.SPDXDocument{}, filename)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q", err, tc.want)
			}
		})
	}
}
