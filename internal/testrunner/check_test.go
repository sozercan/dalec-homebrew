package testrunner

import (
	"strings"
	"testing"
)

func TestCheckOutputAllAssertions(t *testing.T) {
	check := CheckOutput{
		Equals:     "prefix value=42 suffix",
		Contains:   []string{"value", "42"},
		Matches:    []string{`value=\d+`, `^prefix`},
		StartsWith: "prefix",
		EndsWith:   "suffix",
	}
	if err := checkOutput("stdout", []byte("prefix value=42 suffix"), check); err != nil {
		t.Fatal(err)
	}
	if err := checkOutput("stderr", nil, CheckOutput{Empty: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckOutputFailures(t *testing.T) {
	tests := []struct {
		name   string
		actual string
		check  CheckOutput
		want   string
	}{
		{name: "equals", actual: "actual", check: CheckOutput{Equals: "expected"}, want: "equals"},
		{name: "contains", actual: "actual", check: CheckOutput{Contains: []string{"missing"}}, want: "contains"},
		{name: "matches", actual: "42", check: CheckOutput{Matches: []string{`^[a-z]+$`}}, want: "matches"},
		{name: "prefix", actual: "actual", check: CheckOutput{StartsWith: "pre"}, want: "starts_with"},
		{name: "suffix", actual: "actual", check: CheckOutput{EndsWith: "post"}, want: "ends_with"},
		{name: "empty", actual: "not empty", check: CheckOutput{Empty: true}, want: "empty"},
		{name: "bad regex", actual: "x", check: CheckOutput{Matches: []string{"["}}, want: "missing closing"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOutput("subject", []byte(tc.actual), tc.check)
			if err == nil || !strings.Contains(err.Error(), "subject") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCheckOutputEvaluatesEveryConfiguredAssertion(t *testing.T) {
	err := checkOutput("stdout", []byte("actual"), CheckOutput{
		Equals:     "expected",
		Contains:   []string{"missing"},
		StartsWith: "prefix",
		EndsWith:   "suffix",
		Empty:      true,
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"equals", "contains", "starts_with", "ends_with", "empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not include %q", err, want)
		}
	}
}
