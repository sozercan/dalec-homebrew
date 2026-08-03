package policy

import (
	"slices"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
)

func TestRuntimeAllowlistBindsGdkPixbufLoaderCache(t *testing.T) {
	record := &resolution.Record{Nodes: []resolution.Node{
		{Name: "gdk-pixbuf", PkgVersion: "2.44.7"},
		{Name: "librsvg", PkgVersion: "2.62.3"},
	}}

	allow, writable := RuntimeAllowlist(record)
	wantOwner := runtimefs.PathRule{
		Path:     gdkPixbufLoadersCachePath,
		Package:  "gdk-pixbuf",
		Required: true,
	}
	if !slices.Contains(allow.Owners, wantOwner) {
		t.Fatalf("owner rules %#v do not contain %#v", allow.Owners, wantOwner)
	}
	if !slices.Contains(writable, runtimefs.DefaultInstallPrefix+"/var/gdk-pixbuf") {
		t.Fatalf("writable paths %#v omit gdk-pixbuf state", writable)
	}
}

func TestRuntimeAllowlistDoesNotAddGdkPixbufOwnerWithoutFormula(t *testing.T) {
	allow, _ := RuntimeAllowlist(&resolution.Record{Nodes: []resolution.Node{{Name: "hello", PkgVersion: "1"}}})
	if len(allow.Owners) != 0 {
		t.Fatalf("unexpected owner rules: %#v", allow.Owners)
	}
}

func TestRuntimeAllowlistBindsSharedMimeDatabase(t *testing.T) {
	record := &resolution.Record{Nodes: []resolution.Node{{Name: "shared-mime-info", PkgVersion: "2.5.1"}}}
	allow, _ := RuntimeAllowlist(record)
	want := runtimefs.PathRule{Path: sharedMimeDatabasePath, Package: "shared-mime-info", Required: true}
	if !slices.Contains(allow.Owners, want) {
		t.Fatalf("owner rules %#v do not contain %#v", allow.Owners, want)
	}
}

func TestRuntimeAllowlistBindsNodeNPMRuntime(t *testing.T) {
	record := &resolution.Record{Nodes: []resolution.Node{{Name: "node", PkgVersion: "26.5.1"}}}
	allow, _ := RuntimeAllowlist(record)
	want := runtimefs.PathRule{Path: nodeNPMRuntimePath, Package: "node", Required: true}
	if !slices.Contains(allow.Owners, want) {
		t.Fatalf("owner rules %#v do not contain %#v", allow.Owners, want)
	}
}

func TestRuntimeAllowlistRetainsSharedFontconfigConfiguration(t *testing.T) {
	record := &resolution.Record{Nodes: []resolution.Node{{Name: "fontconfig", PkgVersion: "2.18.2"}}}
	allow, _ := RuntimeAllowlist(record)
	want := runtimefs.PathRule{Path: "fonts", Package: "fontconfig", Required: true}
	if !slices.Contains(allow.Etc, want) {
		t.Fatalf("etc rules %#v do not contain %#v", allow.Etc, want)
	}
}
