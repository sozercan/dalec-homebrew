package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

const (
	// DalecFrontendPinSchema is the supported release-bound upstream Dalec pin schema.
	DalecFrontendPinSchema = "dalec-homebrew-dalec-frontend/v1"
	// DalecModulePath is the only upstream module accepted by the pin policy.
	DalecModulePath = "github.com/project-dalec/dalec"
	// DalecHomebrewRoute is the only upstream route accepted by the pin policy.
	DalecHomebrewRoute = "homebrew/image"

	maxDalecFrontendPinBytes = 64 << 10
)

// DalecFrontendPin is the release-bound upstream Dalec dispatcher index,
// platform children, module identity, and forwarding route.
type DalecFrontendPin struct {
	SchemaVersion string            `json:"schema_version"`
	Module        DalecModule       `json:"module"`
	Index         string            `json:"index"`
	Platforms     map[string]string `json:"platforms"`
	Route         string            `json:"route"`
}

// DalecModule identifies the upstream Dalec module used to build the pinned
// dispatcher frontend.
type DalecModule struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// DalecFrontendSelection is the validated upstream Dalec frontend reference
// and route selected for one invocation.
type DalecFrontendSelection struct {
	Index string `json:"index"`
	Route string `json:"route"`
}

// LoadDalecFrontendPin reads, strictly decodes, and validates a release-bound
// upstream Dalec frontend pin. Input is bounded to 64 KiB and duplicate JSON
// object members are rejected at every nesting level.
func LoadDalecFrontendPin(path string) (*DalecFrontendPin, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxDalecFrontendPinBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if len(data) > maxDalecFrontendPinBytes {
		return nil, fmt.Errorf("%q exceeds %d bytes", path, maxDalecFrontendPinBytes)
	}
	if err := validateUniqueJSON(data); err != nil {
		return nil, fmt.Errorf("decode %q: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var pin DalecFrontendPin
	if err := dec.Decode(&pin); err != nil {
		return nil, fmt.Errorf("decode %q: %w", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode %q: multiple JSON values", path)
		}
		return nil, fmt.Errorf("decode %q: %w", path, err)
	}
	if err := ValidateDalecFrontendPin(pin); err != nil {
		return nil, fmt.Errorf("validate %q: %w", path, err)
	}
	return &pin, nil
}

// ValidateDalecFrontendPin validates the complete release policy for an
// upstream Dalec dispatcher pin.
func ValidateDalecFrontendPin(pin DalecFrontendPin) error {
	var errs []error
	if pin.SchemaVersion != DalecFrontendPinSchema {
		errs = append(errs, fmt.Errorf("schema_version must be exactly %q", DalecFrontendPinSchema))
	}
	if pin.Module.Path != DalecModulePath {
		errs = append(errs, fmt.Errorf("module path must be exactly %q", DalecModulePath))
	}
	if !stableDalecModuleVersionPattern.MatchString(pin.Module.Version) {
		errs = append(errs, errors.New("module version must use stable vMAJOR.MINOR.PATCH form"))
	}
	if err := resolution.ValidatePinnedReference(pin.Index); err != nil {
		errs = append(errs, fmt.Errorf("index must be a digest-pinned OCI reference using sha256: %w", err))
	}
	if pin.Route != DalecHomebrewRoute {
		errs = append(errs, fmt.Errorf("route must be exactly %q", DalecHomebrewRoute))
	}

	indexRepository := dalecFrontendReferenceRepository(pin.Index)
	platforms := make([]string, 0, len(pin.Platforms))
	for platform := range pin.Platforms {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		ref := pin.Platforms[platform]
		if platform != "linux/amd64" && platform != "linux/arm64" {
			errs = append(errs, fmt.Errorf("unsupported platform %q", platform))
			continue
		}
		if err := resolution.ValidatePinnedReference(ref); err != nil {
			errs = append(errs, fmt.Errorf("%s child must be a digest-pinned OCI reference using sha256: %w", platform, err))
			continue
		}
		if indexRepository != "" && dalecFrontendReferenceRepository(ref) != indexRepository {
			errs = append(errs, fmt.Errorf("%s child uses a different repository from the index", platform))
		}
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		if _, ok := pin.Platforms[platform]; !ok {
			errs = append(errs, fmt.Errorf("missing platform %q", platform))
		}
	}
	if pin.Platforms["linux/amd64"] != "" && pin.Platforms["linux/amd64"] == pin.Platforms["linux/arm64"] {
		errs = append(errs, errors.New("linux/amd64 and linux/arm64 children must use different manifests"))
	}
	return errors.Join(errs...)
}

// SelectDalecFrontend validates pin and selects its index or the platform child
// matching an explicitly supplied release-bound reference.
func SelectDalecFrontend(pin DalecFrontendPin, ref, route, platform string) (DalecFrontendSelection, error) {
	if err := ValidateDalecFrontendPin(pin); err != nil {
		return DalecFrontendSelection{}, err
	}
	if ref == "" {
		return DalecFrontendSelection{Index: pin.Index, Route: pin.Route}, nil
	}
	if err := resolution.ValidatePinnedReference(ref); err != nil {
		return DalecFrontendSelection{}, fmt.Errorf("DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF must be a digest-pinned OCI reference using sha256: %w", err)
	}
	if route != pin.Route {
		return DalecFrontendSelection{}, fmt.Errorf("DALEC_HOMEBREW_LIVE_TARGET %q does not match release-bound route %q", route, pin.Route)
	}
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return DalecFrontendSelection{}, fmt.Errorf("explicit upstream Dalec frontend requires platform linux/amd64 or linux/arm64, got %q", platform)
	}
	if ref != pin.Index && ref != pin.Platforms[platform] {
		return DalecFrontendSelection{}, fmt.Errorf("DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF %q does not match the release-bound index or %s child", ref, platform)
	}
	return DalecFrontendSelection{Index: ref, Route: route}, nil
}

func dalecFrontendReferenceRepository(ref string) string {
	repository, _, _ := strings.Cut(ref, "@")
	return repository
}

var stableDalecModuleVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
