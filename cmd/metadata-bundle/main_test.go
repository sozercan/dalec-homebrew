package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

func TestRunRejectsInvalidArgumentsBeforeCapture(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(t.Context(), nil, &stderr); err == nil || !strings.Contains(err.Error(), "--output is required") {
		t.Fatalf("missing output error = %v", err)
	}
	stderr.Reset()
	if err := run(t.Context(), []string{"--output", "bundle", "extra"}, &stderr); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("positional argument error = %v", err)
	}
	stderr.Reset()
	if err := run(t.Context(), []string{"--output", "bundle", "--digest-output", "bundle/digest"}, &stderr); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("nested digest error = %v", err)
	}
}

func TestWriteBundleDirectoryCreatesExactBundleAndDigest(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "bundle")
	digestOutput := filepath.Join(root, "bundle.digest")
	bundle := commandTestBundle(t)
	if err := writeBundleDirectory(output, digestOutput, bundle); err != nil {
		t.Fatalf("writeBundleDirectory: %v", err)
	}

	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			t.Fatalf("entry %q type = %v, want regular file", entry.Name(), entry.Type())
		}
	}
	slices.Sort(names)
	wantNames := []string{
		metadata.BundleFormulaFilename,
		metadata.BundleManifestFilename,
		metadata.BundleMigrationsFilename,
	}
	slices.Sort(wantNames)
	if !slices.Equal(names, wantNames) {
		t.Fatalf("bundle entries = %v, want %v", names, wantNames)
	}

	manifest, err := bundle.CanonicalManifest()
	if err != nil {
		t.Fatal(err)
	}
	assertFileBytes(t, filepath.Join(output, metadata.BundleManifestFilename), manifest)
	assertFileBytes(t, filepath.Join(output, metadata.BundleFormulaFilename), bundle.Formula)
	assertFileBytes(t, filepath.Join(output, metadata.BundleMigrationsFilename), bundle.Migrations)
	digest, err := bundle.Digest()
	if err != nil {
		t.Fatal(err)
	}
	assertFileBytes(t, digestOutput, []byte(digest+"\n"))
}

func TestWriteBundleDirectoryRejectsOverwritesAndSymlinks(t *testing.T) {
	t.Run("existing output", func(t *testing.T) {
		root := t.TempDir()
		output := filepath.Join(root, "bundle")
		if err := os.Mkdir(output, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(output, "sentinel")
		if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeBundleDirectory(output, "", commandTestBundle(t)); err == nil || !strings.Contains(err.Error(), "create new") {
			t.Fatalf("error = %v", err)
		}
		assertFileBytes(t, sentinel, []byte("preserve"))
	})

	t.Run("output symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(root, "bundle")
		if err := os.Symlink(target, output); err != nil {
			t.Fatal(err)
		}
		if err := writeBundleDirectory(output, "", commandTestBundle(t)); err == nil {
			t.Fatal("writeBundleDirectory accepted output symlink")
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("symlink target entries = %v", entries)
		}
	})

	t.Run("existing digest", func(t *testing.T) {
		root := t.TempDir()
		output := filepath.Join(root, "bundle")
		digestOutput := filepath.Join(root, "digest")
		if err := os.WriteFile(digestOutput, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeBundleDirectory(output, digestOutput, commandTestBundle(t)); err == nil || !strings.Contains(err.Error(), "digest output") {
			t.Fatalf("error = %v", err)
		}
		assertFileBytes(t, digestOutput, []byte("preserve"))
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("incomplete output still exists: %v", err)
		}
	})

	t.Run("digest symlink", func(t *testing.T) {
		root := t.TempDir()
		output := filepath.Join(root, "bundle")
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		digestOutput := filepath.Join(root, "digest")
		if err := os.Symlink(target, digestOutput); err != nil {
			t.Fatal(err)
		}
		if err := writeBundleDirectory(output, digestOutput, commandTestBundle(t)); err == nil {
			t.Fatal("writeBundleDirectory accepted digest symlink")
		}
		assertFileBytes(t, target, []byte("preserve"))
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("incomplete output still exists: %v", err)
		}
	})
}

func TestWriteBundleDirectoryRejectsDigestInsideBundle(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "bundle")
	digestOutput := filepath.Join(output, "nested", "digest")
	if err := writeBundleDirectory(output, digestOutput, commandTestBundle(t)); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output created after validation failure: %v", err)
	}
}

func commandTestBundle(t *testing.T) *metadata.Bundle {
	t.Helper()
	formula := []byte("exact formula JWS bytes")
	migrations := []byte("exact migrations JWS bytes")
	generatedAt := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC).Format(time.RFC3339)
	return &metadata.Bundle{
		Manifest: metadata.BundleManifest{
			SchemaVersion: metadata.BundleSchema,
			Formula: metadata.BundleDocument{
				Filename:       metadata.BundleFormulaFilename,
				URL:            metadata.OfficialFormulaURL,
				GeneratedAt:    generatedAt,
				Size:           int64(len(formula)),
				EnvelopeDigest: sha256Digest(formula),
			},
			Migrations: metadata.BundleDocument{
				Filename:       metadata.BundleMigrationsFilename,
				URL:            metadata.OfficialMigrationsURL,
				GeneratedAt:    generatedAt,
				Size:           int64(len(migrations)),
				EnvelopeDigest: sha256Digest(migrations),
			},
		},
		Formula:    formula,
		Migrations: migrations,
	}
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
