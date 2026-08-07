package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/project-dalec/dalec"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	speccontract "github.com/sozercan/dalec-homebrew/internal/spec"
)

const (
	maxDalecFrontendPinBytes = 64 << 10
	maxLiveSpecBytes         = 16 << 20
	dalecHomebrewRoute       = "homebrew/image"
	dalecFrontendPinSchema   = "dalec-homebrew-dalec-frontend/v1"
	dalecModulePath          = "github.com/project-dalec/dalec"
)

type namedPinnedRef struct {
	name  string
	value string
}

type namedPinnedRefs []namedPinnedRef

func (refs *namedPinnedRefs) String() string {
	values := make([]string, 0, len(*refs))
	for _, ref := range *refs {
		values = append(values, ref.name+"="+ref.value)
	}
	return strings.Join(values, ",")
}

func (refs *namedPinnedRefs) Set(value string) error {
	name, ref, ok := strings.Cut(value, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return errors.New("pinned reference must use NAME=REFERENCE")
	}
	*refs = append(*refs, namedPinnedRef{name: name, value: ref})
	return nil
}

type options struct {
	runtimeBaseRef       string
	materializerRef      string
	frontendRef          string
	metadataNotBefore    string
	dalecFrontendPinFile string
	dalecFrontendRef     string
	dalecRoute           string
	platform             string
	baseSpecFile         string
	pinnedRefs           namedPinnedRefs
}

type dalecFrontendPin struct {
	SchemaVersion string            `json:"schema_version"`
	Module        dalecModule       `json:"module"`
	Index         string            `json:"index"`
	Platforms     map[string]string `json:"platforms"`
	Route         string            `json:"route"`
}

type dalecModule struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

type dalecFrontendSelection struct {
	Index                  string   `json:"index"`
	Route                  string   `json:"route"`
	RuntimeDependencyOrder []string `json:"runtime_dependency_order,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "dalec-homebrew-live-input-verify:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("dalec-homebrew-live-input-verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.runtimeBaseRef, "runtime-base-ref", "", "digest-pinned runtime-base reference")
	flags.StringVar(&opts.materializerRef, "materializer-ref", "", "digest-pinned materializer reference")
	flags.StringVar(&opts.frontendRef, "frontend-ref", "", "digest-pinned frontend reference")
	flags.StringVar(&opts.metadataNotBefore, "metadata-not-before", "", "RFC3339 metadata rollback floor")
	flags.StringVar(&opts.dalecFrontendPinFile, "dalec-frontend-file", "", "release-bound upstream Dalec frontend pin JSON")
	flags.StringVar(&opts.dalecFrontendRef, "dalec-frontend-ref", "", "explicit digest-pinned upstream Dalec frontend index or platform child")
	flags.StringVar(&opts.dalecRoute, "dalec-route", "", "explicit upstream Dalec forwarding route")
	flags.StringVar(&opts.platform, "platform", "", "target platform for an explicit upstream Dalec frontend child")
	flags.StringVar(&opts.baseSpecFile, "base-spec-file", "", "base Dalec spec to validate before forwarding metadata is injected")
	flags.Var(&opts.pinnedRefs, "pinned-ref", "additional digest-pinned NAME=REFERENCE to validate (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}

	refs := []namedPinnedRef{
		{name: "DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF", value: opts.runtimeBaseRef},
		{name: "DALEC_HOMEBREW_LIVE_MATERIALIZER_REF", value: opts.materializerRef},
		{name: "DALEC_HOMEBREW_LIVE_FRONTEND_REF", value: opts.frontendRef},
	}
	set := 0
	for _, ref := range refs {
		if ref.value != "" {
			set++
		}
	}
	if set != 0 && set != len(refs) {
		return errors.New("DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF, DALEC_HOMEBREW_LIVE_MATERIALIZER_REF, and DALEC_HOMEBREW_LIVE_FRONTEND_REF must be set together")
	}
	if set == len(refs) {
		for _, ref := range refs {
			if err := validatePinnedReference(ref); err != nil {
				return err
			}
		}
	}
	for _, ref := range opts.pinnedRefs {
		if err := validatePinnedReference(ref); err != nil {
			return err
		}
	}
	if opts.metadataNotBefore != "" {
		if err := validateRFC3339(opts.metadataNotBefore); err != nil {
			return fmt.Errorf("DALEC_HOMEBREW_LIVE_METADATA_NOT_BEFORE must be a valid RFC3339 timestamp: %w", err)
		}
	}
	if (opts.dalecFrontendRef == "") != (opts.dalecRoute == "") {
		return errors.New("DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF and DALEC_HOMEBREW_LIVE_TARGET must be set together")
	}
	if opts.dalecFrontendRef != "" && opts.dalecFrontendPinFile == "" {
		return errors.New("an explicit upstream Dalec frontend requires --dalec-frontend-file")
	}
	var selection *dalecFrontendSelection
	if opts.dalecFrontendPinFile != "" {
		pin, err := loadDalecFrontendPin(opts.dalecFrontendPinFile)
		if err != nil {
			return fmt.Errorf("DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN: %w", err)
		}
		selected, err := selectDalecFrontend(*pin, opts.dalecFrontendRef, opts.dalecRoute, opts.platform)
		if err != nil {
			return fmt.Errorf("DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_PIN: %w", err)
		}
		selection = &selected
	}
	if opts.baseSpecFile != "" {
		order, err := validateBaseSpec(opts.baseSpecFile)
		if err != nil {
			return fmt.Errorf("DALEC_HOMEBREW_LIVE_SPEC: %w", err)
		}
		if selection != nil {
			selection.RuntimeDependencyOrder = order
		}
	}
	if selection != nil {
		if err := json.NewEncoder(stdout).Encode(selection); err != nil {
			return fmt.Errorf("write validated upstream Dalec frontend selection: %w", err)
		}
	}
	return nil
}

func validateBaseSpec(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxLiveSpecBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if len(data) > maxLiveSpecBytes {
		return nil, fmt.Errorf("%q exceeds %d bytes", path, maxLiveSpecBytes)
	}
	firstLine := data
	if idx := bytes.IndexByte(firstLine, '\n'); idx >= 0 {
		firstLine = firstLine[:idx]
	}
	firstLine = bytes.TrimSuffix(firstLine, []byte("\r"))
	if !bytes.HasPrefix(firstLine, []byte("# syntax=")) {
		return nil, errors.New("must start with a # syntax= directive")
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode %q routing shape: %w", path, err)
	}
	if _, ok := raw["targets"]; ok {
		return nil, errors.New("must not define top-level targets; the live helper reserves targets.homebrew for forwarding")
	}
	dependencies, ok := raw["dependencies"].(map[string]any)
	if !ok {
		return nil, errors.New("dependencies.runtime must use map form")
	}
	if _, ok := dependencies["runtime"].(map[string]any); !ok {
		return nil, errors.New("dependencies.runtime must use map form")
	}
	spec, err := dalec.LoadSpec(data)
	if err != nil {
		return nil, fmt.Errorf("decode %q: %w", path, err)
	}
	if len(spec.Targets) != 0 {
		return nil, errors.New("must not define top-level targets; the live helper reserves targets.homebrew for forwarding")
	}
	order, err := speccontract.RuntimeDependencyOrder(data, "")
	if err != nil {
		return nil, err
	}
	return order, nil
}

func loadDalecFrontendPin(path string) (*dalecFrontendPin, error) {
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
	var pin dalecFrontendPin
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
	if err := validateDalecFrontendPin(pin); err != nil {
		return nil, fmt.Errorf("validate %q: %w", path, err)
	}
	return &pin, nil
}

func validateDalecFrontendPin(pin dalecFrontendPin) error {
	var errs []error
	if pin.SchemaVersion != dalecFrontendPinSchema {
		errs = append(errs, fmt.Errorf("schema_version must be exactly %q", dalecFrontendPinSchema))
	}
	if pin.Module.Path != dalecModulePath {
		errs = append(errs, fmt.Errorf("module path must be exactly %q", dalecModulePath))
	}
	if pin.Module.Version == "" || strings.TrimSpace(pin.Module.Version) != pin.Module.Version {
		errs = append(errs, errors.New("module version is required and must not contain surrounding whitespace"))
	}
	if err := resolution.ValidatePinnedReference(pin.Index); err != nil {
		errs = append(errs, fmt.Errorf("index must be a digest-pinned OCI reference using sha256: %w", err))
	}
	if pin.Route != dalecHomebrewRoute {
		errs = append(errs, fmt.Errorf("route must be exactly %q", dalecHomebrewRoute))
	}

	indexRepository := referenceRepository(pin.Index)
	for key, ref := range pin.Platforms {
		if key != "linux/amd64" && key != "linux/arm64" {
			errs = append(errs, fmt.Errorf("unsupported platform %q", key))
			continue
		}
		if err := resolution.ValidatePinnedReference(ref); err != nil {
			errs = append(errs, fmt.Errorf("%s child must be a digest-pinned OCI reference using sha256: %w", key, err))
			continue
		}
		if indexRepository != "" && referenceRepository(ref) != indexRepository {
			errs = append(errs, fmt.Errorf("%s child uses a different repository from the index", key))
		}
	}
	for _, key := range []string{"linux/amd64", "linux/arm64"} {
		if _, ok := pin.Platforms[key]; !ok {
			errs = append(errs, fmt.Errorf("missing platform %q", key))
		}
	}
	if pin.Platforms["linux/amd64"] != "" && pin.Platforms["linux/amd64"] == pin.Platforms["linux/arm64"] {
		errs = append(errs, errors.New("linux/amd64 and linux/arm64 children must use different manifests"))
	}
	return errors.Join(errs...)
}

func selectDalecFrontend(pin dalecFrontendPin, ref, route, platform string) (dalecFrontendSelection, error) {
	if ref == "" {
		return dalecFrontendSelection{Index: pin.Index, Route: pin.Route}, nil
	}
	if err := resolution.ValidatePinnedReference(ref); err != nil {
		return dalecFrontendSelection{}, fmt.Errorf("DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF must be a digest-pinned OCI reference using sha256: %w", err)
	}
	if route != pin.Route {
		return dalecFrontendSelection{}, fmt.Errorf("DALEC_HOMEBREW_LIVE_TARGET %q does not match release-bound route %q", route, pin.Route)
	}
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return dalecFrontendSelection{}, fmt.Errorf("explicit upstream Dalec frontend requires platform linux/amd64 or linux/arm64, got %q", platform)
	}
	if ref != pin.Index && ref != pin.Platforms[platform] {
		return dalecFrontendSelection{}, fmt.Errorf("DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF %q does not match the release-bound index or %s child", ref, platform)
	}
	return dalecFrontendSelection{Index: ref, Route: route}, nil
}

func referenceRepository(ref string) string {
	repository, _, _ := strings.Cut(ref, "@")
	return repository
}

func validateUniqueJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if err := walkUniqueJSON(dec, token); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func walkUniqueJSON(dec *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, value); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for dec.More() {
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkUniqueJSON(dec, value); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func validatePinnedReference(ref namedPinnedRef) error {
	if err := resolution.ValidatePinnedReference(ref.value); err != nil {
		return fmt.Errorf("%s must be a digest-pinned OCI reference using sha256: %w", ref.name, err)
	}
	return nil
}

var rfc3339Pattern = regexp.MustCompile(
	`^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]+))?(Z|([+-])([0-9]{2}):([0-9]{2}))$`,
)

func validateRFC3339(value string) error {
	match := rfc3339Pattern.FindStringSubmatch(value)
	if match == nil {
		return errors.New("invalid RFC3339 syntax")
	}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"hour", match[4], 23},
		{"minute", match[5], 59},
		{"second", match[6], 59},
	} {
		n, err := strconv.Atoi(field.value)
		if err != nil || n > field.max {
			return fmt.Errorf("invalid %s", field.name)
		}
	}
	if match[8] != "Z" {
		offsetHour, err := strconv.Atoi(match[10])
		if err != nil || offsetHour > 23 {
			return errors.New("invalid offset hour")
		}
		offsetMinute, err := strconv.Atoi(match[11])
		if err != nil || offsetMinute > 59 {
			return errors.New("invalid offset minute")
		}
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return err
	}
	return nil
}
