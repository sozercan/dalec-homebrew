package release

import (
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"strings"
	"testing"
)

func valid() *Manifest {
	d := "sha256:" + strings.Repeat("a", 64)
	c := func(name string) Component {
		repo := "ghcr.io/x/" + name
		return Component{Index: repo + "@" + d, Platforms: []PlatformRef{{Platform: resolution.Platform{OS: "linux", Architecture: "arm64"}, Ref: repo + "@" + d}, {Platform: resolution.Platform{OS: "linux", Architecture: "amd64"}, Ref: repo + "@" + d}}}
	}
	return &Manifest{SchemaVersion: SchemaVersion, PolicyVersion: resolution.PolicyVersion, Frontend: c("frontend"), RuntimeBase: c("base"), Materializer: c("materializer"), HomebrewCommit: strings.Repeat("a", 40), PortableRubyVersion: "4.0.6", VerificationKeysDigest: d, DalecModule: "v1", BuildKitModule: "v1"}
}
func TestCanonicalAndPlatform(t *testing.T) {
	m := valid()
	a, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical(m)
	if err != nil || string(a) != string(b) {
		t.Fatal("unstable")
	}
	c, err := m.ComponentsFor(resolution.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || c.RuntimeBaseRef == "" {
		t.Fatalf("%+v %v", c, err)
	}
}
func TestMixedRepositoryFails(t *testing.T) {
	m := valid()
	m.Materializer.Platforms[0].Ref = "ghcr.io/other/m@" + strings.Split(m.Materializer.Index, "@")[1]
	if err := Validate(m); err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeRejectsDuplicateMembers(t *testing.T) {
	_, err := Decode(strings.NewReader(`{"schema_version":"dalec-homebrew-components/v1","schema_version":"other"}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err=%v", err)
	}
}
