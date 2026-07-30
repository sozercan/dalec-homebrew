package testrunner

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/project-dalec/dalec"
)

func TestNewPlanPreservesDalecTestData(t *testing.T) {
	source := &dalec.TestSpec{
		Name: "complete",
		Dir:  "/work",
		Env:  map[string]string{"TEST": "value", "EMPTY": ""},
		Steps: []dalec.TestStep{{
			Command: "cat",
			Env:     map[string]string{"STEP": "override"},
			Stdin:   "input\x00data",
			Stdout: dalec.CheckOutput{
				Equals:     "input\x00data",
				Contains:   []string{"input", "data"},
				Matches:    []string{`input.*data`},
				StartsWith: "input",
				EndsWith:   "data",
			},
			Stderr: dalec.CheckOutput{Empty: true},
		}},
		Files: map[string]dalec.FileCheckOutput{
			"/work/result": {
				CheckOutput: dalec.CheckOutput{
					Contains: []string{"result"},
					Matches:  []string{`res.*`},
				},
				Permissions: 0o640,
				NoFollow:    true,
				LinkTarget:  "target",
			},
		},
	}

	plan, err := NewPlan(source)
	if err != nil {
		t.Fatal(err)
	}

	want := Plan{
		SchemaVersion: PlanSchemaVersion,
		Test: TestSpec{
			Name: "complete",
			Dir:  "/work",
			Env:  map[string]string{"TEST": "value", "EMPTY": ""},
			Steps: []TestStep{{
				Command: "cat",
				Env:     map[string]string{"STEP": "override"},
				Stdin:   "input\x00data",
				Stdout: CheckOutput{
					Equals:     "input\x00data",
					Contains:   []string{"input", "data"},
					Matches:    []string{`input.*data`},
					StartsWith: "input",
					EndsWith:   "data",
				},
				Stderr: CheckOutput{Empty: true},
			}},
			Files: map[string]FileCheckOutput{
				"/work/result": {
					CheckOutput: CheckOutput{
						Contains: []string{"result"},
						Matches:  []string{`res.*`},
					},
					Permissions: 0o640,
					NoFollow:    true,
					LinkTarget:  "target",
				},
			},
		},
	}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan mismatch\n got: %#v\nwant: %#v", plan, want)
	}

	// Conversion must not retain mutable maps or slices from the Dalec spec.
	source.Env["TEST"] = "changed"
	source.Steps[0].Env["STEP"] = "changed"
	source.Steps[0].Stdout.Contains[0] = "changed"
	file := source.Files["/work/result"]
	file.Matches[0] = "changed"
	source.Files["/work/result"] = file
	if !reflect.DeepEqual(plan, want) {
		t.Fatal("converted plan changed after mutating source")
	}

	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePlan(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", decoded, want)
	}
}

func TestNewPlanRejectsMountsAndNilTests(t *testing.T) {
	_, err := NewPlan(&dalec.TestSpec{
		Name:   "mounted",
		Mounts: []dalec.SourceMount{{Dest: "/src"}},
	})
	if err == nil || !strings.Contains(err.Error(), "mounts are not supported") {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = NewPlan(nil)
	if err == nil || !strings.Contains(err.Error(), "is nil") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewPlansCreatesIndependentPlans(t *testing.T) {
	plans, err := NewPlans([]*dalec.TestSpec{{Name: "first"}, {Name: "second"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Test.Name != "first" || plans[1].Test.Name != "second" {
		t.Fatalf("plans=%#v", plans)
	}
}

func TestDecodePlanStrictnessAndCompatibility(t *testing.T) {
	t.Run("missing schema defaults to current", func(t *testing.T) {
		plan, err := DecodePlan(strings.NewReader(`{"test":{"name":"ok"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if plan.SchemaVersion != PlanSchemaVersion {
			t.Fatalf("schema=%q", plan.SchemaVersion)
		}
	})

	t.Run("unknown fields including mounts are rejected", func(t *testing.T) {
		_, err := DecodePlan(strings.NewReader(`{"test":{"name":"bad","mounts":[]}}`))
		if err == nil || !strings.Contains(err.Error(), "unknown field \"mounts\"") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("trailing JSON value is rejected", func(t *testing.T) {
		_, err := DecodePlan(strings.NewReader(`{"test":{"name":"ok"}} {"test":{"name":"other"}}`))
		if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unsupported schema is rejected", func(t *testing.T) {
		_, err := DecodePlan(strings.NewReader(`{"schema_version":"testplan/v2","test":{"name":"ok"}}`))
		if err == nil || !strings.Contains(err.Error(), "unsupported test plan schema") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestPlanValidateRejectsInvalidDataBeforeExecution(t *testing.T) {
	tests := []struct {
		name string
		plan Plan
		want string
	}{
		{
			name: "missing test name",
			plan: Plan{},
			want: "test name is required",
		},
		{
			name: "missing command",
			plan: Plan{Test: TestSpec{
				Name:  "x",
				Steps: []TestStep{{}},
			}},
			want: "command is required",
		},
		{
			name: "invalid test env",
			plan: Plan{Test: TestSpec{
				Name: "x",
				Env:  map[string]string{"BAD=KEY": "x"},
			}},
			want: "invalid environment variable name",
		},
		{
			name: "invalid step env",
			plan: Plan{Test: TestSpec{
				Name: "x",
				Steps: []TestStep{{
					Command: "true",
					Env:     map[string]string{"": "x"},
				}},
			}},
			want: "environment variable name is empty",
		},
		{
			name: "invalid stdout regex",
			plan: Plan{Test: TestSpec{
				Name: "x",
				Steps: []TestStep{{
					Command: "true",
					Stdout:  CheckOutput{Matches: []string{"["}},
				}},
			}},
			want: "invalid regular expression",
		},
		{
			name: "invalid file regex",
			plan: Plan{Test: TestSpec{
				Name: "x",
				Files: map[string]FileCheckOutput{
					"/x": {CheckOutput: CheckOutput{Matches: []string{"("}}},
				},
			}},
			want: "invalid regular expression",
		},
		{
			name: "non permission mode bits",
			plan: Plan{Test: TestSpec{
				Name: "x",
				Files: map[string]FileCheckOutput{
					"/x": {Permissions: fs.ModeDir | 0o755},
				},
			}},
			want: "non-permission bits",
		},
		{
			name: "not exist conflict",
			plan: Plan{Test: TestSpec{
				Name: "x",
				Files: map[string]FileCheckOutput{
					"/x": {NotExist: true, CheckOutput: CheckOutput{Empty: true}},
				},
			}},
			want: "not_exist cannot be combined",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
