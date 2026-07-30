package runtime

import (
	"reflect"
	"testing"

	"github.com/project-dalec/dalec"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

func TestMergeEnvByKey(t *testing.T) {
	got := MergeEnv([]string{"PATH=/base", "A=1"}, []string{"PATH=/generated"}, []string{"A=2", "PATH=/user"})
	want := []string{"PATH=/user", "A=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestGeneratedPATH(t *testing.T) {
	nodes := []resolution.Node{{Name: "python@3.13", KegOnly: true, ExecutablePaths: []string{"bin/python3"}}}
	roots := []resolution.RequestedRoot{{Requested: "python@3.13", Canonical: "python@3.13", KegOnly: true}}
	got, err := GeneratedPATH(roots, nodes, "/usr/local/sbin:/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != Prefix+"/opt/python@3.13/bin" || got[len(got)-1] != "/usr/bin" {
		t.Fatalf("path=%v", got)
	}
}

func TestUserValidation(t *testing.T) {
	for _, bad := range []string{"root", "0", "0:1", "alice", "1000:root", "1234"} {
		if _, err := ParseIdentity(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got, err := ParseIdentity("1234:1235"); err != nil || got.UID != 1234 || got.GID != 1235 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestBuildImageConfigUserPATHWins(t *testing.T) {
	base := &dalec.DockerImageSpec{}
	base.Config.Env = []string{"PATH=/bin", "BASE=1"}
	img := &dalec.ImageConfig{Env: []string{"PATH=/custom", "X=1"}}
	out, id, _, err := BuildImageConfig(base, img, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if EnvValue(out.Config.Env, "PATH") != "/custom" || id.User != DefaultUser {
		t.Fatalf("config=%+v id=%+v", out.Config, id)
	}
}

func TestDefaultUserOverridesBaseRoot(t *testing.T) {
	base := &dalec.DockerImageSpec{}
	base.Config.User = "root"
	out, id, _, err := BuildImageConfig(base, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Config.User != DefaultUser || id.UID != DefaultUID {
		t.Fatalf("user=%q id=%+v", out.Config.User, id)
	}
}

func TestEmptyFinalPATHRejected(t *testing.T) {
	base := &dalec.DockerImageSpec{}
	base.Config.Env = []string{"PATH=/bin"}
	_, _, _, err := BuildImageConfig(base, &dalec.ImageConfig{Env: []string{"PATH="}}, nil, nil)
	if err == nil {
		t.Fatal("empty PATH accepted")
	}
}
