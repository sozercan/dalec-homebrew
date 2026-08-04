package catalogservice

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

func TestCommandGeneratorCanonicalInputAndStrictOutput(t *testing.T) {
	request := testRequest(t)
	generated := testGeneratedSet(t)
	output, err := EncodeGeneratedSet(generated)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := catalog.CanonicalRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_CATALOG_GENERATOR_HELPER", "success")
	t.Setenv("CATALOG_GENERATOR_EXPECTED_INPUT", base64.StdEncoding.EncodeToString(canonical))
	t.Setenv("CATALOG_GENERATOR_OUTPUT", base64.StdEncoding.EncodeToString(output))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewCommandGenerator(CommandGeneratorConfig{Path: executable, Args: []string{"-test.run=^TestCommandGeneratorHelper$"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = generator.Close() })
	actual, err := generator.Generate(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual.Catalogs) != 1 || len(actual.Results) != 1 {
		t.Fatalf("generated=%+v", actual)
	}
}

func TestCommandGeneratorStableExitCodeAndOutputBound(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("stable exit", func(t *testing.T) {
		t.Setenv("GO_WANT_CATALOG_GENERATOR_HELPER", "invalid-tap")
		generator, err := NewCommandGenerator(CommandGeneratorConfig{Path: executable, Args: []string{"-test.run=^TestCommandGeneratorHelper$"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = generator.Close() })
		_, err = generator.Generate(context.Background(), testRequest(t))
		var failure *FailureError
		if !errors.As(err, &failure) || failure.Failure.Code != catalog.FailureInvalidTap {
			t.Fatalf("error=%T %v", err, err)
		}
	})
	t.Run("bounded stdout", func(t *testing.T) {
		t.Setenv("GO_WANT_CATALOG_GENERATOR_HELPER", "oversized")
		generator, err := NewCommandGenerator(CommandGeneratorConfig{Path: executable, Args: []string{"-test.run=^TestCommandGeneratorHelper$"}, MaxOutputBytes: 32})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = generator.Close() })
		_, err = generator.Generate(context.Background(), testRequest(t))
		var failure *FailureError
		if !errors.As(err, &failure) || failure.Failure.Code != catalog.FailurePolicy || !strings.Contains(failure.Error(), "limit") {
			t.Fatalf("error=%T %v", err, err)
		}
	})
}

func TestDecodeGeneratedSetRejectsDuplicateMembers(t *testing.T) {
	data, err := EncodeGeneratedSet(testGeneratedSet(t))
	if err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(string(data), `"schema_version":`, `"schema_version":"`+GeneratedSetSchemaVersion+`","schema_version":`, 1)
	if _, err := DecodeGeneratedSet([]byte(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate JSON member") {
		t.Fatalf("error=%v", err)
	}
}

func TestCommandGeneratorCloseRemovesPinnedExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewCommandGenerator(CommandGeneratorConfig{Path: executable})
	if err != nil {
		t.Fatal(err)
	}
	directory := generator.cleanupDir
	if directory == "" || filepath.Dir(generator.execPath) != directory {
		t.Fatalf("cleanup directory=%q exec path=%q", directory, generator.execPath)
	}
	if _, err := os.Lstat(generator.execPath); err != nil {
		t.Fatalf("inspect pinned executable: %v", err)
	}
	if err := generator.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned executable directory remains after Close: %v", err)
	}
	if err := generator.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_, err = generator.Generate(context.Background(), testRequest(t))
	var failure *FailureError
	if !errors.As(err, &failure) || failure.Failure.Code != catalog.FailureUnavailable {
		t.Fatalf("Generate after Close error=%T %v", err, err)
	}
}

func TestCommandGeneratorHelper(t *testing.T) {
	mode := os.Getenv("GO_WANT_CATALOG_GENERATOR_HELPER")
	if mode == "" {
		return
	}
	input, _ := io.ReadAll(os.Stdin)
	if expected := os.Getenv("CATALOG_GENERATOR_EXPECTED_INPUT"); expected != "" {
		decoded, _ := base64.StdEncoding.DecodeString(expected)
		if string(input) != string(decoded) {
			os.Exit(GeneratorExitPolicy)
		}
	}
	switch mode {
	case "success":
		output, _ := base64.StdEncoding.DecodeString(os.Getenv("CATALOG_GENERATOR_OUTPUT"))
		_, _ = os.Stdout.Write(output)
		os.Exit(0)
	case "invalid-tap":
		os.Exit(GeneratorExitInvalidTap)
	case "oversized":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 128))
		os.Exit(0)
	default:
		os.Exit(GeneratorExitUnavailable)
	}
}
