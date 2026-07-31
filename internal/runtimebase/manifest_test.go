package runtimebase

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestReadChiselManifestAndWriteEvidence(t *testing.T) {
	root := t.TempDir()
	writeRoot := func(name, data string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(name, "/")))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeRoot("/usr/bin/tool", "tool")
	writeRoot("/usr/bin/tool-copy", "tool")
	if err := os.Link(filepath.Join(root, "usr/bin/tool"), filepath.Join(root, "usr/bin/tool-hardlink")); err != nil {
		t.Fatal(err)
	}
	writeRoot("/usr/lib/libtool.so", "library")
	writeRoot("/var/lib/chisel/manifest.wall", "generated-manifest")
	manifest := strings.Join([]string{
		`{"jsonwall":"1.0","schema":"1.0"}`,
		`{"kind":"content","slice":"libtool_runtime","path":"/usr/lib/libtool.so"}`,
		`{"kind":"content","slice":"tool_bins","path":"/usr/bin/tool"}`,
		`{"kind":"package","name":"tool","version":"1.2.3","sha256":"` + strings.Repeat("a", 64) + `","arch":"amd64"}`,
		`{"kind":"package","name":"libtool","version":"2.4.7","sha256":"` + strings.Repeat("b", 64) + `","arch":"amd64"}`,
		`{"kind":"slice","name":"tool_bins"}`,
		`{"kind":"path","path":"/usr/bin/tool","mode":"0755","size":4,"inode":1,"slices":["tool_bins"]}`,
		`{"kind":"path","path":"/usr/bin/tool-copy","mode":"0755","size":4,"slices":["tool_bins"]}`,
		`{"kind":"path","path":"/usr/bin/tool-hardlink","mode":"0755","size":4,"inode":1,"slices":["tool_bins"]}`,
		`{"kind":"path","path":"/usr/lib/libtool.so","mode":"0644","size":7,"slices":["libtool_runtime"]}`,
		`{"kind":"path","path":"/var/lib/chisel/manifest.wall","mode":"0644","size":0,"slices":["tool_bins"]}`,
	}, "\n") + "\n"
	manifestPath := filepath.Join(t.TempDir(), "manifest.wall")
	writeZstd(t, manifestPath, manifest)
	packages, err := ReadChiselManifest(manifestPath, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 || packages[0].Name != "libtool" || packages[0].RegularBytes != 7 || packages[1].RegularBytes != 8 {
		t.Fatalf("packages=%#v", packages)
	}
	inventory := filepath.Join(t.TempDir(), "runtime-base-packages.tsv")
	if err := WritePackageEvidence(packages, inventory); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(inventory)
	if err != nil {
		t.Fatal(err)
	}
	want := "libtool\t2.4.7\tamd64\t7\tsha256:" + strings.Repeat("b", 64) + "\n" +
		"tool\t1.2.3\tamd64\t8\tsha256:" + strings.Repeat("a", 64) + "\n"
	if string(data) != want {
		t.Fatalf("inventory=%q want %q", data, want)
	}
}

func TestReadChiselManifestRejectsInvalidRecords(t *testing.T) {
	tests := []struct{ name, manifest, want string }{
		{"digest", `{"jsonwall":"1.0","schema":"1.0"}` + "\n" + `{"kind":"package","name":"tool","version":"1","sha256":"bad","arch":"amd64"}` + "\n", "invalid package digest"},
		{"traversal", `{"jsonwall":"1.0","schema":"1.0"}` + "\n" + `{"kind":"package","name":"tool","version":"1","sha256":"` + strings.Repeat("a", 64) + `","arch":"amd64"}` + "\n" + `{"kind":"path","path":"/usr/../etc/passwd","size":1,"slices":["tool_bins"]}` + "\n", "invalid chisel content path"},
		{"kind", `{"jsonwall":"1.0","schema":"1.0"}` + "\n" + `{"kind":"mystery"}` + "\n", "unknown chisel manifest record kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "manifest.wall")
			writeZstd(t, filename, tc.manifest)
			_, err := ReadChiselManifest(filename, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q", err, tc.want)
			}
		})
	}
}

func writeZstd(t *testing.T, filename, data string) {
	t.Helper()
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = zw.Write([]byte(data))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
