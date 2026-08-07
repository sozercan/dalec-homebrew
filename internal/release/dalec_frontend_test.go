package release

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadDalecFrontendPin(t *testing.T) {
	want := validDalecFrontendPin()
	path := writeDalecFrontendPin(t, mustMarshalDalecFrontendPin(t, want))

	got, err := LoadDalecFrontendPin(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("pin = %#v, want %#v", *got, want)
	}
}

func TestLoadDalecFrontendPinStrictJSON(t *testing.T) {
	pin := validDalecFrontendPin()
	valid := mustMarshalDalecFrontendPin(t, pin)
	modulePath := []byte(`"path":"` + DalecModulePath + `"`)

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "unknown top-level field",
			data: append(bytes.TrimSuffix(bytes.Clone(valid), []byte("}")), []byte(`,"unexpected":true}`)...),
			want: `unknown field "unexpected"`,
		},
		{
			name: "unknown nested field",
			data: bytes.Replace(bytes.Clone(valid), []byte(`"version":"v0.21.5"`), []byte(`"version":"v0.21.5","unexpected":true`), 1),
			want: `unknown field "unexpected"`,
		},
		{
			name: "duplicate nested member",
			data: bytes.Replace(bytes.Clone(valid), modulePath, append(bytes.Clone(modulePath), append([]byte(","), modulePath...)...), 1),
			want: `duplicate JSON member "path"`,
		},
		{
			name: "trailing JSON value",
			data: append(bytes.Clone(valid), []byte("\n{}")...),
			want: "trailing JSON value",
		},
		{
			name: "malformed JSON",
			data: []byte(`{"schema_version":`),
			want: "EOF",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeDalecFrontendPin(t, tt.data)
			_, err := LoadDalecFrontendPin(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), `decode "`+path+`"`) {
				t.Fatalf("err = %v, want path-scoped decode error", err)
			}
		})
	}
}

func TestLoadDalecFrontendPinSizeBound(t *testing.T) {
	valid := mustMarshalDalecFrontendPin(t, validDalecFrontendPin())
	if len(valid) > maxDalecFrontendPinBytes {
		t.Fatalf("valid test pin is unexpectedly large: %d bytes", len(valid))
	}

	t.Run("limit accepted", func(t *testing.T) {
		data := append(bytes.Clone(valid), bytes.Repeat([]byte(" "), maxDalecFrontendPinBytes-len(valid))...)
		path := writeDalecFrontendPin(t, data)
		if _, err := LoadDalecFrontendPin(path); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("limit exceeded", func(t *testing.T) {
		data := append(bytes.Clone(valid), bytes.Repeat([]byte(" "), maxDalecFrontendPinBytes-len(valid)+1)...)
		path := writeDalecFrontendPin(t, data)
		_, err := LoadDalecFrontendPin(path)
		if err == nil || !strings.Contains(err.Error(), "exceeds 65536 bytes") {
			t.Fatalf("err = %v, want size-bound error", err)
		}
	})
}

func TestLoadDalecFrontendPinValidationError(t *testing.T) {
	pin := validDalecFrontendPin()
	pin.Route = "homebrew/debug"
	path := writeDalecFrontendPin(t, mustMarshalDalecFrontendPin(t, pin))

	_, err := LoadDalecFrontendPin(path)
	if err == nil || !strings.Contains(err.Error(), `validate "`+path+`": route must be exactly "`+DalecHomebrewRoute+`"`) {
		t.Fatalf("err = %v, want path-scoped validation error", err)
	}
}

func TestValidateDalecFrontendPinStableModuleVersions(t *testing.T) {
	for _, version := range []string{"v0.0.0", "v1.2.3", "v10.20.30"} {
		t.Run(version, func(t *testing.T) {
			pin := validDalecFrontendPin()
			pin.Module.Version = version
			if err := ValidateDalecFrontendPin(pin); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateDalecFrontendPinRejectsInvalidPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DalecFrontendPin)
		want   string
	}{
		{name: "schema", mutate: func(pin *DalecFrontendPin) { pin.SchemaVersion = "other" }, want: `schema_version must be exactly "` + DalecFrontendPinSchema + `"`},
		{name: "module path", mutate: func(pin *DalecFrontendPin) { pin.Module.Path = "example.com/dalec" }, want: `module path must be exactly "` + DalecModulePath + `"`},
		{name: "module prerelease", mutate: func(pin *DalecFrontendPin) { pin.Module.Version = "v0.21.5-rc.1" }, want: "stable vMAJOR.MINOR.PATCH"},
		{name: "module build metadata", mutate: func(pin *DalecFrontendPin) { pin.Module.Version = "v0.21.5+build" }, want: "stable vMAJOR.MINOR.PATCH"},
		{name: "module leading zero", mutate: func(pin *DalecFrontendPin) { pin.Module.Version = "v0.021.5" }, want: "stable vMAJOR.MINOR.PATCH"},
		{name: "module missing v", mutate: func(pin *DalecFrontendPin) { pin.Module.Version = "0.21.5" }, want: "stable vMAJOR.MINOR.PATCH"},
		{name: "unpinned index", mutate: func(pin *DalecFrontendPin) { pin.Index = "ghcr.io/project-dalec/dalec/frontend:v0.21.5" }, want: "index must be a digest-pinned OCI reference using sha256"},
		{name: "non-sha256 index", mutate: func(pin *DalecFrontendPin) {
			pin.Index = "ghcr.io/project-dalec/dalec/frontend@sha512:" + strings.Repeat("a", 128)
		}, want: "only sha256 is allowed"},
		{name: "route", mutate: func(pin *DalecFrontendPin) { pin.Route = "homebrew/debug" }, want: `route must be exactly "` + DalecHomebrewRoute + `"`},
		{name: "missing platform", mutate: func(pin *DalecFrontendPin) { delete(pin.Platforms, "linux/arm64") }, want: `missing platform "linux/arm64"`},
		{name: "unsupported platform", mutate: func(pin *DalecFrontendPin) { pin.Platforms["linux/s390x"] = dalecFrontendRef("d") }, want: `unsupported platform "linux/s390x"`},
		{name: "unpinned child", mutate: func(pin *DalecFrontendPin) {
			pin.Platforms["linux/amd64"] = "ghcr.io/project-dalec/dalec/frontend:v0.21.5"
		}, want: "linux/amd64 child must be a digest-pinned OCI reference using sha256"},
		{name: "non-sha256 child", mutate: func(pin *DalecFrontendPin) {
			pin.Platforms["linux/amd64"] = "ghcr.io/project-dalec/dalec/frontend@sha512:" + strings.Repeat("b", 128)
		}, want: "only sha256 is allowed"},
		{name: "different child repository", mutate: func(pin *DalecFrontendPin) {
			pin.Platforms["linux/arm64"] = "ghcr.io/other/dalec/frontend@sha256:" + strings.Repeat("c", 64)
		}, want: "linux/arm64 child uses a different repository from the index"},
		{name: "same child manifest", mutate: func(pin *DalecFrontendPin) { pin.Platforms["linux/arm64"] = pin.Platforms["linux/amd64"] }, want: "linux/amd64 and linux/arm64 children must use different manifests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pin := cloneDalecFrontendPin(validDalecFrontendPin())
			tt.mutate(&pin)
			if err := ValidateDalecFrontendPin(pin); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestSelectDalecFrontend(t *testing.T) {
	pin := validDalecFrontendPin()
	tests := []struct {
		name     string
		ref      string
		route    string
		platform string
		want     DalecFrontendSelection
	}{
		{name: "index by default", want: DalecFrontendSelection{Index: pin.Index, Route: pin.Route}},
		{name: "explicit index", ref: pin.Index, route: pin.Route, platform: "linux/amd64", want: DalecFrontendSelection{Index: pin.Index, Route: pin.Route}},
		{name: "amd64 child", ref: pin.Platforms["linux/amd64"], route: pin.Route, platform: "linux/amd64", want: DalecFrontendSelection{Index: pin.Platforms["linux/amd64"], Route: pin.Route}},
		{name: "arm64 child", ref: pin.Platforms["linux/arm64"], route: pin.Route, platform: "linux/arm64", want: DalecFrontendSelection{Index: pin.Platforms["linux/arm64"], Route: pin.Route}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SelectDalecFrontend(pin, tt.ref, tt.route, tt.platform)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("selection = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSelectDalecFrontendRejectsInvalidSelection(t *testing.T) {
	pin := validDalecFrontendPin()
	tests := []struct {
		name     string
		pin      DalecFrontendPin
		ref      string
		route    string
		platform string
		want     string
	}{
		{name: "invalid pin", pin: func() DalecFrontendPin {
			value := cloneDalecFrontendPin(pin)
			value.Route = "homebrew/debug"
			return value
		}(), want: `route must be exactly "` + DalecHomebrewRoute + `"`},
		{name: "unpinned ref", pin: pin, ref: "ghcr.io/project-dalec/dalec/frontend:v0.21.5", route: pin.Route, platform: "linux/amd64", want: "DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF must be a digest-pinned OCI reference using sha256"},
		{name: "route mismatch", pin: pin, ref: pin.Index, route: "homebrew/debug", platform: "linux/amd64", want: `DALEC_HOMEBREW_LIVE_TARGET "homebrew/debug" does not match release-bound route "` + DalecHomebrewRoute + `"`},
		{name: "unsupported platform", pin: pin, ref: pin.Index, route: pin.Route, platform: "linux/s390x", want: "explicit upstream Dalec frontend requires platform linux/amd64 or linux/arm64"},
		{name: "cross-platform child", pin: pin, ref: pin.Platforms["linux/arm64"], route: pin.Route, platform: "linux/amd64", want: "does not match the release-bound index or linux/amd64 child"},
		{name: "unbound ref", pin: pin, ref: dalecFrontendRef("d"), route: pin.Route, platform: "linux/amd64", want: "does not match the release-bound index or linux/amd64 child"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SelectDalecFrontend(tt.pin, tt.ref, tt.route, tt.platform)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestDalecFrontendSelectionJSON(t *testing.T) {
	selection := DalecFrontendSelection{
		Index: dalecFrontendRef("b"),
		Route: DalecHomebrewRoute,
	}
	got, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"index":"` + selection.Index + `","route":"` + DalecHomebrewRoute + `"}`
	if string(got) != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}

func validDalecFrontendPin() DalecFrontendPin {
	return DalecFrontendPin{
		SchemaVersion: DalecFrontendPinSchema,
		Module:        DalecModule{Path: DalecModulePath, Version: "v0.21.5"},
		Index:         dalecFrontendRef("a"),
		Platforms: map[string]string{
			"linux/amd64": dalecFrontendRef("b"),
			"linux/arm64": dalecFrontendRef("c"),
		},
		Route: DalecHomebrewRoute,
	}
}

func cloneDalecFrontendPin(pin DalecFrontendPin) DalecFrontendPin {
	clone := pin
	clone.Platforms = make(map[string]string, len(pin.Platforms))
	for platform, ref := range pin.Platforms {
		clone.Platforms[platform] = ref
	}
	return clone
}

func dalecFrontendRef(hex string) string {
	return "ghcr.io/project-dalec/dalec/frontend@sha256:" + strings.Repeat(hex, 64)
}

func mustMarshalDalecFrontendPin(t *testing.T, pin DalecFrontendPin) []byte {
	t.Helper()
	data, err := json.Marshal(pin)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeDalecFrontendPin(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dalec-frontend.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
