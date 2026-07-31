package runtimecheck

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
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

func TestAuxiliaryScriptsMayOmitOptionalInterpreters(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	for _, dir := range []string{
		filepath.Join(prefix, "bin"),
		filepath.Join(prefix, "Cellar/go/1.26.5/bin"),
		filepath.Join(prefix, "Cellar/go/1.26.5/libexec/src"),
		filepath.Join(prefix, "Cellar/llvm@21/21.1.8/bin"),
		filepath.Join(prefix, "Cellar/ncurses/6.6/share/ncurses/test/package/debian"),
		filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/idlelib/idle_test"),
		filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/encodings"),
		filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/site-packages/pip/_vendor/distro"),
		filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/site-packages/pip/_vendor/requests"),
		filepath.Join(prefix, "Cellar/dbus/1.16.2_1/share/doc/dbus/examples"),
		filepath.Join(root, "usr/bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalELF(t, filepath.Join(root, "usr/bin/env"), "amd64")
	writeMinimalELF(t, filepath.Join(prefix, "Cellar/go/1.26.5/bin/go"), "amd64")
	if err := os.Symlink("../Cellar/go/1.26.5/bin/go", filepath.Join(prefix, "bin/go")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"all.rc", "clean.rc", "make.rc", "run.rc"} {
		if err := os.WriteFile(filepath.Join(prefix, "Cellar/go/1.26.5/libexec/src", name), []byte("#!/bin/rc -e\n"), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/llvm@21/21.1.8/bin/scan-build-py"), []byte("#!/usr/bin/env python3\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/ncurses/6.6/share/ncurses/test/package/debian/rules"), []byte("#!/usr/bin/make -f\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/idlelib/idle_test/example_noext"), []byte("#!usr/bin/env python\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	pythonFiles := []string{
		filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/encodings/rot_13.py"),
		filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/site-packages/pip/_vendor/distro/distro.py"),
		filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/site-packages/pip/_vendor/requests/certs.py"),
		filepath.Join(prefix, "Cellar/dbus/1.16.2_1/share/doc/dbus/examples/GetAllMatchRules.py"),
	}
	for i, filename := range pythonFiles {
		mode := os.FileMode(0o555)
		if i == 0 {
			mode = 0o444
		}
		if err := os.WriteFile(filename, []byte("#!/usr/bin/env python\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	searchPATH := []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin"}
	writeRuntimeScopeEvidence(t, root, searchPATH)

	if err := Verify(Options{
		Root:       root,
		Prefix:     "/home/linuxbrew/.linuxbrew",
		Arch:       "amd64",
		SearchPATH: searchPATH,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExposedAuxiliaryScriptStillRequiresInterpreter(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	for _, dir := range []string{
		filepath.Join(prefix, "bin"),
		filepath.Join(prefix, "Cellar/go/1.26.5/bin"),
		filepath.Join(prefix, "Cellar/llvm@21/21.1.8/bin"),
		filepath.Join(root, "usr/bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalELF(t, filepath.Join(root, "usr/bin/env"), "amd64")
	writeMinimalELF(t, filepath.Join(prefix, "Cellar/go/1.26.5/bin/go"), "amd64")
	if err := os.Symlink("../Cellar/go/1.26.5/bin/go", filepath.Join(prefix, "bin/go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/llvm@21/21.1.8/bin/scan-build-py"), []byte("#!/usr/bin/env python3\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/llvm@21/21.1.8/bin/scan-build-py", filepath.Join(prefix, "bin/scan-build-py")); err != nil {
		t.Fatal(err)
	}
	searchPATH := []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin"}
	writeRuntimeScopeEvidence(t, root, searchPATH)
	err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: searchPATH})
	if err == nil || !strings.Contains(err.Error(), `env interpreter "python3" is unavailable`) {
		t.Fatalf("expected exposed auxiliary interpreter failure, got %v", err)
	}
}

func TestAuthenticatedRequestedScriptStillRequiresItsInterpreter(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	for _, dir := range []string{
		filepath.Join(prefix, "bin"),
		filepath.Join(prefix, "Cellar/go/1.26.5/bin"),
		filepath.Join(prefix, "Cellar/llvm@21/21.1.8/bin"),
		filepath.Join(prefix, "Cellar/ncurses/6.6"),
		filepath.Join(root, "usr/bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalELF(t, filepath.Join(root, "usr/bin/env"), "amd64")
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/go/1.26.5/bin/go"), []byte("#!/usr/bin/env missing-runtime\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/go/1.26.5/bin/go", filepath.Join(prefix, "bin/go")); err != nil {
		t.Fatal(err)
	}
	searchPATH := []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin"}
	writeRuntimeScopeEvidence(t, root, searchPATH)
	err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: searchPATH})
	if err == nil || !strings.Contains(err.Error(), `env interpreter "missing-runtime" is unavailable`) {
		t.Fatalf("expected authenticated requested-script failure, got %v", err)
	}
	requestedScript := filepath.Join(prefix, "Cellar/go/1.26.5/bin/go")
	if err := os.Chmod(requestedScript, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestedScript, []byte("#!usr/bin/env python3\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(requestedScript, 0o555); err != nil {
		t.Fatal(err)
	}
	err = Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: searchPATH})
	if err == nil || !strings.Contains(err.Error(), `has non-absolute interpreter "usr/bin/env"`) {
		t.Fatalf("expected requested relative-shebang failure, got %v", err)
	}
}

func TestRuntimeScopeEvidenceMustMatchManifestBinding(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	searchPATH := []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin"}
	writeRuntimeScopeEvidence(t, root, searchPATH)
	manifest := filepath.Join(root, "usr/share/dalec-homebrew/manifest.json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var binding map[string]any
	if err := json.Unmarshal(data, &binding); err != nil {
		t.Fatal(err)
	}
	binding["resolution_digest"] = "sha256:" + strings.Repeat("c", 64)
	data, err = json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, data, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifest, 0o444); err != nil {
		t.Fatal(err)
	}
	err = Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: searchPATH})
	if err == nil || !strings.Contains(err.Error(), "runtime manifest does not bind the embedded resolution") {
		t.Fatalf("expected evidence binding rejection, got %v", err)
	}
}

func TestRuntimeScopeEvidenceMustBeCompletePair(t *testing.T) {
	for _, missing := range []string{"resolution.json", "manifest.json"} {
		t.Run(missing, func(t *testing.T) {
			root := t.TempDir()
			prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
			if err := os.MkdirAll(prefix, 0o755); err != nil {
				t.Fatal(err)
			}
			searchPATH := []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin"}
			writeRuntimeScopeEvidence(t, root, searchPATH)
			if err := os.Remove(filepath.Join(root, "usr/share/dalec-homebrew", missing)); err != nil {
				t.Fatal(err)
			}
			err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: searchPATH})
			if err == nil || !strings.Contains(err.Error(), "must either both exist or both be absent") {
				t.Fatalf("expected partial evidence rejection, got %v", err)
			}
		})
	}
}

func TestUnknownAuthenticatedDependencyHelperRemainsStrict(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	for _, dir := range []string{
		filepath.Join(prefix, "Cellar/go/1.26.5/bin"),
		filepath.Join(prefix, "Cellar/tool/1.0/libexec"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalELF(t, filepath.Join(prefix, "Cellar/go/1.26.5/bin/go"), "amd64")
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/tool/1.0/libexec/private-helper"), []byte("#!/missing/private-runtime\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	searchPATH := []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin"}
	writeRuntimeScopeEvidence(t, root, searchPATH)
	err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: searchPATH})
	if err == nil || !strings.Contains(err.Error(), "private-helper") {
		t.Fatalf("expected unknown private helper rejection, got %v", err)
	}
}

func TestExposedScriptStillRequiresItsInterpreter(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	for _, dir := range []string{
		filepath.Join(prefix, "bin"),
		filepath.Join(prefix, "Cellar/tool/1.0/bin"),
		filepath.Join(root, "usr/bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalELF(t, filepath.Join(root, "usr/bin/env"), "amd64")
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/tool/1.0/bin/tool"), []byte("#!/usr/bin/env missing-runtime\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/tool/1.0/bin/tool", filepath.Join(prefix, "bin/tool")); err != nil {
		t.Fatal(err)
	}
	err := Verify(Options{
		Root:       root,
		Prefix:     "/home/linuxbrew/.linuxbrew",
		Arch:       "amd64",
		SearchPATH: []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin"},
	})
	if err == nil || !strings.Contains(err.Error(), `env interpreter "missing-runtime" is unavailable`) {
		t.Fatalf("expected exposed-script interpreter failure, got %v", err)
	}
}

func TestKegOnlyPATHScriptStillRequiresItsInterpreter(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	for _, dir := range []string{
		filepath.Join(prefix, "opt"),
		filepath.Join(prefix, "Cellar/tool/1.0/bin"),
		filepath.Join(root, "usr/bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalELF(t, filepath.Join(root, "usr/bin/env"), "amd64")
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/tool/1.0/bin/tool"), []byte("#!/usr/bin/env missing-runtime\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/tool/1.0", filepath.Join(prefix, "opt/tool")); err != nil {
		t.Fatal(err)
	}
	err := Verify(Options{
		Root:       root,
		Prefix:     "/home/linuxbrew/.linuxbrew",
		Arch:       "amd64",
		SearchPATH: []string{"/home/linuxbrew/.linuxbrew/opt/tool/bin", "/usr/bin"},
	})
	if err == nil || !strings.Contains(err.Error(), `env interpreter "missing-runtime" is unavailable`) {
		t.Fatalf("expected keg-only PATH interpreter failure, got %v", err)
	}
}

func TestAuxiliaryScriptStillRequiresStructurallyValidShebang(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar/tool/1.0/share/tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELF(t, filepath.Join(root, "usr/bin/env"), "amd64")
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/tool/1.0/share/tests/helper"), []byte("#!/usr/bin/env python3 -O\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: []string{"/usr/bin"}})
	if err == nil || !strings.Contains(err.Error(), "uses unsupported env shebang") {
		t.Fatalf("expected malformed auxiliary shebang rejection, got %v", err)
	}
}

func TestAuxiliaryScriptRejectsPresentButUnusableInterpreter(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar/tool/1.0/share/tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/bad"), []byte("not an ELF interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/tool/1.0/share/tests/helper"), []byte("#!/usr/bin/bad\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "interpreter /usr/bin/bad is unusable") {
		t.Fatalf("expected unusable auxiliary interpreter rejection, got %v", err)
	}
}

func TestForeignArchitectureRelocatableELFIsRuntimeData(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar/go/1.26.5/libexec/src/runtime/race"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELFType(t, filepath.Join(prefix, "Cellar/go/1.26.5/libexec/src/runtime/race/race_linux_arm64.syso"), 183, 1, 0o444)
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err != nil {
		t.Fatal(err)
	}
}

func TestExposedRelocatableELFIsRejected(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELFType(t, filepath.Join(prefix, "bin/not-a-program"), 62, 1, 0o555)
	err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "relocatable object exposed as a runtime executable") {
		t.Fatalf("expected exposed ET_REL rejection, got %v", err)
	}
}

func TestNonExecutableRelocatableSharedObjectIsRejected(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar/tool/1.0/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELFType(t, filepath.Join(prefix, "Cellar/tool/1.0/lib/plugin.so"), 183, 1, 0o444)
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
		t.Fatal("ET_REL shared-object path was accepted as inert object data")
	}
}

func TestPrivateExecutableObjectFileIsRuntimeData(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/config"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELFType(t, filepath.Join(prefix, "Cellar/python@3.14/3.14.6/lib/python3.14/config/python.o"), 62, 1, 0o555)
	if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateExecutableRelocatableELFIsRejected(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar/tool/1.0/libexec"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalELFType(t, filepath.Join(prefix, "Cellar/tool/1.0/libexec/not-a-program"), 183, 1, 0o555)
	err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "relocatable object exposed as a runtime executable") {
		t.Fatalf("expected private executable ET_REL rejection, got %v", err)
	}
}

func TestBoundEvidenceStillRejectsExposedRelocatableELF(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
	for _, dir := range []string{
		filepath.Join(prefix, "bin"),
		filepath.Join(prefix, "Cellar/go/1.26.5/bin"),
		filepath.Join(prefix, "Cellar/tool/1.0/bin"),
		filepath.Join(root, "usr/bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalELF(t, filepath.Join(prefix, "Cellar/go/1.26.5/bin/go"), "amd64")
	if err := os.Symlink("../Cellar/go/1.26.5/bin/go", filepath.Join(prefix, "bin/go")); err != nil {
		t.Fatal(err)
	}
	writeMinimalELFType(t, filepath.Join(prefix, "Cellar/tool/1.0/bin/not-a-program"), 62, 1, 0o555)
	if err := os.Symlink("../Cellar/tool/1.0/bin/not-a-program", filepath.Join(prefix, "bin/not-a-program")); err != nil {
		t.Fatal(err)
	}
	searchPATH := []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin"}
	writeRuntimeScopeEvidence(t, root, searchPATH)
	err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64", SearchPATH: searchPATH})
	if err == nil || !strings.Contains(err.Error(), "relocatable object exposed as a runtime executable") {
		t.Fatalf("expected bound exposed ET_REL rejection, got %v", err)
	}
}

func TestForeignArchitectureRuntimeLoadableELFIsRejected(t *testing.T) {
	for _, typ := range []uint16{2, 3} {
		t.Run(map[uint16]string{2: "exec", 3: "dyn"}[typ], func(t *testing.T) {
			root := t.TempDir()
			prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
			if err := os.MkdirAll(prefix, 0o755); err != nil {
				t.Fatal(err)
			}
			writeMinimalELFType(t, filepath.Join(prefix, "foreign"), 183, typ, 0o555)
			err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"})
			if err == nil || !strings.Contains(err.Error(), "has machine EM_AARCH64, expected EM_X86_64") {
				t.Fatalf("expected foreign runtime ELF rejection, got %v", err)
			}
		})
	}
}

func TestCompiledPayloadMayContainInertMaterializerPath(t *testing.T) {
	const cargoSource = "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/index.crates.io-1949cf8c6b5b557f/addr2line-0.26.0/src/line.rs"
	const cargoDebugDirectory = "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/index.crates.io-1949cf8c6b5b557f/aws-lc-sys-0.40.0/aws-lc/third_party/jitterentropy/jitterentropy-library/src"
	const cargoDebugSymbolPath = "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/index.crates.io-1949cf8c6b5b557f/anstyle-query-1.0.2/src/lib.rs/@/anstyle_query.908e22e90579d582-cgu.0"
	const cargoContainedParentPath = "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/index.crates.io-1949cf8c6b5b557f/rustls-0.23.40/src/crypto/aws_lc_rs/../ring/quic.rs"
	t.Run("validated ELF and ar provenance", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		writeELFWithSection(t, filepath.Join(prefix, "wasmtime"), 2, 0o555, ".rodata", 2, []byte(cargoSource+"\x00"))
		writeELFWithSection(t, filepath.Join(prefix, "libwasmtime.so"), 3, 0o444, ".rodata", 2, []byte(cargoSource+"\x00"))
		writeELFWithSection(t, filepath.Join(prefix, "rustls-helper"), 2, 0o555, ".rodata", 2, []byte(cargoContainedParentPath+"\x00"))
		writeELFWithSection(t, filepath.Join(prefix, "concatenated-sources"), 2, 0o555, ".rodata", 2, []byte(cargoSource+":983"+cargoSource+"\x00"))
		writeELFWithSection(t, filepath.Join(prefix, "deno"), 2, 0o555, ".debug_line", 0, []byte(cargoDebugDirectory+"\x00"+cargoDebugSymbolPath+"\x00"))
		memberFile := filepath.Join(t.TempDir(), "member.o")
		writeELFWithSection(t, memberFile, 1, 0o444, ".rodata", 2, []byte(cargoSource+"\x00"))
		member, err := os.ReadFile(memberFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "libwasmtime.a"), arArchive(member), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("non-provenance cache path", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		writeELFWithSection(t, filepath.Join(prefix, "bad"), 2, 0o555, ".rodata", 2, []byte("/home/linuxbrew/.cache/Homebrew/downloads\x00"))
		if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
			t.Fatal("non-provenance cache path in ELF was accepted")
		}
	})

	t.Run("Cargo provenance traversal is rejected", func(t *testing.T) {
		tests := []struct {
			name    string
			section string
			flags   uint64
			value   string
		}{
			{name: "dot components", section: ".rodata", flags: 2, value: "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/i/c/../../../../downloads/payload.rs"},
			{name: "continues after rs", section: ".rodata", flags: 2, value: "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/i/c/source.rs/../../payload"},
			{name: "extension after rs", section: ".rodata", flags: 2, value: "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/i/c/source.rs.evil"},
			{name: "bare traversal crate", section: ".debug_line", value: "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/i/../payload"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				root := t.TempDir()
				prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
				if err := os.MkdirAll(prefix, 0o755); err != nil {
					t.Fatal(err)
				}
				writeELFWithSection(t, filepath.Join(prefix, "bad"), 2, 0o555, tt.section, tt.flags, []byte(tt.value+"\x00"))
				if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
					t.Fatalf("unsafe Cargo provenance %q was accepted", tt.value)
				}
			})
		}
	})

	t.Run("window-boundary continuation is rejected", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		base := "/home/linuxbrew/.cache/Homebrew/cargo_cache/registry/src/i/c/"
		value := base + strings.Repeat("a", 4096-len(base)-len(".rs")) + ".rs/../../payload"
		writeELFWithSection(t, filepath.Join(prefix, "bad"), 2, 0o555, ".rodata", 2, []byte(value+"\x00"))
		if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
			t.Fatal("Cargo path continuation after the scan window was accepted")
		}
	})

	t.Run("debug-only directory provenance is rejected in rodata", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		writeELFWithSection(t, filepath.Join(prefix, "bad"), 2, 0o555, ".rodata", 2, []byte(cargoDebugDirectory+"\x00"))
		if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
			t.Fatal("debug-directory provenance in runtime rodata was accepted")
		}
	})

	t.Run("provenance outside rodata", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		writeELFWithSection(t, filepath.Join(prefix, "bad"), 2, 0o555, ".text.extra", 6, []byte(cargoSource+"\x00"))
		if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
			t.Fatal("Cargo source path outside read-only provenance data was accepted")
		}
	})

	t.Run("malformed ar", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "bad.a"), []byte("!<arch>\n"+cargoSource), 0o444); err != nil {
			t.Fatal(err)
		}
		err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"})
		if err == nil || !strings.Contains(err.Error(), "malformed ar archive") {
			t.Fatalf("expected malformed ar rejection, got %v", err)
		}
	})

	t.Run("non-ELF ar member", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "bad.a"), arArchive([]byte(cargoSource)), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
			t.Fatal("Cargo path in non-ELF ar member was accepted")
		}
	})
}

func TestMaterializerReferencesRemainRejectedInTextAndRelocationPlaceholders(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "config.txt"), []byte("cache=/home/linuxbrew/.cache/Homebrew/downloads\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
			t.Fatal("materializer path in retained text was accepted")
		}
	})

	t.Run("compiled relocation placeholder", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "home/linuxbrew/.linuxbrew")
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		filename := filepath.Join(prefix, "tool")
		writeMinimalELF(t, filename, "amd64")
		appendFile(t, filename, []byte("@@HOMEBREW_PREFIX@@"))
		if err := Verify(Options{Root: root, Prefix: "/home/linuxbrew/.linuxbrew", Arch: "amd64"}); err == nil {
			t.Fatal("unresolved relocation placeholder in ELF was accepted")
		}
	})
}

func TestLoaderReferencesRemainSemanticallyForbidden(t *testing.T) {
	for _, value := range []string{
		"/home/linuxbrew/.cache/Homebrew/downloads/lib.so",
		"/home/linuxbrew/.linuxbrew/Homebrew/Library/Homebrew/vendor/lib.so",
		"@@HOMEBREW_PREFIX@@/lib",
	} {
		if got := forbiddenRuntimeReference(value); got == "" {
			t.Fatalf("runtime loader reference %q was accepted", value)
		}
	}
	if got := forbiddenRuntimeReference("$ORIGIN/../lib"); got != "" {
		t.Fatalf("normal runtime loader reference rejected as %q", got)
	}
}

func writeMinimalELF(t *testing.T, filename, arch string) {
	t.Helper()
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	writeMinimalELFType(t, filename, machine, 2, 0o755)
}

func writeMinimalELFType(t *testing.T, filename string, machine, typ uint16, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filename, minimalELFBytes(machine, typ), mode); err != nil {
		t.Fatal(err)
	}
}

func minimalELFBytes(machine, typ uint16) []byte {
	data := make([]byte, 120)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], typ)
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
	return data
}

func writeELFWithSection(t *testing.T, filename string, typ uint16, mode os.FileMode, sectionName string, sectionFlags uint64, payload []byte) {
	t.Helper()
	const payloadOffset = 0x100
	shstr := []byte("\x00" + sectionName + "\x00.shstrtab\x00")
	shstrOffset := payloadOffset + len(payload)
	if rem := shstrOffset % 8; rem != 0 {
		shstrOffset += 8 - rem
	}
	sectionOffset := shstrOffset + len(shstr)
	if rem := sectionOffset % 8; rem != 0 {
		sectionOffset += 8 - rem
	}
	data := make([]byte, sectionOffset+3*64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], typ)
	binary.LittleEndian.PutUint16(data[18:], 62)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint64(data[32:], 64)
	binary.LittleEndian.PutUint64(data[40:], uint64(sectionOffset))
	binary.LittleEndian.PutUint16(data[52:], 64)
	binary.LittleEndian.PutUint16(data[54:], 56)
	binary.LittleEndian.PutUint16(data[56:], 1)
	binary.LittleEndian.PutUint16(data[58:], 64)
	binary.LittleEndian.PutUint16(data[60:], 3)
	binary.LittleEndian.PutUint16(data[62:], 2)
	binary.LittleEndian.PutUint32(data[64:], 1)
	binary.LittleEndian.PutUint32(data[68:], 5)
	binary.LittleEndian.PutUint64(data[96:], uint64(len(data)))
	binary.LittleEndian.PutUint64(data[104:], uint64(len(data)))
	binary.LittleEndian.PutUint64(data[112:], 4096)
	copy(data[payloadOffset:], payload)
	copy(data[shstrOffset:], shstr)
	section := data[sectionOffset+64 : sectionOffset+128]
	binary.LittleEndian.PutUint32(section[0:], 1)
	binary.LittleEndian.PutUint32(section[4:], 1)
	binary.LittleEndian.PutUint64(section[8:], sectionFlags)
	binary.LittleEndian.PutUint64(section[16:], 0x400000+payloadOffset)
	binary.LittleEndian.PutUint64(section[24:], payloadOffset)
	binary.LittleEndian.PutUint64(section[32:], uint64(len(payload)))
	binary.LittleEndian.PutUint64(section[48:], 1)
	shstrSection := data[sectionOffset+128 : sectionOffset+192]
	binary.LittleEndian.PutUint32(shstrSection[0:], uint32(1+len(sectionName)+1))
	binary.LittleEndian.PutUint32(shstrSection[4:], 3)
	binary.LittleEndian.PutUint64(shstrSection[24:], uint64(shstrOffset))
	binary.LittleEndian.PutUint64(shstrSection[32:], uint64(len(shstr)))
	binary.LittleEndian.PutUint64(shstrSection[48:], 1)
	if err := os.WriteFile(filename, data, mode); err != nil {
		t.Fatal(err)
	}
}

func arArchive(member []byte) []byte {
	header := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10d`\n", "member.o/", "0", "0", "0", "100644", len(member))
	data := append([]byte("!<arch>\n"+header), member...)
	if len(member)%2 != 0 {
		data = append(data, '\n')
	}
	return data
}

func appendFile(t *testing.T, filename string, data []byte) {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, info.Mode().Perm()|0o200); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
}

func writeRuntimeScopeEvidence(t *testing.T, root string, searchPATH []string) {
	t.Helper()
	tm := time.Unix(1_800_000_000, 0).UTC()
	digest := "sha256:" + strings.Repeat("a", 64)
	layer := "sha256:" + strings.Repeat("b", 64)
	desc := func() resolution.Descriptor {
		return resolution.Descriptor{Digest: digest, Size: 1, MediaType: "application/test"}
	}
	node := func(name, version string, executables []string, deps ...string) resolution.Node {
		requirements := make([]resolution.Requirement, 0, len(deps))
		for _, dep := range deps {
			requirements = append(requirements, resolution.Requirement{Name: dep})
		}
		manifest := desc()
		manifest.Platform = &resolution.Platform{OS: "linux", Architecture: "amd64"}
		return resolution.Node{
			Name:            name,
			FullName:        "homebrew/core/" + name,
			FormulaVersion:  version,
			PkgVersion:      version,
			Dependencies:    requirements,
			ExecutablePaths: executables,
			Bottle: resolution.Bottle{
				Tag:            "x86_64_linux",
				Filename:       name + "--" + version + ".x86_64_linux.bottle.tar.gz",
				Repository:     "ghcr.io/homebrew/core/" + name,
				Index:          desc(),
				Manifest:       manifest,
				Config:         desc(),
				Layer:          resolution.Descriptor{Digest: layer, Size: 1, MediaType: "application/test"},
				HomebrewSHA256: strings.TrimPrefix(layer, "sha256:"),
				Tab:            resolution.BottleTab{Arch: "x86_64"},
			},
		}
	}
	record := &resolution.Record{
		SchemaVersion: resolution.SchemaVersion,
		PolicyVersion: resolution.PolicyVersion,
		Input: resolution.Input{
			DalecSpecDigest: digest,
			Platform:        resolution.Platform{OS: "linux", Architecture: "amd64"},
		},
		Metadata: resolution.MetadataSnapshot{
			Digest: digest, FormulaDigest: digest, MigrationDigest: digest,
			GeneratedAt: tm, FetchedAt: tm,
		},
		ResolvedAt:      tm,
		SourceDateEpoch: tm.Unix(),
		Requested:       []resolution.RequestedRoot{{Requested: "go", Canonical: "go"}},
		Nodes: []resolution.Node{
			node("go", "1.26.5", []string{"bin/go"}, "llvm@21", "ncurses", "python@3.14", "dbus", "tool"),
			node("llvm@21", "21.1.8", []string{"bin/scan-build-py"}),
			node("ncurses", "6.6", nil),
			node("python@3.14", "3.14.6", nil),
			node("dbus", "1.16.2_1", nil),
			node("tool", "1.0", nil),
		},
		InstallOrder: []string{"llvm@21", "ncurses", "python@3.14", "dbus", "tool", "go"},
		Runtime: resolution.RuntimePolicy{
			User:          "linuxbrew",
			UID:           1000,
			GID:           1000,
			GeneratedPATH: append([]string(nil), searchPATH...),
		},
		AttestationPolicy: resolution.AttestationPolicy{Waiver: "homebrew-jws-and-verified-oci-chain-v1"},
	}
	data, err := resolution.Canonical(record)
	if err != nil {
		t.Fatal(err)
	}
	resolutionDigest, err := resolution.Digest(record)
	if err != nil {
		t.Fatal(err)
	}
	evidenceDir := filepath.Join(root, "usr/share/dalec-homebrew")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "resolution.json"), data, 0o444); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion    string              `json:"schema_version"`
		ResolutionDigest string              `json:"resolution_digest"`
		Platform         resolution.Platform `json:"platform"`
		Prefix           string              `json:"prefix"`
	}{
		SchemaVersion:    "dalec-homebrew-runtime-manifest/v1",
		ResolutionDigest: resolutionDigest.String(),
		Platform:         record.Input.Platform,
		Prefix:           "/home/linuxbrew/.linuxbrew",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "manifest.json"), manifest, 0o444); err != nil {
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
