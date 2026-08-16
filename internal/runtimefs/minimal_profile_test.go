package runtimefs

import (
	"maps"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func TestMinimalRuntimeProfileRetainsReleaseBoundToolchainDevelopmentClosure(t *testing.T) {
	app := minimalCoreNode("app", "1")
	app.Dependencies = []resolution.Requirement{{Name: "open-mpi"}, {Name: "unrelated"}}
	openMPI := minimalCoreNode("open-mpi", "1")
	openMPI.Dependencies = []resolution.Requirement{{Name: "gcc"}, {Name: "libevent"}}
	gcc := minimalCoreNode("gcc", "1")
	gcc.Dependencies = []resolution.Requirement{{Name: "gmp"}}
	gmp := minimalCoreNode("gmp", "1")
	libevent := minimalCoreNode("libevent", "1")
	unrelated := minimalCoreNode("unrelated", "1")
	unrelated.ExecutablePaths = []string{"bin/mpicc"}
	nodes := map[string]resolution.Node{
		app.Name:       app,
		openMPI.Name:   openMPI,
		gcc.Name:       gcc,
		gmp.Name:       gmp,
		libevent.Name:  libevent,
		unrelated.Name: unrelated,
	}

	var entries []*sourceEntry
	for _, name := range []string{"open-mpi", "gcc", "gmp", "libevent", "unrelated"} {
		entries = append(entries,
			minimalEntry("Cellar/"+name+"/1/include/"+name+".h", name, TypeRegular),
			minimalEntry("Cellar/"+name+"/1/lib/pkgconfig/"+name+".pc", name, TypeRegular),
			minimalEntry("Cellar/"+name+"/1/lib/lib"+name+".a", name, TypeRegular),
			minimalEntry("Cellar/"+name+"/1/share/man/man1/"+name+".1", name, TypeRegular),
			minimalEntry("Cellar/"+name+"/1/share/zsh/site-functions/_"+name, name, TypeRegular),
		)
	}
	scan := minimalScan(entries)
	if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(nodes, map[string]struct{}{"app": {}})); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"open-mpi", "gcc", "gmp", "libevent"} {
		for _, rel := range []string{
			"Cellar/" + name + "/1/include/" + name + ".h",
			"Cellar/" + name + "/1/lib/pkgconfig/" + name + ".pc",
			"Cellar/" + name + "/1/lib/lib" + name + ".a",
		} {
			assertMinimalRetained(t, scan, rel)
		}
		assertMinimalPruned(t, scan, "Cellar/"+name+"/1/share/man/man1/"+name+".1", PruneRuntimeDocs)
		assertMinimalPruned(t, scan, "Cellar/"+name+"/1/share/zsh/site-functions/_"+name, PruneRuntimeShell)
	}

	assertMinimalPruned(t, scan, "Cellar/unrelated/1/include/unrelated.h", PruneRuntimeHeaders)
	assertMinimalPruned(t, scan, "Cellar/unrelated/1/lib/pkgconfig/unrelated.pc", PruneRuntimeBuild)
	assertMinimalPruned(t, scan, "Cellar/unrelated/1/lib/libunrelated.a", PruneRuntimeStatic)
}

func TestToolchainDevelopmentClosureUsesExactReleasePolicy(t *testing.T) {
	allowlist := normalizedAllowlist{
		PruningProfile: policyv2.RuntimeProfileMinimalV1,
		PruningRules:   policyv2.MinimalV1RuntimePruneRules(),
	}

	dependency := minimalCoreNode("libevent", "1")
	exact := minimalCoreNode("open-mpi", "1")
	exact.Dependencies = []resolution.Requirement{{Name: dependency.Name}}
	nodes := map[string]resolution.Node{exact.Name: exact, dependency.Name: dependency}
	want := map[string]struct{}{exact.Name: {}, dependency.Name: {}}
	if retained := toolchainDevelopmentClosure(nodes, allowlist); !maps.Equal(retained, want) {
		t.Fatalf("exact release-policy root closure = %#v, want %#v", retained, want)
	}
	exact.ExecutablePaths = []string{"bin/not-a-compiler"}
	nodes[exact.Name] = exact
	if retained := toolchainDevelopmentClosure(nodes, allowlist); !maps.Equal(retained, want) {
		t.Fatalf("executable hints changed release-policy closure: %#v", retained)
	}
	withoutRule := allowlist
	withoutRule.PruningRules = nil
	if retained := toolchainDevelopmentClosure(nodes, withoutRule); len(retained) != 0 {
		t.Fatalf("inactive runtime rule retained toolchain development closure: %#v", retained)
	}

	for name, node := range map[string]resolution.Node{
		"V1": {
			Name:            "open-mpi",
			FullName:        "homebrew/core/open-mpi",
			PkgVersion:      "1",
			ExecutablePaths: []string{"bin/mpicc"},
		},
		"non-core spoof": {
			Name:            "open-mpi",
			FullName:        "acme/tools/open-mpi",
			PolicyFormulaID: "acme/tools/open-mpi",
			PkgVersion:      "1",
			ExecutablePaths: []string{"bin/mpicc"},
		},
		"unlisted core": {
			Name:            "mpich",
			FullName:        "homebrew/core/mpich",
			PolicyFormulaID: "homebrew/core/mpich",
			PkgVersion:      "1",
			ExecutablePaths: []string{"bin/mpicc"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if retained := toolchainDevelopmentClosure(map[string]resolution.Node{node.Name: node}, allowlist); len(retained) != 0 {
				t.Fatalf("untrusted or unlisted executable metadata activated retention: %#v", retained)
			}
		})
	}
}

func TestPruneMinimalRuntimeProfileClasses(t *testing.T) {
	nodes := map[string]resolution.Node{
		"clean":       minimalCoreNode("clean", "1"),
		"dep":         minimalCoreNode("dep", "1"),
		"requested":   minimalCoreNode("requested", "1"),
		"third-party": {Name: "third-party", FullName: "acme/tools/third-party", PolicyFormulaID: "acme/tools/third-party", PkgVersion: "1"},
		"python@3.13": minimalCoreNode("python@3.13", "3.13.12"),
		"python@3.14": minimalCoreNode("python@3.14", "3.14.6"),
		"python@3.15": minimalCoreNode("python@3.15", "3.15.0"),
	}
	entries := []*sourceEntry{
		minimalEntry("Cellar/clean/1/include", "clean", TypeDirectory),
		minimalEntry("Cellar/clean/1/include/clean.h", "clean", TypeRegular),
		minimalEntry("Cellar/dep/1/include", "dep", TypeDirectory),
		minimalEntry("Cellar/dep/1/include/dep.h", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/include/libdep.a", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/man", "dep", TypeDirectory),
		minimalEntry("Cellar/dep/1/share/man/man1", "dep", TypeDirectory),
		minimalEntry("Cellar/dep/1/share/man/man1/dep.1", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/man/man1/LICENSE.txt", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/info/dep.info", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/doc", "dep", TypeDirectory),
		minimalEntry("Cellar/dep/1/share/doc/dep", "dep", TypeDirectory),
		minimalEntry("Cellar/dep/1/share/doc/dep/README.md", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/doc/dep/LICENSE.txt", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/doc/dep/LICENSE", "dep", TypeDirectory),
		minimalEntry("Cellar/dep/1/share/doc/dep/LICENSE/example.txt", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/lib/pkgconfig/dep.pc", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/pkgconfig/dep.pc", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/lib/cmake/dep/dep-config.cmake", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/cmake/dep/dep-config.cmake", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/aclocal/dep.m4", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/lib/libdep.so", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/lib/libdep.a", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/lib/plugins/runtime.a", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/lib/Plugins/runtime.a", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/lib/archive.a", "dep", TypeDirectory),
		minimalEntry("Cellar/dep/1/libexec/dep-helper", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/locale/en/dep.mo", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/bash-completion/completions/dep", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/fish/vendor_completions.d/dep.fish", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/zsh/site-functions/_dep", "dep", TypeRegular),
		minimalEntry("Cellar/dep/1/share/zsh/site-functions/LICENSE", "dep", TypeRegular),
		minimalEntry("Cellar/requested/1/include/requested.h", "requested", TypeRegular),
		minimalEntry("Cellar/requested/1/lib/librequested.a", "requested", TypeRegular),
		minimalEntry("Cellar/requested/1/share/bash-completion/completions/requested", "requested", TypeRegular),
		minimalEntry("Cellar/requested/1/share/doc/requested/README.md", "requested", TypeRegular),
		minimalEntry("Cellar/third-party/1/include/third-party.h", "third-party", TypeRegular),
		minimalEntry("Cellar/third-party/1/lib/libthird-party.a", "third-party", TypeRegular),
		minimalEntry("Cellar/third-party/1/share/fish/vendor_completions.d/third-party.fish", "third-party", TypeRegular),
		minimalEntry("Cellar/third-party/1/share/doc/third-party/README.md", "third-party", TypeRegular),
		minimalEntry("Cellar/python@3.13/3.13.12/lib/python3.13/test/test_os.py", "python@3.13", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/test", "python@3.14", TypeDirectory),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/test/test_os.py", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/unittest/test/test_result.py", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/idlelib/idle_test/test_browser.py", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/test/fixture.a", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/site-packages/pkg/tests/test_pkg.py", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/site-packages/pkg/libfixture.a", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/ensurepip/tests/test_bootstrap.py", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/ensurepip/libfixture.a", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/venv/tests/test_venv.py", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/venv/libfixture.a", "python@3.14", TypeRegular),
		minimalEntry("Cellar/python@3.15/3.15.0/lib/python3.15/test/test_os.py", "python@3.15", TypeRegular),
	}
	scan := minimalScan(entries)
	policy := minimalPolicy(nodes, map[string]struct{}{"requested": {}})
	if err := pruneMinimalRuntimeProfile(scan, policy); err != nil {
		t.Fatal(err)
	}

	assertMinimalPruned(t, scan, "Cellar/clean/1/include", PruneRuntimeHeaders)
	assertMinimalPruned(t, scan, "Cellar/clean/1/include/clean.h", PruneRuntimeHeaders)
	assertMinimalPruned(t, scan, "Cellar/dep/1/include/dep.h", PruneRuntimeHeaders)
	assertMinimalRetained(t, scan, "Cellar/dep/1/include")
	assertMinimalRetained(t, scan, "Cellar/dep/1/include/libdep.a")
	assertMinimalPruned(t, scan, "Cellar/dep/1/share/man/man1/dep.1", PruneRuntimeDocs)
	assertMinimalPruned(t, scan, "Cellar/dep/1/share/info/dep.info", PruneRuntimeDocs)
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/man")
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/man/man1")
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/man/man1/LICENSE.txt")
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/doc/dep/README.md")
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/doc")
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/doc/dep")
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/doc/dep/LICENSE.txt")
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/doc/dep/LICENSE")
	assertMinimalRetained(t, scan, "Cellar/dep/1/share/doc/dep/LICENSE/example.txt")
	for _, rel := range []string{
		"Cellar/dep/1/lib/pkgconfig/dep.pc",
		"Cellar/dep/1/share/pkgconfig/dep.pc",
		"Cellar/dep/1/lib/cmake/dep/dep-config.cmake",
		"Cellar/dep/1/share/cmake/dep/dep-config.cmake",
		"Cellar/dep/1/share/aclocal/dep.m4",
	} {
		assertMinimalPruned(t, scan, rel, PruneRuntimeBuild)
	}
	assertMinimalPruned(t, scan, "Cellar/dep/1/lib/libdep.a", PruneRuntimeStatic)
	for _, rel := range []string{
		"Cellar/dep/1/share/bash-completion/completions/dep",
		"Cellar/dep/1/share/fish/vendor_completions.d/dep.fish",
		"Cellar/dep/1/share/zsh/site-functions/_dep",
	} {
		assertMinimalPruned(t, scan, rel, PruneRuntimeShell)
	}
	for _, rel := range []string{
		"Cellar/dep/1/lib/libdep.so",
		"Cellar/dep/1/lib/plugins/runtime.a",
		"Cellar/dep/1/lib/Plugins/runtime.a",
		"Cellar/dep/1/lib/archive.a",
		"Cellar/dep/1/libexec/dep-helper",
		"Cellar/dep/1/share/locale/en/dep.mo",
		"Cellar/dep/1/share/zsh/site-functions/LICENSE",
		"Cellar/requested/1/include/requested.h",
		"Cellar/requested/1/lib/librequested.a",
		"Cellar/requested/1/share/bash-completion/completions/requested",
		"Cellar/requested/1/share/doc/requested/README.md",
		"Cellar/third-party/1/include/third-party.h",
		"Cellar/third-party/1/lib/libthird-party.a",
		"Cellar/third-party/1/share/fish/vendor_completions.d/third-party.fish",
		"Cellar/third-party/1/share/doc/third-party/README.md",
		"Cellar/python@3.14/3.14.6/lib/python3.14/site-packages/pkg/tests/test_pkg.py",
		"Cellar/python@3.14/3.14.6/lib/python3.14/site-packages/pkg/libfixture.a",
		"Cellar/python@3.14/3.14.6/lib/python3.14/ensurepip/tests/test_bootstrap.py",
		"Cellar/python@3.14/3.14.6/lib/python3.14/ensurepip/libfixture.a",
		"Cellar/python@3.14/3.14.6/lib/python3.14/venv/tests/test_venv.py",
		"Cellar/python@3.14/3.14.6/lib/python3.14/venv/libfixture.a",
		"Cellar/python@3.14/3.14.6/lib/python3.14/unittest/test/test_result.py",
		"Cellar/python@3.14/3.14.6/lib/python3.14/idlelib/idle_test/test_browser.py",
		"Cellar/python@3.15/3.15.0/lib/python3.15/test/test_os.py",
	} {
		assertMinimalRetained(t, scan, rel)
	}
	for _, rel := range []string{
		"Cellar/python@3.13/3.13.12/lib/python3.13/test/test_os.py",
		"Cellar/python@3.14/3.14.6/lib/python3.14/test",
		"Cellar/python@3.14/3.14.6/lib/python3.14/test/test_os.py",
		"Cellar/python@3.14/3.14.6/lib/python3.14/test/fixture.a",
	} {
		assertMinimalPruned(t, scan, rel, PruneRuntimeTests)
	}
}

func TestMinimalRuntimeProfilePropagatesBoundedAliasesAndRevalidatesLinks(t *testing.T) {
	t.Run("bounded Python stdlib test alias", func(t *testing.T) {
		target := minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/test/NormalizationTest-3.2.0.txt", "python@3.14", TypeRegular)
		alias := minimalEntry("lib/python3.14/test/NormalizationTest-3.2.0.txt", "python@3.14", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"python@3.14": minimalCoreNode("python@3.14", "3.14.6")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalPruned(t, scan, target.rel, PruneRuntimeTests)
		assertMinimalPruned(t, scan, alias.rel, PruneRuntimeTests)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wrong-minor Python test alias fails closed", func(t *testing.T) {
		for _, minor := range []string{"3.13", "3.15"} {
			t.Run(minor, func(t *testing.T) {
				target := minimalEntry("Cellar/python@3.14/3.14.6/lib/python3.14/test/NormalizationTest-3.2.0.txt", "python@3.14", TypeRegular)
				alias := minimalEntry("lib/python"+minor+"/test/NormalizationTest-3.2.0.txt", "python@3.14", TypeSymlink)
				alias.linkResolved = target.rel
				scan := minimalScan([]*sourceEntry{target, alias})
				if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"python@3.14": minimalCoreNode("python@3.14", "3.14.6")}, nil)); err != nil {
					t.Fatal(err)
				}
				if err := validateRetainedLinks(scan); errorCode(err) != CodeDanglingLink {
					t.Fatalf("error=%v code=%s", err, errorCode(err))
				}
			})
		}
	})

	t.Run("protected libexec header alias retains target", func(t *testing.T) {
		target := minimalEntry("Cellar/libedit/1/include/editline/readline.h", "libedit", TypeRegular)
		alias := minimalEntry("Cellar/libedit/1/libexec/include/readline/history.h", "libedit", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"libedit": minimalCoreNode("libedit", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, alias.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("protected configuration alias retains target", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/include/dep.h", "dep", TypeRegular)
		alias := minimalEntry("Cellar/dep/1/etc/runtime-header", "dep", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, alias.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("protected libexec static archive aliases retain target", func(t *testing.T) {
		target := minimalEntry("Cellar/libedit/1/lib/libedit.a", "libedit", TypeRegular)
		history := minimalEntry("Cellar/libedit/1/libexec/lib/libhistory.a", "libedit", TypeSymlink)
		history.linkResolved = target.rel
		readline := minimalEntry("Cellar/libedit/1/libexec/lib/libreadline.a", "libedit", TypeSymlink)
		readline.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, history, readline})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"libedit": minimalCoreNode("libedit", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, history.rel)
		assertMinimalRetained(t, scan, readline.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("protected plugin alias retains target", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/lib/libdep.a", "dep", TypeRegular)
		alias := minimalEntry("Cellar/dep/1/libexec/plugins/libdep.a", "dep", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, alias.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("loader alias retains target", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/lib/libdep.a", "dep", TypeRegular)
		alias := minimalEntry("Cellar/dep/1/lib/cmake/dep/libdep.so.1", "dep", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, alias.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("legal alias retains target", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/share/doc/dep/reference.txt", "dep", TypeRegular)
		alias := minimalEntry("Cellar/dep/1/share/doc/dep/LICENCE.txt", "dep", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, alias.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("Formula share doc aliases retain development targets", func(t *testing.T) {
		headerTarget := minimalEntry("Cellar/dep/1/include/api.h", "dep", TypeRegular)
		headerAlias := minimalEntry("Cellar/dep/1/share/doc/dep/include/api.h", "dep", TypeSymlink)
		headerAlias.linkResolved = headerTarget.rel
		archiveTarget := minimalEntry("Cellar/dep/1/lib/libdep.a", "dep", TypeRegular)
		archiveAlias := minimalEntry("Cellar/dep/1/share/doc/dep/lib/libdep.a", "dep", TypeSymlink)
		archiveAlias.linkResolved = archiveTarget.rel
		scan := minimalScan([]*sourceEntry{headerTarget, headerAlias, archiveTarget, archiveAlias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		for _, entry := range []*sourceEntry{headerTarget, headerAlias, archiveTarget, archiveAlias} {
			assertMinimalRetained(t, scan, entry.rel)
		}
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("out-of-class keg aliases fail closed", func(t *testing.T) {
		for name, entries := range map[string][]*sourceEntry{
			"nested include": {
				minimalEntry("Cellar/dep/1/include/api.h", "dep", TypeRegular),
				minimalEntry("Cellar/dep/1/share/runtime/include/api.h", "dep", TypeSymlink),
			},
			"nested lib": {
				minimalEntry("Cellar/dep/1/lib/data.a", "dep", TypeRegular),
				minimalEntry("Cellar/dep/1/share/runtime/lib/data.a", "dep", TypeSymlink),
			},
		} {
			t.Run(name, func(t *testing.T) {
				entries[1].linkResolved = entries[0].rel
				scan := minimalScan(entries)
				if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
					t.Fatal(err)
				}
				assertMinimalRetained(t, scan, entries[1].rel)
				if err := validateRetainedLinks(scan); errorCode(err) != CodeDanglingLink {
					t.Fatalf("error=%v code=%s, want %s", err, errorCode(err), CodeDanglingLink)
				}
			})
		}
	})

	t.Run("protected directory alias retains target subtree", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/include", "dep", TypeDirectory)
		child := minimalEntry("Cellar/dep/1/include/dep.h", "dep", TypeRegular)
		nestedAlias := minimalEntry("Cellar/dep/1/include/libdep.a", "dep", TypeSymlink)
		nestedTarget := minimalEntry("Cellar/dep/1/lib/libdep.a", "dep", TypeRegular)
		nestedAlias.linkResolved = nestedTarget.rel
		alias := minimalEntry("Cellar/dep/1/libexec/runtime-include", "dep", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, child, nestedAlias, nestedTarget, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, child.rel)
		assertMinimalRetained(t, scan, nestedAlias.rel)
		assertMinimalRetained(t, scan, nestedTarget.rel)
		assertMinimalRetained(t, scan, alias.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("pruned descendant alias cannot retain target", func(t *testing.T) {
		nodes := map[string]resolution.Node{
			"app":     minimalCoreNode("app", "1"),
			"dep":     minimalCoreNode("dep", "1"),
			"llvm@21": minimalCoreNode("llvm@21", "1"),
		}
		policy := minimalPolicy(nodes, nil)
		target := minimalEntry("Cellar/dep/1/include/api.h", "dep", TypeRegular)
		bin := minimalEntry("bin", "", TypeDirectory)
		optional := minimalEntry("bin/scan-build-py", "llvm@21", TypeSymlink)
		optional.linkResolved = target.rel
		protected := minimalEntry("Cellar/app/1/libexec/runtime-bin", "app", TypeSymlink)
		protected.linkResolved = bin.rel
		scan := minimalScan([]*sourceEntry{target, bin, optional, protected})

		pruneOptionalDependencyTooling(scan, policy)
		if optional.retain || optional.pruneReason != PruneOptionalTooling {
			t.Fatalf("optional alias was not pruned: %#v", optional)
		}
		if err := pruneMinimalRuntimeProfile(scan, policy); err != nil {
			t.Fatal(err)
		}
		assertMinimalPruned(t, scan, target.rel, PruneRuntimeHeaders)
		assertMinimalRetained(t, scan, bin.rel)
		assertMinimalRetained(t, scan, protected.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("requested keg alias retains transitive target", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/include/dep.h", "dep", TypeRegular)
		alias := minimalEntry("Cellar/requested/1/share/runtime-header", "requested", TypeSymlink)
		alias.linkResolved = target.rel
		nodes := map[string]resolution.Node{
			"dep":       minimalCoreNode("dep", "1"),
			"requested": minimalCoreNode("requested", "1"),
		}
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(nodes, map[string]struct{}{"requested": {}})); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, alias.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("bounded man alias", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/share/man/man1/dep.1", "dep", TypeRegular)
		alias := minimalEntry("share/man/man1/dep.1", "dep", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalPruned(t, scan, target.rel, PruneRuntimeDocs)
		assertMinimalPruned(t, scan, alias.rel, PruneRuntimeDocs)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("man symbol beginning with notice is not legal text", func(t *testing.T) {
		target := minimalEntry("Cellar/openssl@3/1/share/man/man3/X509_dup.3ssl", "openssl@3", TypeRegular)
		alias := minimalEntry("Cellar/openssl@3/1/share/man/man3/NOTICEREF_free.3ssl", "openssl@3", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"openssl@3": minimalCoreNode("openssl@3", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalPruned(t, scan, target.rel, PruneRuntimeDocs)
		assertMinimalPruned(t, scan, alias.rel, PruneRuntimeDocs)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("share doc alias is retained", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/share/doc/dep/README.md", "dep", TypeRegular)
		alias := minimalEntry("share/doc/dep/README.md", "dep", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, target.rel)
		assertMinimalRetained(t, scan, alias.rel)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unbounded alias fails closed", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/share/man/man1/dep.1", "dep", TypeRegular)
		alias := minimalEntry("bin/dep-doc", "dep", TypeSymlink)
		alias.linkResolved = target.rel
		scan := minimalScan([]*sourceEntry{target, alias})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		if err := validateRetainedLinks(scan); errorCode(err) != CodeDanglingLink {
			t.Fatalf("error=%v code=%s", err, errorCode(err))
		}
	})

	t.Run("matching global copy and hardlink", func(t *testing.T) {
		copiedTarget := minimalEntry("Cellar/dep/1/lib/pkgconfig/dep.pc", "dep", TypeRegular)
		copiedTarget.sha256, copiedTarget.size = "same", 4
		copied := minimalEntry("lib/pkgconfig/dep.pc", "dep", TypeRegular)
		copied.sha256, copied.size = "same", 4
		hardlinkTarget := minimalEntry("Cellar/dep/1/share/man/man1/dep.1", "dep", TypeRegular)
		hardlinkTarget.inode = "1:2"
		hardlink := minimalEntry("share/man/man1/dep.1", "dep", TypeRegular)
		hardlink.inode = "1:2"
		scan := minimalScan([]*sourceEntry{copiedTarget, copied, hardlinkTarget, hardlink})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalPruned(t, scan, copied.rel, PruneRuntimeBuild)
		assertMinimalPruned(t, scan, hardlink.rel, PruneRuntimeDocs)
	})

	t.Run("symlink classification is independent of regular alias order", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/share/pkgconfig/dep.pc", "dep", TypeRegular)
		target.sha256, target.size = "same", 4
		alias := minimalEntry("lib/pkgconfig/dep.pc", "dep", TypeSymlink)
		copy := minimalEntry("share/pkgconfig/dep.pc", "dep", TypeRegular)
		copy.sha256, copy.size = "same", 4
		alias.linkResolved = copy.rel
		// This is the lexical order produced by scanAndPlan: the symlink precedes
		// the regular global copy that establishes its removable target.
		scan := minimalScan([]*sourceEntry{target, alias, copy})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalPruned(t, scan, target.rel, PruneRuntimeBuild)
		assertMinimalPruned(t, scan, copy.rel, PruneRuntimeBuild)
		assertMinimalPruned(t, scan, alias.rel, PruneRuntimeBuild)
		if err := validateRetainedLinks(scan); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("matching static archive copy", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/lib/libdep.a", "dep", TypeRegular)
		target.sha256, target.size = "same", 4
		copy := minimalEntry("lib/libdep.a", "dep", TypeRegular)
		copy.sha256, copy.size = "same", 4
		scan := minimalScan([]*sourceEntry{target, copy})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalPruned(t, scan, target.rel, PruneRuntimeStatic)
		assertMinimalPruned(t, scan, copy.rel, PruneRuntimeStatic)
	})

	t.Run("mismatched global copy is retained", func(t *testing.T) {
		target := minimalEntry("Cellar/dep/1/lib/pkgconfig/dep.pc", "dep", TypeRegular)
		target.sha256, target.size = "target", 6
		copy := minimalEntry("lib/pkgconfig/dep.pc", "dep", TypeRegular)
		copy.sha256, copy.size = "different", 9
		scan := minimalScan([]*sourceEntry{target, copy})
		if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(map[string]resolution.Node{"dep": minimalCoreNode("dep", "1")}, nil)); err != nil {
			t.Fatal(err)
		}
		assertMinimalRetained(t, scan, copy.rel)
	})
}

func TestMinimalRuntimeProfilePreservesRuntimeBearingPathsBeforeEveryPruneClass(t *testing.T) {
	nodes := map[string]resolution.Node{
		"dep":         minimalCoreNode("dep", "1"),
		"perl":        minimalCoreNode("perl", "1"),
		"pcre2":       minimalCoreNode("pcre2", "1"),
		"python@3.14": minimalCoreNode("python@3.14", "3.14.6"),
	}
	retained := []string{
		// Legal names are checked before every prune reason.
		"Cellar/dep/1/include/LICENCE.md",
		"Cellar/dep/1/include/GPL-2.0",
		"Cellar/dep/1/share/man/man1/PATENTS.1",
		"Cellar/perl/1/share/man/man1/perlgpl.1",
		"Cellar/perl/1/share/man/man1/perlartistic.1",
		"Cellar/pcre2/1/share/doc/pcre2/LICENCE.md",
		"Cellar/dep/1/lib/cmake/dep/UNLICENSE",
		"Cellar/dep/1/lib/cmake/dep/THIRD_PARTY_NOTICES",
		"Cellar/python@3.14/3.14.6/lib/python3.14/test/UNLICENCE.txt",
		"Cellar/dep/1/share/zsh/site-functions/LEGAL",
		"Cellar/dep/1/lib/LEGAL.a",

		// Shared objects remain loader-visible even inside otherwise removable
		// headers, docs, build metadata, tests, and completion trees.
		"Cellar/dep/1/include/libheader.so",
		"Cellar/dep/1/share/man/man1/libmanual.so.1",
		"Cellar/dep/1/share/doc/dep/libdoc.so",
		"Cellar/dep/1/lib/cmake/dep/plugin.so",
		"Cellar/python@3.14/3.14.6/lib/python3.14/test/extension.so.1",
		"Cellar/dep/1/share/zsh/site-functions/module.so",

		// Protected runtime-data components use one predicate across every class.
		"Cellar/dep/1/include/config/generated.h",
		"Cellar/dep/1/share/man/locale/en/dep.1",
		"Cellar/dep/1/share/doc/dep/plugins/runtime.dat",
		"Cellar/dep/1/lib/cmake/node_modules/runtime.cmake",
		"Cellar/python@3.14/3.14.6/lib/python3.14/test/site-packages/runtime.py",
		"Cellar/dep/1/share/zsh/site-functions/venv/_dep",
		"Cellar/dep/1/lib/libexec/runtime.a",
	}
	entries := make([]*sourceEntry, 0, len(retained)+1)
	for _, rel := range retained {
		pkg := "dep"
		switch {
		case strings.HasPrefix(rel, "Cellar/perl/"):
			pkg = "perl"
		case rel == "Cellar/pcre2/1/share/doc/pcre2/LICENCE.md":
			pkg = "pcre2"
		case rel == "Cellar/python@3.14/3.14.6/lib/python3.14/test/UNLICENCE.txt",
			rel == "Cellar/python@3.14/3.14.6/lib/python3.14/test/extension.so.1",
			rel == "Cellar/python@3.14/3.14.6/lib/python3.14/test/site-packages/runtime.py":
			pkg = "python@3.14"
		}
		entries = append(entries, minimalEntry(rel, pkg, TypeRegular))
	}
	noticeRef := "Cellar/dep/1/share/doc/dep/NOTICEREF_notes.md"
	entries = append(entries, minimalEntry(noticeRef, "dep", TypeRegular))

	scan := minimalScan(entries)
	if err := pruneMinimalRuntimeProfile(scan, minimalPolicy(nodes, nil)); err != nil {
		t.Fatal(err)
	}
	for _, rel := range retained {
		assertMinimalRetained(t, scan, rel)
	}
	assertMinimalRetained(t, scan, noticeRef)
}

func TestLooksLikeLegalTextRecognizesVersionedNamesWithoutPrefixFalsePositives(t *testing.T) {
	for _, name := range []string{
		"COPYING3",
		"COPYING3.LIB",
		"LICENSE2",
		"LICENCEv3.txt",
		"PATENTS-2.0",
		"EULA",
		"THIRD_PARTY_NOTICES",
		"third-party-licences.txt",
		"GPL-2.0",
		"Apache-2.0.txt",
		"perlgpl.1",
		"perlartistic.1",
	} {
		if !looksLikeLegalText(name) {
			t.Errorf("%q was not recognized as legal text", name)
		}
	}
	for _, name := range []string{
		"NOTICEREF_notes.md",
		"LICENSEHEADER",
		"COPYING3REF",
		"PATENTED.txt",
	} {
		if looksLikeLegalText(name) {
			t.Errorf("%q was recognized as legal text", name)
		}
	}
}

func TestValidateInventoryPolicyRejectsMinimalProfileForgery(t *testing.T) {
	nodes := map[string]resolution.Node{
		"dep":       minimalCoreNode("dep", "1"),
		"requested": minimalCoreNode("requested", "1"),
	}
	record := &resolution.Record{
		PolicyVersion: resolution.PolicyVersionV2,
		Nodes:         []resolution.Node{nodes["dep"], nodes["requested"]},
		Runtime:       resolution.RuntimePolicy{UID: 1000, GID: 1000},
	}
	entry := InventoryEntry{Path: "Cellar/dep/1/include/dep.h", Type: TypeRegular, Mode: "0444", Package: "dep", FormulaID: "homebrew/core/dep"}
	policy := minimalPolicy(nodes, map[string]struct{}{"requested": {}})
	if err := validateInventoryPolicy(entry, record, policy, nil); errorCode(err) != CodeVerification {
		t.Fatalf("error=%v code=%s", err, errorCode(err))
	}
	directory := entry
	directory.Path = "Cellar/dep/1/include"
	directory.Type = TypeDirectory
	directory.Mode = "0555"
	if err := validateInventoryPolicy(directory, record, policy, nil); errorCode(err) != CodeVerification {
		t.Fatalf("forged directory error=%v code=%s", err, errorCode(err))
	}

	requested := entry
	requested.Path = "Cellar/requested/1/include/requested.h"
	requested.Package = "requested"
	requested.FormulaID = "homebrew/core/requested"
	if err := validateInventoryPolicy(requested, record, policy, nil); err != nil {
		t.Fatalf("requested inventory entry rejected: %v", err)
	}

	legal := entry
	legal.Path = "Cellar/dep/1/share/man/man1/LICENSE.txt"
	legalPolicy := minimalPolicy(nodes, nil)
	legalAncestors := minimalInventoryExceptionAncestors([]InventoryEntry{
		{Path: "Cellar/dep/1/share/man", Type: TypeDirectory, Mode: "0555", Package: "dep", FormulaID: "homebrew/core/dep"},
		{Path: "Cellar/dep/1/share/man/man1", Type: TypeDirectory, Mode: "0555", Package: "dep", FormulaID: "homebrew/core/dep"},
		legal,
	}, legalPolicy)
	if err := validateInventoryPolicy(legal, record, legalPolicy, legalAncestors); err != nil {
		t.Fatalf("legal text inventory entry rejected: %v", err)
	}
	legalParent := InventoryEntry{Path: "Cellar/dep/1/share/man/man1", Type: TypeDirectory, Mode: "0555", Package: "dep", FormulaID: "homebrew/core/dep"}
	if err := validateInventoryPolicy(legalParent, record, legalPolicy, legalAncestors); err != nil {
		t.Fatalf("proven legal-text ancestor rejected: %v", err)
	}

	shareDoc := entry
	shareDoc.Path = "Cellar/dep/1/share/doc/dep/README.md"
	if err := validateInventoryPolicy(shareDoc, record, policy, nil); err != nil {
		t.Fatalf("share/doc entry rejected: %v", err)
	}
	shareDocParent := InventoryEntry{Path: "Cellar/dep/1/share/doc/dep", Type: TypeDirectory, Mode: "0555", Package: "dep", FormulaID: "homebrew/core/dep"}
	if err := validateInventoryPolicy(shareDocParent, record, policy, nil); err != nil {
		t.Fatalf("share/doc directory rejected: %v", err)
	}

	standard := minimalPolicy(nodes, nil)
	standard.allowlist.PruningProfile = ""
	standard.allowlist.PruningRules = nil
	if err := validateInventoryPolicy(entry, record, standard, nil); err != nil {
		t.Fatalf("unprofiled inventory entry rejected: %v", err)
	}
}

func minimalCoreNode(name, version string) resolution.Node {
	id := "homebrew/core/" + name
	return resolution.Node{Name: name, FullName: id, PolicyFormulaID: id, PkgVersion: version}
}

func minimalPolicy(nodes map[string]resolution.Node, requested map[string]struct{}) *normalizedPolicy {
	if requested == nil {
		requested = map[string]struct{}{}
	}
	rules := policyv2.MinimalV1RuntimePruneRules()
	policy := &normalizedPolicy{
		nodes:     nodes,
		requested: requested,
		allowlist: normalizedAllowlist{
			Cellar:         true,
			Opt:            true,
			Bin:            true,
			Sbin:           true,
			Lib:            true,
			Share:          true,
			PruningProfile: policyv2.RuntimeProfileMinimalV1,
			PruningRules:   rules,
		},
	}
	policy.toolchainDev = toolchainDevelopmentClosure(nodes, policy.allowlist)
	return policy
}

func minimalEntry(rel, packageName string, typ EntryType) *sourceEntry {
	return &sourceEntry{rel: rel, typeName: typ, retain: true, packageName: packageName}
}

func minimalScan(entries []*sourceEntry) *sourceScan {
	byPath := make(map[string]*sourceEntry, len(entries))
	for _, entry := range entries {
		byPath[entry.rel] = entry
	}
	return &sourceScan{entries: entries, byPath: byPath}
}

func assertMinimalPruned(t *testing.T, scan *sourceScan, rel string, reason PruneReason) {
	t.Helper()
	entry := scan.byPath[rel]
	if entry == nil || entry.retain || entry.pruneReason != reason {
		t.Fatalf("%s = %#v, want pruned for %s", rel, entry, reason)
	}
}

func assertMinimalRetained(t *testing.T, scan *sourceScan, rel string) {
	t.Helper()
	entry := scan.byPath[rel]
	if entry == nil || !entry.retain {
		t.Fatalf("%s = %#v, want retained", rel, entry)
	}
}
