package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalogkeys"
	"github.com/sozercan/dalec-homebrew/internal/release"
)

func TestRunWritesCanonicalBindingsAndPolicy(t *testing.T) {
	dir := t.TempDir()
	publicKey := writePublicKey(t, dir)
	output := filepath.Join(dir, "bindings.json")
	policyOutput := filepath.Join(dir, "key-policy.json")
	args := validArgs(publicKey, output)
	args = append(args, "--policy-output", policyOutput)

	var stdout, stderr bytes.Buffer
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	bindingsData, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var bindings release.V2Bindings
	if err := json.Unmarshal(bindingsData, &bindings); err != nil {
		t.Fatal(err)
	}
	canonical, err := release.CanonicalV2Bindings(&bindings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bindingsData, canonical) {
		t.Fatal("bindings output is not canonical")
	}
	policyData, err := os.ReadFile(policyOutput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalogkeys.Decode(policyData); err != nil {
		t.Fatalf("decode policy output: %v", err)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(bindings.IngestionJWSKeyPolicyBase64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, policyData) {
		t.Fatal("policy output differs from embedded base64 policy")
	}
}

func TestRunWritesBindingsToStdout(t *testing.T) {
	publicKey := writePublicKey(t, t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := run(validArgs(publicKey, "-"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var bindings release.V2Bindings
	if err := json.Unmarshal(stdout.Bytes(), &bindings); err != nil {
		t.Fatal(err)
	}
	canonical, err := release.CanonicalV2Bindings(&bindings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), canonical) {
		t.Fatal("stdout is not canonical")
	}
}

func TestRunRequiresEveryInput(t *testing.T) {
	publicKey := writePublicKey(t, t.TempDir())
	args := validArgs(publicKey, "-")
	for i := 0; i < len(args); i += 2 {
		name := args[i]
		t.Run(strings.TrimPrefix(name, "--"), func(t *testing.T) {
			missing := append([]string(nil), args...)
			missing = append(missing[:i], missing[i+2:]...)
			if err := run(missing, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), name+" is required") {
				t.Fatalf("err=%v, want required %s", err, name)
			}
		})
	}
}

func TestRunHelpListsExactFlags(t *testing.T) {
	var stderr bytes.Buffer
	err := run([]string{"--help"}, &bytes.Buffer{}, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err=%v", err)
	}
	for _, name := range []string{"--key-id", "--public-key", "--catalog-service-digest", "--catalog-extractor-digest", "--output", "--policy-output"} {
		if !strings.Contains(stderr.String(), "  -"+strings.TrimPrefix(name, "--")) {
			t.Fatalf("help does not contain %s:\n%s", name, stderr.String())
		}
	}
	for _, legacy := range []string{"--public-key-pem", "--key-policy-output"} {
		if strings.Contains(stderr.String(), "  -"+strings.TrimPrefix(legacy, "--")) {
			t.Fatalf("help contains legacy flag %s", legacy)
		}
	}
}

func TestReadBoundedFileRejectsOversizedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.pem")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, catalogkeys.MaxPolicyBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(path, catalogkeys.MaxPolicyBytes); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err=%v", err)
	}
}

func validArgs(publicKey, output string) []string {
	return []string{
		"--key-id", "catalog-e2e",
		"--public-key", publicKey,
		"--catalog-service-digest", "sha256:" + strings.Repeat("a", 64),
		"--catalog-extractor-digest", "sha256:" + strings.Repeat("b", 64),
		"--output", output,
	}
}

func writePublicKey(t *testing.T, dir string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "catalog-public.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunBuildLocalBindings(t *testing.T) {
	output := filepath.Join(t.TempDir(), "bindings.json")
	ref := "ghcr.io/example/catalog-extractor@sha256:" + strings.Repeat("c", 64)
	if err := run([]string{"--catalog-extractor-ref", ref, "--output", output}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var bindings release.V2Bindings
	if err := json.Unmarshal(data, &bindings); err != nil {
		t.Fatal(err)
	}
	if bindings.CatalogExtractorRef != ref || bindings.IngestionJWSKeyPolicyDigest != "" {
		t.Fatalf("bindings = %+v", bindings)
	}
}
