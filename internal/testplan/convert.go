// Package testplan converts Dalec test specifications into the restricted,
// BuildKit-independent plans consumed by the runtime test runner.
package testplan

import (
	"errors"
	"fmt"

	"github.com/project-dalec/dalec"
	"github.com/sozercan/dalec-homebrew/internal/testrunner"
)

// FromDalec converts one public Dalec test spec to the restricted JSON plan
// used by the runtime runner. It rejects source mounts rather than silently
// dropping them.
func FromDalec(test *dalec.TestSpec) (testrunner.Plan, error) {
	if test == nil {
		return testrunner.Plan{}, errors.New("test is nil")
	}
	if len(test.Mounts) != 0 {
		return testrunner.Plan{}, fmt.Errorf("test %q: mounts are not supported", test.Name)
	}

	converted := testrunner.TestSpec{
		Name:  test.Name,
		Dir:   test.Dir,
		Env:   cloneMap(test.Env),
		Steps: make([]testrunner.TestStep, len(test.Steps)),
		Files: make(map[string]testrunner.FileCheckOutput, len(test.Files)),
	}
	for i, step := range test.Steps {
		converted.Steps[i] = testrunner.TestStep{
			Command: step.Command,
			Env:     cloneMap(step.Env),
			Stdin:   step.Stdin,
			Stdout:  fromDalecCheck(step.Stdout),
			Stderr:  fromDalecCheck(step.Stderr),
		}
	}
	for path, check := range test.Files {
		converted.Files[path] = testrunner.FileCheckOutput{
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

	plan := testrunner.Plan{SchemaVersion: testrunner.PlanSchemaVersion, Test: converted}
	if err := plan.Validate(); err != nil {
		return testrunner.Plan{}, err
	}
	return plan, nil
}

// FromDalecTests converts each Dalec TestSpec into an independent plan so
// callers can execute every test against its own isolated rootfs.
func FromDalecTests(tests []*dalec.TestSpec) ([]testrunner.Plan, error) {
	plans := make([]testrunner.Plan, 0, len(tests))
	for i, test := range tests {
		plan, err := FromDalec(test)
		if err != nil {
			return nil, fmt.Errorf("test %d: %w", i+1, err)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func fromDalecCheck(check dalec.CheckOutput) testrunner.CheckOutput {
	return testrunner.CheckOutput{
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
