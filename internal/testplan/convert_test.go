package testplan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/project-dalec/dalec"
	"github.com/sozercan/dalec-homebrew/internal/testrunner"
)

func TestFromDalecPreservesTestData(t *testing.T) {
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

	plan, err := FromDalec(source)
	if err != nil {
		t.Fatal(err)
	}

	want := testrunner.Plan{
		SchemaVersion: testrunner.PlanSchemaVersion,
		Test: testrunner.TestSpec{
			Name: "complete",
			Dir:  "/work",
			Env:  map[string]string{"TEST": "value", "EMPTY": ""},
			Steps: []testrunner.TestStep{{
				Command: "cat",
				Env:     map[string]string{"STEP": "override"},
				Stdin:   "input\x00data",
				Stdout: testrunner.CheckOutput{
					Equals:     "input\x00data",
					Contains:   []string{"input", "data"},
					Matches:    []string{`input.*data`},
					StartsWith: "input",
					EndsWith:   "data",
				},
				Stderr: testrunner.CheckOutput{Empty: true},
			}},
			Files: map[string]testrunner.FileCheckOutput{
				"/work/result": {
					CheckOutput: testrunner.CheckOutput{
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
	decoded, err := testrunner.DecodePlan(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", decoded, want)
	}
}

func TestFromDalecRejectsMountsAndNilTests(t *testing.T) {
	_, err := FromDalec(&dalec.TestSpec{
		Name:   "mounted",
		Mounts: []dalec.SourceMount{{Dest: "/src"}},
	})
	if err == nil || !strings.Contains(err.Error(), "mounts are not supported") {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = FromDalec(nil)
	if err == nil || !strings.Contains(err.Error(), "is nil") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFromDalecTestsCreatesIndependentPlans(t *testing.T) {
	plans, err := FromDalecTests([]*dalec.TestSpec{{Name: "first"}, {Name: "second"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Test.Name != "first" || plans[1].Test.Name != "second" {
		t.Fatalf("plans=%#v", plans)
	}
}
