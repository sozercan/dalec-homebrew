package policyv2

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestEmbeddedA365PrebuiltArchivePolicy(t *testing.T) {
	policy, err := LoadTapPolicy()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := policy.PrebuiltArchiveForFormula("sozercan/repo/a365")
	if !ok {
		t.Fatal("sozercan/repo/a365 prebuilt archive policy is absent")
	}
	want := expectedA365PrebuiltArchivePolicy()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policy mismatch\n got: %#v\nwant: %#v", got, want)
	}
	for _, id := range []string{"a365", "homebrew/core/a365", "sozercan/repo/A365", "acme/tools/a365"} {
		if unexpected, ok := policy.PrebuiltArchiveForFormula(id); ok {
			t.Fatalf("unexpected policy for %q: %#v", id, unexpected)
		}
	}

	canonicalA, err := CanonicalTapPolicy()
	if err != nil {
		t.Fatal(err)
	}
	canonicalB, err := CanonicalTapPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonicalA, canonicalB) {
		t.Fatal("canonical tap-policy bytes changed between loads")
	}
	for _, marker := range []string{
		`"prebuilt_archives"`,
		`"formula_id":"sozercan/repo/a365"`,
		`"policy_version":"prebuilt-derived-bottle-v1"`,
		`"formula_source_digest":"sha256:d6c00086e77905de6f2c93c59ff2b560101925dc77a1f3094468fd154a89e997"`,
		`"sha256":"sha256:71461c31e350cabf4e718a5e1331b39a395a6dc9183bb3ea5922f0fac67404ce"`,
		`"sha256":"sha256:fe7e6b2efa8bab9b804e401e3664dcb6adbb4e2cdcf7d2049b05e645f3eccc83"`,
	} {
		if !bytes.Contains(canonicalA, []byte(marker)) {
			t.Fatalf("canonical tap policy does not bind %s", marker)
		}
	}
}

func TestPrebuiltArchiveForFormulaReturnsDefensiveCopy(t *testing.T) {
	policy, err := LoadTapPolicy()
	if err != nil {
		t.Fatal(err)
	}
	first, ok := policy.PrebuiltArchiveForFormula("sozercan/repo/a365")
	if !ok {
		t.Fatal("prebuilt policy not found")
	}
	first.Dependencies = append(first.Dependencies, "acme/tools/dependency")
	first.Platforms[0].URL = "https://example.invalid/substitution.tar.gz"
	first.Archive.Members[0].Path = "SUBSTITUTED"
	first.Binary.Machines[0].Machine = "substituted"
	first.Binary.NeededLibraries = append(first.Binary.NeededLibraries, "libc.so.6")
	*first.Binary.CGOEnabled = true

	second, ok := policy.PrebuiltArchiveForFormula("sozercan/repo/a365")
	if !ok {
		t.Fatal("prebuilt policy disappeared")
	}
	want := expectedA365PrebuiltArchivePolicy()
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("stored policy was mutated through lookup\n got: %#v\nwant: %#v", second, want)
	}
	if first.Binary.CGOEnabled == second.Binary.CGOEnabled {
		t.Fatal("CGO constraint pointer was not defensively copied")
	}
}

func TestPrebuiltArchivePolicyRejectsMalformedFormulaIDs(t *testing.T) {
	for _, id := range []string{
		"a365",
		"SOZERCAN/repo/a365",
		"https://github.com/sozercan/homebrew-repo/a365",
		"sozercan/repo/a365:latest",
		`sozercan/repo/a365\\evil`,
		"sozercan/../a365",
		"sozercan/repo/å365",
		strings.Repeat("a", 40) + "/repo/a365",
		"homebrew/core/a365",
	} {
		t.Run(id, func(t *testing.T) {
			policy := mustLoadTapPolicy(t)
			policy.PrebuiltArchives[0].FormulaID = id
			if err := ValidateTapPolicy(policy); err == nil {
				t.Fatalf("malformed or unauthorized Formula ID %q accepted", id)
			}
		})
	}
}

func TestValidatePrebuiltArchivePolicyRejectsAdversarialChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TapPolicy)
		want   string
	}{
		{name: "missing authorization", mutate: func(p *TapPolicy) { p.PrebuiltArchives = nil }, want: "release-bound prebuilt archive authorization"},
		{name: "too many authorizations", mutate: func(p *TapPolicy) {
			entry := p.PrebuiltArchives[0]
			for len(p.PrebuiltArchives) <= prebuiltMaxPolicyFormulaEntries {
				entry.FormulaID = "acme/tools/a" + strings.Repeat("a", len(p.PrebuiltArchives))
				p.PrebuiltArchives = append(p.PrebuiltArchives, entry)
			}
		}, want: "maximum is 16"},
		{name: "authorization order", mutate: func(p *TapPolicy) {
			entry := p.PrebuiltArchives[0]
			entry.FormulaID = "acme/tools/widget"
			p.PrebuiltArchives = append(p.PrebuiltArchives, entry)
		}, want: "strictly sorted and unique"},
		{name: "policy version", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].PolicyVersion = "prebuilt-derived-bottle-v2" }, want: "unsupported derivation policy"},
		{name: "release version", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Version = "0.3.4" }, want: "version must be exactly"},
		{name: "Formula digest", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].FormulaSourceDigest = "sha256:" + strings.Repeat("A", 64) }, want: "canonical lowercase sha256"},
		{name: "not root only", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].RootOnly = false }, want: "root-only"},
		{name: "bottle allowed", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].RequireNoBottle = false }, want: "require the Formula to have no bottle"},
		{name: "dependencies absent", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Dependencies = nil }, want: "explicit empty list"},
		{name: "dependency added", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Dependencies = []string{"homebrew/core/hello"} }, want: "must have no dependencies"},
		{name: "platform order", mutate: func(p *TapPolicy) {
			p.PrebuiltArchives[0].Platforms[0], p.PrebuiltArchives[0].Platforms[1] = p.PrebuiltArchives[0].Platforms[1], p.PrebuiltArchives[0].Platforms[0]
		}, want: "platforms must be strictly sorted"},
		{name: "unsupported platform", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Platforms[0].Platform = "linux/386" }, want: "unsupported prebuilt archive platform"},
		{name: "HTTP URL", mutate: func(p *TapPolicy) {
			p.PrebuiltArchives[0].Platforms[0].URL = "http://github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz"
		}, want: "must use HTTPS"},
		{name: "URL userinfo", mutate: func(p *TapPolicy) {
			p.PrebuiltArchives[0].Platforms[0].URL = "https://user@github.com/sozercan/a365cli/releases/download/v0.3.3/a365_0.3.3_linux_amd64.tar.gz"
		}, want: "userinfo"},
		{name: "URL query", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Platforms[0].URL += "?token=secret" }, want: "query"},
		{name: "artifact digest", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Platforms[0].SHA256 = "sha256:" + strings.Repeat("G", 64) }, want: "canonical lowercase sha256"},
		{name: "archive format", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.Format = "zip" }, want: "archive format must be"},
		{name: "multiple gzip members", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.SingleGzipMember = false }, want: "exactly one gzip member"},
		{name: "compressed lower bound", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.MaxCompressedBytes = 0 }, want: "max compressed bytes"},
		{name: "compressed upper bound", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.MaxCompressedBytes = prebuiltMaxCompressedBytes + 1 }, want: "max compressed bytes"},
		{name: "expanded upper bound", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.MaxExpandedBytes = prebuiltMaxExpandedBytes + 1 }, want: "max expanded bytes"},
		{name: "ratio lower bound", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.MaxExpansionRatio = 0 }, want: "max expansion ratio"},
		{name: "entry lower bound", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.MaxEntries = 0 }, want: "max entries"},
		{name: "file upper bound", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.MaxFileBytes = prebuiltMaxFileBytes + 1 }, want: "max file bytes"},
		{name: "depth lower bound", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.MaxDepth = 0 }, want: "max archive depth"},
		{name: "path lower bound", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.MaxPathBytes = 0 }, want: "max archive path bytes"},
		{name: "compression ratio", mutate: func(p *TapPolicy) {
			p.PrebuiltArchives[0].Archive.MaxCompressedBytes = 1
			p.PrebuiltArchives[0].Archive.MaxExpandedBytes = 9
		}, want: "bounded compression ratio"},
		{name: "members absent", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.Members = nil }, want: "archive members must be present"},
		{name: "member order", mutate: func(p *TapPolicy) {
			p.PrebuiltArchives[0].Archive.Members[0], p.PrebuiltArchives[0].Archive.Members[1] = p.PrebuiltArchives[0].Archive.Members[1], p.PrebuiltArchives[0].Archive.Members[0]
		}, want: "members must be strictly sorted"},
		{name: "case folded member collision", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.Members[1].Path = "license" }, want: "case-folding collision"},
		{name: "member traversal", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.Members[0].Path = "../LICENSE" }, want: "invalid component"},
		{name: "member symlink", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.Members[0].Type = "symlink" }, want: "regular file"},
		{name: "member setid", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Archive.Members[2].Mode = "4755" }, want: "canonical octal digits"},
		{name: "unknown payload", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Install.Source = "unknown" }, want: "permitted regular archive member"},
		{name: "destination traversal", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Install.Destination = "../bin/a365" }, want: "invalid component"},
		{name: "writable destination mode", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Install.Mode = "0755" }, want: "must be non-writable"},
		{name: "binary format", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.Format = "pe32" }, want: "binary format must be"},
		{name: "machine order", mutate: func(p *TapPolicy) {
			p.PrebuiltArchives[0].Binary.Machines[0], p.PrebuiltArchives[0].Binary.Machines[1] = p.PrebuiltArchives[0].Binary.Machines[1], p.PrebuiltArchives[0].Binary.Machines[0]
		}, want: "machine constraints must be strictly sorted"},
		{name: "wrong machine", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.Machines[0].Machine = "aarch64" }, want: "must be x86_64"},
		{name: "dynamic linkage", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.Linkage = "dynamic" }, want: "statically linked"},
		{name: "interpreter allowed", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.Interpreter = "allowed" }, want: "interpreter must be forbidden"},
		{name: "needed libraries absent", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.NeededLibraries = nil }, want: "explicit empty list"},
		{name: "needed library", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.NeededLibraries = []string{"libc.so.6"} }, want: "no needed libraries"},
		{name: "RPATH allowed", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.RPath = "allowed" }, want: "RPATH and RUNPATH must be forbidden"},
		{name: "writable executable segment", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.WritableExecutableSegments = "allowed" }, want: "writable executable segments must be forbidden"},
		{name: "wrong Go module", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.GoModule = "example.com/substitution" }, want: "Go module must be exactly"},
		{name: "CGO absent", mutate: func(p *TapPolicy) { p.PrebuiltArchives[0].Binary.CGOEnabled = nil }, want: "CGO constraint must be explicit"},
		{name: "CGO enabled", mutate: func(p *TapPolicy) {
			value := true
			p.PrebuiltArchives[0].Binary.CGOEnabled = &value
		}, want: "CGO must be disabled"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := mustLoadTapPolicy(t)
			test.mutate(policy)
			err := ValidateTapPolicy(policy)
			if err == nil {
				t.Fatal("adversarial policy mutation was accepted")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
}

func mustLoadTapPolicy(t *testing.T) *TapPolicy {
	t.Helper()
	policy, err := LoadTapPolicy()
	if err != nil {
		t.Fatal(err)
	}
	return policy
}
