package testrunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/project-dalec/dalec"
)

// PlanSchemaVersion is the JSON schema version understood by this runner.
const PlanSchemaVersion = "testplan/v1"

// Plan is a self-contained, BuildKit-independent plan for one Dalec test.
// Running one TestSpec per process lets the caller provide the isolated rootfs
// required by Dalec semantics without coupling this package to BuildKit.
type Plan struct {
	SchemaVersion string   `json:"schema_version,omitempty"`
	Test          TestSpec `json:"test"`
}

// TestSpec is the executable subset of dalec.TestSpec. Source mounts are
// intentionally absent because the final-image runner does not permit them.
type TestSpec struct {
	Name  string                     `json:"name"`
	Dir   string                     `json:"dir,omitempty"`
	Env   map[string]string          `json:"env,omitempty"`
	Steps []TestStep                 `json:"steps,omitempty"`
	Files map[string]FileCheckOutput `json:"files,omitempty"`
}

// TestStep is the executable subset of dalec.TestStep.
type TestStep struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Stdin   string            `json:"stdin,omitempty"`
	Stdout  CheckOutput       `json:"stdout,omitempty"`
	Stderr  CheckOutput       `json:"stderr,omitempty"`
}

// CheckOutput describes assertions made against command output or file data.
// Every configured assertion is evaluated.
type CheckOutput struct {
	Equals     string   `json:"equals,omitempty"`
	Contains   []string `json:"contains,omitempty"`
	Matches    []string `json:"matches,omitempty"`
	StartsWith string   `json:"starts_with,omitempty"`
	EndsWith   string   `json:"ends_with,omitempty"`
	Empty      bool     `json:"empty,omitempty"`
}

// FileCheckOutput extends CheckOutput with filesystem assertions.
//
// Permissions uses the same numeric JSON representation as fs.FileMode and
// dalec.FileCheckOutput. A zero value means that permissions are not checked.
type FileCheckOutput struct {
	CheckOutput
	Permissions fs.FileMode `json:"permissions,omitempty"`
	IsDir       bool        `json:"is_dir,omitempty"`
	NotExist    bool        `json:"not_exist,omitempty"`
	NoFollow    bool        `json:"no_follow,omitempty"`
	LinkTarget  string      `json:"link_target,omitempty"`
}

// NewPlan converts one public Dalec test spec to the restricted JSON plan used
// by this runner. It rejects source mounts rather than silently dropping them.
func NewPlan(test *dalec.TestSpec) (Plan, error) {
	if test == nil {
		return Plan{}, errors.New("test is nil")
	}
	if len(test.Mounts) != 0 {
		return Plan{}, fmt.Errorf("test %q: mounts are not supported", test.Name)
	}

	converted := TestSpec{
		Name:  test.Name,
		Dir:   test.Dir,
		Env:   cloneMap(test.Env),
		Steps: make([]TestStep, len(test.Steps)),
		Files: make(map[string]FileCheckOutput, len(test.Files)),
	}
	for i, step := range test.Steps {
		converted.Steps[i] = TestStep{
			Command: step.Command,
			Env:     cloneMap(step.Env),
			Stdin:   step.Stdin,
			Stdout:  fromDalecCheck(step.Stdout),
			Stderr:  fromDalecCheck(step.Stderr),
		}
	}
	for path, check := range test.Files {
		converted.Files[path] = FileCheckOutput{
			CheckOutput: fromDalecCheck(check.CheckOutput),
			Permissions: check.Permissions,
			IsDir:       check.IsDir,
			NotExist:    check.NotExist,
			NoFollow:    check.NoFollow,
			LinkTarget:  check.LinkTarget,
		}
	}
	if len(converted.Env) == 0 {
		converted.Env = nil
	}
	if len(converted.Files) == 0 {
		converted.Files = nil
	}

	plan := Plan{SchemaVersion: PlanSchemaVersion, Test: converted}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// NewPlans converts each Dalec TestSpec into an independent plan so callers can
// execute every test against its own isolated rootfs.
func NewPlans(tests []*dalec.TestSpec) ([]Plan, error) {
	plans := make([]Plan, 0, len(tests))
	for i, test := range tests {
		plan, err := NewPlan(test)
		if err != nil {
			return nil, fmt.Errorf("test %d: %w", i+1, err)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// DecodePlan decodes exactly one strict JSON plan. Unknown fields and trailing
// JSON values are rejected. A missing schema_version is treated as the current
// version for compatibility with early generated plans.
func DecodePlan(r io.Reader) (Plan, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var plan Plan
	if err := dec.Decode(&plan); err != nil {
		return Plan{}, fmt.Errorf("decode test plan: %w", err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Plan{}, errors.New("decode test plan: multiple JSON values")
		}
		return Plan{}, fmt.Errorf("decode test plan: %w", err)
	}
	if plan.SchemaVersion == "" {
		plan.SchemaVersion = PlanSchemaVersion
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// Validate checks the complete plan before any command is executed.
func (p Plan) Validate() error {
	if p.SchemaVersion != "" && p.SchemaVersion != PlanSchemaVersion {
		return fmt.Errorf("unsupported test plan schema %q (want %q)", p.SchemaVersion, PlanSchemaVersion)
	}
	return p.Test.validate()
}

func (test TestSpec) validate() error {
	name := test.Name
	if strings.TrimSpace(name) == "" {
		return errors.New("test name is required")
	}
	if strings.IndexByte(test.Dir, 0) >= 0 {
		return fmt.Errorf("test %q: working directory contains NUL", name)
	}
	if err := validateEnv(test.Env); err != nil {
		return fmt.Errorf("test %q: %w", name, err)
	}
	for i, step := range test.Steps {
		if strings.TrimSpace(step.Command) == "" {
			return fmt.Errorf("test %q step %d: command is required", name, i+1)
		}
		if strings.IndexByte(step.Command, 0) >= 0 {
			return fmt.Errorf("test %q step %d: command must not contain NUL", name, i+1)
		}
		if err := validateEnv(step.Env); err != nil {
			return fmt.Errorf("test %q step %d: %w", name, i+1, err)
		}
		if err := validateCheck(step.Stdout); err != nil {
			return fmt.Errorf("test %q step %d stdout: %w", name, i+1, err)
		}
		if err := validateCheck(step.Stderr); err != nil {
			return fmt.Errorf("test %q step %d stderr: %w", name, i+1, err)
		}
	}
	for path, check := range test.Files {
		if path == "" {
			return fmt.Errorf("test %q: file path is empty", name)
		}
		if strings.IndexByte(path, 0) >= 0 {
			return fmt.Errorf("test %q file %q: path contains NUL", name, path)
		}
		if check.Permissions&^fs.ModePerm != 0 {
			return fmt.Errorf("test %q file %q: permissions contain non-permission bits", name, path)
		}
		if check.NotExist && (check.IsDir || check.Permissions != 0 || check.LinkTarget != "" || check.CheckOutput.configured()) {
			return fmt.Errorf("test %q file %q: not_exist cannot be combined with other file assertions", name, path)
		}
		if err := validateCheck(check.CheckOutput); err != nil {
			return fmt.Errorf("test %q file %q: %w", name, path, err)
		}
	}
	return nil
}

func validateCheck(check CheckOutput) error {
	for _, pattern := range check.Matches {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("invalid regular expression %q: %w", pattern, err)
		}
	}
	return nil
}

func validateEnv(env map[string]string) error {
	for key, value := range env {
		if key == "" {
			return errors.New("environment variable name is empty")
		}
		if strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("environment variable %q contains NUL", key)
		}
	}
	return nil
}

func fromDalecCheck(check dalec.CheckOutput) CheckOutput {
	return CheckOutput{
		Equals:     check.Equals,
		Contains:   append([]string(nil), check.Contains...),
		Matches:    append([]string(nil), check.Matches...),
		StartsWith: check.StartsWith,
		EndsWith:   check.EndsWith,
		Empty:      check.Empty,
	}
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (c CheckOutput) configured() bool {
	return c.Equals != "" || len(c.Contains) != 0 || len(c.Matches) != 0 || c.StartsWith != "" || c.EndsWith != "" || c.Empty
}
