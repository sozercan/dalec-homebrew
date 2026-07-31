package testrunner

import (
	"io/fs"
	"strings"
	"testing"
)

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
