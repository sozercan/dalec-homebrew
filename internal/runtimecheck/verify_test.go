package runtimecheck

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyScriptAndSymlink(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELF(t, filepath.Join(root, "bin/sh"), "amd64")
	if err := os.WriteFile(filepath.Join(prefix, "bin/tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tool", filepath.Join(prefix, "bin/alias")); err != nil {
		t.Fatal(err)
	}
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectEscapingLink(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../outside", filepath.Join(prefix, "bad")); err != nil {
		t.Fatal(err)
	}
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRejectMaterializerReference(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "bad.txt"), []byte("/home/linuxbrew/.cache/Homebrew/download"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRejectUnusableAndMissingEnvInterpreters(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELF(t, filepath.Join(root, "usr/bin/env"), "amd64")
	if err := os.WriteFile(filepath.Join(prefix, "bin/tool"), []byte("#!/usr/bin/env definitely-missing\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
		t.Fatal("expected missing env interpreter error")
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin/tool"), []byte("#!/usr/bin/bad\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/bad"), []byte(""), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
		t.Fatal("expected unusable interpreter error")
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin/tool"), []byte("#!/usr/bin/env python -O\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: []string{"/usr/bin"}}); err == nil {
		t.Fatal("expected multiword env shebang rejection")
	}
}

func TestRejectMissingOrEscapingPrefix(t *testing.T) {
	root := t.TempDir()
	if err := Verify(Options{Root: root, Prefix: "/missing", Arch: "amd64"}); err == nil {
		t.Fatal("missing prefix accepted")
	}
	if err := Verify(Options{Root: root, Prefix: "../../etc", Arch: "amd64"}); err == nil {
		t.Fatal("relative escaping prefix accepted")
	}
}

func TestEnvShebangUsesProvidedImagePATH(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELF(t, filepath.Join(root, "usr/bin/env"), "amd64")
	writeMinimalELF(t, filepath.Join(root, "bin/sh"), "amd64")
	writeMinimalELF(t, filepath.Join(prefix, "bin/helper"), "amd64")
	if err := os.WriteFile(filepath.Join(prefix, "bin/tool"), []byte("#!/usr/bin/env helper\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: []string{"/home/linuxbrew/.linuxbrew/bin"}}); err != nil {
		t.Fatal(err)
	}
}

func writeMinimalELF(t *testing.T, filename, arch string) {
	t.Helper()
	data := make([]byte, 120)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 2)
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(data[18:], machine)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint64(data[32:], 64)
	binary.LittleEndian.PutUint16(data[52:], 64)
	binary.LittleEndian.PutUint16(data[54:], 56)
	binary.LittleEndian.PutUint16(data[56:], 1)
	binary.LittleEndian.PutUint16(data[58:], 64)
	binary.LittleEndian.PutUint32(data[64:], 1)
	binary.LittleEndian.PutUint32(data[68:], 5)
	binary.LittleEndian.PutUint64(data[96:], uint64(len(data)))
	binary.LittleEndian.PutUint64(data[104:], uint64(len(data)))
	binary.LittleEndian.PutUint64(data[112:], 4096)
	if err := os.WriteFile(filename, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRejectPrefixAncestorSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "linuxbrew/.linuxbrew"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "home")); err != nil {
		t.Fatal(err)
	}
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
		t.Fatal("ancestor symlink escape accepted")
	}
}

func TestRejectLoaderIncludeTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/ld.so.conf"), []byte("include /../../../../dev/zero\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := systemLibraryDirs(root, "amd64"); err == nil {
		t.Fatal("loader include traversal accepted")
	}
}

func TestResolveInRootUsesRootRelativeAbsoluteSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib", filepath.Join(root, "lib64")); err != nil {
		t.Fatal(err)
	}
	writeMinimalELF(t, filepath.Join(root, "usr/lib/loader"), "amd64")
	resolved, err := resolveInRoot(root, filepath.Join(root, "home/linuxbrew/.linuxbrew"), "/home/linuxbrew/.linuxbrew", "/lib64/loader")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Join(root, "usr/lib/loader") {
		t.Fatalf("resolved=%q", resolved)
	}
	if err := os.Symlink("../../outside", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInRoot(root, "/unused", "/prefix", "/escape/file"); err == nil {
		t.Fatal("escaping symlink accepted")
	}
}

func TestConfiguredCPUBaselineIsEnforced(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELF(t, filepath.Join(prefix, "tool"), "amd64")
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", CPUBaseline: "x86-64-v3"}); err == nil {
		t.Fatal("unsupported CPU baseline accepted")
	}
}

func TestLoaderConfigAbsoluteSymlinkUsesRuntimeRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/lib/ld.so.conf"), []byte("/lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib/ld.so.conf", filepath.Join(root, "etc/ld.so.conf")); err != nil {
		t.Fatal(err)
	}
	if _, err := systemLibraryDirs(root, "amd64"); err != nil {
		t.Fatal(err)
	}
}
