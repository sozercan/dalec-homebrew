package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRejectsMissingOrUnexpectedArguments(t *testing.T) {
	if err := run(context.Background(), nil, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing-argument error = %v", err)
	}
	if err := run(context.Background(), []string{"--request", "-", "--output", "out", "--evidence", "evidence", "extra"}, strings.NewReader("{}")); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("positional-argument error = %v", err)
	}
}

func TestRunRejectsInvalidRequestBeforeNetworking(t *testing.T) {
	err := run(context.Background(), []string{"--request", "-", "--output", "out", "--evidence", "evidence"}, strings.NewReader(`{"schema_version":"wrong"}`))
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("error = %v", err)
	}
}
