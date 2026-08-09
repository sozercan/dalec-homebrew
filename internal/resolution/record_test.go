package resolution

import (
	"testing"
	"time"
)

func validRecord() *Record {
	tm := time.Unix(1_800_000_000, 0).UTC()
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	layer := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	desc := func(x string) Descriptor { return Descriptor{Digest: x, Size: 1, MediaType: "application/test"} }
	return &Record{
		SchemaVersion: SchemaVersion, PolicyVersion: PolicyVersion,
		Input:      Input{DalecSpecDigest: d, Platform: Platform{OS: "linux", Architecture: "amd64"}},
		Metadata:   MetadataSnapshot{Digest: d, FormulaDigest: d, MigrationDigest: d, GeneratedAt: tm, FetchedAt: tm, FormulaURL: "https://example/formula", MigrationURL: "https://example/migrations", Signatures: []Signature{{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true}}},
		ResolvedAt: tm, SourceDateEpoch: tm.Unix(),
		Requested: []RequestedRoot{{Requested: "a", Canonical: "a"}},
		Nodes: []Node{{Name: "a", FullName: "homebrew/core/a", FormulaVersion: "1", PkgVersion: "1", License: "MIT", Bottle: Bottle{Tag: "x86_64_linux", Filename: "a--1.x86_64_linux.bottle.tar.gz", Repository: "ghcr.io/homebrew/core/a", Index: desc(d), Manifest: func() Descriptor {
			value := desc(d)
			value.Platform = &Platform{OS: "linux", Architecture: "amd64"}
			return value
		}(), Config: desc(d), Layer: desc(layer), HomebrewSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Tab: BottleTab{Arch: "x86_64"}}}},
		InstallOrder:      []string{"a"},
		Runtime:           RuntimePolicy{User: "linuxbrew", UID: 1000, GID: 1000, CPUBaseline: "core2"},
		AttestationPolicy: AttestationPolicy{Waiver: "test"},
	}
}

func TestCanonicalDigestStable(t *testing.T) {
	r := validRecord()
	a, err := Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("canonical bytes changed")
	}
	da, _ := Digest(r)
	db, _ := Digest(r)
	if da != db {
		t.Fatalf("digest %s != %s", da, db)
	}
	decoded, err := Decode(a)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Nodes[0].Name != "a" {
		t.Fatal("round trip failed")
	}
}

func TestRejectBadInstallOrder(t *testing.T) {
	r := validRecord()
	r.Nodes = append(r.Nodes, Node{Name: "b", FullName: "homebrew/core/b", FormulaVersion: "1", PkgVersion: "1", Dependencies: []Requirement{{Name: "a", Minimum: "1"}}, Bottle: r.Nodes[0].Bottle})
	r.Nodes[1].Bottle.Filename = "b--1.x86_64_linux.bottle.tar.gz"
	r.InstallOrder = []string{"b", "a"}
	if err := Validate(r); err == nil {
		t.Fatal("expected order error")
	}
}

func TestValidatePinnedReferenceParsesWholeReference(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := ValidatePinnedReference("ghcr.io/example/image@" + d); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"bad name@" + d, "ghcr.io/x@y@" + d, "ghcr.io/x:latest"} {
		if err := ValidatePinnedReference(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSameReferenceRepository(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "registry host case", a: "GHCR.IO/example/frontend@" + d, b: "ghcr.io/example/frontend:child@" + d, want: true},
		{name: "normalized Docker Hub", a: "example/frontend@" + d, b: "docker.io/example/frontend:child@" + d, want: true},
		{name: "different path", a: "ghcr.io/example/frontend@" + d, b: "ghcr.io/other/frontend@" + d},
		{name: "different port", a: "registry.example:5000/example/frontend@" + d, b: "registry.example:5001/example/frontend@" + d},
		{name: "invalid", a: "not a reference", b: "ghcr.io/example/frontend@" + d},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameReferenceRepository(tt.a, tt.b); got != tt.want {
				t.Fatalf("SameReferenceRepository(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCanonicalSortsWritablePathSet(t *testing.T) {
	r := validRecord()
	r.Runtime.WritablePaths = []string{"/b", "/a"}
	a, err := Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Runtime.WritablePaths = []string{"/a", "/b"}
	b, err := Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("writable path ordering changed canonical bytes")
	}
}

func TestRuntimeIdentityMustMatchConfiguredUser(t *testing.T) {
	for _, tc := range []RuntimePolicy{{User: "root", UID: 1000, GID: 1000}, {User: "2000:2000", UID: 1000, GID: 1000}, {User: "linuxbrew", UID: 2000, GID: 2000}} {
		r := validRecord()
		r.Runtime = tc
		if err := Validate(r); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
	r := validRecord()
	r.Runtime = RuntimePolicy{User: "2000:3000", UID: 2000, GID: 3000}
	if err := Validate(r); err != nil {
		t.Fatal(err)
	}
}

func TestRejectPlatformVariant(t *testing.T) {
	r := validRecord()
	r.Input.Platform.Variant = "v9"
	if err := Validate(r); err == nil {
		t.Fatal("variant accepted")
	}
}

func TestCanonicalSignatureOrderingIncludesVerificationState(t *testing.T) {
	r := validRecord()
	r.Metadata.Signatures = []Signature{{KeyID: "k", Algorithm: "PS512", Verified: true}, {KeyID: "k", Algorithm: "PS512", Verified: false}}
	a, err := Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Metadata.Signatures = []Signature{{KeyID: "k", Algorithm: "PS512", Verified: false}, {KeyID: "k", Algorithm: "PS512", Verified: true}}
	b, err := Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("signature ordering is not canonical")
	}
}

func TestRejectBottlePlatformMismatch(t *testing.T) {
	r := validRecord()
	r.Input.Platform = Platform{OS: "linux", Architecture: "arm64"}
	if err := Validate(r); err == nil {
		t.Fatal("amd64 bottle accepted for arm64 record")
	}
}

func TestRejectUnsafeBottleFilename(t *testing.T) {
	r := validRecord()
	r.Nodes[0].Bottle.Filename = "../../bottle"
	if err := Validate(r); err == nil {
		t.Fatal("unsafe filename accepted")
	}
}
